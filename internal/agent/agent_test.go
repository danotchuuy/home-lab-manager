package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestAllowed(t *testing.T) {
	for _, tt := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/_ping", true},
		{"HEAD", "/_ping", true},
		{"GET", "/v1.47/version", true},
		{"GET", "/v1.47/containers/json", true},
		{"POST", "/v1.47/containers/create", true},
		{"GET", "/v1.47/containers/abc123/logs", true},
		{"PUT", "/v1.47/containers/minecraft/archive", true},
		{"POST", "/v1.47/containers/minecraft/attach", true},
		{"DELETE", "/v1.47/containers/minecraft", true},
		{"POST", "/v1.47/exec/0123abc/start", true},
		{"GET", "/v1.47/images/ghcr.io/team/app:1.2/json", true},
		{"GET", "/v1.47/distribution/itzg/minecraft-server:latest/json", true},
		{"DELETE", "/v1.47/images/sha256:0123abcd", true},
		{"POST", "/v1.47/images/create", true},
		{"POST", "/v1.47/networks/lan/connect", true},
		{"GET", "/v1.47/events", true},
		{"POST", "/v1.47/build/prune", true},
		// Not what hlm does.
		{"POST", "/v1.47/swarm/init", false},
		{"GET", "/v1.47/secrets", false},
		{"POST", "/v1.47/plugins/pull", false},
		{"POST", "/v1.47/build", false},
		{"POST", "/v1.47/commit", false},
		{"GET", "/v1.47/containers/x/export", false},
		{"POST", "/v1.47/images/load", false},
		{"POST", "/v1.47/containers/json", false},
		{"GET", "/v1.47/containers/create", false},
		{"DELETE", "/v1.47/containers/x/json", false},
		// Tricks.
		{"GET", "/v1.47/containers/x/../../swarm", false},
		{"GET", "/v1.47/containers//json", false},
		{"GET", "/v1.47/containers/./json", false},
		{"GET", "/", false},
		{"GET", "", false},
	} {
		if got := Allowed(tt.method, tt.path); got != tt.want {
			t.Errorf("Allowed(%s %s) = %v", tt.method, tt.path, got)
		}
	}
}

func TestCredential(t *testing.T) {
	c := Credential("nas-2", "abc_def")
	if h, s, ok := ParseCredential(c); !ok || h != "nas-2" || s != "abc_def" {
		t.Errorf("parsed %q %q %v", h, s, ok)
	}
	for _, bad := range []string{"", "hlma_", "hlma_nas", "hlma__x", "hlma_nas_", "nope_nas_x"} {
		if _, _, ok := ParseCredential(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

// fakeDocker serves a few Docker API calls on a Unix socket.
type fakeDocker struct {
	mu    sync.Mutex
	stdin []string
	calls []string
}

func (f *fakeDocker) serve(t *testing.T) string {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.47")
		io.WriteString(w, "OK")
	})
	mux.HandleFunc("GET /v1.47/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Version": "29.0.0", "ApiVersion": "1.47", "Os": "linux", "Arch": "arm64"})
	})
	mux.HandleFunc("GET /v1.47/events", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"start","id":"abc"}`+"\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /v1.47/containers/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		rw.Flush()
		data, _ := io.ReadAll(rw)
		f.mu.Lock()
		f.stdin = append(f.stdin, r.PathValue("id")+":"+string(data))
		f.mu.Unlock()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An agent enrols, connects out to the hub, and hlm's Docker client works
// through it: plain calls, streams, and the attach upgrade; calls outside
// the allow-list are refused at the agent; a revoked agent stops.
func TestAgentEndToEnd(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	fd := &fakeDocker{}
	sock := fd.serve(t)

	hub := NewHub(log)
	var valid atomic.Bool
	valid.Store(true)
	hub.Authenticate = func(host, secret string) bool { return valid.Load() && host == "nas" && secret == "s3cret" }
	enrolled := 0
	mux := http.NewServeMux()
	mux.Handle("GET /api/agent/connect", hub)
	mux.HandleFunc("POST /api/agent/enrol", func(w http.ResponseWriter, r *http.Request) {
		var in EnrolRequest
		json.NewDecoder(r.Body).Decode(&in)
		if in.Token != "tok" || enrolled > 0 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"this enrolment token isn't valid"}`)
			return
		}
		enrolled++
		json.NewEncoder(w).Encode(EnrolResponse{Host: "nas", Credential: Credential("nas", "s3cret")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data := t.TempDir()
	// Plain HTTP is refused unless allowed.
	if err := Run(context.Background(), Config{Server: srv.URL, Token: "tok", DataDir: data, Socket: sock, Log: log}); err == nil || !strings.Contains(err.Error(), "--allow-http") {
		t.Fatalf("plain http: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	cfg := Config{Server: srv.URL + "/", Token: "tok", DataDir: data, Socket: sock, AllowHTTP: true, Version: "1.2.3", Log: log}
	go func() { done <- Run(ctx, cfg) }()
	waitFor(t, "the agent to connect", func() bool { return hub.Status("nas").Connected })
	if st := hub.Status("nas"); st.Version != "1.2.3" {
		t.Errorf("status = %+v", st)
	}
	info, err := os.Stat(filepath.Join(data, "agent.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", info, err)
	}

	client, err := docker.NewClient(docker.Config{Host: "agent://nas", Dial: func(ctx context.Context) (net.Conn, error) { return hub.Dial(ctx, "nas") }})
	if err != nil {
		t.Fatal(err)
	}
	v, err := client.Version(ctx)
	if err != nil || v.Version != "29.0.0" || v.Arch != "arm64" {
		t.Fatalf("version %+v %v", v, err)
	}
	// The attach upgrade, as the console uses it.
	if err := client.WriteStdin(ctx, "minecraft", []byte("say hi\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stdin", func() bool {
		fd.mu.Lock()
		defer fd.mu.Unlock()
		return len(fd.stdin) == 1 && fd.stdin[0] == "minecraft:say hi\n"
	})

	// A stream arrives as it's sent.
	raw := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return hub.Dial(ctx, "nas") }}}
	ectx, ecancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(ectx, "GET", "http://docker/v1.47/events", nil)
	resp, err := raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	ecancel()
	resp.Body.Close()
	if !strings.Contains(line, `"start"`) {
		t.Errorf("event %q", line)
	}

	// Outside the allow-list: refused by the agent, never reaching Docker.
	req, _ = http.NewRequest("POST", "http://docker/v1.47/swarm/init", strings.NewReader("{}"))
	resp, err = raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "doesn't pass on POST /v1.47/swarm/init") {
		t.Errorf("swarm: %d %s", resp.StatusCode, body)
	}
	fd.mu.Lock()
	if len(fd.calls) != 0 {
		t.Errorf("reached Docker: %v", fd.calls)
	}
	fd.mu.Unlock()

	// Dropped (hlm restarted, say): it comes back with the saved credential.
	hub.Drop("nas")
	waitFor(t, "the agent to reconnect", func() bool { return hub.Status("nas").Connected })
	if enrolled != 1 {
		t.Errorf("enrolled %d times", enrolled)
	}

	// Revoked: it stops.
	valid.Store(false)
	hub.Drop("nas")
	select {
	case err := <-done:
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("revoked: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept going after it was revoked")
	}

	// Started again with the same token (the container restarted): it
	// doesn't enrol again, and is still refused. With a new token (the host
	// was added again): it enrols again.
	if err := Run(ctx, cfg); !errors.Is(err, ErrRevoked) || enrolled != 1 {
		t.Errorf("restarted: %v, enrolled %d", err, enrolled)
	}
	valid.Store(true)
	enrolled = 0
	go func() {
		done <- Run(ctx, Config{Server: srv.URL, Token: "tok2", DataDir: data, Socket: sock, AllowHTTP: true, Log: log})
	}()
	time.Sleep(200 * time.Millisecond)
	if hub.Status("nas").Connected {
		t.Error("connected with a refused token")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "isn't valid") {
		t.Errorf("new bad token: %v", err)
	}
	if _, err := hub.Dial(ctx, "nas"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("dial after: %v", err)
	}
	if st := hub.Status("nas"); st.Connected {
		t.Errorf("status after: %+v", st)
	}
}

// Frames of every length, both ways, masked from the client.
func TestWebSocketFrames(t *testing.T) {
	a, b := net.Pipe()
	client := newWSConn(bufio.NewReader(a), a, a, true)
	server := newWSConn(bufio.NewReader(b), b, b, false)
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, maxFrame + 10} {
		msg := make([]byte, n)
		for i := range msg {
			msg[i] = byte(i * 7)
		}
		go func() { client.Write(msg) }()
		got := make([]byte, n)
		if _, err := io.ReadFull(server, got); err != nil || string(got) != string(msg) {
			t.Fatalf("client→server %d: %v", n, err)
		}
		go func() { server.Write(msg) }()
		if _, err := io.ReadFull(client, got); err != nil || string(got) != string(msg) {
			t.Fatalf("server→client %d: %v", n, err)
		}
	}
	// A server refuses unmasked frames.
	go func() { (&wsConn{w: a, client: false}).writeFrame(opBinary, []byte("x")) }()
	if _, err := server.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "masked") {
		t.Errorf("unmasked: %v", err)
	}
}
