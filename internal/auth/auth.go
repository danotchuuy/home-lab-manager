// Package auth decides who may use hlm: people signed in with a local
// account or through an OpenID Connect provider (such as Authelia), and
// scripts with an API key.
//
// Signed-in people get a session cookie signed with a key kept in the
// secrets store, so sessions survive restarts. Whether a session still
// counts is checked on every request: a local account's session ends when
// its password changes or the account is removed, and an OIDC session when
// its user is no longer allowed in. Removing the OIDC settings replaces the
// key, signing everyone out.
//
// Sign-in can only be required by someone signed in, so there's a way in
// that's known to work. Without one (forgotten password, provider down),
// "hlm reset-password <user>" on the host sets a local account's password.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretConfig     = "auth-config"
	secretSessionKey = "auth-session-key"

	// SessionCookie holds a signed-in person's session.
	SessionCookie = "hlm_session"

	DefaultSessionHours = 24
	maxSessionHours     = 24 * 90
)

// Methods people sign in with, in User.Method.
const (
	MethodLocal = "local"
	MethodOIDC  = "oidc"
)

// Config is how sign-in applies to everyone.
type Config struct {
	// Required makes the API refuse requests without a session, an API key
	// or the HLM_TOKEN bearer token.
	Required bool `json:"required"`
	// SessionHours is how long a sign-in lasts.
	SessionHours int `json:"sessionHours,omitempty"`
}

func (c Config) sessionLength() time.Duration {
	h := c.SessionHours
	if h <= 0 {
		h = DefaultSessionHours
	}
	return time.Duration(h) * time.Hour
}

// User is who a session belongs to.
type User struct {
	// Method is MethodLocal or MethodOIDC.
	Method   string   `json:"method"`
	Subject  string   `json:"sub"`
	Username string   `json:"username,omitempty"`
	Name     string   `json:"name,omitempty"`
	Email    string   `json:"email,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	// Version is a local account's password version when the session
	// started; a new password ends older sessions.
	Version int64     `json:"version,omitempty"`
	Expires time.Time `json:"expires"`
	// Confirmed is when the session's password, or single sign-on, was last
	// confirmed; see ConfirmWindow.
	Confirmed time.Time `json:"confirmed,omitzero"`
	// AuthTime is when the provider says the user last signed in, for
	// confirming. It isn't kept in the session.
	AuthTime time.Time `json:"-"`
}

// Display is the name to show for the user.
func (u User) Display() string {
	for _, s := range []string{u.Name, u.Username, u.Email} {
		if s != "" {
			return s
		}
	}
	return u.Subject
}

// Service manages sign-in, sessions and API keys.
type Service struct {
	secrets *secrets.Store
	oidcOff bool
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time

	mu          sync.Mutex
	provider    *oidc.Provider
	providerFor string // the issuer provider was discovered from
	key         []byte // session key, once loaded

	limiter limiter
	tickets tickets
	// notifier hears about locked sign-ins; see UseNotifier.
	notifier notify.Notifier
	// boot is made each time hlm starts, so SSH challenges from before a
	// restart, whose use isn't remembered, don't work after it.
	boot string

	sshMu      sync.Mutex
	sshSpent   map[string]time.Time // used challenges, until they expire
	accountsMu sync.Mutex           // serializes changes to the account list
	keyMu      sync.Mutex
	keySeen    map[string]time.Time // API keys' last use, by ID
	keySaved   map[string]time.Time // when keySeen was last saved
}

// New creates the service. oidcOff (HLM_OIDC=off) ignores the OIDC settings.
func New(store *secrets.Store, oidcOff bool, log *slog.Logger) *Service {
	return &Service{
		secrets:  store,
		oidcOff:  oidcOff,
		client:   &http.Client{Timeout: 15 * time.Second},
		log:      log,
		now:      time.Now,
		keySeen:  map[string]time.Time{},
		keySaved: map[string]time.Time{},
		boot:     randomString(),
	}
}

func (s *Service) config() Config {
	var c Config
	if _, err := s.secrets.Get(secretConfig, &c); err != nil {
		s.log.Error("read sign-in settings", "err", err)
	}
	return c
}

// Config returns how sign-in applies to everyone.
func (s *Service) Config() Config {
	c := s.config()
	if c.SessionHours <= 0 {
		c.SessionHours = DefaultSessionHours
	}
	return c
}

// Required reports whether the API needs a session, an API key or the token.
func (s *Service) Required() bool {
	return s.config().Required
}

// ErrNotSignedIn means requiring sign-in was asked for by someone who isn't
// signed in, so there's no way in known to work.
var ErrNotSignedIn = errors.New("sign in first, with a local account or single sign-on, so requiring sign-in can't lock everyone out")

// SaveConfig stores how sign-in applies. Requiring it takes a signed-in
// request.
func (s *Service) SaveConfig(r *http.Request, in Config) (Config, error) {
	if in.SessionHours < 0 || in.SessionHours > maxSessionHours {
		return Config{}, fmt.Errorf("a sign-in can last between 1 and %d hours", maxSessionHours)
	}
	if in.Required && !s.config().Required {
		if _, ok := s.Session(r); !ok {
			return Config{}, ErrNotSignedIn
		}
	}
	if err := s.secrets.Set(secretConfig, in); err != nil {
		return Config{}, err
	}
	s.log.Info("saved sign-in settings", "required", in.Required, "sessionHours", in.SessionHours)
	return s.Config(), nil
}

// Status tells the web UI how to sign in, and who is signed in.
type Status struct {
	Required bool `json:"required"`
	// Local is set when there are local accounts to sign in with.
	Local bool `json:"local"`
	// OIDC is set when people can sign in with a provider, named OIDCName.
	OIDC     bool   `json:"oidc"`
	OIDCName string `json:"oidcName,omitempty"`
	User     *User  `json:"user,omitempty"`
}

// Status returns the sign-in options and the request's user.
func (s *Service) Status(r *http.Request) Status {
	st := Status{Required: s.Required(), Local: len(s.accounts()) > 0}
	if c, ok := s.activeOIDC(); ok {
		st.OIDC, st.OIDCName = true, c.name()
	}
	if u, ok := s.Session(r); ok {
		st.User = &u
	}
	return st
}

// --- signed cookies ---

// sessionKey returns the key cookies are signed with, making one the first
// time.
func (s *Service) sessionKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return s.key, nil
	}
	var k []byte
	ok, err := s.secrets.Get(secretSessionKey, &k)
	if err != nil {
		return nil, err
	}
	if !ok || len(k) < 32 {
		k = make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
		if err := s.secrets.Set(secretSessionKey, k); err != nil {
			return nil, err
		}
	}
	s.key = k
	return k, nil
}

// resetSessionKey replaces the session key, ending every session.
func (s *Service) resetSessionKey() error {
	s.mu.Lock()
	s.key = nil
	s.mu.Unlock()
	return s.secrets.Delete(secretSessionKey)
}

// sign encodes v as <payload>.<mac>. purpose keeps a value signed for one
// cookie from being accepted as another.
func (s *Service) sign(purpose string, v any) (string, error) {
	key, err := s.sessionKey()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac(key, purpose, payload)), nil
}

// open checks a value from sign and decodes it into v.
func (s *Service) open(purpose, value string, v any) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	key, err := s.sessionKey()
	if err != nil {
		s.log.Error("read the session key", "err", err)
		return false
	}
	if !hmac.Equal(got, mac(key, purpose, payload)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(b, v) == nil
}

func mac(key []byte, purpose, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(purpose + "\x00" + payload))
	return m.Sum(nil)
}

// --- sessions ---

// Session returns the signed-in user of a request, if the session is valid
// and still counts.
func (s *Service) Session(r *http.Request) (User, bool) {
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return User{}, false
	}
	var u User
	if !s.open("session", ck.Value, &u) || !s.now().Before(u.Expires) {
		return User{}, false
	}
	switch u.Method {
	case MethodLocal:
		a, ok := s.account(u.Username)
		return u, ok && a.version() == u.Version
	case MethodOIDC:
		c, ok := s.activeOIDC()
		return u, ok && c.allows(u)
	}
	return User{}, false
}

// Principal returns who a session's user acts as. Their global role is read
// now, not kept in the session, so a change applies at once. An OIDC user's
// groups are those of when they signed in.
func (s *Service) Principal(u User) rbac.Principal {
	p := rbac.Principal{Name: u.Display()}
	switch u.Method {
	case MethodLocal:
		if a, ok := s.account(u.Username); ok {
			p.Role = a.role()
		}
		p.Subjects = []rbac.Subject{{Kind: rbac.SubjectLocal, Name: u.Username}}
	case MethodOIDC:
		if c, ok := s.activeOIDC(); ok {
			p.Role = c.role(u)
		}
		for _, name := range []string{u.Username, u.Email} {
			if name != "" {
				p.Subjects = append(p.Subjects, rbac.Subject{Kind: rbac.SubjectOIDCUser, Name: name})
			}
		}
		for _, g := range u.Groups {
			p.Subjects = append(p.Subjects, rbac.Subject{Kind: rbac.SubjectOIDCGroup, Name: g})
		}
	}
	return p
}

// secureRequest reports whether the browser reached hlm over HTTPS, maybe
// through a proxy (such as cloudflared) that says so.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// startSession gives the browser a session cookie for u.
func (s *Service) startSession(w http.ResponseWriter, u User, secure bool) error {
	v, err := s.sign("session", u)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: v, Path: "/", Expires: u.Expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Logout ends the session. An OIDC provider's own session stays, so signing
// in again there may not ask for a password.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.Session(r); ok {
		s.log.Info("signed out", "user", u.Display(), "method", u.Method)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Home Lab Manager</title>
<link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="icon" href="/favicon.ico" sizes="48x48">
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;background:#f5f6f8;color:#1c2027;margin:0;padding:48px 16px}
main{max-width:520px;margin:0 auto;background:#fff;border:1px solid #dde1e6;border-radius:8px;padding:24px 28px}
h1{font-size:1.25rem;margin:0 0 12px}p{line-height:1.5;overflow-wrap:anywhere}a{color:#e8590c}
@media (prefers-color-scheme:dark){body{background:#111317;color:#e4e7ec}main{background:#1a1d23;border-color:#2e333d}}
</style></head>
<body><main><h1>{{.Title}}</h1><p>{{.Message}}</p><p><a href="/">Back to Home Lab Manager</a></p></main></body></html>
`))

// page shows a short message for the browser, which arrives here by
// navigating rather than from the web UI's API calls.
func (s *Service) page(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, struct{ Title, Message string }{title, message})
}
