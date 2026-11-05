package auth

// Two-factor sign-in for local accounts: a code from an authenticator app
// (TOTP, RFC 6238), or a single-use recovery code, after the password.
//
// A password that's right for an account with two-factor gives a "half
// sign-in" cookie, good for a few minutes, that only LoginCode (or, for an
// account that has to set two-factor up first, LoginSetup and LoginEnable)
// accept. Wrong codes count towards the same lockout as wrong passwords.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/qr"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

const (
	// HalfCookie holds a sign-in waiting for its second factor.
	HalfCookie = "hlm_2fa"
	halfWindow = 5 * time.Minute
	// setupWindow is how long a secret being set up waits for its first
	// code.
	setupWindow = 15 * time.Minute

	totpStep   = 30 // seconds
	totpDigits = 6
	// recoveryCodes is how many recovery codes an account gets.
	recoveryCodes = 10
	issuer        = "hlm"
)

// Who has to use two-factor (Config.RequireTwoFactor).
const (
	RequireNone   = ""
	RequireAdmins = "admins"
	RequireAll    = "all"
)

// TwoFactor is an account's authenticator app and recovery codes.
type TwoFactor struct {
	Secret string `json:"secret"` // base32
	// Recovery are the unused recovery codes' hashes (argon2id).
	Recovery []string  `json:"recovery"`
	LastStep int64     `json:"lastStep"` // the newest code used, so it can't be used again
	Enabled  time.Time `json:"enabled"`
}

// pendingTwoFactor is a secret being set up.
type pendingTwoFactor struct {
	Secret  string    `json:"secret"`
	Started time.Time `json:"started"`
}

// --- TOTP ---

// totpCode is the code for a secret at a 30-second step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

func decodeSecret(s string) ([]byte, error) {
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
}

// checkTOTP returns the step a code is for, allowing a step of clock drift
// either way, if it's newer than last.
func checkTOTP(secret, code string, now time.Time, last int64) (int64, bool) {
	key, err := decodeSecret(secret)
	if err != nil || len(code) != totpDigits {
		return 0, false
	}
	step := now.Unix() / totpStep
	for _, s := range []int64{step, step - 1, step + 1} {
		if s > last && hmac.Equal([]byte(totpCode(key, s)), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}

func newSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

// newRecoveryCodes makes recovery codes (xxxxx-xxxxx, 50 bits each) and
// their hashes.
func newRecoveryCodes() (codes, hashes []string) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i
	for i := 0; i < recoveryCodes; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		c := make([]byte, 0, 11)
		for j, x := range b {
			if j == 5 {
				c = append(c, '-')
			}
			c = append(c, alphabet[int(x)%len(alphabet)])
		}
		codes = append(codes, string(c))
		hashes = append(hashes, hashPassword(normalizeRecovery(string(c))))
	}
	return codes, hashes
}

func normalizeRecovery(c string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(c))
}

// --- Setting it up ---

// Setup is a secret being set up: as text, as an otpauth:// URL, and as a QR
// code of that URL (SVG).
type Setup struct {
	Secret string `json:"secret"`
	URL    string `json:"url"`
	QR     string `json:"qr"`
}

// ErrNoAccount2FA is returned for two-factor changes to an account that
// doesn't exist.
var errNoPending = errors.New("start setting up two-factor again: the code to confirm it wasn't entered within 15 minutes")

// updateAccount changes one account.
func (s *Service) updateAccount(username string, fn func(*Account) error) (Account, error) {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	list := s.accounts()
	i := slices.IndexFunc(list, func(a Account) bool { return strings.EqualFold(a.Username, username) })
	if i < 0 {
		return Account{}, ErrNoAccount
	}
	if err := fn(&list[i]); err != nil {
		return Account{}, err
	}
	if err := s.secrets.Set(secretAccounts, list); err != nil {
		return Account{}, err
	}
	return list[i], nil
}

// BeginTwoFactor makes a new secret for an account, to confirm with
// EnableTwoFactor. An account that has two-factor keeps it until then.
func (s *Service) BeginTwoFactor(username string) (Setup, error) {
	secret := newSecret()
	a, err := s.updateAccount(username, func(a *Account) error {
		a.Pending = &pendingTwoFactor{Secret: secret, Started: s.now()}
		return nil
	})
	if err != nil {
		return Setup{}, err
	}
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + issuer + ":" + a.Username}
	u.RawQuery = url.Values{"secret": {secret}, "issuer": {issuer}}.Encode()
	setup := Setup{Secret: secret, URL: u.String()}
	if c, err := qr.Encode([]byte(setup.URL)); err == nil {
		setup.QR = c.SVG()
	}
	return setup, nil
}

// EnableTwoFactor turns on two-factor for an account with the secret from
// BeginTwoFactor, once a code from it is right, and returns the recovery
// codes (shown only now).
func (s *Service) EnableTwoFactor(username, code string) ([]string, error) {
	var codes []string
	a, err := s.updateAccount(username, func(a *Account) error {
		p := a.Pending
		if p == nil || s.now().Sub(p.Started) > setupWindow {
			return errNoPending
		}
		step, ok := checkTOTP(p.Secret, cleanCode(code), s.now(), 0)
		if !ok {
			return errors.New("that code isn't right: check the app has the new entry, and that this machine's clock is right")
		}
		var hashes []string
		codes, hashes = newRecoveryCodes()
		a.TwoFactor = &TwoFactor{Secret: p.Secret, Recovery: hashes, LastStep: step, Enabled: s.now().UTC()}
		a.Pending = nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("turned on two-factor", "user", a.Username)
	return codes, nil
}

// ErrTwoFactorRequired is returned for turning two-factor off on an account
// that has to use it.
var ErrTwoFactorRequired = errors.New("two-factor is required for this account (System → Sign-in); an admin can reset it, and it's set up again at the next sign-in")

// DisableTwoFactor turns off an account's authenticator app. Unless forced
// (an admin resetting it), an account that must use two-factor can't,
// unless it has a passkey. Forced, it removes the account's passkeys too, so
// someone locked out can set a second factor up again.
func (s *Service) DisableTwoFactor(username string, force bool) error {
	a, err := s.updateAccount(username, func(a *Account) error {
		if !force && s.mustUseTwoFactor(*a) && len(a.Passkeys) == 0 {
			return ErrTwoFactorRequired
		}
		a.TwoFactor, a.Pending = nil, nil
		if force {
			a.Passkeys = nil
		}
		return nil
	})
	if err == nil {
		s.log.Info("turned off two-factor", "user", a.Username, "reset", force)
	}
	return err
}

// NewRecoveryCodes replaces an account's recovery codes.
func (s *Service) NewRecoveryCodes(username string) ([]string, error) {
	var codes []string
	_, err := s.updateAccount(username, func(a *Account) error {
		if a.TwoFactor == nil {
			return errors.New("two-factor isn't on for this account")
		}
		var hashes []string
		codes, hashes = newRecoveryCodes()
		a.TwoFactor.Recovery = hashes
		return nil
	})
	return codes, err
}

// HasTwoFactor reports whether a local account uses two-factor.
func (s *Service) HasTwoFactor(username string) bool {
	a, ok := s.account(username)
	return ok && a.TwoFactor != nil
}

// mustUseTwoFactor reports whether the settings require an account to use
// two-factor.
func (s *Service) mustUseTwoFactor(a Account) bool {
	switch s.config().RequireTwoFactor {
	case RequireAll:
		return true
	case RequireAdmins:
		return a.role() == rbac.Admin
	}
	return false
}

// cleanCode drops spaces and dashes people type in codes.
func cleanCode(c string) string {
	return strings.NewReplacer(" ", "", "\t", "").Replace(strings.TrimSpace(c))
}

// useCode checks a code (from the app, or a recovery code) for an account,
// and uses it up. It says which kind it was.
func (s *Service) useCode(username, code string) (string, bool) {
	code = cleanCode(code)
	kind := ""
	_, err := s.updateAccount(username, func(a *Account) error {
		t := a.TwoFactor
		if t == nil {
			return errors.New("no two-factor")
		}
		if len(code) == totpDigits && strings.Trim(code, "0123456789") == "" {
			if step, ok := checkTOTP(t.Secret, code, s.now(), t.LastStep); ok {
				t.LastStep, kind = step, "app"
				return nil
			}
			return errors.New("wrong code")
		}
		rc := normalizeRecovery(code)
		for i, h := range t.Recovery {
			if passwordMatches(h, rc) {
				t.Recovery = slices.Delete(t.Recovery, i, i+1)
				kind = "recovery code"
				return nil
			}
		}
		return errors.New("wrong code")
	})
	return kind, err == nil
}

// --- Signing in ---

// half is a sign-in waiting for its second factor.
type half struct {
	Username string    `json:"u"`
	Version  int64     `json:"v"`
	Expires  time.Time `json:"e"`
	// Setup says the account has to set two-factor up first.
	Setup bool `json:"s,omitempty"`
}

// TwoFactorNeeded is LocalLogin's answer to a right password for an account
// that needs a second factor (or, with Setup, to set one up): Code if it has
// an authenticator app, Passkey if it has passkeys.
type TwoFactorNeeded struct{ Setup, Code, Passkey bool }

func (e *TwoFactorNeeded) Error() string { return "enter the code from your authenticator app" }

// startHalf gives the browser a half sign-in cookie.
func (s *Service) startHalf(w http.ResponseWriter, r *http.Request, a Account, setup bool) error {
	h := half{Username: a.Username, Version: a.version(), Expires: s.now().Add(halfWindow), Setup: setup}
	v, err := s.sign("2fa", h)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: HalfCookie, Value: v, Path: "/api/auth/", Expires: h.Expires,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	return nil
}

func clearHalf(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: HalfCookie, Path: "/api/auth/", MaxAge: -1, HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
}

// ErrHalfExpired is returned when the half sign-in is missing or too old.
var ErrHalfExpired = errors.New("sign in again: the password was entered more than 5 minutes ago")

// halfLogin reads the request's half sign-in.
func (s *Service) halfLogin(r *http.Request, setup bool) (Account, error) {
	ck, err := r.Cookie(HalfCookie)
	if err != nil {
		return Account{}, ErrHalfExpired
	}
	var h half
	if !s.open("2fa", ck.Value, &h) || !s.now().Before(h.Expires) || h.Setup != setup {
		return Account{}, ErrHalfExpired
	}
	a, ok := s.account(h.Username)
	if !ok || a.version() != h.Version {
		return Account{}, ErrHalfExpired
	}
	return a, nil
}

// LoginCode finishes a sign-in with the code from the account's
// authenticator app, or a recovery code.
func (s *Service) LoginCode(w http.ResponseWriter, r *http.Request, code string) (User, error) {
	a, err := s.halfLogin(r, false)
	if err != nil {
		return User{}, err
	}
	key := strings.ToLower(a.Username)
	if wait := s.limiter.wait(key, s.now()); wait > 0 {
		return User{}, &LoginError{fmt.Errorf("too many wrong codes for %s; try again in %s", a.Username, roundUp(wait)), wait}
	}
	kind, ok := s.useCode(a.Username, code)
	if !ok {
		wait := s.limiter.fail(key, s.now())
		s.log.Warn("refused two-factor code", "user", a.Username, "remote", r.RemoteAddr)
		s.record(audit.Entry{Actor: a.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.login",
			Result: audit.ResultDenied, Error: "wrong two-factor code"})
		return User{}, &LoginError{errors.New("that code isn't right"), wait}
	}
	clearHalf(w, r)
	return s.finishLogin(w, r, a, kind)
}

// LoginSetup starts setting up two-factor for an account that has to have
// it, during sign-in.
func (s *Service) LoginSetup(r *http.Request) (Setup, error) {
	a, err := s.halfLogin(r, true)
	if err != nil {
		return Setup{}, err
	}
	return s.BeginTwoFactor(a.Username)
}

// LoginEnable turns on two-factor during sign-in, and finishes it.
func (s *Service) LoginEnable(w http.ResponseWriter, r *http.Request, code string) ([]string, User, error) {
	a, err := s.halfLogin(r, true)
	if err != nil {
		return nil, User{}, err
	}
	codes, err := s.EnableTwoFactor(a.Username, code)
	if err != nil {
		return nil, User{}, err
	}
	clearHalf(w, r)
	a, _ = s.account(a.Username)
	u, err := s.finishLogin(w, r, a, "set up")
	return codes, u, err
}

// finishLogin starts a local account's session.
func (s *Service) finishLogin(w http.ResponseWriter, r *http.Request, a Account, twoFactor string) (User, error) {
	s.limiter.succeed(strings.ToLower(a.Username))
	u := User{
		Method: MethodLocal, Subject: "local:" + a.Username, Username: a.Username, Version: a.version(),
		Expires: s.now().Add(s.config().sessionLength()),
	}
	if err := s.startSession(w, u, secureRequest(r)); err != nil {
		return User{}, err
	}
	s.log.Info("signed in", "user", a.Username, "method", MethodLocal, "twoFactor", twoFactor)
	e := audit.Entry{Actor: a.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.login"}
	if twoFactor != "" {
		e.Details = map[string]string{"twoFactor": twoFactor}
	}
	s.record(e)
	return u, nil
}
