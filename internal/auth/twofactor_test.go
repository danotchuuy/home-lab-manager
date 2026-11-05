package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238's SHA-1 test vectors (the last 6 of their 8 digits).
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
		20000000000: "353130",
	} {
		if got := totpCode(secret, unix/30); got != want {
			t.Errorf("at %d: %s, want %s", unix, got, want)
		}
	}
}

func TestCheckTOTP(t *testing.T) {
	raw := []byte("12345678901234567890")
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	now := time.Unix(1111111111, 0)
	step := now.Unix() / 30
	for _, tt := range []struct {
		step int64
		last int64
		ok   bool
	}{
		{step, 0, true},
		{step - 1, 0, true}, // a step of drift either way
		{step + 1, 0, true},
		{step - 2, 0, false},
		{step + 2, 0, false},
		{step, step, false}, // used already
		{step + 1, step, true},
	} {
		got, ok := checkTOTP(secret, totpCode(raw, tt.step), now, tt.last)
		if ok != tt.ok || (ok && got != tt.step) {
			t.Errorf("code for step %+d (last %+d): %v %d", tt.step-step, tt.last-step, ok, got)
		}
	}
	if _, ok := checkTOTP(secret, "12345", now, 0); ok {
		t.Error("5 digits accepted")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := newRecoveryCodes()
	if len(codes) != recoveryCodes || len(hashes) != recoveryCodes || len(codes[0]) != 11 || codes[0][5] != '-' {
		t.Fatalf("codes = %q", codes)
	}
	if !passwordMatches(hashes[3], normalizeRecovery(" "+codes[3][:5]+codes[3][6:]+" ")) {
		t.Error("a code without its dash didn't match")
	}
}
