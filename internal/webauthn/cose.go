package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

// COSE algorithms (RFC 9053) hlm accepts, most preferred first.
const (
	AlgES256 = -7   // ECDSA with P-256 and SHA-256
	AlgEdDSA = -8   // Ed25519
	AlgRS256 = -257 // RSASSA-PKCS1-v1_5 with SHA-256
)

// Algorithms are the algorithms offered when registering, in order.
var Algorithms = []int{AlgES256, AlgEdDSA, AlgRS256}

// COSE key parameters.
const (
	coseKty = 1
	coseAlg = 3
	coseCrv = -1 // EC2 and OKP; RSA's n
	coseX   = -2 // EC2 and OKP; RSA's e
	coseY   = -3

	ktyOKP = 1
	ktyEC2 = 2
	ktyRSA = 3

	crvP256    = 1
	crvEd25519 = 6
)

// PublicKey is a credential's public key.
type PublicKey struct {
	Alg int
	key crypto.PublicKey
}

func intOf(m map[any]any, k int64) (int64, bool) {
	v, ok := m[k].(int64)
	return v, ok
}

func bytesOf(m map[any]any, k int64) []byte {
	v, _ := m[k].([]byte)
	return v
}

// ParsePublicKey reads a COSE_Key.
func ParsePublicKey(cose []byte) (PublicKey, error) {
	v, rest, err := decodeCBOR(cose)
	if err != nil {
		return PublicKey{}, err
	}
	if len(rest) != 0 {
		return PublicKey{}, errors.New("extra data after the public key")
	}
	return publicKeyFrom(v)
}

func publicKeyFrom(v any) (PublicKey, error) {
	m, ok := v.(map[any]any)
	if !ok {
		return PublicKey{}, errors.New("the public key isn't a COSE key")
	}
	kty, _ := intOf(m, coseKty)
	alg, ok := intOf(m, coseAlg)
	if !ok {
		return PublicKey{}, errors.New("the public key doesn't say its algorithm")
	}
	switch alg {
	case AlgES256:
		crv, _ := intOf(m, coseCrv)
		x, y := bytesOf(m, coseX), bytesOf(m, coseY)
		if kty != ktyEC2 || crv != crvP256 || len(x) != 32 || len(y) != 32 {
			return PublicKey{}, errors.New("not a P-256 key")
		}
		// Check the point is on the curve (and not the identity).
		raw := append(append([]byte{4}, x...), y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
		if err != nil {
			return PublicKey{}, fmt.Errorf("the P-256 key isn't valid: %w", err)
		}
		return PublicKey{Alg: AlgES256, key: pub}, nil
	case AlgEdDSA:
		crv, _ := intOf(m, coseCrv)
		x := bytesOf(m, coseX)
		if kty != ktyOKP || crv != crvEd25519 || len(x) != ed25519.PublicKeySize {
			return PublicKey{}, errors.New("not an Ed25519 key")
		}
		return PublicKey{Alg: AlgEdDSA, key: ed25519.PublicKey(x)}, nil
	case AlgRS256:
		n, e := bytesOf(m, coseCrv), bytesOf(m, coseX)
		if kty != ktyRSA || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return PublicKey{}, errors.New("not an RSA key of 2048 bits or more")
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		if exp < 3 || exp%2 == 0 {
			return PublicKey{}, errors.New("the RSA key's exponent isn't valid")
		}
		return PublicKey{Alg: AlgRS256, key: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}}, nil
	}
	return PublicKey{}, fmt.Errorf("unsupported algorithm %d (hlm takes ES256, EdDSA and RS256)", alg)
}

// Verify checks a signature over data.
func (k PublicKey) Verify(data, sig []byte) bool {
	switch pub := k.key.(type) {
	case *ecdsa.PublicKey:
		h := sha256.Sum256(data)
		return ecdsa.VerifyASN1(pub, h[:], sig)
	case ed25519.PublicKey:
		return ed25519.Verify(pub, data, sig)
	case *rsa.PublicKey:
		h := sha256.Sum256(data)
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) == nil
	}
	return false
}
