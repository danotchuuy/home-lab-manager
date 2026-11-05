package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// RFC 8949 Appendix A examples (the ones WebAuthn's subset covers).
func TestCBORVectors(t *testing.T) {
	for _, tt := range []struct {
		hex  string
		want any
	}{
		{"00", int64(0)},
		{"17", int64(23)},
		{"1818", int64(24)},
		{"1903e8", int64(1000)},
		{"1a000f4240", int64(1000000)},
		{"1b000000e8d4a51000", int64(1000000000000)},
		{"20", int64(-1)},
		{"3863", int64(-100)},
		{"3903e7", int64(-1000)},
		{"f4", false},
		{"f5", true},
		{"f6", nil},
		{"40", []byte{}},
		{"4401020304", []byte{1, 2, 3, 4}},
		{"60", ""},
		{"6449455446", "IETF"},
		{"62c3bc", "ü"},
		{"80", []any{}},
		{"83010203", []any{int64(1), int64(2), int64(3)}},
		{"8301820203820405", []any{int64(1), []any{int64(2), int64(3)}, []any{int64(4), int64(5)}}},
		{"a0", map[any]any{}},
		{"a201020304", map[any]any{int64(1): int64(2), int64(3): int64(4)}},
		{"a26161016162820203", map[any]any{"a": int64(1), "b": []any{int64(2), int64(3)}}},
		{"c11a514b67b0", int64(1363896240)}, // a tag reads as the tagged item
	} {
		b, _ := hex.DecodeString(tt.hex)
		got, rest, err := decodeCBOR(b)
		if err != nil || len(rest) != 0 || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %#v %x %v, want %#v", tt.hex, got, rest, err, tt.want)
		}
		// And back, for the ones with one encoding.
		if !strings.HasPrefix(tt.hex, "a2") && !strings.HasPrefix(tt.hex, "c1") {
			if enc, err := encodeCBOR(got); err != nil || hex.EncodeToString(enc) != tt.hex {
				t.Errorf("encode %s: %x %v", tt.hex, enc, err)
			}
		}
	}
	for _, bad := range []string{
		"", "18", "19ff", "1bffffffffffffffff", "45010203", "5f", "9f", "bf", "f97c00", "fa47c35000", "a1f400",
		"a2010201ff", "a201020103", "82",
		strings.Repeat("81", 40) + "00", // too deep
	} {
		b, _ := hex.DecodeString(bad)
		if _, _, err := decodeCBOR(b); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
}

// authenticator plays a passkey with one key.
type authenticator struct {
	t      *testing.T
	alg    int
	signer crypto.Signer
	edKey  ed25519.PrivateKey
	credID []byte
	count  uint32
	flags  byte
}

func newAuthenticator(t *testing.T, alg int) *authenticator {
	a := &authenticator{t: t, alg: alg, credID: []byte("credential-" + strings.Repeat("x", 10)), flags: flagUP | flagUV}
	switch alg {
	case AlgES256:
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		a.signer = k
	case AlgEdDSA:
		_, k, _ := ed25519.GenerateKey(rand.Reader)
		a.edKey = k
	case AlgRS256:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		a.signer = k
	}
	return a
}

func (a *authenticator) coseKey() []byte {
	var m map[any]any
	switch a.alg {
	case AlgES256:
		pub := a.signer.Public().(*ecdsa.PublicKey)
		raw, _ := pub.Bytes()
		m = map[any]any{int64(1): int64(2), int64(3): int64(-7), int64(-1): int64(1), int64(-2): raw[1:33], int64(-3): raw[33:]}
	case AlgEdDSA:
		m = map[any]any{int64(1): int64(1), int64(3): int64(-8), int64(-1): int64(6), int64(-2): []byte(a.edKey.Public().(ed25519.PublicKey))}
	case AlgRS256:
		pub := a.signer.Public().(*rsa.PublicKey)
		e := big32(pub.E)
		m = map[any]any{int64(1): int64(3), int64(3): int64(-257), int64(-1): pub.N.Bytes(), int64(-2): e}
	}
	b, err := encodeCBOR(m)
	if err != nil {
		a.t.Fatal(err)
	}
	return b
}

func big32(e int) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(e))
	return bytes.TrimLeft(b, "\x00")
}

func (a *authenticator) authData(rpID string, withCred bool) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append([]byte{}, h[:]...)
	flags := a.flags
	if withCred {
		flags |= flagAT
	}
	b = append(b, flags)
	b = binary.BigEndian.AppendUint32(b, a.count)
	if withCred {
		b = append(b, make([]byte, 16)...)
		b = binary.BigEndian.AppendUint16(b, uint16(len(a.credID)))
		b = append(b, a.credID...)
		b = append(b, a.coseKey()...)
	}
	return b
}

func clientJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": B64.EncodeToString(challenge), "origin": origin, "crossOrigin": false})
	return b
}

func (a *authenticator) register(rpID, origin string, challenge []byte) Registration {
	att, _ := encodeCBOR(map[any]any{"fmt": "none", "attStmt": map[any]any{}, "authData": a.authData(rpID, true)})
	return Registration{ClientDataJSON: clientJSON("webauthn.create", challenge, origin), AttestationObject: att}
}

func (a *authenticator) sign(rpID, origin string, challenge []byte) Assertion {
	a.count++
	ad := a.authData(rpID, false)
	cd := clientJSON("webauthn.get", challenge, origin)
	h := sha256.Sum256(cd)
	signed := append(append([]byte{}, ad...), h[:]...)
	var sig []byte
	var err error
	switch a.alg {
	case AlgES256:
		d := sha256.Sum256(signed)
		sig, err = ecdsa.SignASN1(rand.Reader, a.signer.(*ecdsa.PrivateKey), d[:])
	case AlgEdDSA:
		sig = ed25519.Sign(a.edKey, signed)
	case AlgRS256:
		d := sha256.Sum256(signed)
		sig, err = rsa.SignPKCS1v15(rand.Reader, a.signer.(*rsa.PrivateKey), crypto.SHA256, d[:])
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return Assertion{CredentialID: a.credID, ClientDataJSON: cd, AuthenticatorData: ad, Signature: sig, UserHandle: []byte("user")}
}

func TestRegisterAndSignIn(t *testing.T) {
	rp := RelyingParty{ID: "hlm.example.com"}
	origin := "https://hlm.example.com"
	for _, alg := range Algorithms {
		a := newAuthenticator(t, alg)
		challenge := []byte("register-challenge-0123456789abc")
		cred, err := rp.VerifyRegistration(a.register(rp.ID, origin, challenge), challenge, true)
		if err != nil {
			t.Fatalf("alg %d: register: %v", alg, err)
		}
		if cred.Alg != alg || !bytes.Equal(cred.ID, a.credID) || !cred.UserVerified {
			t.Errorf("alg %d: credential %+v", alg, cred)
		}
		ch := []byte("signin-challenge-0123456789abcdef")
		count, err := rp.VerifyAssertion(cred, a.sign(rp.ID, origin, ch), ch, true)
		if err != nil || count != a.count {
			t.Fatalf("alg %d: sign in: %d %v", alg, count, err)
		}
	}
}

func TestRegistrationChecks(t *testing.T) {
	rp := RelyingParty{ID: "hlm.example.com"}
	a := newAuthenticator(t, AlgES256)
	ch := []byte("register-challenge-0123456789abc")
	good := a.register(rp.ID, "https://hlm.example.com", ch)
	for name, tt := range map[string]struct {
		reg       func() Registration
		requireUV bool
		want      string
	}{
		"wrong challenge": {func() Registration { return a.register(rp.ID, "https://hlm.example.com", []byte("other")) }, false, "challenge"},
		"other site":      {func() Registration { return a.register(rp.ID, "https://evil.example", ch) }, false, "isn't hlm.example.com"},
		"plain http":      {func() Registration { return a.register(rp.ID, "http://hlm.example.com", ch) }, false, "isn't hlm.example.com"},
		"other rp id":     {func() Registration { return a.register("evil.example", "https://hlm.example.com", ch) }, false, "another site"},
		"a get": {func() Registration {
			r := good
			r.ClientDataJSON = clientJSON("webauthn.get", ch, "https://hlm.example.com")
			return r
		}, false, "expected a webauthn.create"},
		"cross-origin": {func() Registration {
			r := good
			r.ClientDataJSON = []byte(`{"type":"webauthn.create","challenge":"` + B64.EncodeToString(ch) + `","origin":"https://hlm.example.com","crossOrigin":true}`)
			return r
		}, false, "frame"},
		"no user verification": {func() Registration {
			b := newAuthenticator(t, AlgES256)
			b.flags = flagUP
			return b.register(rp.ID, "https://hlm.example.com", ch)
		}, true, "verify you"},
		"no user presence": {func() Registration {
			b := newAuthenticator(t, AlgES256)
			b.flags = flagUV
			return b.register(rp.ID, "https://hlm.example.com", ch)
		}, false, "were there"},
		"trailing data": {func() Registration {
			r := good
			r.AttestationObject = append(bytes.Clone(r.AttestationObject), 0)
			return r
		}, false, "extra data"},
		"not cbor": {func() Registration { r := good; r.AttestationObject = []byte{0xff}; return r }, false, "CBOR"},
	} {
		_, err := rp.VerifyRegistration(tt.reg(), ch, tt.requireUV)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A subdomain may use the RP's credentials; localhost may use http.
	if _, err := rp.VerifyRegistration(a.register(rp.ID, "https://sub.hlm.example.com:8443", ch), ch, false); err != nil {
		t.Errorf("subdomain: %v", err)
	}
	local := RelyingParty{ID: "localhost"}
	if _, err := local.VerifyRegistration(a.register("localhost", "http://localhost:8080", ch), ch, false); err != nil {
		t.Errorf("localhost: %v", err)
	}
}

func TestAssertionChecks(t *testing.T) {
	rp := RelyingParty{ID: "hlm.example.com"}
	origin := "https://hlm.example.com"
	a := newAuthenticator(t, AlgES256)
	reg := []byte("register-challenge-0123456789abc")
	cred, err := rp.VerifyRegistration(a.register(rp.ID, origin, reg), reg, true)
	if err != nil {
		t.Fatal(err)
	}
	ch := []byte("signin-challenge-0123456789abcdef")
	as := a.sign(rp.ID, origin, ch)
	count, err := rp.VerifyAssertion(cred, as, ch, true)
	if err != nil {
		t.Fatal(err)
	}
	cred.SignCount = count
	// The same assertion again: the counter didn't go up.
	if _, err := rp.VerifyAssertion(cred, as, ch, true); !errors.Is(err, ErrCloned) {
		t.Errorf("replayed: %v", err)
	}
	bad := func(name string, as Assertion, want string, requireUV bool) {
		t.Helper()
		if _, err := rp.VerifyAssertion(cred, as, ch, requireUV); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	next := a.sign(rp.ID, origin, ch)
	sig := bytes.Clone(next.Signature)
	sig[len(sig)-1] ^= 1
	bad("tampered signature", Assertion{CredentialID: next.CredentialID, ClientDataJSON: next.ClientDataJSON, AuthenticatorData: next.AuthenticatorData, Signature: sig}, "signature", true)
	tampered := next
	tampered.ClientDataJSON = clientJSON("webauthn.get", ch, "https://sub.hlm.example.com")
	bad("client data changed after signing", tampered, "signature", true)
	other := next
	other.CredentialID = []byte("another")
	bad("another credential", other, "isn't the one", true)
	bad("other site", a.sign(rp.ID, "https://evil.example", ch), "isn't hlm.example.com", true)
	bad("wrong challenge", a.sign(rp.ID, origin, []byte("old")), "challenge", true)
	bad("a create", func() Assertion {
		x := a.sign(rp.ID, origin, ch)
		x.ClientDataJSON = clientJSON("webauthn.create", ch, origin)
		return x
	}(), "expected a webauthn.get", true)
	a.flags = flagUP
	bad("not verified", a.sign(rp.ID, origin, ch), "verify you", true)
	if _, err := rp.VerifyAssertion(cred, a.sign(rp.ID, origin, ch), ch, false); err != nil {
		t.Errorf("not verified, as a second factor: %v", err)
	}
	// Synced passkeys count 0 every time: fine while the stored count is 0.
	synced := newAuthenticator(t, AlgEdDSA)
	sc, _ := rp.VerifyRegistration(synced.register(rp.ID, origin, reg), reg, true)
	for range 2 {
		synced.count = ^uint32(0) // sign() adds one, wrapping to 0
		if n, err := rp.VerifyAssertion(sc, synced.sign(rp.ID, origin, ch), ch, true); err != nil || n != 0 {
			t.Errorf("synced passkey: %d %v", n, err)
		}
	}
}

func TestPublicKeys(t *testing.T) {
	for _, bad := range []map[any]any{
		{int64(1): int64(2), int64(3): int64(-7), int64(-1): int64(1), int64(-2): make([]byte, 32), int64(-3): make([]byte, 32)}, // not on the curve
		{int64(1): int64(2), int64(3): int64(-7), int64(-1): int64(2), int64(-2): make([]byte, 48), int64(-3): make([]byte, 48)}, // P-384
		{int64(1): int64(1), int64(3): int64(-8), int64(-1): int64(4), int64(-2): make([]byte, 32)},                              // X25519
		{int64(1): int64(3), int64(3): int64(-257), int64(-1): make([]byte, 128), int64(-2): []byte{1, 0, 1}},                    // RSA-1024
		{int64(1): int64(2), int64(3): int64(-35)}, // ES384
		{int64(1): int64(2)},
	} {
		b, _ := encodeCBOR(bad)
		if _, err := ParsePublicKey(b); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestRPIDFor(t *testing.T) {
	for host, want := range map[string]string{"HLM.example.com": "hlm.example.com", "hlm.example.com.": "hlm.example.com", "localhost": "localhost", "192.168.1.10": "", "::1": "", "": ""} {
		got, err := RPIDFor(host)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q %v", host, got, err)
		}
	}
}
