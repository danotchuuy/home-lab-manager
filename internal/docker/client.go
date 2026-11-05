// Package docker is a small client for the Docker Engine API. It talks to the
// daemon over a unix socket or TCP (optionally with TLS) using only the
// standard library, and covers just what the game server manager needs.
package docker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxAPIVersion is the newest API version this client is written against. The
// client negotiates down if the daemon is older.
const maxAPIVersion = "1.47"

// Client is a Docker Engine API client.
type Client struct {
	http    *http.Client
	base    string
	host    string
	mu      sync.Mutex
	version string // negotiated API version; empty until the first successful ping
	// credentials finds the registry login for an image; see SetCredentials.
	credentials func(ctx context.Context, ref string) (RegistryAuth, bool)
}

// DefaultHost is the local Docker socket, used when no host is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// Config says how to reach the Docker daemon.
type Config struct {
	// Host is a DOCKER_HOST-style address: unix:///var/run/docker.sock for a
	// local socket, or tcp://host:2376 for a remote daemon. Empty means
	// DefaultHost.
	Host string
	// TLSVerify enables TLS and verifies the daemon's certificate against
	// ca.pem in CertPath (or the system roots if there is no ca.pem).
	TLSVerify bool
	// CertPath is a directory holding ca.pem, cert.pem and key.pem. Setting it
	// enables TLS; cert.pem and key.pem, if present, are sent as the client
	// certificate.
	CertPath string
	// TLS, when set, is used for a tcp:// host instead of the files in
	// CertPath (for certificates kept in hlm's settings).
	TLS *tls.Config
	// Dial, when set, opens connections to the daemon instead of Host's
	// address, such as a socket reached over SSH. Host only names it then.
	Dial func(ctx context.Context) (net.Conn, error)
}

// ConfigFromEnv reads the same environment variables as the docker CLI:
// DOCKER_HOST, DOCKER_TLS_VERIFY and DOCKER_CERT_PATH.
func ConfigFromEnv() Config {
	c := Config{
		Host:      os.Getenv("DOCKER_HOST"),
		TLSVerify: os.Getenv("DOCKER_TLS_VERIFY") != "",
		CertPath:  os.Getenv("DOCKER_CERT_PATH"),
	}
	if c.TLSVerify && c.CertPath == "" {
		// Like the docker CLI, default to ~/.docker when verification is on.
		if home, err := os.UserHomeDir(); err == nil {
			c.CertPath = filepath.Join(home, ".docker")
		}
	}
	return c
}

// UsesTLS reports whether connections to a TCP daemon are encrypted.
func (c Config) UsesTLS() bool {
	return c.TLS != nil || c.TLSVerify || c.CertPath != "" || strings.HasPrefix(c.Host, "https://")
}

// NewClient creates a client for the daemon described by cfg.
func NewClient(cfg Config) (*Client, error) {
	host := cfg.Host
	if host == "" {
		host = DefaultHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse docker host: %w", err)
	}

	transport := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 90 * time.Second,
	}
	var base string
	switch {
	case cfg.Dial != nil:
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return cfg.Dial(ctx) }
		base = "http://docker"
	case u.Scheme == "unix":
		// TLS settings don't apply to a local socket and are ignored, as the
		// docker CLI does.
		sock := u.Path
		if sock == "" {
			return nil, fmt.Errorf("docker host %q has no socket path", host)
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	case u.Scheme == "tcp" || u.Scheme == "http" || u.Scheme == "https":
		if u.Host == "" {
			return nil, fmt.Errorf("docker host %q has no address", host)
		}
		if cfg.UsesTLS() {
			tlsConfig := cfg.TLS
			if tlsConfig == nil {
				if tlsConfig, err = loadTLS(cfg); err != nil {
					return nil, err
				}
			}
			transport.TLSClientConfig = tlsConfig
			base = "https://" + u.Host
		} else {
			base = "http://" + u.Host
		}
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q (use unix:// or tcp://)", u.Scheme)
	}

	return &Client{http: &http.Client{Transport: transport}, base: base, host: host}, nil
}

// Close closes idle connections to the daemon.
func (c *Client) Close() {
	if t, ok := c.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// Host returns the daemon address the client connects to.
func (c *Client) Host() string {
	return c.host
}

func loadTLS(cfg Config) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CertPath == "" {
		return tc, nil
	}

	caFile := filepath.Join(cfg.CertPath, "ca.pem")
	if pem, err := os.ReadFile(caFile); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		tc.RootCAs = pool
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	certFile := filepath.Join(cfg.CertPath, "cert.pem")
	keyFile := filepath.Join(cfg.CertPath, "key.pem")
	if _, err := os.Stat(certFile); err == nil {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}

	// Without DOCKER_TLS_VERIFY the connection is encrypted but the daemon's
	// certificate is not checked, matching the docker CLI.
	tc.InsecureSkipVerify = !cfg.TLSVerify
	return tc, nil
}

// APIError is returned when the daemon responds with a non-2xx status.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker: %s (status %d)", e.Message, e.Status)
}

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// negotiate picks the API version to use, based on the daemon's ping header.
// If the daemon cannot be reached it retries on the next call.
func (c *Client) negotiate(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_ping", nil)
	if err != nil {
		return maxAPIVersion
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return maxAPIVersion
	}
	resp.Body.Close()
	c.version = maxAPIVersion
	if v := resp.Header.Get("API-Version"); v != "" && versionLess(v, maxAPIVersion) {
		c.version = v
	}
	return c.version
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// do sends a request with an optional JSON body and returns the response if
// it has a 2xx status. The caller must close the body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	if body == nil {
		return c.doRaw(ctx, method, path, query, nil, "")
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.doRaw(ctx, method, path, query, bytes.NewReader(buf), "application/json")
}

// doRaw is do with a body of any content type.
func (c *Client) doRaw(ctx context.Context, method, path string, query url.Values, r io.Reader, contentType string) (*http.Response, error) {
	return c.doHeaders(ctx, method, path, query, r, contentType, nil)
}

// doHeaders is doRaw with extra request headers.
func (c *Client) doHeaders(ctx context.Context, method, path string, query url.Values, r io.Reader, contentType string, header http.Header) (*http.Response, error) {
	version := c.negotiate(ctx)
	u := c.base + "/v" + version + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, &APIError{Status: resp.StatusCode, Message: apiMessage(data)}
}

// apiMessage is the message in an error response from the daemon.
func apiMessage(data []byte) string {
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(data))
	}
	return msg.Message
}

// doJSON sends a request and decodes a JSON response into out (if non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Version describes the daemon.
type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	OS         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Version returns the daemon version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.doJSON(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v, err
}
