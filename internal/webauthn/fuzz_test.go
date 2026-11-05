package webauthn

import "testing"

// Whatever a browser (or anyone) sends, parsing doesn't panic.
func FuzzParse(f *testing.F) {
	f.Add([]byte{0xa1, 0x01, 0x02})
	f.Add(make([]byte, 37))
	f.Add(append(make([]byte, 32), 0x45, 0, 0, 0, 1, 0, 0))
	f.Fuzz(func(t *testing.T, b []byte) {
		decodeCBOR(b)
		parseAuthData(b)
		ParsePublicKey(b)
		rp := RelyingParty{ID: "hlm.example.com"}
		rp.VerifyRegistration(Registration{ClientDataJSON: b, AttestationObject: b}, []byte("x"), false)
	})
}
