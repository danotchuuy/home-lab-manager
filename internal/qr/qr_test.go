package qr

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// matrix hashes a code's modules, row by row.
func matrix(c *Code) string {
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Dark(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:8])
}

func TestEncode(t *testing.T) {
	url := "otpauth://totp/hlm:alice?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=hlm"
	c, err := Encode([]byte(url))
	if err != nil {
		t.Fatal(err)
	}
	if c.Size != 37 { // version 5
		t.Errorf("size = %d", c.Size)
	}
	// Finder patterns' centres and the dark module.
	for _, p := range [][2]int{{3, 3}, {c.Size - 4, 3}, {3, c.Size - 4}} {
		if !c.Dark(p[0], p[1]) || c.Dark(p[0]+2, p[1]) || !c.Dark(p[0]+3, p[1]) {
			t.Errorf("finder at %v is wrong", p)
		}
	}
	if !c.Dark(8, c.Size-8) {
		t.Error("no dark module")
	}
	// This exact code, and the others versions 1 to 10 make, were read back
	// with zbar when this encoder was written; a change here needs that
	// check again.
	if got := matrix(c); got != goldenOTPAuth {
		t.Errorf("modules changed: %s", got)
	}
	if !strings.HasPrefix(c.SVG(), "<svg") {
		t.Error("no SVG")
	}
	if _, err := Encode(make([]byte, 214)); err != ErrTooLong {
		t.Errorf("214 bytes: %v", err)
	}
	if c, err := Encode(make([]byte, 213)); err != nil || c.Size != 57 {
		t.Errorf("213 bytes: %v", err)
	}
}

var goldenOTPAuth = "05a7f22f03a70235"
