package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/players"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Operators can kick; banning and the lists take a manager.
func TestPlayerActionRoles(t *testing.T) {
	e := setup(t)
	store, _ := secrets.Open(t.TempDir(), "")
	e.opts.Players = players.New(players.Options{Secrets: store, Servers: e.opts.Manager, Console: e.opts.Console})
	h := Handler(e.opts)
	body := `{"name":"alex","reason":"testing"}`
	if w := call(h, "operator", "POST", "/api/servers/valheim/players/ban", body, e.keys); w.Code != http.StatusForbidden {
		t.Errorf("operator ban: %d %s", w.Code, w.Body)
	}
	// Past the role check: valheim's image has no profile here.
	if w := call(h, "operator", "POST", "/api/servers/valheim/players/kick", body, e.keys); w.Code == http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "doesn't know how to manage") {
		t.Errorf("operator kick: %d %s", w.Code, w.Body)
	}
	if w := call(h, "manager", "POST", "/api/servers/valheim/players/ban", body, e.keys); w.Code == http.StatusForbidden {
		t.Errorf("manager ban: %d %s", w.Code, w.Body)
	}
	if w := call(h, "operator", "GET", "/api/servers/valheim/players", "", e.keys); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"unsupported"`) {
		t.Errorf("overview: %d %s", w.Code, w.Body)
	}
}
