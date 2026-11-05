package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/hosts"
	"github.com/danotchuuy/home-lab-manager/internal/metrics"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// A host's agent enrols and connects through hlm's real HTTP handler (the
// connection is taken over, so it's a real server), without signing in;
// the rest of the API still needs it.
func TestAgentAPI(t *testing.T) {
	e := setup(t)
	log := slog.New(slog.DiscardHandler)
	store, _ := secrets.Open(t.TempDir(), "")
	hub := agent.NewHub(log)
	reg := hosts.New(hosts.Options{Secrets: store, Agents: hub, Log: log})
	sampler := metrics.New(nil, e.opts.Manager, nil, log)
	e.opts.Hosts, e.opts.Agents, e.opts.Sampler = reg, hub, sampler
	srv := httptest.NewServer(Handler(e.opts))
	defer srv.Close()

	post := func(path, token string, body any) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// Making a token needs signing in.
	if resp := post("/api/hosts/agent-enrolment", "", map[string]string{"id": "nas"}); resp.StatusCode != 401 {
		t.Errorf("enrolment without signing in: %d", resp.StatusCode)
	}
	// Nor can a token or API key make one: it needs a recently confirmed
	// admin session, like adding any host.
	if resp := post("/api/hosts/agent-enrolment", adminToken, map[string]string{"id": "nas"}); resp.StatusCode != 403 {
		t.Errorf("enrolment with the token: %d", resp.StatusCode)
	}
	made, err := reg.CreateEnrolment(hosts.Host{ID: "nas", AppdataRoot: "/srv", Address: "192.168.1.20"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := post("/api/agent/enrol", "", agent.EnrolRequest{Token: "hlme_wrong"}); resp.StatusCode != 401 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	// A connection without a credential is refused.
	req, _ := http.NewRequest("GET", srv.URL+"/api/agent/connect", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("connect without a credential: %d", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Config{Server: srv.URL, Token: made.Token, DataDir: t.TempDir(), AllowHTTP: true,
			Socket: filepath.Join(t.TempDir(), "no-docker.sock"), Version: "test", Log: log})
	}()
	for deadline := time.Now().Add(5 * time.Second); !hub.Status("nas").Connected; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the agent didn't connect")
		}
	}
	// hlm reaches the agent, which says its Docker isn't there.
	if _, err := reg.Check(ctx, "nas"); err == nil || !strings.Contains(err.Error(), "can't reach Docker at") {
		t.Errorf("check: %v", err)
	}
	// Listed with its agent's state.
	req, _ = http.NewRequest("GET", srv.URL+"/api/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ := http.DefaultClient.Do(req)
	var list struct {
		Hosts []struct {
			ID    string        `json:"id"`
			Kind  string        `json:"kind"`
			Agent *agent.Status `json:"agent"`
		} `json:"hosts"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Hosts) != 2 || list.Hosts[1].Kind != "agent" || list.Hosts[1].Agent == nil || !list.Hosts[1].Agent.Connected || list.Hosts[1].Agent.Version != "test" {
		t.Errorf("hosts: %+v", list.Hosts)
	}
	// The host's own metrics, from its agent: on the Host page's API, in
	// its history and in /metrics with its host label.
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	var snap hostmetrics.Snapshot
	for deadline := time.Now().Add(5 * time.Second); !snap.Available && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		json.NewDecoder(get("/api/host?host=nas").Body).Decode(&snap)
	}
	if !snap.Available || snap.CPU.Cores == 0 {
		t.Fatalf("host metrics: %+v", snap)
	}
	if resp := get("/api/host?host=nope"); resp.StatusCode != 404 {
		t.Errorf("unknown host: %d", resp.StatusCode)
	}
	sampler.ObserveHostOn("nas", snap)
	if resp := get("/api/host/history?host=nas&range=1h"); resp.StatusCode != 200 {
		t.Errorf("history: %d", resp.StatusCode)
	}
	if resp := get("/api/host/history?host=nope&range=1h"); resp.StatusCode != 404 {
		t.Errorf("history of an unknown host: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(get("/metrics").Body)
	if !strings.Contains(string(body), `hlm_host_cpu_cores{host="nas"}`) {
		t.Errorf("metrics:\n%s", body)
	}

	// Removing the host disconnects the agent for good.
	if err := reg.Delete("nas"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("after removal: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the agent kept going")
	}
}
