package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/transfer"
)

func TestExportPermissions(t *testing.T) {
	e := setup(t)
	e.opts.Transfer = transfer.New(transfer.Options{Manager: e.opts.Manager, Catalog: e.opts.Catalog})
	h := Handler(e.opts)

	// Operators export the settings, without the passwords.
	w := call(h, "operator", "GET", "/api/servers/valheim/export", "", e.keys)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "s3cret") || !strings.Contains(w.Body.String(), `"hlmExport":1`) ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "valheim.hlm.json") {
		t.Fatalf("operator export = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"omitted":["pw"]`) {
		t.Errorf("omitted not listed: %s", w.Body)
	}
	// Passwords take a manager, at a browser.
	if w := call(h, "operator", "GET", "/api/servers/valheim/export?secrets=1", "", e.keys); w.Code != http.StatusForbidden {
		t.Errorf("operator got passwords: %d", w.Code)
	}
	if w := call(h, "manager", "GET", "/api/servers/valheim/export?secrets=1", "", e.keys); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), "interactive_required") {
		t.Errorf("API key got passwords: %d %s", w.Code, w.Body)
	}
	// So does the token: only a confirmed browser session gets them.
	if w := call(h, "admin", "GET", "/api/servers/valheim/export?secrets=1", "", e.keys); w.Code != http.StatusForbidden {
		t.Errorf("token got passwords: %d %s", w.Code, w.Body)
	}
	// Importing and cloning are for admins.
	for _, path := range []string{"/api/servers/import/preview", "/api/servers/valheim/clone/preview"} {
		if w := call(h, "manager", "POST", path, "{}", e.keys); w.Code != http.StatusForbidden {
			t.Errorf("%s by a manager = %d", path, w.Code)
		}
	}
	if w := call(h, "admin", "POST", "/api/servers/import/preview", `{"hlmExport":9,"request":{"templateId":"t1"}}`, e.keys); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "newer hlm") {
		t.Errorf("newer file = %d %s", w.Code, w.Body)
	}
}
