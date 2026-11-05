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

func TestStatusPagePasswords(t *testing.T) {
	e := setup(t)
	store, _ := secrets.Open(t.TempDir(), "")
	sp := statuspage.New(statuspage.Options{Secrets: store, Servers: e.opts.Manager,
		HashPassword: auth.HashPassword, PasswordMatches: auth.PasswordMatches})
	e.opts.StatusPage = sp
	h := Handler(e.opts)
	oldStatus, oldUnlock := statusLimiter, unlockLimiter
	t.Cleanup(func() { statusLimiter, unlockLimiter = oldStatus, oldUnlock })
	statusLimiter, unlockLimiter = &statuspage.Limiter{Rate: 100, Burst: 100}, &statuspage.Limiter{Rate: 0.001, Burst: 5}

	// Set through the API: the passwords go in, only "is it set" comes out.
	body := `{"enabled":true,"servers":[{"server":"valheim","displayName":"Vikings","address":"play.example.com:2456","hint":"ask"}],` +
		`"viewPassword":"friends-only","detailsPassword":"inner-circle"}`
	w := call(h, "admin", "PUT", "/api/system/status-page", body, e.keys)
	if w.Code != 200 || strings.Contains(w.Body.String(), "argon2") || !strings.Contains(w.Body.String(), `"hasViewPassword":true`) {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if w := call(h, "admin", "GET", "/api/system/status-page", "", e.keys); strings.Contains(w.Body.String(), "argon2") {
		t.Errorf("settings show a hash: %s", w.Body)
	}

	req := func(method, path, body, cookie string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.7:4000"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: statuspage.CookieName, Value: cookie})
		}
		if csrf {
			r.Header.Set("X-HLM-Request", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	cookieOf := func(w *httptest.ResponseRecorder) string {
		for _, c := range w.Result().Cookies() {
			if c.Name == statuspage.CookieName {
				return c.Value
			}
		}
		return ""
	}

	w = req("GET", "/api/status-page", "", "", false)
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"passwordRequired":true`) || strings.Contains(w.Body.String(), "Vikings") {
		t.Errorf("locked page: %d %s", w.Code, w.Body)
	}
	if w := req("GET", "/api/status-page/vikings.svg", "", "", false); w.Code != 401 {
		t.Errorf("locked badge: %d", w.Code)
	}
	if w := req("POST", "/api/status-page/unlock", `{"password":"friends-only"}`, "", false); w.Code != 403 {
		t.Errorf("unlock without the header: %d", w.Code)
	}
	if w := req("POST", "/api/status-page/unlock", `{"password":"wrong"}`, "", true); w.Code != 401 {
		t.Errorf("wrong password: %d", w.Code)
	}

	// The view password: the page, without names, addresses or notes.
	w = req("POST", "/api/status-page/unlock", `{"password":"friends-only"}`, "", true)
	view := cookieOf(w)
	if w.Code != 200 || view == "" || !strings.Contains(w.Body.String(), `"detailsLocked":true`) {
		t.Fatalf("unlock view: %d %s", w.Code, w.Body)
	}
	if c := w.Result().Cookies()[0]; !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v", c)
	}
	w = req("GET", "/api/status-page", "", view, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Vikings") || strings.Contains(w.Body.String(), "play.example.com") ||
		strings.Contains(w.Body.String(), `"hint"`) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("view level: %d %s %v", w.Code, w.Body, w.Header())
	}
	if w := req("GET", "/api/status-page/vikings.json", "", view, false); strings.Contains(w.Body.String(), "play.example.com") {
		t.Errorf("server JSON at view level: %s", w.Body)
	}

	// The details password: everything.
	details := cookieOf(req("POST", "/api/status-page/unlock", `{"password":"inner-circle"}`, "", true))
	if w := req("GET", "/api/status-page", "", details, false); !strings.Contains(w.Body.String(), "play.example.com:2456") || !strings.Contains(w.Body.String(), `"hint":"ask"`) {
		t.Errorf("details level: %s", w.Body)
	}
	// Forgetting it.
	if w := req("POST", "/api/status-page/lock", "", details, true); w.Code != 204 || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Errorf("lock: %d", w.Code)
	}

	// Guessing is slowed down: five tries in all, then 429.
	codes := []int{}
	for range 4 {
		codes = append(codes, req("POST", "/api/status-page/unlock", `{"password":"guess"}`, "", true).Code)
	}
	if codes[len(codes)-1] != 429 {
		t.Errorf("guesses: %v", codes)
	}

	// On the status-only hostname, unlocking still works.
	sp.Save(statuspage.Settings{Enabled: true, Hostname: "status.example.com", Servers: []statuspage.Entry{{Server: "valheim"}}}, nil)
	unlockLimiter = &statuspage.Limiter{Rate: 100, Burst: 100}
	r := httptest.NewRequest("POST", "/api/status-page/unlock", strings.NewReader(`{"password":"friends-only"}`))
	r.Host, r.RemoteAddr = "status.example.com", "198.51.100.8:1"
	r.Header.Set("X-HLM-Request", "1")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("unlock on the status host: %d %s", w.Code, w.Body)
	}
}
