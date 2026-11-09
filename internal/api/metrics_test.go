package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/idle"
	"github.com/danotchuuy/home-lab-manager/internal/metrics"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestMetricsEndpoint(t *testing.T) {
	e := setup(t)
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	e.opts.MetricsConfig = metrics.NewConfig(store)
	e.opts.Version = "v9.9.9"
	key, _, err := e.opts.Auth.CreateAPIKey("prometheus", 0, rbac.Metrics, nil)
	if err != nil {
		t.Fatal(err)
	}
	// valheim is asleep, with a wake listener that couldn't start.
	idl := idle.New(idle.Options{Secrets: store, Servers: &stoppedServers{},
		Players: func(string) (int, int, bool) { return 0, 0, false }})
	since := time.Date(2026, 11, 9, 4, 58, 0, 0, time.UTC)
	store.Set("idle-asleep", map[string]idle.Asleep{"valheim": {Since: since, Minutes: 30, ListenerError: "the host's agent isn't connected"}})
	e.opts.Idle = idl
	h := Handler(e.opts)

	w := callFrom(h, "10.0.0.9:5000", "GET", "/metrics", key)
	if w.Code != 200 || w.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatalf("metrics key: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{`hlm_build_info{version="v9.9.9"} 1`, `hlm_server_up{server="valheim",app=`, "# TYPE hlm_server_restarts_total counter",
		`hlm_server_asleep{server="valheim",`, "# TYPE hlm_server_asleep gauge"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, `hlm_server_asleep{server="valheim",`) && !strings.HasSuffix(line, " 1"),
			strings.HasPrefix(line, `hlm_server_asleep_since_timestamp_seconds{server="valheim",`) && value(line) != float64(since.Unix()),
			strings.HasPrefix(line, `hlm_server_wake_listener_up{server="valheim",`) && !strings.HasSuffix(line, " 0"),
			strings.HasPrefix(line, "hlm_server_asleep{") && !strings.Contains(line, `"valheim"`) && !strings.HasSuffix(line, " 0"):
			t.Errorf("wrong: %s", line)
		}
	}
	if !strings.Contains(body, `hlm_server_wake_listener_up{server="valheim",`) {
		t.Error("no wake listener metric for a sleeping server that wakes on a connection")
	}
	if strings.Contains(body, "postgres") {
		t.Error("an unmanaged container is listed")
	}
	if got := callFrom(h, "10.0.0.9:5000", "GET", "/metrics", adminToken).Code; got != 200 {
		t.Errorf("HLM_TOKEN: %d", got)
	}
	// A metrics key can't use the API.
	for _, path := range []string{"/api/servers", "/api/auth/status/../me", "/api/updates/auto"} {
		if got := callFrom(h, "10.0.0.9:5000", "GET", path, key).Code; got == 200 {
			t.Errorf("metrics key on %s: %d", path, got)
		}
	}
	// Other keys and nobody can't scrape.
	for who, token := range map[string]string{"viewer": e.keys["viewer"], "deployer": e.keys["deployer"], "nobody": ""} {
		w := callFrom(h, "10.0.0.9:5000", "GET", "/metrics", token)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", who, w.Code)
		}
	}
	// Unless from an allowed address.
	if _, err := e.opts.MetricsConfig.Set(metrics.Settings{AllowFrom: []string{"10.0.0.0/24", "bad"}}); err == nil {
		t.Error("accepted a bad address")
	}
	if _, err := e.opts.MetricsConfig.Set(metrics.Settings{AllowFrom: []string{"10.0.0.0/24"}}); err != nil {
		t.Fatal(err)
	}
	if got := callFrom(h, "10.0.0.9:5000", "GET", "/metrics", "").Code; got != 200 {
		t.Errorf("allowed address: %d", got)
	}
	if got := callFrom(h, "10.0.1.9:5000", "GET", "/metrics", "").Code; got != http.StatusUnauthorized {
		t.Errorf("other address: %d", got)
	}
}

// value is a sample line's value.
func value(line string) float64 {
	v, _ := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
	return v
}
