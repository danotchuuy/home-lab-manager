package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/webauthn/webauthntest"
)

// passkeyEnv is hlm at http://localhost:<port>, where browsers allow
// passkeys without HTTPS.
type passkeyEnv struct {
	*env
	base, origin string
}

func passkeySetup(t *testing.T) *passkeyEnv {
	e := setup(t, "")
	port := e.hlm.URL[strings.LastIndex(e.hlm.URL, ":"):]
	return &passkeyEnv{env: e, base: "http://localhost" + port, origin: "http://localhost" + port}
}

func newBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// addPasskey adds a passkey to the signed-in, confirmed browser's account.
func (p *passkeyEnv) addPasskey(t *testing.T, b *http.Client, user, name string) *webauthntest.Authenticator {
	t.Helper()
	code, opts := do(t, b, "POST", p.base+"/api/auth/accounts/"+user+"/passkeys/begin", csrf, "")
	if code != 200 {
		t.Fatalf("begin = %d %s", code, opts)
	}
	a := webauthntest.New()
	cd, att, err := a.Create([]byte(opts), p.origin)
	if err != nil {
		t.Fatal(err)
	}
	code, body := do(t, b, "POST", p.base+"/api/auth/accounts/"+user+"/passkeys", csrf,
		jsonBody(map[string]any{"name": name, "clientDataJSON": cd, "attestationObject": att, "transports": []string{"internal", "nonsense"}}))
	if code != 201 {
		t.Fatalf("add = %d %s", code, body)
	}
	return a
}

// use runs a passkey ceremony: begin, then the authenticator's answer.
func (p *passkeyEnv) use(t *testing.T, b *http.Client, a *webauthntest.Authenticator, beginPath, finishPath string) (int, string) {
	t.Helper()
	code, opts := do(t, b, "POST", p.base+beginPath, csrf, "")
	if code != 200 {
		return code, opts
	}
	as, err := a.Get([]byte(opts), p.origin)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, b, "POST", p.base+finishPath, csrf, jsonBody(as))
}

func (p *passkeyEnv) signedInAs(t *testing.T, b *http.Client) string {
	t.Helper()
	_, body := do(t, b, "GET", p.base+"/api/auth/status", nil, "")
	var st auth.Status
	json.Unmarshal([]byte(body), &st)
	if st.User == nil {
		return ""
	}
	return st.User.Username
}

func (p *passkeyEnv) login(t *testing.T, b *http.Client, user string) (int, string) {
	t.Helper()
	return do(t, b, "POST", p.base+"/api/auth/login", csrf, jsonBody(map[string]string{"username": user, "password": "correct horse"}))
}

func TestPasskeys(t *testing.T) {
	p := passkeySetup(t)
	for _, u := range []string{"admin", "bob"} {
		role := rbac.Admin
		if u == "bob" {
			role = rbac.Member
		}
		if _, err := p.svc.CreateAccount(u, "correct horse", role); err != nil {
			t.Fatal(err)
		}
	}
	b := newBrowser()
	p.login(t, b, "admin")
	// Adding a passkey takes a confirmed session.
	if code, body := do(t, b, "POST", p.base+"/api/auth/accounts/admin/passkeys/begin", csrf, ""); code != 403 || !strings.Contains(body, "confirm_required") {
		t.Fatalf("unconfirmed = %d %s", code, body)
	}
	if code, body := do(t, b, "POST", p.base+"/api/auth/confirm", csrf, jsonBody(map[string]string{"password": "correct horse"})); code != 200 {
		t.Fatalf("confirm = %d %s", code, body)
	}
	// Only from the account's owner.
	bob := newBrowser()
	p.login(t, bob, "bob")
	if code, _ := do(t, bob, "POST", p.base+"/api/auth/accounts/admin/passkeys/begin", csrf, ""); code != 403 {
		t.Errorf("bob adding admin's passkey = %d", code)
	}
	phone := p.addPasskey(t, b, "admin", "  My\nphone  ")
	_, body := do(t, b, "GET", p.base+"/api/auth/accounts/admin/passkeys", nil, "")
	var list []auth.PasskeyInfo
	json.Unmarshal([]byte(body), &list)
	if len(list) != 1 || list[0].Name != "My phone" || list[0].RPID != "localhost" || strings.Contains(body, "publicKey") {
		t.Fatalf("passkeys = %s", body)
	}
	if code, _ := do(t, bob, "GET", p.base+"/api/auth/accounts/admin/passkeys", nil, ""); code != 403 {
		t.Errorf("bob listing admin's = %d", code)
	}
	// The same authenticator can't be added twice.
	if code, opts := do(t, b, "POST", p.base+"/api/auth/accounts/admin/passkeys/begin", csrf, ""); code == 200 {
		cd, att, _ := phone.Create([]byte(opts), p.origin)
		if code, body := do(t, b, "POST", p.base+"/api/auth/accounts/admin/passkeys", csrf, jsonBody(map[string]any{"clientDataJSON": cd, "attestationObject": att})); code != 400 || !strings.Contains(body, "already") {
			t.Errorf("added twice = %d %s", code, body)
		}
		if !strings.Contains(opts, phone.CredIDB64()) {
			t.Errorf("the account's passkeys aren't excluded: %s", opts)
		}
	}

	// Signing in with the passkey alone.
	nb := newBrowser()
	if code, body := p.use(t, nb, phone, "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 200 {
		t.Fatalf("passkey sign-in = %d %s", code, body)
	}
	if got := p.signedInAs(t, nb); got != "admin" {
		t.Fatalf("signed in as %q", got)
	}
	// Without user verification, a passkey alone isn't enough.
	phone.NoUV = true
	if code, body := p.use(t, newBrowser(), phone, "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 401 || !strings.Contains(body, "verify you") {
		t.Errorf("unverified sign-in = %d %s", code, body)
	}
	phone.NoUV = false
	// A challenge works once.
	rb := newBrowser()
	_, opts := do(t, rb, "POST", p.base+"/api/auth/login/passkey/begin", csrf, "")
	as, _ := phone.Get([]byte(opts), p.origin)
	cookies := rb.Jar.Cookies(mustURL(p.base + "/api/auth/"))
	if code, _ := do(t, rb, "POST", p.base+"/api/auth/login/passkey", csrf, jsonBody(as)); code != 200 {
		t.Fatalf("first use = %d", code)
	}
	replay := newBrowser()
	replay.Jar.SetCookies(mustURL(p.base+"/api/auth/"), cookies)
	as2, _ := phone.Get([]byte(opts), p.origin) // a fresh signature, same challenge
	if code, body := do(t, replay, "POST", p.base+"/api/auth/login/passkey", csrf, jsonBody(as2)); code != 401 || !strings.Contains(body, "already used") {
		t.Errorf("replayed challenge = %d %s", code, body)
	}
	// Another site's origin is refused.
	ob := newBrowser()
	_, opts = do(t, ob, "POST", p.base+"/api/auth/login/passkey/begin", csrf, "")
	as, _ = phone.Get([]byte(opts), "https://evil.example")
	if code, body := do(t, ob, "POST", p.base+"/api/auth/login/passkey", csrf, jsonBody(as)); code != 401 || !strings.Contains(body, "evil.example") {
		t.Errorf("other origin = %d %s", code, body)
	}
	// An unknown passkey.
	if code, body := p.use(t, newBrowser(), webauthntest.New(), "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 401 || !strings.Contains(body, "isn't one of") {
		t.Errorf("unknown passkey = %d %s", code, body)
	}
	// Not over plain HTTP on an address (or another name than localhost).
	if code, body := do(t, newBrowser(), "POST", p.hlm.URL+"/api/auth/login/passkey/begin", csrf, ""); code != 400 || !strings.Contains(body, "passkeys_unavailable") {
		t.Errorf("on an IP address = %d %s", code, body)
	}

	// After the password: the passkey is the second factor.
	sb := newBrowser()
	code, body := p.login(t, sb, "admin")
	if code != 200 || !strings.Contains(body, `"twoFactor":true`) || !strings.Contains(body, `"passkey":true`) || !strings.Contains(body, `"code":false`) {
		t.Fatalf("password = %d %s", code, body)
	}
	if p.signedInAs(t, sb) != "" {
		t.Fatal("signed in by the password alone")
	}
	if code, body := p.use(t, sb, phone, "/api/auth/login/2fa/passkey/begin", "/api/auth/login/2fa/passkey"); code != 200 {
		t.Fatalf("second factor = %d %s", code, body)
	}
	if p.signedInAs(t, sb) != "admin" {
		t.Fatal("not signed in after the passkey")
	}
	// Without the password first, there's no second step.
	if code, _ := p.use(t, newBrowser(), phone, "/api/auth/login/2fa/passkey/begin", "/api/auth/login/2fa/passkey"); code != 401 {
		t.Errorf("second factor without a password = %d", code)
	}
	// Bob's browser, half signed in, can't use admin's passkey.
	hb := newBrowser()
	p.login(t, hb, "bob") // bob has no second factor: fully signed in, so no half sign-in
	if code, _ := p.use(t, hb, phone, "/api/auth/login/2fa/passkey/begin", "/api/auth/login/2fa/passkey"); code != 401 {
		t.Errorf("second factor without a half sign-in = %d", code)
	}

	// Confirming: the password alone isn't enough any more; the passkey is.
	if code, body := do(t, sb, "POST", p.base+"/api/auth/confirm", csrf, jsonBody(map[string]string{"password": "correct horse"})); code != 403 || !strings.Contains(body, "passkey") {
		t.Errorf("confirm with the password = %d %s", code, body)
	}
	if code, body := do(t, sb, "POST", p.base+"/api/auth/keys", csrf, `{"name":"x"}`); code != 403 || !strings.Contains(body, `"passkey":true`) {
		t.Errorf("step-up prompt = %d %s", code, body)
	}
	if code, body := p.use(t, sb, phone, "/api/auth/confirm/passkey/begin", "/api/auth/confirm/passkey"); code != 200 || !strings.Contains(body, "confirmedUntil") {
		t.Fatalf("confirm with the passkey = %d %s", code, body)
	}

	// A cloned authenticator (its counter behind) is refused.
	clone := *phone
	clone.Count = 0
	if code, body := p.use(t, newBrowser(), &clone, "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 401 || !strings.Contains(body, "copied") {
		t.Errorf("cloned = %d %s", code, body)
	}

	// Renaming and removing.
	id := list[0].ID
	if code, _ := do(t, sb, "PUT", p.base+"/api/auth/accounts/admin/passkeys/"+id, csrf, `{"name":"Old phone"}`); code != 204 {
		t.Errorf("rename = %d", code)
	}
	// With two-factor required, the last one stays (the owner can't remove it).
	p.store.Set("auth-config", auth.Config{RequireTwoFactor: auth.RequireAll})
	if code, body := do(t, sb, "DELETE", p.base+"/api/auth/accounts/admin/passkeys/"+id, csrf, ""); code != 400 || !strings.Contains(body, "last passkey") {
		t.Errorf("remove the last required = %d %s", code, body)
	}
	p.store.Set("auth-config", auth.Config{})
	if code, _ := do(t, sb, "DELETE", p.base+"/api/auth/accounts/admin/passkeys/"+id, csrf, ""); code != 204 {
		t.Errorf("remove = %d", code)
	}
	if p.svc.HasPasskeys("admin") {
		t.Error("still has passkeys")
	}
	if code, _ := p.use(t, newBrowser(), phone, "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 401 {
		t.Errorf("removed passkey signed in = %d", code)
	}
}

// With two-factor required, an account without any sets one up during
// sign-in: a passkey will do.
func TestPasskeyAsRequiredTwoFactor(t *testing.T) {
	p := passkeySetup(t)
	if _, err := p.svc.CreateAccount("carol", "correct horse", rbac.Member); err != nil {
		t.Fatal(err)
	}
	p.store.Set("auth-config", auth.Config{RequireTwoFactor: auth.RequireAll})
	b := newBrowser()
	code, body := p.login(t, b, "carol")
	if code != 200 || !strings.Contains(body, "twoFactorSetup") {
		t.Fatalf("password = %d %s", code, body)
	}
	_, opts := do(t, b, "POST", p.base+"/api/auth/login/2fa/setup/passkey/begin", csrf, "")
	a := webauthntest.New()
	cd, att, err := a.Create([]byte(opts), p.origin)
	if err != nil {
		t.Fatalf("%v: %s", err, opts)
	}
	if code, body := do(t, b, "POST", p.base+"/api/auth/login/2fa/setup/passkey", csrf, jsonBody(map[string]any{"name": "Key", "clientDataJSON": cd, "attestationObject": att})); code != 200 {
		t.Fatalf("setup = %d %s", code, body)
	}
	if p.signedInAs(t, b) != "carol" || !p.svc.HasPasskeys("carol") {
		t.Fatal("not signed in with the new passkey")
	}
	// Next time, the password asks for it, and the passkey alone works.
	nb := newBrowser()
	if code, body := p.login(t, nb, "carol"); !strings.Contains(body, `"passkey":true`) {
		t.Errorf("next sign-in = %d %s", code, body)
	}
	if code, body := p.use(t, newBrowser(), a, "/api/auth/login/passkey/begin", "/api/auth/login/passkey"); code != 200 {
		t.Errorf("passwordless = %d %s", code, body)
	}
	// An admin's reset (or hlm reset-password --reset-2fa) removes passkeys too.
	if err := p.svc.DisableTwoFactor("carol", true); err != nil || p.svc.HasPasskeys("carol") {
		t.Errorf("reset: %v, passkeys left %v", err, p.svc.HasPasskeys("carol"))
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
