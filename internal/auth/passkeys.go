package auth

// Passkeys (WebAuthn) for local accounts. A passkey can be used:
//
//   - on its own, to sign in without a password ("Sign in with a passkey"):
//     the authenticator has to verify the user (PIN, fingerprint, face), so
//     it's two factors in one;
//   - as the second factor after a password, instead of the authenticator
//     app's code;
//   - to confirm it's you, before changes to access.
//
// Passkeys count as two-factor where it's required. Each ceremony gets a
// random challenge, kept in a short-lived signed cookie and usable once.
// A passkey belongs to the hostname hlm was opened on when it was added
// (the WebAuthn relying party ID), and works there.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/webauthn"
)

const (
	// PasskeyCookie holds a passkey ceremony's challenge.
	PasskeyCookie = "hlm_passkey"
	passkeyWindow = 5 * time.Minute
	// maxPasskeys is how many passkeys an account may have.
	maxPasskeys = 20
	rpName      = "Home Lab Manager"
)

// Passkey is a passkey registered for an account.
type Passkey struct {
	// ID is the credential ID, base64url.
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"` // COSE_Key, base64url
	Alg       int    `json:"alg"`
	SignCount uint32 `json:"signCount"`
	// RPID is the hostname it was added on, where it works.
	RPID       string   `json:"rpId"`
	Transports []string `json:"transports,omitempty"`
	// Synced is set for passkeys that can be on several devices (a
	// password manager or a platform account's keychain).
	Synced   bool      `json:"synced,omitempty"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

// PasskeyInfo is a passkey as its owner sees it.
type PasskeyInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	RPID     string    `json:"rpId"`
	Synced   bool      `json:"synced,omitempty"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

func (p Passkey) info() PasskeyInfo {
	return PasskeyInfo{ID: p.ID, Name: p.Name, RPID: p.RPID, Synced: p.Synced, Created: p.Created, LastUsed: p.LastUsed}
}

func (p Passkey) credential() (webauthn.Credential, error) {
	id, err := webauthn.B64.DecodeString(p.ID)
	if err != nil {
		return webauthn.Credential{}, err
	}
	key, err := webauthn.B64.DecodeString(p.PublicKey)
	if err != nil {
		return webauthn.Credential{}, err
	}
	return webauthn.Credential{ID: id, PublicKey: key, Alg: p.Alg, SignCount: p.SignCount}, nil
}

// hasSecondFactor reports whether an account has an authenticator app or a
// passkey.
func (a Account) hasSecondFactor() bool { return a.TwoFactor != nil || len(a.Passkeys) > 0 }

// HasPasskeys reports whether a local account has passkeys.
func (s *Service) HasPasskeys(username string) bool {
	a, ok := s.account(username)
	return ok && len(a.Passkeys) > 0
}

// Passkeys lists an account's passkeys.
func (s *Service) Passkeys(username string) ([]PasskeyInfo, error) {
	a, ok := s.account(username)
	if !ok {
		return nil, ErrNoAccount
	}
	out := []PasskeyInfo{}
	for _, p := range a.Passkeys {
		out = append(out, p.info())
	}
	return out, nil
}

// --- Ceremonies ---

// Kinds of passkey ceremony.
const (
	ceremonyRegister = "register" // adding a passkey, signed in
	ceremonySetup    = "setup"    // adding one during sign-in, when two-factor is required
	ceremonyLogin    = "login"    // signing in with a passkey alone
	ceremonySecond   = "second"   // the second factor after a password
	ceremonyConfirm  = "confirm"  // confirming it's you
)

// ceremony is a passkey ceremony in progress, in a signed cookie.
type ceremony struct {
	Kind      string    `json:"k"`
	Challenge string    `json:"c"`
	RPID      string    `json:"r"`
	Username  string    `json:"u,omitempty"`
	Version   int64     `json:"v,omitempty"` // the account's password version, for sign-in ceremonies
	Expires   time.Time `json:"e"`
}

// ErrPasskeyExpired is returned when a ceremony's cookie is missing or old,
// or its challenge was used.
var ErrPasskeyExpired = errors.New("that took too long, or was already used: try the passkey again")

// rpFor is the relying party for the request's host.
func rpFor(r *http.Request) (string, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return webauthn.RPIDFor(host)
}

func (s *Service) startCeremony(w http.ResponseWriter, r *http.Request, c ceremony) ([]byte, error) {
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return nil, err
	}
	c.Challenge = webauthn.B64.EncodeToString(challenge)
	c.Expires = s.now().Add(passkeyWindow)
	v, err := s.sign("passkey", c)
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{Name: PasskeyCookie, Value: v, Path: "/api/auth/", Expires: c.Expires,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	return challenge, nil
}

// endCeremony reads the request's ceremony of a kind, and uses its
// challenge up: it can't be used again, whatever happens next.
func (s *Service) endCeremony(w http.ResponseWriter, r *http.Request, kind string) (ceremony, []byte, error) {
	http.SetCookie(w, &http.Cookie{Name: PasskeyCookie, Path: "/api/auth/", MaxAge: -1, HttpOnly: true,
		Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	ck, err := r.Cookie(PasskeyCookie)
	if err != nil {
		return ceremony{}, nil, ErrPasskeyExpired
	}
	var c ceremony
	if !s.open("passkey", ck.Value, &c) || c.Kind != kind || !s.now().Before(c.Expires) {
		return ceremony{}, nil, ErrPasskeyExpired
	}
	if rpID, err := rpFor(r); err != nil || rpID != c.RPID {
		return ceremony{}, nil, ErrPasskeyExpired
	}
	s.passkeyMu.Lock()
	defer s.passkeyMu.Unlock()
	now := s.now()
	for k, exp := range s.passkeySpent {
		if now.After(exp) {
			delete(s.passkeySpent, k)
		}
	}
	if _, used := s.passkeySpent[c.Challenge]; used {
		return ceremony{}, nil, ErrPasskeyExpired
	}
	if s.passkeySpent == nil {
		s.passkeySpent = map[string]time.Time{}
	}
	s.passkeySpent[c.Challenge] = c.Expires
	challenge, err := webauthn.B64.DecodeString(c.Challenge)
	if err != nil {
		return ceremony{}, nil, ErrPasskeyExpired
	}
	return c, challenge, nil
}

// CreationOptions are navigator.credentials.create()'s options, with binary
// values in base64url.
type CreationOptions struct {
	PublicKey creationPublicKey `json:"publicKey"`
}

type creationPublicKey struct {
	RP                     map[string]string `json:"rp"`
	User                   map[string]string `json:"user"`
	Challenge              string            `json:"challenge"`
	PubKeyCredParams       []credParam       `json:"pubKeyCredParams"`
	Timeout                int               `json:"timeout"`
	Attestation            string            `json:"attestation"`
	AuthenticatorSelection map[string]any    `json:"authenticatorSelection"`
	ExcludeCredentials     []credDescriptor  `json:"excludeCredentials"`
}

type credParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

type credDescriptor struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Transports []string `json:"transports,omitempty"`
}

// RequestOptions are navigator.credentials.get()'s options.
type RequestOptions struct {
	PublicKey requestPublicKey `json:"publicKey"`
}

type requestPublicKey struct {
	Challenge        string           `json:"challenge"`
	RPID             string           `json:"rpId"`
	Timeout          int              `json:"timeout"`
	UserVerification string           `json:"userVerification"`
	AllowCredentials []credDescriptor `json:"allowCredentials"`
}

func descriptors(list []Passkey, rpID string) []credDescriptor {
	out := []credDescriptor{}
	for _, p := range list {
		if p.RPID == rpID {
			out = append(out, credDescriptor{Type: "public-key", ID: p.ID, Transports: p.Transports})
		}
	}
	return out
}

// userHandle is an account's WebAuthn user ID, made the first time.
func (s *Service) userHandle(username string) (string, error) {
	a, err := s.updateAccount(username, func(a *Account) error {
		if a.WebAuthnID == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			a.WebAuthnID = webauthn.B64.EncodeToString(b)
		}
		return nil
	})
	return a.WebAuthnID, err
}

// --- Adding passkeys ---

// BeginPasskey starts adding a passkey to a (signed-in) account.
func (s *Service) BeginPasskey(w http.ResponseWriter, r *http.Request, username string) (CreationOptions, error) {
	return s.beginRegistration(w, r, username, ceremonyRegister, 0)
}

func (s *Service) beginRegistration(w http.ResponseWriter, r *http.Request, username, kind string, version int64) (CreationOptions, error) {
	rpID, err := rpFor(r)
	if err != nil {
		return CreationOptions{}, err
	}
	a, ok := s.account(username)
	if !ok {
		return CreationOptions{}, ErrNoAccount
	}
	if len(a.Passkeys) >= maxPasskeys {
		return CreationOptions{}, fmt.Errorf("an account can have up to %d passkeys: remove one first", maxPasskeys)
	}
	handle, err := s.userHandle(a.Username)
	if err != nil {
		return CreationOptions{}, err
	}
	challenge, err := s.startCeremony(w, r, ceremony{Kind: kind, RPID: rpID, Username: a.Username, Version: version})
	if err != nil {
		return CreationOptions{}, err
	}
	params := []credParam{}
	for _, alg := range webauthn.Algorithms {
		params = append(params, credParam{Type: "public-key", Alg: alg})
	}
	return CreationOptions{PublicKey: creationPublicKey{
		RP:               map[string]string{"id": rpID, "name": rpName},
		User:             map[string]string{"id": handle, "name": a.Username, "displayName": a.Username},
		Challenge:        webauthn.B64.EncodeToString(challenge),
		PubKeyCredParams: params,
		Timeout:          int(passkeyWindow / time.Millisecond),
		Attestation:      "none",
		// Discoverable, so it can sign in without a username.
		AuthenticatorSelection: map[string]any{"residentKey": "preferred", "requireResidentKey": false, "userVerification": "preferred"},
		ExcludeCredentials:     descriptors(a.Passkeys, rpID),
	}}, nil
}

// NewPasskey is a browser's new credential, with binary values in base64url.
type NewPasskey struct {
	Name              string   `json:"name"`
	ClientDataJSON    string   `json:"clientDataJSON"`
	AttestationObject string   `json:"attestationObject"`
	Transports        []string `json:"transports"`
}

// FinishPasskey adds the passkey made for BeginPasskey.
func (s *Service) FinishPasskey(w http.ResponseWriter, r *http.Request, username string, in NewPasskey) (PasskeyInfo, error) {
	c, challenge, err := s.endCeremony(w, r, ceremonyRegister)
	if err != nil {
		return PasskeyInfo{}, err
	}
	if !strings.EqualFold(c.Username, username) {
		return PasskeyInfo{}, ErrPasskeyExpired
	}
	return s.addPasskey(r, c, challenge, in)
}

// cleanName tidies a passkey's name: one line, up to 64 characters.
func cleanName(name string) string {
	name = strings.ToValidUTF8(strings.Join(strings.Fields(name), " "), "")
	if utf8.RuneCountInString(name) > 64 {
		name = strings.TrimSpace(string([]rune(name)[:64]))
	}
	return name
}

func decodeB64(field, v string) ([]byte, error) {
	b, err := webauthn.B64.DecodeString(strings.TrimRight(v, "="))
	if err != nil {
		if b, err = base64.StdEncoding.DecodeString(v); err != nil {
			return nil, fmt.Errorf("%s isn't base64", field)
		}
	}
	return b, nil
}

var allowedTransports = []string{"usb", "nfc", "ble", "smart-card", "hybrid", "internal"}

func (s *Service) addPasskey(r *http.Request, c ceremony, challenge []byte, in NewPasskey) (PasskeyInfo, error) {
	cd, err := decodeB64("clientDataJSON", in.ClientDataJSON)
	if err != nil {
		return PasskeyInfo{}, err
	}
	att, err := decodeB64("attestationObject", in.AttestationObject)
	if err != nil {
		return PasskeyInfo{}, err
	}
	rp := webauthn.RelyingParty{ID: c.RPID}
	cred, err := rp.VerifyRegistration(webauthn.Registration{ClientDataJSON: cd, AttestationObject: att}, challenge, false)
	if err != nil {
		s.log.Warn("refused a new passkey", "user", c.Username, "err", err)
		return PasskeyInfo{}, err
	}
	name := cleanName(in.Name)
	if name == "" {
		name = "Passkey"
	}
	p := Passkey{
		ID: webauthn.B64.EncodeToString(cred.ID), Name: name, PublicKey: webauthn.B64.EncodeToString(cred.PublicKey),
		Alg: cred.Alg, SignCount: cred.SignCount, RPID: c.RPID, Synced: cred.BackupEligible, Created: s.now().UTC(),
	}
	for _, t := range in.Transports {
		if slices.Contains(allowedTransports, t) && !slices.Contains(p.Transports, t) {
			p.Transports = append(p.Transports, t)
		}
	}
	_, err = s.updateAccount(c.Username, func(a *Account) error {
		if len(a.Passkeys) >= maxPasskeys {
			return fmt.Errorf("an account can have up to %d passkeys", maxPasskeys)
		}
		// A credential ID belongs to one passkey, in one account.
		for _, other := range s.accounts() {
			for _, q := range other.Passkeys {
				if q.ID == p.ID {
					return errors.New("that passkey is already added")
				}
			}
		}
		a.Passkeys = append(a.Passkeys, p)
		return nil
	})
	if err != nil {
		return PasskeyInfo{}, err
	}
	s.log.Info("added a passkey", "user", c.Username, "name", p.Name, "rpId", p.RPID)
	s.record(audit.Entry{Actor: c.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.passkey.add",
		Target: p.Name, Details: map[string]string{"rpId": p.RPID}})
	return p.info(), nil
}

// RenamePasskey renames one of an account's passkeys.
func (s *Service) RenamePasskey(username, id, name string) error {
	name = cleanName(name)
	if name == "" {
		return errors.New("give the passkey a name")
	}
	_, err := s.updateAccount(username, func(a *Account) error {
		i := slices.IndexFunc(a.Passkeys, func(p Passkey) bool { return p.ID == id })
		if i < 0 {
			return ErrNoPasskey
		}
		a.Passkeys[i].Name = name
		return nil
	})
	return err
}

// ErrNoPasskey is returned for a passkey that doesn't exist.
var ErrNoPasskey = errors.New("no such passkey")

// RemovePasskey removes one of an account's passkeys. Unless forced (an
// admin), an account that must use two-factor keeps its last second factor.
func (s *Service) RemovePasskey(username, id string, force bool) error {
	a, err := s.updateAccount(username, func(a *Account) error {
		i := slices.IndexFunc(a.Passkeys, func(p Passkey) bool { return p.ID == id })
		if i < 0 {
			return ErrNoPasskey
		}
		if !force && s.mustUseTwoFactor(*a) && a.TwoFactor == nil && len(a.Passkeys) == 1 {
			return errors.New("two-factor is required for this account, and this is its last passkey: add another passkey or the authenticator app first")
		}
		a.Passkeys = slices.Delete(a.Passkeys, i, i+1)
		return nil
	})
	if err == nil {
		s.log.Info("removed a passkey", "user", a.Username, "reset", force)
	}
	return err
}

// --- Using passkeys ---

// UsedPasskey is a browser's assertion, with binary values in base64url.
type UsedPasskey struct {
	ID                string `json:"id"`
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"userHandle"`
}

func (u UsedPasskey) assertion() (webauthn.Assertion, error) {
	var as webauthn.Assertion
	var err error
	if as.CredentialID, err = decodeB64("id", u.ID); err != nil {
		return as, err
	}
	if as.ClientDataJSON, err = decodeB64("clientDataJSON", u.ClientDataJSON); err != nil {
		return as, err
	}
	if as.AuthenticatorData, err = decodeB64("authenticatorData", u.AuthenticatorData); err != nil {
		return as, err
	}
	if as.Signature, err = decodeB64("signature", u.Signature); err != nil {
		return as, err
	}
	if u.UserHandle != "" {
		if as.UserHandle, err = decodeB64("userHandle", u.UserHandle); err != nil {
			return as, err
		}
	}
	return as, nil
}

// beginAssertion starts a ceremony using a passkey: any account's
// (username ""), or one account's.
func (s *Service) beginAssertion(w http.ResponseWriter, r *http.Request, kind, username string, version int64, requireUV bool) (RequestOptions, error) {
	rpID, err := rpFor(r)
	if err != nil {
		return RequestOptions{}, err
	}
	allow := []credDescriptor{}
	if username != "" {
		a, ok := s.account(username)
		if !ok {
			return RequestOptions{}, ErrNoAccount
		}
		allow = descriptors(a.Passkeys, rpID)
		if len(allow) == 0 {
			var where []string
			for _, p := range a.Passkeys {
				if !slices.Contains(where, p.RPID) {
					where = append(where, p.RPID)
				}
			}
			if len(where) == 0 {
				return RequestOptions{}, errors.New("this account has no passkeys")
			}
			return RequestOptions{}, fmt.Errorf("your passkeys were added on %s: open hlm there to use them", strings.Join(where, ", "))
		}
	}
	challenge, err := s.startCeremony(w, r, ceremony{Kind: kind, RPID: rpID, Username: username, Version: version})
	if err != nil {
		return RequestOptions{}, err
	}
	uv := "preferred"
	if requireUV {
		uv = "required"
	}
	return RequestOptions{PublicKey: requestPublicKey{
		Challenge: webauthn.B64.EncodeToString(challenge), RPID: rpID, Timeout: int(passkeyWindow / time.Millisecond),
		UserVerification: uv, AllowCredentials: allow,
	}}, nil
}

// usePasskey checks an assertion for a ceremony, against the ceremony's
// account (or, with none, whichever account has the passkey), and keeps the
// passkey's new counter. It returns the account and the passkey's name.
func (s *Service) usePasskey(r *http.Request, c ceremony, challenge []byte, in UsedPasskey, requireUV bool) (Account, string, error) {
	as, err := in.assertion()
	if err != nil {
		return Account{}, "", err
	}
	id := webauthn.B64.EncodeToString(as.CredentialID)
	var owner Account
	var pk Passkey
	found := false
	for _, a := range s.accounts() {
		if c.Username != "" && !strings.EqualFold(a.Username, c.Username) {
			continue
		}
		for _, p := range a.Passkeys {
			if p.ID == id {
				owner, pk, found = a, p, true
			}
		}
	}
	if !found {
		return Account{}, "", errors.New("that passkey isn't one of this hlm's (it may have been removed)")
	}
	if pk.RPID != c.RPID {
		return Account{}, "", fmt.Errorf("that passkey was added on %s: open hlm there to use it", pk.RPID)
	}
	// A passkey without a username says whose it is; it must agree.
	if len(as.UserHandle) > 0 {
		want, _ := webauthn.B64.DecodeString(owner.WebAuthnID)
		if subtle.ConstantTimeCompare(as.UserHandle, want) != 1 {
			return Account{}, "", errors.New("that passkey is for another account")
		}
	}
	cred, err := pk.credential()
	if err != nil {
		return Account{}, "", err
	}
	rp := webauthn.RelyingParty{ID: pk.RPID}
	count, err := rp.VerifyAssertion(cred, as, challenge, requireUV)
	if err != nil {
		s.log.Warn("refused a passkey", "user", owner.Username, "passkey", pk.Name, "err", err, "remote", r.RemoteAddr)
		return owner, pk.Name, err
	}
	a, err := s.updateAccount(owner.Username, func(a *Account) error {
		i := slices.IndexFunc(a.Passkeys, func(p Passkey) bool { return p.ID == id })
		if i < 0 {
			return errors.New("that passkey was removed")
		}
		// Checked again under the lock, against a concurrent use.
		if count != 0 && count <= a.Passkeys[i].SignCount {
			return webauthn.ErrCloned
		}
		a.Passkeys[i].SignCount = count
		a.Passkeys[i].LastUsed = s.now().UTC()
		return nil
	})
	if err != nil {
		return owner, pk.Name, err
	}
	return a, pk.Name, nil
}

// --- Signing in ---

// BeginPasskeyLogin starts signing in with a passkey alone.
func (s *Service) BeginPasskeyLogin(w http.ResponseWriter, r *http.Request) (RequestOptions, error) {
	return s.beginAssertion(w, r, ceremonyLogin, "", 0, true)
}

// PasskeyLogin signs in with a passkey alone: one the authenticator
// verified the user for, which stands for two factors.
func (s *Service) PasskeyLogin(w http.ResponseWriter, r *http.Request, in UsedPasskey) (User, error) {
	c, challenge, err := s.endCeremony(w, r, ceremonyLogin)
	if err != nil {
		return User{}, err
	}
	a, name, err := s.usePasskey(r, c, challenge, in, true)
	if err != nil {
		e := audit.Entry{ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.login", Result: audit.ResultDenied,
			Error: "passkey: " + err.Error()}
		if a.Username != "" {
			e.Actor = a.Username
			if wait := s.limiter.fail(strings.ToLower(a.Username), s.now()); wait > 0 {
				e.Error += "; now locked for " + roundUp(wait).String()
			}
		}
		s.record(e)
		return User{}, &LoginError{Err: err}
	}
	if wait := s.limiter.wait(strings.ToLower(a.Username), s.now()); wait > 0 {
		return User{}, &LoginError{fmt.Errorf("too many failed sign-ins for %s; try again in %s", a.Username, roundUp(wait)), wait}
	}
	clearHalf(w, r)
	return s.finishLogin(w, r, a, "passkey "+name)
}

// BeginPasskeySecond starts using a passkey as the second factor, after
// the password (a half sign-in).
func (s *Service) BeginPasskeySecond(w http.ResponseWriter, r *http.Request) (RequestOptions, error) {
	a, err := s.halfLogin(r, false)
	if err != nil {
		return RequestOptions{}, err
	}
	return s.beginAssertion(w, r, ceremonySecond, a.Username, a.version(), false)
}

// PasskeySecond finishes a sign-in with a passkey as the second factor.
func (s *Service) PasskeySecond(w http.ResponseWriter, r *http.Request, in UsedPasskey) (User, error) {
	a, err := s.halfLogin(r, false)
	if err != nil {
		return User{}, err
	}
	key := strings.ToLower(a.Username)
	if wait := s.limiter.wait(key, s.now()); wait > 0 {
		return User{}, &LoginError{fmt.Errorf("too many failed sign-ins for %s; try again in %s", a.Username, roundUp(wait)), wait}
	}
	c, challenge, err := s.endCeremony(w, r, ceremonySecond)
	if err != nil {
		return User{}, err
	}
	if !strings.EqualFold(c.Username, a.Username) || c.Version != a.version() {
		return User{}, ErrPasskeyExpired
	}
	a, name, err := s.usePasskey(r, c, challenge, in, false)
	if err != nil {
		wait := s.limiter.fail(key, s.now())
		s.record(audit.Entry{Actor: c.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.login",
			Result: audit.ResultDenied, Error: "passkey: " + err.Error()})
		return User{}, &LoginError{err, wait}
	}
	clearHalf(w, r)
	return s.finishLogin(w, r, a, "passkey "+name)
}

// BeginPasskeySetup starts adding a passkey during sign-in, for an account
// that has to use two-factor and has none yet.
func (s *Service) BeginPasskeySetup(w http.ResponseWriter, r *http.Request) (CreationOptions, error) {
	a, err := s.halfLogin(r, true)
	if err != nil {
		return CreationOptions{}, err
	}
	return s.beginRegistration(w, r, a.Username, ceremonySetup, a.version())
}

// FinishPasskeySetup adds the passkey and finishes signing in.
func (s *Service) FinishPasskeySetup(w http.ResponseWriter, r *http.Request, in NewPasskey) (User, error) {
	a, err := s.halfLogin(r, true)
	if err != nil {
		return User{}, err
	}
	c, challenge, err := s.endCeremony(w, r, ceremonySetup)
	if err != nil {
		return User{}, err
	}
	if !strings.EqualFold(c.Username, a.Username) || c.Version != a.version() {
		return User{}, ErrPasskeyExpired
	}
	if _, err := s.addPasskey(r, c, challenge, in); err != nil {
		return User{}, err
	}
	clearHalf(w, r)
	a, _ = s.account(a.Username)
	return s.finishLogin(w, r, a, "passkey set up")
}

// --- Confirming ---

// BeginPasskeyConfirm starts confirming the session with a passkey.
func (s *Service) BeginPasskeyConfirm(w http.ResponseWriter, r *http.Request) (RequestOptions, error) {
	u, ok := s.Session(r)
	if !ok {
		return RequestOptions{}, ErrNoSession
	}
	if u.Method != MethodLocal {
		return RequestOptions{}, ErrConfirmWithSSO
	}
	return s.beginAssertion(w, r, ceremonyConfirm, u.Username, u.Version, true)
}

// ConfirmPasskey confirms the session with a passkey the authenticator
// verified the user for.
func (s *Service) ConfirmPasskey(w http.ResponseWriter, r *http.Request, in UsedPasskey) (User, error) {
	u, ok := s.Session(r)
	if !ok {
		return User{}, ErrNoSession
	}
	if u.Method != MethodLocal {
		return User{}, ErrConfirmWithSSO
	}
	key := strings.ToLower(u.Username)
	if wait := s.limiter.wait(key, s.now()); wait > 0 {
		return User{}, &LoginError{fmt.Errorf("too many failed attempts; try again in %s", roundUp(wait)), wait}
	}
	c, challenge, err := s.endCeremony(w, r, ceremonyConfirm)
	if err != nil {
		return User{}, err
	}
	if !strings.EqualFold(c.Username, u.Username) || c.Version != u.Version {
		return User{}, ErrPasskeyExpired
	}
	if _, _, err := s.usePasskey(r, c, challenge, in, true); err != nil {
		wait := s.limiter.fail(key, s.now())
		s.record(audit.Entry{Actor: u.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.confirm",
			Result: audit.ResultDenied, Error: "passkey: " + err.Error()})
		return User{}, &LoginError{err, wait}
	}
	s.limiter.succeed(key)
	u.Confirmed = s.now().UTC()
	if err := s.startSession(w, u, secureRequest(r)); err != nil {
		return User{}, err
	}
	s.log.Info("confirmed sign-in with a passkey", "user", u.Username, "remote", r.RemoteAddr)
	s.record(audit.Entry{Actor: u.Username, ActorKind: audit.ActorLocal, IP: clientIP(r), Action: "auth.confirm",
		Details: map[string]string{"with": "passkey"}})
	return u, nil
}
