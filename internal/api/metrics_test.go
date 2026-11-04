package api

import (
	"net/http"
	"strings"
	"testing"

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
	h := Handler(e.opts)

	w := callFrom(h, "10.0.0.9:5000", "GET", "/metrics", key)
	if w.Code != 200 || w.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatalf("metrics key: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{`hlm_build_info{version="v9.9.9"} 1`, `hlm_server_up{server="valheim",app=`, "# TYPE hlm_server_restarts_total counter"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
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
