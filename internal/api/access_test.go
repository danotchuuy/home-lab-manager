package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

func callFrom(h http.Handler, remote, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestTokensOnlyInTheHeader(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	for who, token := range map[string]string{"HLM_TOKEN": adminToken, "API key": e.keys["manager"]} {
		if got := call(h, "anonymous", "GET", "/api/servers?token="+token, "", e.keys).Code; got != http.StatusUnauthorized {
			t.Errorf("%s in the URL = %d", who, got)
		}
	}
}

func TestDownloadTickets(t *testing.T) {
	e := setup(t)
	h := e.stubbed()
	const path = "/api/system/backup/download?key=hlm%2Fone.tar.gz"
	// The stubbed routes don't include the ticket handler, so use the real one.
	h2 := Handler(e.opts)
	w := call(h2, "admin", "POST", "/api/auth/download-ticket", `{"url":"`+path+`"}`, e.keys)
	var got struct{ URL string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || !strings.Contains(got.URL, "ticket=") {
		t.Fatalf("ticket = %d %s", w.Code, w.Body)
	}
	if code := call(h, "anonymous", "GET", got.URL, "", e.keys).Code; code != http.StatusTeapot {
		t.Errorf("download with the ticket = %d", code)
	}
	if code := call(h, "anonymous", "GET", got.URL, "", e.keys).Code; code != http.StatusUnauthorized {
		t.Errorf("second use = %d", code)
	}
	// A ticket is only for the URL it was made for, and only for downloads.
	json.Unmarshal(call(h2, "admin", "POST", "/api/auth/download-ticket", `{"url":"`+path+`"}`, e.keys).Body.Bytes(), &got)
	ticket := got.URL[strings.Index(got.URL, "ticket="):]
	if code := call(h, "anonymous", "GET", "/api/system/backup/download?key=other&"+ticket, "", e.keys).Code; code != http.StatusUnauthorized {
		t.Errorf("for another download = %d", code)
	}
	if code := call(h, "anonymous", "GET", "/api/auth/keys?"+ticket, "", e.keys).Code; code != http.StatusUnauthorized {
		t.Errorf("for another endpoint = %d", code)
	}
	for _, url := range []string{"/api/auth/keys", "https://evil.example/api/system/backup/download", "//evil.example/api/system/backup/download"} {
		if code := call(h2, "admin", "POST", "/api/auth/download-ticket", `{"url":"`+url+`"}`, e.keys).Code; code != http.StatusBadRequest {
			t.Errorf("ticket for %s = %d", url, code)
		}
	}
}

func TestKeysOnlyFromTheirAddresses(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	key, _, err := e.opts.Auth.CreateAPIKey("ops", 0, rbac.Admin, []string{"10.0.3.115"})
	if err != nil {
		t.Fatal(err)
	}
	if code := callFrom(h, "10.0.3.115:41000", "GET", "/api/groups", key).Code; code != 200 {
		t.Errorf("from its address = %d", code)
	}
	if code := callFrom(h, "10.0.3.116:41000", "GET", "/api/groups", key).Code; code != http.StatusUnauthorized {
		t.Errorf("from another = %d", code)
	}
	// X-Forwarded-For is anyone's to send, so it changes nothing.
	r := httptest.NewRequest("GET", "/api/groups", nil)
	r.RemoteAddr = "10.0.3.116:41000"
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Forwarded-For", "10.0.3.115")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("with a forged X-Forwarded-For = %d", w.Code)
	}
}

// Operators can run a server but not change it, so they don't get what
// look like its secrets; managers, who can change them, do.
func TestOperatorsDontSeeSecrets(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	for _, path := range []string{"/api/servers", "/api/servers/valheim", "/api/servers/valheim/config"} {
		op := call(h, "operator", "GET", path, "", e.keys)
		if op.Code != 200 || strings.Contains(op.Body.String(), "s3cret") || !strings.Contains(op.Body.String(), `"hidden"`) {
			t.Errorf("%s as operator = %d %s", path, op.Code, op.Body)
		}
		if !strings.Contains(op.Body.String(), "/srv/valheim") {
			t.Errorf("%s as operator lost the non-secret settings: %s", path, op.Body)
		}
		mgr := call(h, "manager", "GET", path, "", e.keys)
		if mgr.Code != 200 || !strings.Contains(mgr.Body.String(), "s3cret") || strings.Contains(mgr.Body.String(), `"hidden"`) {
			t.Errorf("%s as manager = %d %s", path, mgr.Code, mgr.Body)
		}
	}
}

func TestStepUp(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	e.browser(t, h, "root", rbac.Admin, false)
	e.browser(t, h, "bob", rbac.Member, true)

	w := call(h, "root", "POST", "/api/auth/keys", `{"name":"k"}`, e.keys)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"confirm_required"`) || !strings.Contains(w.Body.String(), `"method":"local"`) {
		t.Errorf("unconfirmed admin = %d %s", w.Code, w.Body)
	}
	if w := call(h, "root", "POST", "/api/auth/confirm", `{"password":"wrong"}`, e.keys); w.Code != 403 {
		t.Errorf("confirm with the wrong password = %d", w.Code)
	}
	if w := call(h, "bob", "POST", "/api/auth/keys", `{"name":"k"}`, e.keys); w.Code != 403 {
		t.Errorf("confirmed member = %d", w.Code)
	}
	if w := call(h, "admin", "PUT", "/api/auth/accounts/bob/password", `{"password":"a whole new password"}`, e.keys); w.Code != 403 {
		t.Errorf("admin token setting a password = %d %s", w.Code, w.Body)
	}
	// Revoking stays open to tokens.
	_, info, _ := e.opts.Auth.CreateAPIKey("spare", 0, rbac.Member, nil)
	if w := call(h, "admin", "DELETE", "/api/auth/keys/"+info.ID, "", e.keys); w.Code != 204 {
		t.Errorf("admin token revoking = %d", w.Code)
	}
	if w := call(h, "admin", "DELETE", "/api/groups/"+e.game.ID, "", e.keys); w.Code != 204 && w.Code != 200 {
		t.Errorf("admin token deleting a group = %d", w.Code)
	}
}
