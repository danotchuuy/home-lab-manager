package auth_test

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/danotchuuy/home-lab-manager/internal/api"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// provider is a minimal OpenID Connect provider. Like Authelia by default,
// its ID tokens carry no profile or groups; those come from userinfo.
type provider struct {
	*httptest.Server
	key    *rsa.PrivateKey
	secret string

	mu     sync.Mutex
	codes  map[string]authRequest
	user   string
	groups []string
	// authTime is when the user last signed in, as the ID token says; nil
	// means just now. prompt and maxAge are what the last sign-in asked for.
	authTime       func() time.Time
	prompt, maxAge string
}

type authRequest struct {
	nonce, challenge, redirect string
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{key: key, secret: "s3cret", codes: map[string]authRequest{}, user: "alice", groups: []string{"admins"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"userinfo_endpoint": p.URL + "/userinfo", "jwks_uri": p.URL + "/jwks",
			"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	// authorize signs the user straight in.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "hlm" || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
			http.Error(w, "bad request", 400)
			return
		}
		// Like Authelia 4.39, max_age=0 can never be met: the user is asked
		// to sign in again and again, and the sign-in never comes back.
		if q.Get("max_age") == "0" {
			http.Error(w, "Failed to confirm your identity", http.StatusForbidden)
			return
		}
		code := "code-" + q.Get("state")
		p.mu.Lock()
		p.prompt, p.maxAge = q.Get("prompt"), q.Get("max_age")
		p.codes[code] = authRequest{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, secret, _ := r.BasicAuth()
		if id == "" {
			id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
		}
		if id != "hlm" || secret != p.secret {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		p.mu.Lock()
		req, ok := p.codes[r.PostFormValue("code")]
		delete(p.codes, r.PostFormValue("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge || r.PostFormValue("redirect_uri") != req.redirect {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		now := time.Now()
		authTime := now
		p.mu.Lock()
		if p.authTime != nil {
			authTime = p.authTime()
		}
		p.mu.Unlock()
		idt, _ := jwt.Signed(signer).Claims(map[string]any{
			"iss": p.URL, "sub": "uid-" + p.user, "aud": "hlm", "nonce": req.nonce,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "auth_time": authTime.Unix(),
		}).Serialize()
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + p.user, "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-"+p.user {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sub": "uid-" + p.user, "preferred_username": p.user, "name": "Alice A", "email": p.user + "@example.com", "groups": p.groups})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

type env struct {
	prov  *provider
	hlm   *httptest.Server
	svc   *auth.Service
	store *secrets.Store
}

func setup(t *testing.T, token string) *env {
	t.Helper()
	e := &env{prov: newProvider(t)}
	e.store, _ = secrets.Open(t.TempDir(), "")
	e.svc = auth.New(e.store, false, slog.New(slog.DiscardHandler))
	log := slog.New(slog.DiscardHandler)
	e.hlm = httptest.NewServer(api.Handler(api.Options{Auth: e.svc, Groups: rbac.NewStore(e.store, log), Token: token, Log: log}))
	t.Cleanup(e.hlm.Close)
	return e
}

func (e *env) oidc() auth.OIDCSettings {
	return auth.OIDCSettings{Issuer: e.prov.URL, ClientID: "hlm", ClientSecret: e.prov.secret, PublicURL: e.hlm.URL}
}

// browser is a client with cookies, like a browser.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

var csrf = map[string]string{"X-HLM-Request": "1"}

func do(t *testing.T, c *http.Client, method, u string, header map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// saveOIDC saves provider settings through the API, as c.
func (e *env) saveOIDC(t *testing.T, c *http.Client, s auth.OIDCSettings) (int, string) {
	t.Helper()
	return do(t, c, "PUT", e.hlm.URL+"/api/auth/oidc", csrf, jsonBody(s))
}

func (e *env) require(t *testing.T, c *http.Client, on bool) (int, string) {
	t.Helper()
	return do(t, c, "PUT", e.hlm.URL+"/api/auth/config", csrf, jsonBody(auth.Config{Required: on}))
}

// confirm confirms a browser's session, with its password, or with single
// sign-on if password is empty.
func (e *env) confirm(t *testing.T, c *http.Client, password string) {
	t.Helper()
	if password == "" {
		do(t, c, "GET", e.hlm.URL+"/auth/login?confirm=1", nil, "") // ends at the web UI, which isn't served here
		return
	}
	if code, body := do(t, c, "POST", e.hlm.URL+"/api/auth/confirm", csrf, jsonBody(map[string]string{"password": password})); code != 200 {
		t.Fatalf("confirm = %d %s", code, body)
	}
}

func (e *env) localLogin(t *testing.T, c *http.Client, user, pass string) (int, string) {
	t.Helper()
	return do(t, c, "POST", e.hlm.URL+"/api/auth/login", csrf, jsonBody(map[string]string{"username": user, "password": pass}))
}

func TestSignInFlow(t *testing.T) {
	e := setup(t, "")
	anon := &http.Client{}
	if code, body := e.saveOIDC(t, anon, e.oidc()); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	// Nobody is signed in, so sign-in can't be required yet.
	if code, body := e.require(t, anon, true); code != 400 || !strings.Contains(body, "sign in first") {
		t.Fatalf("requiring while signed out = %d %s", code, body)
	}

	b := browser(t)
	// Signing in returns to the page it started from, with a session.
	code, body := do(t, b, "GET", e.hlm.URL+"/auth/login?return="+url.QueryEscape("/system#sign-in"), nil, "")
	if code != 200 && code != 404 { // the SPA isn't built in tests; the redirect target is what matters
		t.Fatalf("login = %d %s", code, body)
	}
	code, body = do(t, b, "GET", e.hlm.URL+"/api/auth/status", nil, "")
	var st auth.Status
	json.Unmarshal([]byte(body), &st)
	if code != 200 || st.User == nil || st.User.Method != "oidc" || st.User.Username != "alice" || st.User.Email != "alice@example.com" || len(st.User.Groups) != 1 || !st.OIDC {
		t.Fatalf("status = %d %s", code, body)
	}
	if pub := e.svc.PublicOIDC(); pub.Verified.IsZero() || pub.VerifiedBy != "Alice A" || !pub.HasSecret {
		t.Fatalf("not verified: %+v", pub)
	}

	// Signed in and confirmed, sign-in can be required.
	if code, _ := e.require(t, b, true); code != 403 {
		t.Fatalf("require before confirming = %d", code)
	}
	e.confirm(t, b, "")
	if code, body := e.require(t, b, true); code != 200 {
		t.Fatalf("require = %d %s", code, body)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 401 {
		t.Errorf("anonymous = %d", code)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 200 {
		t.Errorf("signed in = %d", code)
	}
	// Changes need the CSRF header with a cookie.
	if code, _ := do(t, b, "POST", e.hlm.URL+"/api/auth/oidc/test", nil, jsonBody(e.oidc())); code != 403 {
		t.Errorf("without header = %d", code)
	}
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/oidc/test", csrf, jsonBody(e.oidc())); code != 200 {
		t.Errorf("with header = %d %s", code, body)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/status", nil, ""); code != 200 {
		t.Errorf("status anonymous = %d", code)
	}

	// Without a local account, settings that would shut alice out, or
	// removing single sign-on, are refused.
	c := e.oidc()
	c.ClientSecret = ""
	c.AllowedGroups = []string{"family"}
	if code, body := e.saveOIDC(t, b, c); code != 400 || !strings.Contains(body, "you") {
		t.Errorf("locking myself out = %d %s", code, body)
	}
	if code, body := do(t, b, "DELETE", e.hlm.URL+"/api/auth/oidc", csrf, ""); code != 409 {
		t.Errorf("removing the only way in = %d %s", code, body)
	}

	// With a local account to fall back on, they're allowed, and narrowing
	// who may sign in applies to existing sessions.
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/accounts", csrf, jsonBody(map[string]string{"username": "admin", "password": "correct horse"})); code != 201 {
		t.Fatalf("add account = %d %s", code, body)
	}
	if code, body := e.saveOIDC(t, b, c); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 401 {
		t.Errorf("no longer allowed = %d", code)
	}
	code, body = do(t, browser(t), "GET", e.hlm.URL+"/auth/login", nil, "")
	if code != 403 || !strings.Contains(body, "Not allowed") {
		t.Errorf("sign-in of someone not allowed = %d %s", code, body)
	}
	local := browser(t)
	if code, body := e.localLogin(t, local, "admin", "correct horse"); code != 200 {
		t.Fatalf("local login = %d %s", code, body)
	}
	e.confirm(t, local, "correct horse")
	c.AllowedUsers = []string{"ALICE@example.com"}
	if code, body := e.saveOIDC(t, local, c); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 200 {
		t.Errorf("allowed by email = %d", code)
	}

	// Signing out ends the session.
	if code, _ := do(t, b, "POST", e.hlm.URL+"/api/auth/logout", csrf, ""); code != 204 {
		t.Errorf("logout = %d", code)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 401 {
		t.Errorf("after logout = %d", code)
	}

	// Removing single sign-on signs everyone out.
	if code, body := do(t, local, "DELETE", e.hlm.URL+"/api/auth/oidc", csrf, ""); code != 204 {
		t.Fatalf("remove = %d %s", code, body)
	}
	if code, _ := do(t, local, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 401 {
		t.Errorf("session survived removing single sign-on = %d", code)
	}
}

func TestLocalAccounts(t *testing.T) {
	e := setup(t, "")
	anon := &http.Client{}
	add := func(c *http.Client, user, pass string) (int, string) {
		return do(t, c, "POST", e.hlm.URL+"/api/auth/accounts", csrf, jsonBody(map[string]string{"username": user, "password": pass}))
	}
	for _, bad := range [][2]string{{"admin", "short"}, {"-x", "long enough pw"}, {"has space", "long enough pw"}} {
		if code, _ := add(anon, bad[0], bad[1]); code != 400 {
			t.Errorf("add %q = %d", bad, code)
		}
	}
	if code, body := add(anon, "Admin", "correct horse"); code != 201 {
		t.Fatalf("add = %d %s", code, body)
	}
	if code, _ := add(anon, "admin", "another password"); code != 409 {
		t.Errorf("duplicate (ignoring case) = %d", code)
	}

	b := browser(t)
	if code, _ := e.localLogin(t, b, "admin", "wrong password"); code != 401 {
		t.Errorf("wrong password = %d", code)
	}
	if code, _ := do(t, b, "POST", e.hlm.URL+"/api/auth/login", nil, jsonBody(map[string]string{"username": "admin", "password": "correct horse"})); code != 403 {
		t.Errorf("login without the CSRF header = %d", code)
	}
	code, body := e.localLogin(t, b, "ADMIN", "correct horse")
	if code != 200 || !strings.Contains(body, `"method":"local"`) || !strings.Contains(body, `"username":"Admin"`) {
		t.Fatalf("login = %d %s", code, body)
	}
	e.confirm(t, b, "correct horse")
	if code, body := e.require(t, b, true); code != 200 {
		t.Fatalf("require = %d %s", code, body)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 401 {
		t.Errorf("anonymous = %d", code)
	}
	code, body = do(t, b, "GET", e.hlm.URL+"/api/auth/accounts", nil, "")
	if code != 200 || !strings.Contains(body, `"username":"Admin"`) || strings.Contains(body, "argon2") {
		t.Errorf("accounts = %d %s", code, body)
	}

	// The last way in can't be removed while sign-in is required.
	if code, body := do(t, b, "DELETE", e.hlm.URL+"/api/auth/accounts/admin", csrf, ""); code != 409 {
		t.Errorf("remove the only account = %d %s", code, body)
	}

	// A new password ends other sessions, but not the one that set it.
	other := browser(t)
	e.localLogin(t, other, "admin", "correct horse")
	if code, body := do(t, b, "PUT", e.hlm.URL+"/api/auth/accounts/admin/password", csrf, jsonBody(map[string]string{"password": "battery staple"})); code != 204 {
		t.Fatalf("change password = %d %s", code, body)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 200 {
		t.Errorf("own session after changing the password = %d", code)
	}
	if code, _ := do(t, other, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 401 {
		t.Errorf("other session after changing the password = %d", code)
	}
	if code, _ := e.localLogin(t, browser(t), "admin", "correct horse"); code != 401 {
		t.Errorf("old password = %d", code)
	}

	// Removing an account ends its sessions.
	add(b, "second", "another password")
	s2 := browser(t)
	e.localLogin(t, s2, "second", "another password")
	if code, body := do(t, b, "DELETE", e.hlm.URL+"/api/auth/accounts/second", csrf, ""); code != 204 {
		t.Fatalf("remove = %d %s", code, body)
	}
	if code, _ := do(t, s2, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 401 {
		t.Errorf("removed account's session = %d", code)
	}

	// Guessing gets refused for a while.
	var last int
	var retry string
	for range 6 {
		req, _ := http.NewRequest("POST", e.hlm.URL+"/api/auth/login", strings.NewReader(jsonBody(map[string]string{"username": "admin", "password": "guess guess"})))
		req.Header.Set("X-HLM-Request", "1")
		res, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		last, retry = res.StatusCode, res.Header.Get("Retry-After")
	}
	if last != 401 || retry == "" {
		t.Errorf("after guessing: %d, Retry-After %q", last, retry)
	}
	if code, body := e.localLogin(t, browser(t), "admin", "battery staple"); code != 401 || !strings.Contains(body, "too many") {
		t.Errorf("right password while locked out = %d %s", code, body)
	}

	// reset-password works on the same store, even with hlm running.
	pw, created, err := auth.New(e.store, false, slog.New(slog.DiscardHandler)).ResetPassword("rescue", false, false)
	if err != nil || !created || len(pw) < 20 {
		t.Fatalf("reset = %q %v %v", pw, created, err)
	}
	if code, body := e.localLogin(t, browser(t), "rescue", pw); code != 200 {
		t.Errorf("login with the reset password = %d %s", code, body)
	}
}

func TestAPIKeys(t *testing.T) {
	e := setup(t, "tok")
	b := browser(t)
	tokenHeader := map[string]string{"Authorization": "Bearer tok", "X-HLM-Request": "1"}
	// HLM_TOKEN can't make keys, but can make the first account, which can.
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/keys", tokenHeader, `{"name":"x"}`); code != 403 || !strings.Contains(body, "interactive_required") {
		t.Fatalf("key from HLM_TOKEN = %d %s", code, body)
	}
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/accounts", tokenHeader, jsonBody(map[string]string{"username": "first", "password": "correct horse"})); code != 201 {
		t.Fatalf("first account from HLM_TOKEN = %d %s", code, body)
	}
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/accounts", tokenHeader, jsonBody(map[string]string{"username": "second", "password": "correct horse"})); code != 403 {
		t.Fatalf("second account from HLM_TOKEN = %d %s", code, body)
	}
	e.localLogin(t, b, "first", "correct horse")
	e.confirm(t, b, "correct horse")
	code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/keys", csrf, `{"name":"backup script","days":30,"role":"admin"}`)
	var made struct {
		Key     string    `json:"key"`
		ID      string    `json:"id"`
		Prefix  string    `json:"prefix"`
		Expires time.Time `json:"expires"`
	}
	json.Unmarshal([]byte(body), &made)
	if code != 201 || !strings.HasPrefix(made.Key, "hlm_"+made.ID+"_") || made.Expires.IsZero() {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, _ := do(t, b, "POST", e.hlm.URL+"/api/auth/keys", csrf, `{"name":""}`); code != 400 {
		t.Errorf("unnamed key = %d", code)
	}

	anon := &http.Client{}
	bearer := func(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys", bearer(made.Key), ""); code != 200 {
		t.Errorf("with the key = %d", code)
	}
	// Keys need no CSRF header, and only work in the header: in a URL they'd
	// end up in logs and history.
	if code, _ := do(t, anon, "POST", e.hlm.URL+"/api/auth/oidc/test", bearer(made.Key), `{}`); code != 400 {
		t.Errorf("key, POST = %d (want 400 for the empty settings, not 401/403)", code)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys?token="+url.QueryEscape(made.Key), nil, ""); code != 401 {
		t.Errorf("key in query = %d", code)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys?token=tok", nil, ""); code != 401 {
		t.Errorf("HLM_TOKEN in query = %d", code)
	}
	// An admin key can't make more keys, so a stolen one can't outlive its
	// revocation.
	if code, _ := do(t, anon, "POST", e.hlm.URL+"/api/auth/keys", bearer(made.Key), `{"name":"spare"}`); code != 403 {
		t.Errorf("key making a key = %d", code)
	}
	// The secret is base64url, so it can hold "_" too.
	secret := strings.TrimPrefix(made.Key, "hlm_"+made.ID+"_")
	for _, bad := range []string{made.Key + "x", "hlm_" + made.ID + "_", "hlm_nope_" + secret, strings.TrimPrefix(made.Key, "hlm_")} {
		if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys", bearer(bad), ""); code != 401 {
			t.Errorf("bad key %q = %d", bad, code)
		}
	}
	code, body = do(t, anon, "GET", e.hlm.URL+"/api/auth/keys", bearer(made.Key), "")
	if !strings.Contains(body, `"lastUsed"`) || strings.Contains(body, `"hash"`) || strings.Contains(body, secret) {
		t.Errorf("list = %s", body)
	}

	// Revoked keys stop working.
	if code, _ := do(t, anon, "DELETE", e.hlm.URL+"/api/auth/keys/"+made.ID, bearer(made.Key), ""); code != 204 {
		t.Errorf("revoke = %d", code)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys", bearer(made.Key), ""); code != 401 {
		t.Errorf("revoked key = %d", code)
	}
}

// Records from before roles existed keep the full access they had, and
// nothing about being unreadable or unknown turns into access.
func TestRolesOfOlderRecords(t *testing.T) {
	e := setup(t, "")
	sum := sha256.Sum256([]byte("oldsecret"))
	if err := e.store.Set("auth-api-keys", []map[string]any{{"id": "old", "name": "old", "hash": hex.EncodeToString(sum[:])}, {"id": "odd", "name": "odd", "hash": hex.EncodeToString(sum[:]), "role": "owner"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Set("auth-config", auth.Config{Required: true}); err != nil {
		t.Fatal(err)
	}
	anon := &http.Client{}
	key := func(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }
	if code, body := do(t, anon, "GET", e.hlm.URL+"/api/auth/keys", key("hlm_old_oldsecret"), ""); code != 200 {
		t.Errorf("key without a role = %d %s", code, body)
	}
	if code, _ := do(t, anon, "GET", e.hlm.URL+"/api/jobs", key("hlm_odd_oldsecret"), ""); code != 403 {
		t.Errorf("key with an unknown role = %d", code)
	}
}

func TestRoles(t *testing.T) {
	e := setup(t, "")
	b := browser(t)
	csrf := map[string]string{"X-HLM-Request": "1"}
	account := func(c *http.Client, user, role string) (int, string) {
		return do(t, c, "POST", e.hlm.URL+"/api/auth/accounts", csrf, jsonBody(map[string]string{"username": user, "password": "correct horse", "role": role}))
	}
	// The first account is an admin whatever was asked for: it's what
	// requiring sign-in will rest on.
	if code, body := account(b, "first", "member"); code != 201 || !strings.Contains(body, `"role":"admin"`) {
		t.Fatalf("first account = %d %s", code, body)
	}
	if code, body := e.localLogin(t, b, "first", "correct horse"); code != 200 {
		t.Fatalf("login = %d %s", code, body)
	}
	e.confirm(t, b, "correct horse")
	if code, body := e.require(t, b, true); code != 200 {
		t.Fatalf("require sign-in = %d %s", code, body)
	}
	if code, body := account(b, "bob", ""); code != 201 || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("second account = %d %s", code, body)
	}
	if code, _ := account(b, "bad", "owner"); code != 400 {
		t.Errorf("unknown role = %d", code)
	}

	bob := browser(t)
	if code, body := e.localLogin(t, bob, "bob", "correct horse"); code != 200 {
		t.Fatalf("bob's login = %d %s", code, body)
	}
	if code, body := do(t, bob, "GET", e.hlm.URL+"/api/auth/status", nil, ""); code != 200 || !strings.Contains(body, `"role":"member"`) {
		t.Errorf("bob's status = %d %s", code, body)
	}
	for _, try := range []struct{ method, path string }{
		{"GET", "/api/auth/accounts"}, {"POST", "/api/auth/accounts"}, {"GET", "/api/auth/keys"}, {"POST", "/api/auth/keys"},
		{"GET", "/api/auth/oidc"}, {"PUT", "/api/auth/config"}, {"PUT", "/api/auth/accounts/bob/role"},
	} {
		if code, _ := do(t, bob, try.method, e.hlm.URL+try.path, csrf, `{"username":"x","password":"long enough pass","name":"x","role":"admin"}`); code != 403 {
			t.Errorf("member %s %s = %d", try.method, try.path, code)
		}
	}

	// A member changes only their own password, and has to know it.
	password := func(c *http.Client, user, current, next string) (int, string) {
		return do(t, c, "PUT", e.hlm.URL+"/api/auth/accounts/"+user+"/password", csrf, jsonBody(map[string]string{"current": current, "password": next}))
	}
	if code, _ := password(bob, "first", "correct horse", "stolen password"); code != 403 {
		t.Errorf("member changing another's password = %d", code)
	}
	if code, _ := password(bob, "bob", "wrong password", "battery staple"); code != 403 {
		t.Errorf("wrong current password = %d", code)
	}
	if code, body := password(bob, "bob", "correct horse", "battery staple"); code != 204 {
		t.Errorf("own password = %d %s", code, body)
	}
	if code, _ := do(t, bob, "GET", e.hlm.URL+"/api/groups", nil, ""); code != 200 {
		t.Error("changing their password should keep the session")
	}

	// A role applies at once, to sessions that already exist.
	role := func(user, r string) (int, string) {
		return do(t, b, "PUT", e.hlm.URL+"/api/auth/accounts/"+user+"/role", csrf, jsonBody(map[string]string{"role": r}))
	}
	if code, _ := role("first", "member"); code != 409 {
		t.Errorf("demoting the last admin = %d", code)
	}
	if code, _ := do(t, b, "DELETE", e.hlm.URL+"/api/auth/accounts/first", csrf, ""); code != 409 {
		t.Errorf("deleting the last admin = %d", code)
	}
	if code, body := role("bob", "admin"); code != 200 {
		t.Fatalf("promote = %d %s", code, body)
	}
	if code, _ := do(t, bob, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 200 {
		t.Errorf("promoted session = %d", code)
	}
	if code, body := role("first", "member"); code != 200 {
		t.Errorf("demoting with another admin left = %d %s", code, body)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 403 {
		t.Errorf("demoted session = %d", code)
	}

	// reset-password can restore an admin, but only says so without --admin.
	svc := auth.New(e.store, false, slog.New(slog.DiscardHandler))
	if _, _, err := svc.ResetPassword("first", false, false); err != nil || svc.AccountRole("first") != rbac.Member {
		t.Errorf("reset without --admin: %v, role %s", err, svc.AccountRole("first"))
	}
	if _, _, err := svc.ResetPassword("first", true, false); err != nil || svc.AccountRole("first") != rbac.Admin {
		t.Errorf("reset with --admin: %v, role %s", err, svc.AccountRole("first"))
	}
}

func TestOIDCAdmins(t *testing.T) {
	e := setup(t, "")
	b := browser(t)
	if code, body := e.saveOIDC(t, b, e.oidc()); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	do(t, b, "GET", e.hlm.URL+"/auth/login", nil, "") // ends at the web UI, which isn't served here
	e.confirm(t, b, "")
	if code, body := e.require(t, b, true); code != 200 {
		t.Fatalf("require = %d %s", code, body)
	}
	// With no admin lists, everyone let in is an admin, as before roles.
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 200 {
		t.Errorf("without admin lists = %d", code)
	}
	// Lists that would leave nobody known to be an admin are refused.
	c := e.oidc()
	c.ClientSecret = ""
	c.AdminGroups = []string{"family"}
	if code, body := e.saveOIDC(t, b, c); code != 400 || !strings.Contains(body, "admin") {
		t.Errorf("giving up admin = %d %s", code, body)
	}
	c.AdminGroups = []string{"admins"}
	if code, body := e.saveOIDC(t, b, c); code != 200 {
		t.Fatalf("keeping admin = %d %s", code, body)
	}
	if code, _ := do(t, b, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 200 {
		t.Errorf("on the list = %d", code)
	}
	// Someone not on the lists is a member, and it's the local account that
	// makes giving it up possible.
	e.prov.mu.Lock()
	e.prov.user, e.prov.groups = "carol", []string{"guests"}
	e.prov.mu.Unlock()
	carol := browser(t)
	do(t, carol, "GET", e.hlm.URL+"/auth/login", nil, "")
	if code, _ := do(t, carol, "GET", e.hlm.URL+"/api/auth/oidc", nil, ""); code != 403 {
		t.Errorf("not on the list = %d", code)
	}
	if code, body := do(t, carol, "GET", e.hlm.URL+"/api/auth/status", nil, ""); code != 200 || !strings.Contains(body, `"role":"member"`) {
		t.Errorf("carol's status = %d %s", code, body)
	}
}

func TestSignInFailures(t *testing.T) {
	e := setup(t, "")
	anon := &http.Client{}
	c := e.oidc()
	c.ClientSecret = "wrong"
	if code, body := e.saveOIDC(t, anon, c); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	code, body := do(t, browser(t), "GET", e.hlm.URL+"/auth/login", nil, "")
	if code != 502 || !strings.Contains(body, "invalid_client") {
		t.Errorf("wrong secret = %d %s", code, body)
	}
	if !e.svc.PublicOIDC().Verified.IsZero() {
		t.Error("a failed sign-in verified the settings")
	}

	// A callback without the sign-in's cookie, or with another state, fails.
	e.saveOIDC(t, anon, e.oidc())
	code, body = do(t, browser(t), "GET", e.hlm.URL+"/auth/callback?code=x&state=y", nil, "")
	if code != 400 || !strings.Contains(body, "expired") {
		t.Errorf("no cookie = %d %s", code, body)
	}
	b := browser(t)
	noFollow := *b
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := noFollow.Get(e.hlm.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if code, body := do(t, b, "GET", e.hlm.URL+"/auth/callback?code=x&state=forged", nil, ""); code != 400 || !strings.Contains(body, "belong") {
		t.Errorf("forged state = %d %s", code, body)
	}

	// The provider's own refusal is shown.
	res, _ = noFollow.Get(e.hlm.URL + "/auth/login")
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	state := loc.Query().Get("state")
	if code, body := do(t, b, "GET", e.hlm.URL+"/auth/callback?error=access_denied&error_description=nope&state="+state, nil, ""); code != 403 || !strings.Contains(body, "access_denied: nope") {
		t.Errorf("refused = %d %s", code, body)
	}

	// Bad settings are refused.
	for _, bad := range []auth.OIDCSettings{
		{Issuer: "ftp://x", ClientID: "a", ClientSecret: "b", PublicURL: e.hlm.URL},
		{Issuer: e.prov.URL, ClientID: "a", ClientSecret: "b", PublicURL: e.hlm.URL + "/sub"},
		{Issuer: e.prov.URL, ClientID: "", ClientSecret: "b", PublicURL: e.hlm.URL},
		{Issuer: e.prov.URL, ClientID: "a", ClientSecret: "b", PublicURL: e.hlm.URL, Scopes: "openid offline_access"},
	} {
		if code, _ := e.saveOIDC(t, anon, bad); code != 400 {
			t.Errorf("saved %+v: %d", bad, code)
		}
	}
}

func TestOIDCOff(t *testing.T) {
	e := setup(t, "")
	if code, body := e.saveOIDC(t, &http.Client{}, e.oidc()); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	off := auth.New(e.store, true, slog.New(slog.DiscardHandler))
	if st := off.Status(httptest.NewRequest("GET", "/", nil)); st.OIDC || !off.PublicOIDC().Disabled {
		t.Error("HLM_OIDC=off should ignore the provider settings")
	}
}

func TestDeployerIsForKeys(t *testing.T) {
	e := setup(t, "")
	if _, info, err := e.svc.CreateAPIKey("ci", 0, rbac.Deployer, nil); err != nil || info.Role != rbac.Deployer {
		t.Errorf("key = %+v, %v", info, err)
	}
	if _, err := e.svc.CreateAccount("first", "correct horse battery", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateAccount("second", "correct horse battery", rbac.Deployer); err == nil {
		t.Error("an account was made a deployer")
	}
}

func TestConfirmWithSingleSignOn(t *testing.T) {
	e := setup(t, "")
	b := browser(t)
	if code, body := e.saveOIDC(t, b, e.oidc()); code != 200 {
		t.Fatalf("save = %d %s", code, body)
	}
	do(t, b, "GET", e.hlm.URL+"/auth/login", nil, "")
	makeKey := func() (int, string) {
		return do(t, b, "POST", e.hlm.URL+"/api/auth/keys", csrf, `{"name":"k"}`)
	}
	if code, body := makeKey(); code != 403 || !strings.Contains(body, `"method":"oidc"`) {
		t.Fatalf("before confirming = %d %s", code, body)
	}

	// A provider that didn't make the user sign in again can't confirm.
	e.prov.mu.Lock()
	e.prov.authTime = func() time.Time { return time.Now().Add(-time.Hour) }
	e.prov.mu.Unlock()
	if code, body := do(t, b, "GET", e.hlm.URL+"/auth/login?confirm=1", nil, ""); code != 403 || !strings.Contains(body, "ask you to sign in again") {
		t.Errorf("stale sign-in = %d %s", code, body)
	}
	if e.prov.prompt != "login" || e.prov.maxAge != "" {
		t.Errorf("asked for prompt=%q max_age=%q", e.prov.prompt, e.prov.maxAge)
	}
	if code, _ := makeKey(); code != 403 {
		t.Errorf("after a stale sign-in = %d", code)
	}

	// Nor can someone else's sign-in.
	e.prov.mu.Lock()
	e.prov.authTime, e.prov.user = nil, "bob"
	e.prov.mu.Unlock()
	if code, body := do(t, b, "GET", e.hlm.URL+"/auth/login?confirm=1", nil, ""); code != 403 || !strings.Contains(body, "different account") {
		t.Errorf("someone else = %d %s", code, body)
	}
	if code, _ := makeKey(); code != 403 {
		t.Errorf("after someone else signed in = %d", code)
	}

	e.prov.mu.Lock()
	e.prov.user = "alice"
	e.prov.mu.Unlock()
	e.confirm(t, b, "")
	if code, body := makeKey(); code != 201 {
		t.Errorf("after confirming = %d %s", code, body)
	}
	// Confirming is for a session there already is.
	if code, _ := do(t, browser(t), "GET", e.hlm.URL+"/auth/login?confirm=1", nil, ""); code != 400 {
		t.Errorf("confirm without a session = %d", code)
	}

	// In a popup (so the page being worked on stays as it was), it ends on
	// a page that tells the web UI and closes, rather than going back.
	p := browser(t)
	do(t, p, "GET", e.hlm.URL+"/auth/login", nil, "")
	code, body := do(t, p, "GET", e.hlm.URL+"/auth/login?confirm=1&popup=1&return=/servers", nil, "")
	if code != 200 || !strings.Contains(body, `new BroadcastChannel("hlm-confirm")`) || !strings.Contains(body, "window.close()") {
		t.Errorf("popup confirm = %d %s", code, body)
	}
	if code, body := do(t, p, "POST", e.hlm.URL+"/api/auth/keys", csrf, `{"name":"p"}`); code != 201 {
		t.Errorf("after confirming in a popup = %d %s", code, body)
	}
	// popup only changes how a confirmation ends.
	if code, body := do(t, browser(t), "GET", e.hlm.URL+"/auth/login?popup=1", nil, ""); strings.Contains(body, "BroadcastChannel") {
		t.Errorf("sign-in with popup=1 = %d %s", code, body)
	}
}

func TestConfirmWithPassword(t *testing.T) {
	e := setup(t, "")
	if _, err := e.svc.CreateAccount("admin", "correct horse", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	b := browser(t)
	e.localLogin(t, b, "admin", "correct horse")
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/confirm", csrf, `{"password":"wrong one"}`); code != 403 {
		t.Errorf("wrong password = %d %s", code, body)
	}
	if code, body := do(t, b, "GET", e.hlm.URL+"/api/auth/status", nil, ""); !strings.Contains(body, "confirmedUntil") == false || code != 200 {
		t.Errorf("status before = %s", body)
	}
	e.confirm(t, b, "correct horse")
	if _, body := do(t, b, "GET", e.hlm.URL+"/api/auth/status", nil, ""); !strings.Contains(body, "confirmedUntil") {
		t.Errorf("status after = %s", body)
	}
	if code, body := do(t, b, "POST", e.hlm.URL+"/api/auth/keys", csrf, `{"name":"k"}`); code != 201 {
		t.Errorf("after confirming = %d %s", code, body)
	}
	// Without a session there's nothing to confirm.
	if code, _ := do(t, &http.Client{}, "POST", e.hlm.URL+"/api/auth/confirm", csrf, `{"password":"correct horse"}`); code != 403 && code != 401 {
		t.Errorf("no session = %d", code)
	}
}

// totp is an authenticator app for secret, at now plus steps of 30 seconds.
func totp(t *testing.T, secret string, steps int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(time.Now().Unix()/30+steps))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff%1_000_000)
}

func TestTwoFactor(t *testing.T) {
	e := setup(t, "")
	for _, u := range []string{"admin", "bob"} {
		role := rbac.Admin
		if u == "bob" {
			role = rbac.Member
		}
		if _, err := e.svc.CreateAccount(u, "correct horse", role); err != nil {
			t.Fatal(err)
		}
	}
	b := browser(t)
	e.localLogin(t, b, "admin", "correct horse")
	e.confirm(t, b, "correct horse")
	if code, body := e.require(t, b, true); code != 200 {
		t.Fatalf("require = %d %s", code, body)
	}

	// Set up: it takes a confirmed session, then a right code.
	setupURL := e.hlm.URL + "/api/auth/accounts/admin/2fa/setup"
	if code, body := do(t, b2fa(t, e), "POST", setupURL, csrf, ""); code != 403 {
		t.Fatalf("setup by another = %d %s", code, body)
	}
	code, body := do(t, b, "POST", setupURL, csrf, "")
	var setup auth.Setup
	json.Unmarshal([]byte(body), &setup)
	if code != 200 || len(setup.Secret) != 32 || !strings.HasPrefix(setup.URL, "otpauth://totp/hlm:admin?") || !strings.HasPrefix(setup.QR, "<svg") {
		t.Fatalf("setup = %d %s", code, body)
	}
	// Someone else can't set up admin's.
	b2 := browser(t)
	e.localLogin(t, b2, "bob", "correct horse")
	if code, _ := do(t, b2, "POST", setupURL, csrf, ""); code != 403 {
		t.Errorf("bob set up admin's = %d", code)
	}
	enable := e.hlm.URL + "/api/auth/accounts/admin/2fa/enable"
	if code, _ := do(t, b, "POST", enable, csrf, `{"code":"000000"}`); code != 400 {
		t.Errorf("wrong code enabled = %d", code)
	}
	enableCode := totp(t, setup.Secret, 0)
	code, body = do(t, b, "POST", enable, csrf, jsonBody(map[string]string{"code": enableCode}))
	var enabled struct {
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	json.Unmarshal([]byte(body), &enabled)
	if code != 200 || len(enabled.RecoveryCodes) != 10 {
		t.Fatalf("enable = %d %s", code, body)
	}

	// Signing in now takes a code.
	c := browser(t)
	if code, body := e.localLogin(t, c, "admin", "correct horse"); code != 200 || !strings.Contains(body, `"twoFactor":true`) {
		t.Fatalf("password = %d %s", code, body)
	}
	if code, _ := do(t, c, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 401 {
		t.Errorf("signed in with only the password: %d", code)
	}
	login2fa := e.hlm.URL + "/api/auth/login/2fa"
	// The code used to enable it can't be used again.
	if code, _ := do(t, c, "POST", login2fa, csrf, jsonBody(map[string]string{"code": enableCode})); code != 401 {
		t.Errorf("replayed code = %d", code)
	}
	if code, body := do(t, c, "POST", login2fa, csrf, jsonBody(map[string]string{"code": totp(t, setup.Secret, 1)})); code != 200 {
		t.Fatalf("code = %d %s", code, body)
	}
	if code, _ := do(t, c, "GET", e.hlm.URL+"/api/auth/accounts", nil, ""); code != 200 {
		t.Errorf("not signed in after the code: %d", code)
	}
	// Without the password first, a code does nothing.
	if code, _ := do(t, browser(t), "POST", login2fa, csrf, `{"code":"123456"}`); code != 401 {
		t.Errorf("code without password = %d", code)
	}

	// A recovery code works once.
	d := browser(t)
	e.localLogin(t, d, "admin", "correct horse")
	rc := enabled.RecoveryCodes[0]
	if code, body := do(t, d, "POST", login2fa, csrf, jsonBody(map[string]string{"code": strings.ToUpper(rc)})); code != 200 {
		t.Fatalf("recovery code = %d %s", code, body)
	}
	d = browser(t)
	e.localLogin(t, d, "admin", "correct horse")
	if code, _ := do(t, d, "POST", login2fa, csrf, jsonBody(map[string]string{"code": rc})); code != 401 {
		t.Errorf("recovery code used twice = %d", code)
	}
	for _, a := range e.svc.Accounts() {
		if a.Username == "admin" && (!a.TwoFactor || a.RecoveryLeft != 9) {
			t.Errorf("account = %+v", a)
		}
	}

	// Confirming asks for a code too.
	if code, body := do(t, c, "POST", e.hlm.URL+"/api/auth/confirm", csrf, `{"password":"correct horse"}`); code != 403 || !strings.Contains(body, "code_required") {
		t.Errorf("confirm without code = %d %s", code, body)
	}
	if code, body := do(t, c, "PUT", e.hlm.URL+"/api/auth/keys/x", csrf, "{}"); !strings.Contains(body, `"twoFactor":true`) {
		t.Errorf("confirm prompt doesn't say a code is needed: %d %s", code, body)
	}
	if code, body := do(t, c, "POST", e.hlm.URL+"/api/auth/confirm", csrf,
		jsonBody(map[string]string{"password": "correct horse", "code": totp(t, setup.Secret, -1)})); code != 200 {
		// -1 is older than the code just used: refused. A fresh one works.
		if code, body = do(t, c, "POST", e.hlm.URL+"/api/auth/confirm", csrf,
			jsonBody(map[string]string{"password": "correct horse", "code": enabled.RecoveryCodes[1]})); code != 200 {
			t.Fatalf("confirm with code = %d %s", code, body)
		}
	}

	// Requiring it for admins: only someone who has it can turn it on.
	cfg := func(c *http.Client, req string) (int, string) {
		return do(t, c, "PUT", e.hlm.URL+"/api/auth/config", csrf, jsonBody(auth.Config{Required: true, RequireTwoFactor: req}))
	}
	if code, body := cfg(c, auth.RequireAll); code != 200 {
		t.Fatalf("require for all = %d %s", code, body)
	}
	// bob has to set it up while signing in.
	f := browser(t)
	if code, body := e.localLogin(t, f, "bob", "correct horse"); !strings.Contains(body, `"twoFactorSetup":true`) {
		t.Fatalf("bob's password = %d %s", code, body)
	}
	if code, _ := do(t, f, "POST", login2fa, csrf, `{"code":"123456"}`); code != 401 {
		t.Errorf("setup half used for a code = %d", code)
	}
	code, body = do(t, f, "POST", e.hlm.URL+"/api/auth/login/2fa/setup", csrf, "")
	var bobSetup auth.Setup
	json.Unmarshal([]byte(body), &bobSetup)
	if code != 200 || bobSetup.Secret == "" {
		t.Fatalf("bob's setup = %d %s", code, body)
	}
	code, body = do(t, f, "POST", e.hlm.URL+"/api/auth/login/2fa/enable", csrf, jsonBody(map[string]string{"code": totp(t, bobSetup.Secret, 0)}))
	if code != 200 || !strings.Contains(body, "recoveryCodes") {
		t.Fatalf("bob's enable = %d %s", code, body)
	}
	if _, body := do(t, f, "GET", e.hlm.URL+"/api/auth/status", nil, ""); !strings.Contains(body, `"username":"bob"`) {
		t.Errorf("bob not signed in after setting it up: %s", body)
	}
	// bob can't turn his own off while it's required; an admin can reset it.
	e.confirm2(t, f, "correct horse", totp(t, bobSetup.Secret, 1))
	if code, _ := do(t, f, "DELETE", e.hlm.URL+"/api/auth/accounts/bob/2fa", csrf, ""); code != 400 {
		t.Errorf("bob turned off required two-factor = %d", code)
	}
	if code, body := do(t, c, "DELETE", e.hlm.URL+"/api/auth/accounts/bob/2fa", csrf, ""); code != 204 {
		t.Errorf("admin reset = %d %s", code, body)
	}
	if e.svc.HasTwoFactor("bob") {
		t.Error("bob still has two-factor")
	}

	// The host can reset it too.
	if _, _, err := e.svc.ResetPassword("admin", false, true); err != nil || e.svc.HasTwoFactor("admin") {
		t.Errorf("reset-password --reset-2fa: %v", err)
	}
}

// b2fa is a browser signed in as a member, bob2.
func b2fa(t *testing.T, e *env) *http.Client {
	e.svc.CreateAccount("bob2", "correct horse", rbac.Member)
	c := browser(t)
	e.localLogin(t, c, "bob2", "correct horse")
	return c
}

// confirm2 confirms with a password and a two-factor code.
func (e *env) confirm2(t *testing.T, c *http.Client, password, code string) {
	t.Helper()
	if status, body := do(t, c, "POST", e.hlm.URL+"/api/auth/confirm", csrf,
		jsonBody(map[string]string{"password": password, "code": code})); status != 200 {
		t.Fatalf("confirm = %d %s", status, body)
	}
}

func TestTwoFactorCodesAreLimited(t *testing.T) {
	e := setup(t, "")
	e.svc.CreateAccount("admin", "correct horse", rbac.Admin)
	setup, _ := e.svc.BeginTwoFactor("admin")
	if _, err := e.svc.EnableTwoFactor("admin", totp(t, setup.Secret, 0)); err != nil {
		t.Fatal(err)
	}
	// Five wrong codes after the right password lock it, as wrong passwords do.
	c := browser(t)
	e.localLogin(t, c, "admin", "correct horse")
	var last int
	for i := 0; i < 5; i++ {
		last, _ = do(t, c, "POST", e.hlm.URL+"/api/auth/login/2fa", csrf, `{"code":"000000"}`)
	}
	code, body := do(t, c, "POST", e.hlm.URL+"/api/auth/login/2fa", csrf, jsonBody(map[string]string{"code": totp(t, setup.Secret, 1)}))
	if last != 401 || code != 401 || !strings.Contains(body, "too many") {
		t.Errorf("after 5 wrong codes: %d %s", code, body)
	}
	// Turning on "require" without having it yourself is refused.
	e.svc.CreateAccount("other", "correct horse", rbac.Admin)
	o := browser(t)
	e.localLogin(t, o, "other", "correct horse")
	e.confirm(t, o, "correct horse")
	if code, body := do(t, o, "PUT", e.hlm.URL+"/api/auth/config", csrf, jsonBody(auth.Config{RequireTwoFactor: auth.RequireAdmins})); code != 400 ||
		!strings.Contains(body, "your own account first") {
		t.Errorf("require without having it = %d %s", code, body)
	}
}
