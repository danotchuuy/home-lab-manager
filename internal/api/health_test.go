package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestServerHealthSettings(t *testing.T) {
	e := setup(t)
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	e.opts.Health = health.NewStore(store)
	e.opts.Manager.UseHealthPolicy(e.opts.Health)
	h := Handler(e.opts)

	body := `{"crashLoop":{"exits":3,"minutes":5},"check":{"kind":"command","command":"rcon-cli -p hunter2 list"},"restartAfter":10}`
	if w := call(h, "operator", "PUT", "/api/servers/valheim/health", body, e.keys); w.Code != http.StatusForbidden {
		t.Errorf("operator saved health settings: %d", w.Code)
	}
	w := call(h, "manager", "PUT", "/api/servers/valheim/health", body, e.keys)
	if w.Code != http.StatusOK {
		t.Fatalf("save = %d %s", w.Code, w.Body)
	}
	var st healthStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	// Saved, but the running container doesn't have it until it's
	// recreated.
	if st.Settings.CrashLoop.Exits != 3 || st.Settings.RestartAfter != 10 || st.Applied || st.Defaults.Exits != 5 {
		t.Errorf("status = %+v", st)
	}
	if n, _ := e.opts.Health.CrashLoopLimit("valheim"); n != 3 {
		t.Errorf("limit = %d", n)
	}

	// Viewers see the settings, but not the command.
	w = call(h, "viewer", "GET", "/api/servers/valheim/health", "", e.keys)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "hunter2") || !strings.Contains(w.Body.String(), `"restartAfter":10`) {
		t.Errorf("viewer got %d %s", w.Code, w.Body)
	}
	w = call(h, "operator", "GET", "/api/servers/valheim/health", "", e.keys)
	if !strings.Contains(w.Body.String(), "hunter2") {
		t.Errorf("operator got %s", w.Body)
	}

	if w := call(h, "manager", "PUT", "/api/servers/valheim/health", `{"check":{"kind":"tcp"}}`, e.keys); w.Code != http.StatusBadRequest {
		t.Errorf("bad settings = %d", w.Code)
	}
}
