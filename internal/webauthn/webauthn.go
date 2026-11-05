// Package webauthn checks passkeys (Web Authentication, level 2): new
// credentials from navigator.credentials.create() and sign-in assertions
// from navigator.credentials.get().
//
// It's deliberately small. Attestation is requested as "none", as most
// sites do, so the authenticator's attestation statement isn't checked
// (whatever format it comes in): hlm trusts a passkey because a signed-in,
// confirmed user registered it, not because of who made the authenticator.
// Everything else is: the ceremony type, the challenge, the origin, the
// relying party ID hash, the user presence and verification flags, the
// signature and the signature counter.
package webauthn

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Authenticator data flags.
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified (PIN, biometrics)
	flagBE = 0x08 // backup eligible (a synced passkey)
	flagBS = 0x10 // backed up
	flagAT = 0x40 // attested credential data included
	flagED = 0x80 // extension data included
)

// B64 is how WebAuthn's binary values travel in JSON: unpadded base64url.
var B64 = base64.RawURLEncoding

// RelyingParty is the site credentials belong to.
type RelyingParty struct {
	// ID is the domain credentials are scoped to: hlm's hostname.
	ID string
}

// ErrInsecureOrigin is returned for an origin passkeys can't be used on.
var ErrInsecureOrigin = errors.New("passkeys need hlm opened over HTTPS with a name (or as localhost), not plain HTTP or an IP address")

// RPIDFor is the relying party ID for a page's host (without the port):
// the host itself, if passkeys can be used there.
func RPIDFor(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return "", ErrInsecureOrigin
	}
	return host, nil
}

// originAllowed reports whether a page's origin may use credentials of the
// relying party: https on the RP ID or a subdomain of it, or http on
// localhost.
func (rp RelyingParty) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	onRP := host == rp.ID || strings.HasSuffix(host, "."+rp.ID)
	switch u.Scheme {
	case "https":
		return onRP
	case "http":
		return onRP && (host == "localhost" || strings.HasSuffix(host, ".localhost"))
	}
	return false
}

// clientData is the browser's CollectedClientData.
type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func (rp RelyingParty) checkClientData(raw []byte, wantType string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return fmt.Errorf("the browser's client data isn't JSON: %w", err)
	}
	if cd.Type != wantType {
		return fmt.Errorf("expected a %s, got %q", wantType, cd.Type)
	}
	got, err := B64.DecodeString(strings.TrimRight(cd.Challenge, "="))
	if err != nil || len(challenge) == 0 || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return errors.New("the challenge doesn't match: start again")
	}
	if !rp.originAllowed(cd.Origin) {
		return fmt.Errorf("the passkey was used on %s, which isn't %s", cd.Origin, rp.ID)
	}
	if cd.CrossOrigin {
		return errors.New("the passkey was used in a frame from another site")
	}
	return nil
}

// authData is the parsed authenticator data.
type authData struct {
	rpIDHash  []byte
	flags     byte
	signCount uint32
	// With flagAT:
	aaguid       []byte
	credentialID []byte
	publicKey    []byte // the COSE key, as sent
}

func parseAuthData(b []byte) (authData, error) {
	if len(b) < 37 {
		return authData{}, errors.New("the authenticator data is too short")
	}
	a := authData{rpIDHash: b[:32], flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	rest := b[37:]
	if a.flags&flagAT != 0 {
		if len(rest) < 18 {
			return authData{}, errors.New("the credential data is too short")
		}
		a.aaguid = rest[:16]
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		if n == 0 || n > 1023 || len(rest) < n {
			return authData{}, errors.New("the credential ID's length isn't valid")
		}
		a.credentialID, rest = rest[:n], rest[n:]
		_, after, err := decodeCBOR(rest)
		if err != nil {
			return authData{}, fmt.Errorf("the public key: %w", err)
		}
		a.publicKey, rest = rest[:len(rest)-len(after)], after
	}
	if a.flags&flagED != 0 {
		_, after, err := decodeCBOR(rest)
		if err != nil {
			return authData{}, fmt.Errorf("the extensions: %w", err)
		}
		rest = after
	}
	if len(rest) != 0 {
		return authData{}, errors.New("extra data after the authenticator data")
	}
	return a, nil
}

func (rp RelyingParty) checkAuthData(a authData, requireUV bool) error {
	want := sha256.Sum256([]byte(rp.ID))
	if subtle.ConstantTimeCompare(a.rpIDHash, want[:]) != 1 {
		return errors.New("the passkey belongs to another site")
	}
	if a.flags&flagUP == 0 {
		return errors.New("the authenticator didn't check you were there")
	}
	if requireUV && a.flags&flagUV == 0 {
		return errors.New("the authenticator didn't verify you (with a PIN, fingerprint or face): that's needed to sign in with a passkey alone")
	}
	if a.flags&flagBS != 0 && a.flags&flagBE == 0 {
		return errors.New("the authenticator data's backup flags aren't valid")
	}
	return nil
}

// Credential is a registered passkey.
type Credential struct {
	ID        []byte
	PublicKey []byte // COSE_Key
	Alg       int
	SignCount uint32
	// BackupEligible is set for synced passkeys (in a password manager or
	// an account's keychain), which can be on more than one device.
	BackupEligible bool
	// UserVerified says the authenticator verified the user when it was
	// made.
	UserVerified bool
	AAGUID       []byte
}

// Registration is a browser's answer to navigator.credentials.create().
type Registration struct {
	ClientDataJSON    []byte
	AttestationObject []byte
}

// VerifyRegistration checks a new credential made for challenge.
func (rp RelyingParty) VerifyRegistration(reg Registration, challenge []byte, requireUV bool) (Credential, error) {
	if err := rp.checkClientData(reg.ClientDataJSON, "webauthn.create", challenge); err != nil {
		return Credential{}, err
	}
	v, rest, err := decodeCBOR(reg.AttestationObject)
	if err != nil {
		return Credential{}, fmt.Errorf("the attestation: %w", err)
	}
	if len(rest) != 0 {
		return Credential{}, errors.New("extra data after the attestation")
	}
	att, ok := v.(map[any]any)
	if !ok {
		return Credential{}, errors.New("the attestation isn't a map")
	}
	if _, ok := att["fmt"].(string); !ok {
		return Credential{}, errors.New("the attestation has no format")
	}
	raw, ok := att["authData"].([]byte)
	if !ok {
		return Credential{}, errors.New("the attestation has no authenticator data")
	}
	a, err := parseAuthData(raw)
	if err != nil {
		return Credential{}, err
	}
	if err := rp.checkAuthData(a, requireUV); err != nil {
		return Credential{}, err
	}
	if a.flags&flagAT == 0 {
		return Credential{}, errors.New("the authenticator sent no credential")
	}
	key, err := ParsePublicKey(a.publicKey)
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		ID: bytes.Clone(a.credentialID), PublicKey: bytes.Clone(a.publicKey), Alg: key.Alg, SignCount: a.signCount,
		BackupEligible: a.flags&flagBE != 0, UserVerified: a.flags&flagUV != 0, AAGUID: bytes.Clone(a.aaguid),
	}, nil
}

// Assertion is a browser's answer to navigator.credentials.get().
type Assertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
	UserHandle        []byte
}

// ErrCloned is returned when a credential's signature counter went back:
// the authenticator may have been copied.
var ErrCloned = errors.New("this passkey's signature counter went backwards, so it may have been copied; it was refused")

// VerifyAssertion checks a sign-in with cred for challenge, and returns the
// new signature counter to keep.
func (rp RelyingParty) VerifyAssertion(cred Credential, as Assertion, challenge []byte, requireUV bool) (uint32, error) {
	if subtle.ConstantTimeCompare(as.CredentialID, cred.ID) != 1 {
		return 0, errors.New("the passkey isn't the one expected")
	}
	if err := rp.checkClientData(as.ClientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	a, err := parseAuthData(as.AuthenticatorData)
	if err != nil {
		return 0, err
	}
	if a.flags&flagAT != 0 {
		return 0, errors.New("unexpected credential data in a sign-in")
	}
	if err := rp.checkAuthData(a, requireUV); err != nil {
		return 0, err
	}
	key, err := ParsePublicKey(cred.PublicKey)
	if err != nil {
		return 0, err
	}
	cdHash := sha256.Sum256(as.ClientDataJSON)
	signed := append(bytes.Clone(as.AuthenticatorData), cdHash[:]...)
	if !key.Verify(signed, as.Signature) {
		return 0, errors.New("the passkey's signature isn't right")
	}
	// Authenticators that count signatures must count up; synced passkeys
	// usually send 0 every time.
	if a.signCount != 0 || cred.SignCount != 0 {
		if a.signCount <= cred.SignCount {
			return 0, ErrCloned
		}
	}
	return a.signCount, nil
}
