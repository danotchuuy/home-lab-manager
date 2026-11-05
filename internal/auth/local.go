package auth

// Local accounts: a username and password kept by hlm itself, for when the
// OIDC provider is down or there isn't one.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const (
	secretAccounts = "auth-accounts"

	minPasswordLength = 10
	maxPasswordLength = 1024
)

// argon2id parameters, OWASP's recommendation: 19 MiB, 2 passes.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// Account is a local account. Only the password's hash is kept.
type Account struct {
	Username        string    `json:"username"`
	Hash            string    `json:"hash"`
	Created         time.Time `json:"created"`
	PasswordChanged time.Time `json:"passwordChanged"`
	// Role is the account's global role. Accounts made before roles existed
	// have none, and stay admins; see role.
	Role rbac.Role `json:"role,omitempty"`
	// SSHKeys can sign in as this account through the API.
	SSHKeys []SSHKey `json:"sshKeys,omitempty"`
	// TwoFactor is the account's authenticator app, if it uses one, and
	// Pending a new one being set up.
	TwoFactor *TwoFactor        `json:"twoFactor,omitempty"`
	Pending   *pendingTwoFactor `json:"pendingTwoFactor,omitempty"`
	// Passkeys can sign in alone, or be the second factor.
	Passkeys []Passkey `json:"passkeys,omitempty"`
	// WebAuthnID is the account's user handle for passkeys (base64url).
	WebAuthnID string `json:"webauthnId,omitempty"`
}

// role returns the account's global role. An account without one predates
// roles, when every account could do everything.
func (a Account) role() rbac.Role {
	if a.Role == rbac.None {
		return rbac.Admin
	}
	return a.Role
}

func (a Account) info() AccountInfo {
	i := AccountInfo{Username: a.Username, Created: a.Created, PasswordChanged: a.PasswordChanged, Role: a.role(), SSHKeys: len(a.SSHKeys)}
	if a.TwoFactor != nil {
		i.TwoFactor, i.RecoveryLeft = true, len(a.TwoFactor.Recovery)
	}
	i.Passkeys = len(a.Passkeys)
	return i
}

// version identifies the account's current password, so a new one ends
// sessions started with the old.
func (a Account) version() int64 { return a.PasswordChanged.UnixNano() }

// AccountInfo is an account without its hash, for the UI.
type AccountInfo struct {
	Username        string    `json:"username"`
	Created         time.Time `json:"created"`
	PasswordChanged time.Time `json:"passwordChanged"`
	Role            rbac.Role `json:"role"`
	SSHKeys         int       `json:"sshKeys"`
	// TwoFactor says the account uses an authenticator app; RecoveryLeft is
	// how many of its recovery codes are unused.
	TwoFactor    bool `json:"twoFactor"`
	RecoveryLeft int  `json:"recoveryLeft,omitempty"`
	Passkeys     int  `json:"passkeys,omitempty"`
}

func (s *Service) accounts() []Account {
	var list []Account
	if _, err := s.secrets.Get(secretAccounts, &list); err != nil {
		s.log.Error("read local accounts", "err", err)
	}
	return list
}

func (s *Service) account(username string) (Account, bool) {
	for _, a := range s.accounts() {
		if strings.EqualFold(a.Username, username) {
			return a, true
		}
	}
	return Account{}, false
}

// Accounts lists the local accounts.
func (s *Service) Accounts() []AccountInfo {
	out := []AccountInfo{}
	for _, a := range s.accounts() {
		out = append(out, a.info())
	}
	slices.SortFunc(out, func(a, b AccountInfo) int {
		return strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username))
	})
	return out
}

var validUsername = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

func checkPassword(p string) error {
	if len(p) < minPasswordLength {
		return fmt.Errorf("the password needs at least %d characters", minPasswordLength)
	}
	if len(p) > maxPasswordLength {
		return errors.New("the password is too long")
	}
	return nil
}

// ErrAccountExists and ErrNoAccount are returned for a username that is, or
// isn't, taken.
var (
	ErrAccountExists = errors.New("there's already an account with that username")
	ErrNoAccount     = errors.New("there's no account with that username")
)

// CreateAccount adds a local account with a global role: a member if none is
// given. The first account is always an admin, since it's what requiring
// sign-in will depend on.
func (s *Service) CreateAccount(username, password string, role rbac.Role) (AccountInfo, error) {
	if role == rbac.None {
		role = rbac.Member
	}
	if !role.ForAccount() {
		return AccountInfo{}, fmt.Errorf("an account's role is admin or member, not %q", role)
	}
	username = strings.TrimSpace(username)
	if !validUsername.MatchString(username) {
		return AccountInfo{}, errors.New("a username is 1 to 64 letters, digits, dots, dashes, underscores or @, starting with a letter or digit")
	}
	if err := checkPassword(password); err != nil {
		return AccountInfo{}, err
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	if _, ok := s.account(username); ok {
		return AccountInfo{}, ErrAccountExists
	}
	if len(list) == 0 {
		role = rbac.Admin
	}
	now := s.now().UTC()
	a := Account{Username: username, Hash: hashPassword(password), Created: now, PasswordChanged: now, Role: role}
	if err := s.secrets.Set(secretAccounts, append(list, a)); err != nil {
		return AccountInfo{}, err
	}
	s.log.Info("added local account", "user", username, "role", role)
	return a.info(), nil
}

// hasLocalAdmin reports whether any of the accounts is an admin.
func hasLocalAdmin(list []Account) bool {
	return slices.ContainsFunc(list, func(a Account) bool { return a.role() == rbac.Admin })
}

// adminWayIn reports whether an admin could still sign in if accounts were
// the local accounts, with the request's user being the one signed in.
func (s *Service) adminWayIn(r *http.Request, accounts []Account) bool {
	if hasLocalAdmin(accounts) {
		return true
	}
	c, ok := s.activeOIDC()
	if !ok || !c.verifiedNow() {
		return false
	}
	if !c.limitsAdmins() {
		return true
	}
	// Only the provider knows who's on the lists, except for this request.
	u, signedIn := s.Session(r)
	return signedIn && u.Method == MethodOIDC && c.role(u) == rbac.Admin
}

// ErrLastAdmin means a change would leave nobody able to sign in as an admin.
var ErrLastAdmin = errors.New("sign-in is required and this is the only admin way in, so this would lock everyone out; set up single sign-on or add another admin account first")

// SetAccountRole changes an account's global role, which applies at once.
// While sign-in is required, the last way in as an admin can't be demoted.
func (s *Service) SetAccountRole(r *http.Request, username string, role rbac.Role) (AccountInfo, error) {
	if !role.ForAccount() {
		return AccountInfo{}, fmt.Errorf("an account's role is admin or member, not %q", role)
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return AccountInfo{}, ErrNoAccount
	}
	demoted := list[i].role() == rbac.Admin && role != rbac.Admin
	list[i].Role = role
	if demoted && s.Required() && !s.adminWayIn(r, list) {
		return AccountInfo{}, ErrLastAdmin
	}
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		return AccountInfo{}, err
	}
	s.log.Info("changed local account's role", "user", list[i].Username, "role", role)
	return list[i].info(), nil
}

// VerifyPassword checks a local account's password, with the same limit on
// guesses as signing in, so a stolen session can't change it without
// knowing it.
func (s *Service) VerifyPassword(username, password string) error {
	if err := s.checkPasswordGuess(username, password); err != nil {
		return err
	}
	s.limiter.succeed(strings.ToLower(strings.TrimSpace(username)))
	return nil
}

// checkPasswordGuess is VerifyPassword without forgetting earlier failures,
// for when a second factor is still to come.
func (s *Service) checkPasswordGuess(username, password string) error {
	key := strings.ToLower(strings.TrimSpace(username))
	if wait := s.limiter.wait(key, s.now()); wait > 0 {
		return &LoginError{fmt.Errorf("too many wrong passwords for %s; try again in %s", username, roundUp(wait)), wait}
	}
	a, ok := s.account(key)
	hash := dummyHash
	if ok {
		hash = a.Hash
	}
	if !passwordMatches(hash, password) || !ok {
		return &LoginError{errors.New("wrong password"), s.limiter.fail(key, s.now())}
	}
	return nil
}

// setPassword changes an account's password, ending its sessions.
func (s *Service) setPassword(username, password string) (Account, error) {
	if err := checkPassword(password); err != nil {
		return Account{}, err
	}
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return Account{}, ErrNoAccount
	}
	changed := s.now().UTC()
	if !changed.After(list[i].PasswordChanged) { // a new version even within the same instant
		changed = list[i].PasswordChanged.Add(time.Nanosecond)
	}
	list[i].Hash, list[i].PasswordChanged = hashPassword(password), changed
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		return Account{}, err
	}
	s.log.Info("changed local account's password", "user", list[i].Username)
	return list[i], nil
}

// ChangePassword sets an account's password. Its other sessions end; if the
// request is signed in as that account, it gets a new session.
func (s *Service) ChangePassword(w http.ResponseWriter, r *http.Request, username, password string) error {
	me, signedIn := s.Session(r)
	a, err := s.setPassword(username, password)
	if err != nil {
		return err
	}
	if signedIn && me.Method == MethodLocal && strings.EqualFold(me.Username, a.Username) {
		me.Version = a.version()
		return s.startSession(w, me, secureRequest(r))
	}
	return nil
}

// DeleteAccount removes a local account, ending its sessions. While sign-in
// is required, the last way in as an admin can't be removed.
func (s *Service) DeleteAccount(r *http.Request, username string) error {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return ErrNoAccount
	}
	wasAdmin := list[i].role() == rbac.Admin
	name := list[i].Username
	rest := slices.Delete(slices.Clone(list), i, i+1)
	if wasAdmin && s.Required() && !s.adminWayIn(r, rest) {
		return ErrLastAdmin
	}
	if err := s.secrets.Set(secretAccounts, rest); err != nil {
		return err
	}
	s.log.Info("removed local account", "user", name)
	return nil
}

// ResetPassword gives an account a new random password and returns it. It's
// for "hlm reset-password", when nobody can sign in. An account it has to
// create is an admin; makeAdmin makes an existing account one too, and
// resetTwoFactor turns off its two-factor (it's asked for again at the next
// sign-in if required).
func (s *Service) ResetPassword(username string, makeAdmin, resetTwoFactor bool) (password string, created bool, err error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	password = base64.RawURLEncoding.EncodeToString(b)
	if _, ok := s.account(username); ok {
		if _, err = s.setPassword(username, password); err != nil {
			return "", false, err
		}
		if makeAdmin {
			// Whoever runs this is on the host, so there's no lockout to prevent.
			if _, err = s.SetAccountRole(nil, username, rbac.Admin); err != nil {
				return "", false, err
			}
		}
		if resetTwoFactor {
			err = s.DisableTwoFactor(username, true)
		}
		return password, false, err
	}
	_, err = s.CreateAccount(username, password, rbac.Admin)
	return password, err == nil, err
}

// AccountRole returns a local account's global role, or rbac.None if there's
// no such account.
func (s *Service) AccountRole(username string) rbac.Role {
	a, ok := s.account(username)
	if !ok {
		return rbac.None
	}
	return a.role()
}

// --- password hashes ---

// hashPassword returns an argon2id hash in the usual
// $argon2id$v=19$m=…,t=…,p=…$salt$hash form.
func hashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// passwordMatches checks password against a hash from hashPassword.
func passwordMatches(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 || m > 1<<20 || t > 16 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when there's no such account, so a wrong
// username takes as long as a wrong password.
var dummyHash = hashPassword("not a real password")

// --- signing in ---

// ErrBadLogin is returned for a wrong username or password, without
// saying which.
var ErrBadLogin = errors.New("wrong username or password")

// LoginError says why a local sign-in failed, and for how long more
// attempts are refused.
type LoginError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *LoginError) Error() string { return e.Err.Error() }
func (e *LoginError) Unwrap() error { return e.Err }

// LocalLogin signs in with a local account's username and password (a JSON
// body) and starts a session.
func (s *Service) LocalLogin(w http.ResponseWriter, r *http.Request) (User, error) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		return User{}, err
	}
	key := strings.ToLower(strings.TrimSpace(in.Username))
	refused := func(why string) {
		s.record(audit.Entry{Actor: key, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.login",
			Result: audit.ResultDenied, Error: why})
	}
	if wait := s.limiter.wait(key, s.now()); wait > 0 {
		refused("locked after repeated wrong passwords")
		return User{}, &LoginError{fmt.Errorf("too many wrong passwords for %s; try again in %s", in.Username, roundUp(wait)), wait}
	}
	a, ok := s.account(key)
	hash := dummyHash
	if ok {
		hash = a.Hash
	}
	if !passwordMatches(hash, in.Password) || !ok {
		wait := s.limiter.fail(key, s.now())
		s.log.Warn("refused local sign-in", "user", in.Username, "remote", r.RemoteAddr)
		if wait > 0 {
			refused("wrong password; now locked for " + roundUp(wait).String())
		} else {
			refused("wrong username or password")
		}
		if wait > 0 && s.notifier != nil {
			s.notifier.Notify(notify.Event{
				Kind:  notify.KindLockedOut,
				Title: "Sign-in locked for " + key,
				Message: fmt.Sprintf("After repeated wrong passwords for %s, sign-in for it is refused for %s. The last attempt came from %s.",
					key, roundUp(wait), clientIP(r)),
				Link: "/system#sign-in",
				Key:  notify.DedupeKey(notify.KindLockedOut, key),
			})
		}
		return User{}, &LoginError{ErrBadLogin, wait}
	}
	// The password is right; a second factor may still be needed. Failures
	// aren't forgotten until it's given, so codes count towards them too.
	if a.hasSecondFactor() || s.mustUseTwoFactor(a) {
		setup := !a.hasSecondFactor()
		if err := s.startHalf(w, r, a, setup); err != nil {
			return User{}, err
		}
		return User{}, &TwoFactorNeeded{Setup: setup, Code: a.TwoFactor != nil, Passkey: len(a.Passkeys) > 0}
	}
	return s.finishLogin(w, r, a, "")
}

func roundUp(d time.Duration) time.Duration {
	return d.Round(time.Second) + time.Second
}

// limiter slows down password guessing: after a few wrong passwords for a
// username, more attempts are refused for a while, doubling each time.
type limiter struct {
	mu   sync.Mutex
	byID map[string]*failures
}

type failures struct {
	count int
	until time.Time
	last  time.Time
}

const (
	freeFailures = 5
	firstLockout = 30 * time.Second
	maxLockout   = 15 * time.Minute
	forgetAfter  = time.Hour
)

// wait returns how long attempts for key are still refused.
func (l *limiter) wait(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.byID[key]; f != nil && now.Before(f.until) {
		return f.until.Sub(now)
	}
	return 0
}

// fail counts a wrong password and returns how long attempts are refused.
func (l *limiter) fail(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = map[string]*failures{}
	}
	for k, f := range l.byID { // keep the map small
		if now.Sub(f.last) > forgetAfter {
			delete(l.byID, k)
		}
	}
	f := l.byID[key]
	if f == nil {
		f = &failures{}
		l.byID[key] = f
	}
	f.count++
	f.last = now
	if f.count < freeFailures {
		return 0
	}
	d := firstLockout << min(f.count-freeFailures, 10)
	f.until = now.Add(min(d, maxLockout))
	return f.until.Sub(now)
}

func (l *limiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byID, key)
}

// UseNotifier sets where locked sign-ins are reported.
func (s *Service) UseNotifier(n notify.Notifier) { s.notifier = n }

// clientIP is the address a request came from, without the port.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// HashPassword hashes a password the way local accounts' are (argon2id),
// for other features' passwords, such as the status page's.
func HashPassword(password string) string { return hashPassword(password) }

// PasswordMatches checks a password against a hash from HashPassword, in
// constant time.
func PasswordMatches(hash, password string) bool { return passwordMatches(hash, password) }
