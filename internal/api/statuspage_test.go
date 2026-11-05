package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

func get(h http.Handler, host, path, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	if remote != "" {
		r.RemoteAddr = remote
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStatusPageAPI(t *testing.T) {
	e := setup(t)
	store, _ := secrets.Open(t.TempDir(), "")
	sp := statuspage.New(statuspage.Options{Secrets: store, Servers: e.opts.Manager})
	e.opts.StatusPage = sp
	h := Handler(e.opts)

	// Off: nothing, to anyone (HLM_TOKEN is set, so other API calls need it).
	if w := get(h, "hlm.lan", "/api/status-page", ""); w.Code != 404 {
		t.Errorf("off: %d", w.Code)
	}
	if _, err := sp.Save(statuspage.Settings{Enabled: true, Title: "Our servers", Servers: []statuspage.Entry{{Server: "valheim", DisplayName: "Vikings"}}}, auth.ParseAllowFrom); err != nil {
		t.Fatal(err)
	}
	w := get(h, "hlm.lan", "/api/status-page", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Vikings"`) || strings.Contains(w.Body.String(), "aaaa1111") {
		t.Errorf("on: %d %s", w.Code, w.Body)
	}
	if w := get(h, "hlm.lan", "/api/status-page/vikings.svg", ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" ||
		!strings.Contains(w.Body.String(), "Vikings") {
		t.Errorf("badge: %d %s", w.Code, w.Header())
	}
	if w := get(h, "hlm.lan", "/api/status-page/vikings.json", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"slug":"vikings"`) {
		t.Errorf("json: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/status-page/nobody.json", "/api/status-page/vikings.txt", "/api/status-page/vikings"} {
		if w := get(h, "hlm.lan", p, ""); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	// The rest of the API still needs signing in.
	if w := get(h, "hlm.lan", "/api/servers", ""); w.Code != 401 {
		t.Errorf("servers without a token: %d", w.Code)
	}

	// Only from the allowed addresses.
	sp.Save(statuspage.Settings{Enabled: true, AllowFrom: []string{"10.8.0.0/24"}, Servers: []statuspage.Entry{{Server: "valheim"}}}, auth.ParseAllowFrom)
	if w := get(h, "hlm.lan", "/api/status-page", "192.0.2.1:1234"); w.Code != 404 {
		t.Errorf("from elsewhere: %d", w.Code)
	}
	if w := get(h, "hlm.lan", "/api/status-page", "10.8.0.7:1234"); w.Code != 200 {
		t.Errorf("from the VPN: %d", w.Code)
	}

	// Its own hostname reaches the status page and nothing else, even with
	// the token.
	sp.Save(statuspage.Settings{Enabled: true, Hostname: "status.example.com", Servers: []statuspage.Entry{{Server: "valheim"}}}, auth.ParseAllowFrom)
	if w := get(h, "status.example.com", "/api/status-page", ""); w.Code != 200 {
		t.Errorf("status host, page: %d", w.Code)
	}
	if w := get(h, "status.example.com:443", "/", ""); w.Code != 302 || w.Header().Get("Location") != "/status" {
		t.Errorf("status host, /: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/api/servers", "/api/auth/status", "/api/system/status-page", "/host", "/metrics"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Host = "STATUS.example.com"
		r.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("status host, %s: %d", p, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/status-page", nil)
	r.Host = "status.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("status host, POST: %d", w.Code)
	}
	// Other names reach hlm as usual.
	if w := call(h, "admin", "GET", "/api/servers", "", e.keys); w.Code != 200 {
		t.Errorf("normal host: %d", w.Code)
	}
}
