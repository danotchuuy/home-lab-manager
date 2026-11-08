package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgentMoves has hlm reachable at two addresses (as before and after
// its HTTPS domain), and moves the agent from one to the other.
func TestAgentMoves(t *testing.T) {
	moveDelay = 10 * time.Millisecond
	defer func() { moveDelay = time.Second }()
	log := slog.New(slog.DiscardHandler)
	sock := (&fakeDocker{}).serve(t)
	hub := NewHub(log)
	hub.Authenticate = func(host, secret string) bool { return host == "nas" && secret == "s3cret" }
	mux := http.NewServeMux()
	mux.Handle("GET /api/agent/connect", hub)
	mux.HandleFunc("GET "+CheckPath, hub.ServeCheck)
	mux.HandleFunc("POST /api/agent/enrol", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(EnrolResponse{Host: "nas", Credential: Credential("nas", "s3cret")})
	})
	oldAddr := httptest.NewServer(mux)
	defer oldAddr.Close()
	newAddr := httptest.NewServer(mux)
	defer newAddr.Close()
	// Something else, which doesn't know the agent.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer other.Close()

	data := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Server: oldAddr.URL, Token: "tok", DataDir: data, Socket: sock, AllowHTTP: true, Version: "1.2.3", Log: log})
	}()
	waitFor(t, "the agent to connect", func() bool { return hub.Status("nas").Connected })
	if st := hub.Status("nas"); st.Server != oldAddr.URL {
		t.Fatalf("server = %q", st.Server)
	}

	// Somewhere that isn't this hlm, or not reachable: it stays.
	if err := hub.MoveTo(ctx, "nas", other.URL); err == nil || !strings.Contains(err.Error(), "doesn't know this agent") {
		t.Errorf("to another: %v", err)
	}
	if err := hub.MoveTo(ctx, "nas", "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "can't reach") {
		t.Errorf("unreachable: %v", err)
	}
	if err := hub.MoveTo(ctx, "nas", "ftp://x"); err == nil {
		t.Error("an ftp address accepted")
	}
	if st := hub.Status("nas"); !st.Connected || st.Server != oldAddr.URL {
		t.Fatalf("after refusals: %+v", st)
	}

	// hlm's new address: it checks, keeps it and reconnects there.
	if err := hub.MoveTo(ctx, "nas", newAddr.URL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent at the new address", func() bool {
		st := hub.Status("nas")
		return st.Connected && st.Server == newAddr.URL
	})
	var creds credentials
	b, _ := os.ReadFile(filepath.Join(data, "agent.json"))
	if json.Unmarshal(b, &creds); creds.Server != newAddr.URL || creds.Previous != oldAddr.URL || creds.Credential == "" {
		t.Errorf("saved = %+v", creds)
	}
	// Moving to where it is is fine.
	if err := hub.MoveTo(ctx, "nas", newAddr.URL+"/"); err != nil {
		t.Errorf("same address: %v", err)
	}
	cancel()
	<-done
}

// TestAgentFallsBack: the new address stops working; the agent tries the
// one it was moved from too.
func TestAgentFallsBack(t *testing.T) {
	firstRetry = 10 * time.Millisecond
	defer func() { firstRetry = time.Second }()
	log := slog.New(slog.DiscardHandler)
	sock := (&fakeDocker{}).serve(t)
	hub := NewHub(log)
	hub.Authenticate = func(host, secret string) bool { return host == "nas" && secret == "s3cret" }
	mux := http.NewServeMux()
	mux.Handle("GET /api/agent/connect", hub)
	oldAddr := httptest.NewServer(mux)
	defer oldAddr.Close()

	data := t.TempDir()
	creds := credentials{Server: "http://127.0.0.1:1", Previous: oldAddr.URL, Host: "nas", Credential: Credential("nas", "s3cret")}
	b, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(data, "agent.json"), b, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{DataDir: data, Socket: sock, AllowHTTP: true, Log: log})
	waitFor(t, "the agent at the previous address", func() bool { return hub.Status("nas").Connected })
	if st := hub.Status("nas"); st.Server != oldAddr.URL {
		t.Errorf("server = %q", st.Server)
	}
}
