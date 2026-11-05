// Package webauthntest is a virtual passkey for tests: it answers WebAuthn
// creation and request options as a browser and authenticator would.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
)

var b64 = base64.RawURLEncoding

// Authenticator is one ES256 passkey.
type Authenticator struct {
	key    *ecdsa.PrivateKey
	CredID []byte
	// UserHandle is the account's handle, from the creation options.
	UserHandle []byte
	// RPID is where it was made.
	RPID string
	// Count is the signature counter; Synced passkeys keep it at 0.
	Count  uint32
	Synced bool
	// NoUV makes it not verify the user.
	NoUV bool
}

// New makes a passkey.
func New() *Authenticator {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	rand.Read(id)
	return &Authenticator{key: k, CredID: id}
}

// CredIDB64 is the credential ID as hlm shows it.
func (a *Authenticator) CredIDB64() string { return b64.EncodeToString(a.CredID) }

// Creation is the creation options hlm sends.
type Creation struct {
	PublicKey struct {
		RP        struct{ ID string }
		User      struct{ ID string }
		Challenge string
	}
}

// Request is the request options hlm sends.
type Request struct {
	PublicKey struct {
		RPID             string `json:"rpId"`
		Challenge        string
		UserVerification string
		AllowCredentials []struct{ ID string }
	}
}

func (a *Authenticator) flags(withCred bool) byte {
	f := byte(0x01) // UP
	if !a.NoUV {
		f |= 0x04
	}
	if a.Synced {
		f |= 0x08 | 0x10
	}
	if withCred {
		f |= 0x40
	}
	return f
}

func (a *Authenticator) authData(rpID string, withCred bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append(h[:0:0], h[:]...)
	b = append(b, a.flags(withCred))
	b = binary.BigEndian.AppendUint32(b, a.Count)
	if withCred {
		b = append(b, make([]byte, 16)...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.CredID)))
		b = append(b, a.CredID...)
		raw, _ := a.key.PublicKey.Bytes()
		// {1: 2, 3: -7, -1: 1, -2: x, -3: y}
		b = append(b, 0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20)
		b = append(b, raw[1:33]...)
		b = append(b, 0x22, 0x58, 0x20)
		b = append(b, raw[33:]...)
	}
	return b
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

// Create answers creation options (JSON) from origin, returning what the
// browser sends back: clientDataJSON and attestationObject, base64url.
func (a *Authenticator) Create(options []byte, origin string) (clientDataJSON, attestationObject string, err error) {
	var o Creation
	if err := json.Unmarshal(options, &o); err != nil {
		return "", "", err
	}
	a.RPID = o.PublicKey.RP.ID
	a.UserHandle, _ = b64.DecodeString(o.PublicKey.User.ID)
	ad := a.authData(a.RPID, true)
	// {"fmt": "none", "attStmt": {}, "authData": ad}
	att := []byte{0xa3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xa0,
		0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x58, byte(len(ad))}
	att = append(att, ad...)
	return b64.EncodeToString(clientData("webauthn.create", o.PublicKey.Challenge, origin)), b64.EncodeToString(att), nil
}

// Assertion is what the browser sends back for request options.
type Assertion struct {
	ID                string `json:"id"`
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"userHandle"`
}

// Get answers request options (JSON) from origin.
func (a *Authenticator) Get(options []byte, origin string) (Assertion, error) {
	var o Request
	if err := json.Unmarshal(options, &o); err != nil {
		return Assertion{}, err
	}
	if len(o.PublicKey.AllowCredentials) > 0 && !slices.ContainsFunc(o.PublicKey.AllowCredentials, func(c struct{ ID string }) bool {
		return c.ID == b64.EncodeToString(a.CredID)
	}) {
		return Assertion{}, fmt.Errorf("this passkey isn't one of those allowed")
	}
	if !a.Synced {
		a.Count++
	}
	ad := a.authData(o.PublicKey.RPID, false)
	cd := clientData("webauthn.get", o.PublicKey.Challenge, origin)
	h := sha256.Sum256(cd)
	d := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, d[:])
	if err != nil {
		return Assertion{}, err
	}
	return Assertion{ID: b64.EncodeToString(a.CredID), ClientDataJSON: b64.EncodeToString(cd),
		AuthenticatorData: b64.EncodeToString(ad), Signature: b64.EncodeToString(sig), UserHandle: b64.EncodeToString(a.UserHandle)}, nil
}
