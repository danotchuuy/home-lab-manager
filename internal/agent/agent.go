package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// Config is how "hlm agent" runs.
type Config struct {
	// Server is hlm's address, https://hlm.example.com.
	Server string
	// Token enrols the agent the first time (from hlm's Hosts page).
	Token string
	// DataDir keeps the agent's credential (agent.json).
	DataDir string
	// Socket is Docker's socket on this host.
	Socket string
	// CAFile trusts a certificate authority for hlm's certificate, besides
	// the system's.
	CAFile string
	// AllowHTTP allows a plain http:// server (on a trusted network only:
	// whoever can see or change the traffic can control this host's Docker).
	AllowHTTP bool
	// ProcPath and RootPath are where the host's procfs and root
	// filesystem are, for its CPU, memory, network and disk metrics (as
	// HLM_HOST_PROC and HLM_HOST_ROOT for hlm).
	ProcPath, RootPath string
	Version            string
	Log                *slog.Logger
}

// HostMetricsPath is where the agent answers with the host's metrics,
// beside the Docker API it passes on.
const HostMetricsPath = "/_hlm/host"

// ErrRevoked is returned when hlm no longer knows the agent.
var ErrRevoked = errors.New("hlm refused the agent's credential: the host was removed from hlm. To add it again, run the agent with a new enrolment token from hlm's Hosts page")

// firstRetry is how long the agent waits before trying hlm again; it
// doubles each time, up to a minute.
var firstRetry = time.Second

// unreachable is an error from not reaching hlm, or hlm not answering
// properly (down, starting, or behind a proxy that says so): worth trying
// again, unlike hlm refusing.
type unreachable struct{ err error }

func (e unreachable) Error() string { return e.err.Error() }
func (e unreachable) Unwrap() error { return e.err }

// credentials are what the agent keeps.
type credentials struct {
	Server     string `json:"server"`
	Host       string `json:"host"`
	Credential string `json:"credential"`
	// Token is the hash of the enrolment token used: given another one,
	// the agent enrols again (the host was added to hlm again, say).
	Token string `json:"token"`
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(h[:])
}

// EnrolRequest and EnrolResponse are the enrolment call.
type EnrolRequest struct {
	Token   string `json:"token"`
	Version string `json:"version,omitempty"`
}

type EnrolResponse struct {
	Host       string `json:"host"`
	Credential string `json:"credential"`
}

func (c *Config) file() string { return filepath.Join(c.DataDir, "agent.json") }

// checkServer tidies and checks hlm's address.
func checkServer(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("hlm's address is like https://hlm.example.com, not %q", raw)
	}
	if u.Scheme == "http" && !allowHTTP {
		return "", errors.New("hlm's address is http://, so anyone who can see the traffic could take this host's Docker over: use https://, or --allow-http on a network you trust")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func (c *Config) httpClient() (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s has no PEM certificates", c.CAFile)
		}
		tc.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tc,
		TLSHandshakeTimeout: 15 * time.Second,
		// The WebSocket needs HTTP/1.1.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}}, nil
}

// Run enrols the agent if it hasn't been, then keeps it connected to hlm,
// serving the host's Docker, until ctx ends or hlm refuses it.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Socket == "" {
		cfg.Socket = "/var/run/docker.sock"
	}
	if cfg.DataDir == "" {
		return errors.New("the agent needs a folder to keep its credential in (--data)")
	}
	client, err := cfg.httpClient()
	if err != nil {
		return err
	}
	// Enrolling needs hlm: if it's down (both machines starting after a
	// power cut, say), keep trying, as connecting does.
	var creds credentials
	backoff := firstRetry
	for {
		creds, err = cfg.credentials(ctx, client)
		if err == nil {
			break
		}
		if !errors.As(err, new(unreachable)) {
			return err
		}
		cfg.Log.Warn("can't enrol with hlm yet; trying again", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	if _, err := checkServer(creds.Server, cfg.AllowHTTP); err != nil {
		return err
	}
	cfg.Log.Info("agent starting", "server", creds.Server, "host", creds.Host, "socket", cfg.Socket)
	metrics := hostmetrics.New(hostmetrics.Config{ProcPath: cfg.ProcPath, RootPath: cfg.RootPath}, "")
	if snap := metrics.Snapshot(); snap.Reason != "" {
		cfg.Log.Warn("host metrics aren't available", "reason", snap.Reason)
	}
	go metrics.Run(ctx)
	proxy := Handler(DockerProxy(cfg.Socket, cfg.Log), metrics)
	backoff = firstRetry
	for {
		started := time.Now()
		err := connect(ctx, client, creds, cfg.Version, proxy, cfg.Log)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRevoked) {
			return err
		}
		if time.Since(started) > time.Minute {
			backoff = firstRetry
		}
		cfg.Log.Warn("not connected to hlm; trying again", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// credentials loads the saved credential, or enrols with the token.
func (c *Config) credentials(ctx context.Context, client *http.Client) (credentials, error) {
	var creds credentials
	data, err := os.ReadFile(c.file())
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &creds); err != nil || creds.Credential == "" {
			return creds, fmt.Errorf("%s is damaged; remove it and enrol again", c.file())
		}
		if c.Token == "" || tokenHash(c.Token) == creds.Token {
			return creds, nil
		}
		c.Log.Info("given a new enrolment token: enrolling again")
	case !errors.Is(err, os.ErrNotExist):
		return creds, err
	}
	if c.Token == "" || c.Server == "" {
		return creds, errors.New("not enrolled yet: give hlm's address (--server) and an enrolment token (--token) from hlm's Hosts page")
	}
	server, err := checkServer(c.Server, c.AllowHTTP)
	if err != nil {
		return creds, err
	}
	body, _ := json.Marshal(EnrolRequest{Token: strings.TrimSpace(c.Token), Version: c.Version})
	// A hung hlm counts as a down one.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/api/agent/enrol", bytes.NewReader(body))
	if err != nil {
		return creds, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return creds, unreachable{fmt.Errorf("enrol with hlm: %w", err)}
	}
	defer resp.Body.Close()
	data, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		err := fmt.Errorf("enrol with hlm: %s", e.Error)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return creds, unreachable{err}
		}
		return creds, err
	}
	var out EnrolResponse
	if err := json.Unmarshal(data, &out); err != nil || out.Credential == "" {
		return creds, errors.New("enrol with hlm: unexpected answer")
	}
	creds = credentials{Server: server, Host: out.Host, Credential: out.Credential, Token: tokenHash(c.Token)}
	data, _ = json.MarshalIndent(creds, "", "  ")
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return creds, err
	}
	tmp := c.file() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return creds, err
	}
	if err := os.Rename(tmp, c.file()); err != nil {
		return creds, err
	}
	c.Log.Info("enrolled with hlm", "host", out.Host)
	return creds, nil
}

// Dial opens the WebSocket to hlm, signed in with the credential.
func Dial(ctx context.Context, client *http.Client, server, credential, version string) (io.ReadWriteCloser, error) {
	key := newKey()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/agent/connect", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Protocol", Protocol)
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-HLM-Agent-Version", version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, ErrRevoked
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("hlm answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		resp.Body.Close()
		return nil, errors.New("what answered isn't hlm's agent endpoint (a proxy in the way?)")
	}
	return newWSConn(bufio.NewReader(rwc), rwc, rwc, true), nil
}

// connect serves Docker to hlm over one connection, until it drops.
func connect(ctx context.Context, client *http.Client, creds credentials, version string, proxy http.Handler, log *slog.Logger) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, err := Dial(dctx, client, creds.Server, creds.Credential, version)
	cancel()
	if err != nil {
		return err
	}
	mux, err := yamux.Server(ws, muxConfig())
	if err != nil {
		ws.Close()
		return err
	}
	log.Info("connected to hlm", "server", creds.Server)
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second, ErrorLog: quietLog()}
	stop := context.AfterFunc(ctx, func() { mux.Close() })
	defer stop()
	err = srv.Serve(mux)
	mux.Close()
	if ctx.Err() != nil {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "session shutdown") {
		err = errors.New("the connection dropped")
	}
	return err
}

// DockerProxy passes the allowed Docker API calls on to the socket.
func DockerProxy(socket string, log *slog.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "docker", "docker"
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			MaxIdleConns:    10,
			IdleConnTimeout: 90 * time.Second,
		},
		// Logs, events and pulls stream.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				log.Warn("Docker call failed", "method", r.Method, "path", r.URL.Path, "err", err)
			}
			writeDockerError(w, http.StatusBadGateway, "the agent can't reach Docker at "+socket+": "+err.Error())
		},
		ErrorLog: quietLog(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Allowed(r.Method, r.URL.Path) {
			log.Warn("refused a Docker call that isn't allowed", "method", r.Method, "path", r.URL.Path)
			writeDockerError(w, http.StatusForbidden, "hlm's agent doesn't pass on "+r.Method+" "+r.URL.Path)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// Handler is what the agent serves hlm: the host's metrics, and the
// Docker API calls it passes on.
func Handler(docker http.Handler, metrics *hostmetrics.Collector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HostMetricsPath && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metrics.Snapshot())
			return
		}
		docker.ServeHTTP(w, r)
	})
}

// writeDockerError answers as Docker does, so hlm shows the message.
func writeDockerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func quietLog() *log.Logger { return log.New(io.Discard, "", 0) }
