package auth

// Single sign-on through an OpenID Connect provider such as Authelia,
// Authentik, Keycloak or Pocket ID.
//
// hlm is a confidential client using the authorization code flow with PKCE.
// The provider's ID token is checked (signature, issuer, audience, expiry
// and nonce), and the user's name, email and groups come from it and from
// the userinfo endpoint (Authelia puts them only there by default).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const (
	secretOIDC = "auth-oidc"

	// loginCookie holds the state of a sign-in in progress.
	loginCookie = "hlm_login"

	// LoginPath starts a sign-in; ?return= is where to go afterwards.
	LoginPath = "/auth/login"
	// CallbackPath is where the provider sends people back. It goes in the
	// provider's list of redirect URIs.
	CallbackPath = "/auth/callback"

	DefaultOIDCName = "Authelia"
	DefaultScopes   = "openid profile email groups"
	loginTTL        = 10 * time.Minute
)

// OIDCSettings are the stored provider settings, client secret included.
// They only ever live in the encrypted secrets store.
type OIDCSettings struct {
	// Name is the provider's name on the sign-in button.
	Name string `json:"name"`
	// Issuer is the provider's issuer URL, where its discovery document is
	// (<Issuer>/.well-known/openid-configuration).
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// PublicURL is the address people open hlm at; the redirect URI is
	// PublicURL + CallbackPath.
	PublicURL string `json:"publicUrl"`
	// Scopes are space-separated; "openid" is always asked for.
	Scopes string `json:"scopes,omitempty"`
	// AllowedUsers and AllowedGroups limit who may sign in: a username or
	// email in AllowedUsers, or a group in AllowedGroups. Both empty lets in
	// everyone the provider signs in.
	AllowedUsers  []string `json:"allowedUsers,omitempty"`
	AllowedGroups []string `json:"allowedGroups,omitempty"`
	// AdminUsers and AdminGroups say who of those is an admin, in the same
	// way; everyone else is a member. Both empty makes everyone an admin,
	// as everyone was before there were roles.
	AdminUsers  []string `json:"adminUsers,omitempty"`
	AdminGroups []string `json:"adminGroups,omitempty"`
	// Verified is when someone last signed in with VerifiedFor, the issuer
	// and client ID of the time.
	Verified    time.Time `json:"verified,omitzero"`
	VerifiedBy  string    `json:"verifiedBy,omitempty"`
	VerifiedFor string    `json:"verifiedFor,omitempty"`
}

// PublicOIDC is OIDCSettings without the client secret, for the UI.
type PublicOIDC struct {
	Configured    bool     `json:"configured"`
	Name          string   `json:"name"`
	Issuer        string   `json:"issuer,omitempty"`
	ClientID      string   `json:"clientId,omitempty"`
	HasSecret     bool     `json:"hasSecret"`
	PublicURL     string   `json:"publicUrl,omitempty"`
	RedirectURL   string   `json:"redirectUrl,omitempty"`
	Scopes        string   `json:"scopes"`
	AllowedUsers  []string `json:"allowedUsers"`
	AllowedGroups []string `json:"allowedGroups"`
	AdminUsers    []string `json:"adminUsers"`
	AdminGroups   []string `json:"adminGroups"`
	// Verified is set when someone has signed in with these settings'
	// provider and client.
	Verified   time.Time `json:"verified,omitzero"`
	VerifiedBy string    `json:"verifiedBy,omitempty"`
	// Disabled is set when HLM_OIDC=off makes hlm ignore these settings.
	Disabled bool   `json:"disabled,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ErrOIDCNotConfigured means no provider is set up.
var ErrOIDCNotConfigured = errors.New("single sign-on with an OpenID Connect provider isn't set up")

func (s *Service) loadOIDC() (OIDCSettings, bool, error) {
	var c OIDCSettings
	ok, err := s.secrets.Get(secretOIDC, &c)
	return c, ok, err
}

// activeOIDC returns the provider settings when they're set up and not
// turned off.
func (s *Service) activeOIDC() (OIDCSettings, bool) {
	if s.oidcOff {
		return OIDCSettings{}, false
	}
	c, ok, err := s.loadOIDC()
	if err != nil {
		s.log.Error("read single sign-on settings", "err", err)
		return OIDCSettings{}, false
	}
	return c, ok
}

func (c OIDCSettings) name() string {
	if c.Name == "" {
		return DefaultOIDCName
	}
	return c.Name
}

func (c OIDCSettings) redirectURL() string {
	if c.PublicURL == "" {
		return ""
	}
	return c.PublicURL + CallbackPath
}

func (c OIDCSettings) scopes() []string {
	out := []string{oidc.ScopeOpenID}
	for _, sc := range strings.Fields(c.Scopes) {
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// verifiedNow reports whether someone has signed in with the current
// provider and client.
func (c OIDCSettings) verifiedNow() bool {
	return !c.Verified.IsZero() && c.VerifiedFor == c.Issuer+" "+c.ClientID
}

// secure reports whether cookies should be sent only over HTTPS.
func (c OIDCSettings) secure() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}

// listed reports whether u is one of users (by username or email) or in one
// of groups.
func listed(users, groups []string, u User) bool {
	for _, a := range users {
		if (u.Username != "" && strings.EqualFold(a, u.Username)) || (u.Email != "" && strings.EqualFold(a, u.Email)) {
			return true
		}
	}
	for _, g := range u.Groups {
		if slices.Contains(groups, g) {
			return true
		}
	}
	return false
}

// allows reports whether the settings let u in.
func (c OIDCSettings) allows(u User) bool {
	if len(c.AllowedUsers) == 0 && len(c.AllowedGroups) == 0 {
		return true
	}
	return listed(c.AllowedUsers, c.AllowedGroups, u)
}

// limitsAdmins reports whether only some of the people let in are admins.
func (c OIDCSettings) limitsAdmins() bool {
	return len(c.AdminUsers) > 0 || len(c.AdminGroups) > 0
}

// role returns u's global role.
func (c OIDCSettings) role(u User) rbac.Role {
	if !c.limitsAdmins() || listed(c.AdminUsers, c.AdminGroups, u) {
		return rbac.Admin
	}
	return rbac.Member
}

// PublicOIDC returns the provider settings for the UI.
func (s *Service) PublicOIDC() PublicOIDC {
	empty := PublicOIDC{Name: DefaultOIDCName, Scopes: DefaultScopes, AllowedUsers: []string{}, AllowedGroups: []string{}, AdminUsers: []string{}, AdminGroups: []string{}, Disabled: s.oidcOff}
	c, ok, err := s.loadOIDC()
	if err != nil {
		empty.Error = err.Error()
		return empty
	}
	if !ok {
		return empty
	}
	p := PublicOIDC{
		Configured: true, Name: c.name(), Issuer: c.Issuer, ClientID: c.ClientID, HasSecret: c.ClientSecret != "",
		PublicURL: c.PublicURL, RedirectURL: c.redirectURL(), Scopes: c.Scopes,
		AllowedUsers: c.AllowedUsers, AllowedGroups: c.AllowedGroups, AdminUsers: c.AdminUsers, AdminGroups: c.AdminGroups, Disabled: s.oidcOff,
	}
	if p.AllowedUsers == nil {
		p.AllowedUsers = []string{}
	}
	if p.AllowedGroups == nil {
		p.AllowedGroups = []string{}
	}
	if p.AdminUsers == nil {
		p.AdminUsers = []string{}
	}
	if p.AdminGroups == nil {
		p.AdminGroups = []string{}
	}
	if c.verifiedNow() {
		p.Verified, p.VerifiedBy = c.Verified, c.VerifiedBy
	}
	return p
}

// mergeOIDC tidies in, keeps the saved client secret when in leaves it
// empty for the same provider and client, and keeps the saved verification.
func (s *Service) mergeOIDC(in OIDCSettings) (OIDCSettings, error) {
	saved, _, err := s.loadOIDC()
	if err != nil {
		return in, err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Issuer = strings.TrimRight(strings.TrimSpace(in.Issuer), "/")
	in.ClientID = strings.TrimSpace(in.ClientID)
	in.PublicURL = strings.TrimRight(strings.TrimSpace(in.PublicURL), "/")
	in.Scopes = strings.Join(strings.Fields(in.Scopes), " ")
	if in.Scopes == "" {
		in.Scopes = DefaultScopes
	}
	in.AllowedUsers = tidyList(in.AllowedUsers)
	in.AllowedGroups = tidyList(in.AllowedGroups)
	in.AdminUsers = tidyList(in.AdminUsers)
	in.AdminGroups = tidyList(in.AdminGroups)
	if in.ClientSecret == "" && in.Issuer == saved.Issuer && in.ClientID == saved.ClientID {
		in.ClientSecret = saved.ClientSecret
	}
	in.Verified, in.VerifiedBy, in.VerifiedFor = saved.Verified, saved.VerifiedBy, saved.VerifiedFor

	if err := checkURL("issuer URL", in.Issuer, false); err != nil {
		return in, err
	}
	if err := checkURL("address of Home Lab Manager", in.PublicURL, true); err != nil {
		return in, err
	}
	if in.ClientID == "" {
		return in, errors.New("enter the client ID registered with the provider")
	}
	if in.ClientSecret == "" {
		return in, errors.New("enter the client secret registered with the provider")
	}
	return in, nil
}

func tidyList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func checkURL(what, s string, bare bool) error {
	if s == "" {
		return fmt.Errorf("enter the %s", what)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the %s must be an http:// or https:// address, like https://auth.example.com", what)
	}
	if bare && u.Path != "" {
		return fmt.Errorf("the %s can't have a path (%s); hlm is served from the root of its address", what, u.Path)
	}
	return nil
}

// discover fetches the provider's discovery document, caching it for the
// saved issuer.
func (s *Service) discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	if s.provider != nil && s.providerFor == issuer {
		p := s.provider
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, s.client), issuer)
	if err != nil {
		return nil, fmt.Errorf("reach the provider's discovery document (%s/.well-known/openid-configuration): %w", issuer, err)
	}
	s.mu.Lock()
	s.provider, s.providerFor = p, issuer
	s.mu.Unlock()
	return p, nil
}

func (s *Service) forgetProvider() {
	s.mu.Lock()
	s.provider, s.providerFor = nil, ""
	s.mu.Unlock()
}

// OIDCTest describes a provider found by TestOIDC.
type OIDCTest struct {
	Issuer           string   `json:"issuer"`
	AuthorizationURL string   `json:"authorizationUrl"`
	Scopes           []string `json:"scopes,omitempty"` // the provider's supported scopes
	PKCE             bool     `json:"pkce"`
}

// TestOIDC checks (possibly unsaved) settings by fetching the provider's
// discovery document. The client ID and secret can only be checked by
// signing in.
func (s *Service) TestOIDC(ctx context.Context, in OIDCSettings) (OIDCTest, error) {
	c, err := s.mergeOIDC(in)
	if err != nil {
		return OIDCTest{}, err
	}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, s.client), c.Issuer)
	if err != nil {
		return OIDCTest{}, fmt.Errorf("reach the provider's discovery document (%s/.well-known/openid-configuration): %w", c.Issuer, err)
	}
	var claims struct {
		Scopes []string `json:"scopes_supported"`
		PKCE   []string `json:"code_challenge_methods_supported"`
	}
	_ = p.Claims(&claims)
	res := OIDCTest{Issuer: c.Issuer, AuthorizationURL: p.Endpoint().AuthURL, Scopes: claims.Scopes, PKCE: slices.Contains(claims.PKCE, "S256")}
	if len(claims.Scopes) > 0 {
		for _, sc := range c.scopes() {
			if !slices.Contains(claims.Scopes, sc) {
				return res, fmt.Errorf("the provider doesn't offer the %q scope; it offers %s", sc, strings.Join(claims.Scopes, ", "))
			}
		}
	}
	return res, nil
}

// SaveOIDC stores the provider settings after checking the provider can be
// reached. While sign-in is required and there are no local accounts, the
// person saving must stay allowed in.
func (s *Service) SaveOIDC(r *http.Request, in OIDCSettings) (PublicOIDC, error) {
	c, err := s.mergeOIDC(in)
	if err != nil {
		return PublicOIDC{}, err
	}
	if _, err := s.TestOIDC(r.Context(), c); err != nil {
		return PublicOIDC{}, err
	}
	if u, ok := s.Session(r); ok && u.Method == MethodOIDC && !c.allows(u) && s.Required() && len(s.accounts()) == 0 {
		return PublicOIDC{}, fmt.Errorf("these settings wouldn't let %s (you) in, and there's no local account to sign in with instead", u.Display())
	}
	if s.Required() && !hasLocalAdmin(s.accounts()) && c.limitsAdmins() {
		saved, _, err := s.loadOIDC()
		if err != nil {
			return PublicOIDC{}, err
		}
		u, ok := s.Session(r)
		changed := !slices.Equal(saved.AdminUsers, c.AdminUsers) || !slices.Equal(saved.AdminGroups, c.AdminGroups)
		if changed && (!ok || u.Method != MethodOIDC || c.role(u) != rbac.Admin) {
			return PublicOIDC{}, errors.New("these settings wouldn't keep you an admin, and there's no local admin account to sign in with instead")
		}
	}
	if err := s.secrets.Set(secretOIDC, c); err != nil {
		return PublicOIDC{}, err
	}
	s.forgetProvider()
	s.log.Info("saved single sign-on settings", "issuer", c.Issuer, "client", c.ClientID)
	return s.PublicOIDC(), nil
}

// DeleteOIDC forgets the provider settings and signs everyone out. While
// sign-in is required, a local admin account must be left to sign in with.
func (s *Service) DeleteOIDC() error {
	if s.Required() && !hasLocalAdmin(s.accounts()) {
		return errors.New("sign-in is required and there's no local admin account, so removing single sign-on would lock everyone out; add one first")
	}
	if err := s.secrets.Delete(secretOIDC); err != nil {
		return err
	}
	s.forgetProvider()
	return s.resetSessionKey()
}

// login is the state of a sign-in in progress, kept in loginCookie.
type login struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Return   string    `json:"return"`
	Expires  time.Time `json:"expires"`
	// Confirm is set when the user is confirming the session they have,
	// whose subject is Subject, by signing in again from Started.
	Confirm bool      `json:"confirm,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Started time.Time `json:"started,omitzero"`
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeReturn keeps a return path on this site.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

func (c OIDCSettings) oauth(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  c.redirectURL(),
		Scopes:       c.scopes(),
	}
}

// Login sends the browser to the provider to sign in.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	c, ok := s.activeOIDC()
	if !ok {
		s.page(w, http.StatusNotFound, "Single sign-on isn't set up", ErrOIDCNotConfigured.Error())
		return
	}
	p, err := s.discover(r.Context(), c.Issuer)
	if err != nil {
		s.page(w, http.StatusBadGateway, "Couldn't reach "+c.name(), err.Error()+" Sign in with a local account instead, if you have one.")
		return
	}
	l := login{
		State: randomString(), Nonce: randomString(), Verifier: oauth2.GenerateVerifier(),
		Return: safeReturn(r.URL.Query().Get("return")), Expires: s.now().Add(loginTTL),
	}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier)}
	if r.URL.Query().Get("confirm") == "1" {
		cur, ok := s.Session(r)
		if !ok || cur.Method != MethodOIDC {
			s.page(w, http.StatusBadRequest, "Nothing to confirm", "Confirming signs you in to "+c.name()+" again, which is for a single sign-on session.")
			return
		}
		l.Confirm, l.Subject, l.Started = true, cur.Subject, s.now()
		// Ask the provider to check who it is again, rather than reuse its
		// own session. Whether it did is checked from auth_time. Don't send
		// max_age=0 as well: Authelia treats it as never satisfied, so after
		// the user signs in again it still wants a sign-in and shows "Failed
		// to confirm your identity" instead of coming back here.
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
	}
	v, err := s.sign("login", l)
	if err != nil {
		s.page(w, http.StatusInternalServerError, "Couldn't start signing in", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: v, Path: "/auth/", MaxAge: int(loginTTL / time.Second),
		HttpOnly: true, Secure: c.secure(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, c.oauth(p).AuthCodeURL(l.State, opts...), http.StatusFound)
}

// Callback finishes a sign-in: it checks the provider's answer, starts a
// session and sends the browser back to where it signed in from.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	c, ok := s.activeOIDC()
	if !ok {
		s.page(w, http.StatusNotFound, "Single sign-on isn't set up", ErrOIDCNotConfigured.Error())
		return
	}
	var l login
	ck, err := r.Cookie(loginCookie)
	if err != nil || !s.open("login", ck.Value, &l) || !s.now().Before(l.Expires) {
		s.page(w, http.StatusBadRequest, "Sign-in expired", "This sign-in took too long or was started in another browser. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: c.secure(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if q.Get("state") != l.State {
		s.page(w, http.StatusBadRequest, "Sign-in failed", "The answer from "+c.name()+" doesn't belong to this sign-in. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		msg := e
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		s.page(w, http.StatusForbidden, c.name()+" didn't sign you in", msg)
		return
	}
	u, err := s.exchange(r.Context(), c, l, q.Get("code"))
	if err != nil {
		s.log.Warn("sign-in failed", "err", err)
		s.page(w, http.StatusBadGateway, "Sign-in failed", err.Error())
		return
	}
	if !c.allows(u) {
		s.log.Warn("refused sign-in", "user", u.Display(), "groups", u.Groups)
		s.page(w, http.StatusForbidden, "Not allowed", u.Display()+" signed in, but isn't one of the users or groups allowed into Home Lab Manager.")
		return
	}
	if l.Confirm {
		cur, ok := s.Session(r)
		switch {
		case !ok || cur.Method != MethodOIDC || cur.Subject != l.Subject || u.Subject != l.Subject:
			s.page(w, http.StatusForbidden, "Couldn't confirm", "That was a different account than the one signed in here.")
			return
		case u.AuthTime.IsZero() || u.AuthTime.Before(l.Started.Add(-time.Minute)):
			s.log.Warn("refused confirmation", "user", u.Display(), "authTime", u.AuthTime)
			s.page(w, http.StatusForbidden, "Couldn't confirm", c.name()+" didn't ask you to sign in again, so this can't confirm it's you. Sign in with a local account to make this change instead.")
			return
		}
		u.Confirmed = s.now().UTC()
	}
	if !c.verifiedNow() {
		c.Verified, c.VerifiedBy, c.VerifiedFor = s.now().UTC(), u.Display(), c.Issuer+" "+c.ClientID
		if err := s.secrets.Set(secretOIDC, c); err != nil {
			s.log.Error("save single sign-on verification", "err", err)
		}
	}
	if err := s.startSession(w, u, c.secure()); err != nil {
		s.page(w, http.StatusInternalServerError, "Couldn't start a session", err.Error())
		return
	}
	if l.Confirm {
		s.log.Info("confirmed sign-in", "user", u.Display(), "method", MethodOIDC, "remote", r.RemoteAddr)
	} else {
		s.log.Info("signed in", "user", u.Display(), "method", MethodOIDC, "remote", r.RemoteAddr)
	}
	http.Redirect(w, r, l.Return, http.StatusFound)
}

// exchange trades the code for tokens, checks the ID token, and works out
// who signed in.
func (s *Service) exchange(ctx context.Context, c OIDCSettings, l login, code string) (User, error) {
	if code == "" {
		return User{}, errors.New("the provider sent no authorization code")
	}
	p, err := s.discover(ctx, c.Issuer)
	if err != nil {
		return User{}, err
	}
	ctx = oidc.ClientContext(ctx, s.client)
	tok, err := c.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(l.Verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_client" {
			return User{}, errors.New("the provider refused the client ID or secret (invalid_client)")
		}
		return User{}, fmt.Errorf("exchange the authorization code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return User{}, errors.New("the provider sent no ID token")
	}
	idt, err := p.VerifierContext(ctx, &oidc.Config{ClientID: c.ClientID, Now: s.now}).Verify(ctx, raw)
	if err != nil {
		return User{}, fmt.Errorf("check the ID token: %w", err)
	}
	if idt.Nonce != l.Nonce {
		return User{}, errors.New("the ID token's nonce doesn't match this sign-in")
	}
	var claims userClaims
	if err := idt.Claims(&claims); err != nil {
		return User{}, fmt.Errorf("read the ID token: %w", err)
	}
	// Authelia (and others) put profile, email and groups only in userinfo
	// unless told otherwise.
	if p.UserInfoEndpoint() != "" {
		info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			return User{}, fmt.Errorf("read the user's details: %w", err)
		}
		if info.Subject != idt.Subject {
			return User{}, errors.New("the user's details are for someone else than the ID token")
		}
		var more userClaims
		if err := info.Claims(&more); err != nil {
			return User{}, fmt.Errorf("read the user's details: %w", err)
		}
		claims.fill(more)
	}
	var when struct {
		AuthTime int64 `json:"auth_time"`
	}
	_ = idt.Claims(&when)
	u := User{
		Method: MethodOIDC, Subject: idt.Subject, Username: claims.Username, Name: claims.Name, Email: claims.Email,
		Groups: claims.Groups, Expires: s.now().Add(s.config().sessionLength()),
	}
	if when.AuthTime > 0 {
		u.AuthTime = time.Unix(when.AuthTime, 0)
	}
	return u, nil
}

type userClaims struct {
	Username string   `json:"preferred_username"`
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Groups   []string `json:"groups"`
}

// fill takes the claims c doesn't have from o.
func (c *userClaims) fill(o userClaims) {
	if c.Username == "" {
		c.Username = o.Username
	}
	if c.Name == "" {
		c.Name = o.Name
	}
	if c.Email == "" {
		c.Email = o.Email
	}
	if len(c.Groups) == 0 {
		c.Groups = o.Groups
	}
}
