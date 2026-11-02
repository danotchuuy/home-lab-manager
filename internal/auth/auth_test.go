package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"/system#sign-in":      "/system#sign-in",
		"/servers/x?tab=logs":  "/servers/x?tab=logs",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"/a\r\nSet-Cookie: x":  "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionCookie(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, false, slog.New(slog.DiscardHandler))
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := store.Set(secretOIDC, OIDCSettings{Issuer: "https://auth.example.com", ClientID: "hlm"}); err != nil {
		t.Fatal(err)
	}
	session := func(v string) (User, bool) {
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: v})
		return s.Session(r)
	}
	v, err := s.sign("session", User{Method: MethodOIDC, Subject: "u1", Username: "alice", Expires: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := session(v); !ok || u.Username != "alice" {
		t.Fatalf("session = %+v, %v", u, ok)
	}
	// Tampering with the payload breaks the signature.
	payload, sig, _ := strings.Cut(v, ".")
	forged, _ := s.sign("session", User{Method: MethodOIDC, Subject: "u2", Username: "mallory", Expires: now.Add(time.Hour)})
	fpayload, _, _ := strings.Cut(forged, ".")
	if _, ok := session(fpayload + "." + sig); ok {
		t.Error("accepted a forged payload")
	}
	if _, ok := session(payload); ok {
		t.Error("accepted an unsigned payload")
	}
	// A login cookie can't be used as a session.
	l, _ := s.sign("login", User{Method: MethodOIDC, Subject: "u1", Expires: now.Add(time.Hour)})
	if _, ok := session(l); ok {
		t.Error("accepted a login cookie as a session")
	}
	// Sessions expire.
	now = now.Add(2 * time.Hour)
	if _, ok := session(v); ok {
		t.Error("accepted an expired session")
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %s", h)
	}
	if !passwordMatches(h, "correct horse") || passwordMatches(h, "correct horsE") || passwordMatches("", "") || passwordMatches("$argon2id$v=19$m=0,t=0,p=0$$", "") {
		t.Error("passwordMatches is wrong")
	}
	if hashPassword("correct horse") == h {
		t.Error("hashes aren't salted")
	}
}

func TestLimiter(t *testing.T) {
	var l limiter
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for i := 1; i < freeFailures; i++ {
		if d := l.fail("admin", now); d != 0 {
			t.Fatalf("failure %d locked out for %s", i, d)
		}
	}
	if d := l.fail("admin", now); d != firstLockout {
		t.Errorf("first lockout = %s", d)
	}
	if d := l.fail("admin", now); d != 2*firstLockout {
		t.Errorf("second lockout = %s", d)
	}
	if l.wait("admin", now.Add(30*time.Second)) == 0 || l.wait("other", now) != 0 {
		t.Error("lockout applies to the wrong usernames")
	}
	for range 20 {
		l.fail("admin", now)
	}
	if d := l.wait("admin", now); d != maxLockout {
		t.Errorf("lockout = %s, want at most %s", d, maxLockout)
	}
	l.succeed("admin")
	if l.wait("admin", now) != 0 {
		t.Error("success should clear the lockout")
	}
}

func TestGenerateClientSecret(t *testing.T) {
	a, err := GenerateClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateClientSecret()
	if len(a.Secret) != 64 || a.Secret == b.Secret || strings.ContainsAny(a.Secret, "+/=") {
		t.Errorf("secrets = %q, %q", a.Secret, b.Secret)
	}
	parts := strings.Split(a.AutheliaDigest, "$")
	if len(parts) != 5 || parts[1] != "pbkdf2-sha512" || parts[2] != "310000" || len(parts[3]) != 22 || len(parts[4]) != 86 || strings.ContainsAny(a.AutheliaDigest, "+=") {
		t.Errorf("digest = %q", a.AutheliaDigest)
	}
}
