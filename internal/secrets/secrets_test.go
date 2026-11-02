package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type cred struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func TestRoundTripAndEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("unifi", cred{"admin", "hunter2-secret"}); err != nil {
		t.Fatal(err)
	}

	// Nothing readable on disk, and both files are private.
	data, _ := os.ReadFile(filepath.Join(dir, "secrets.enc"))
	if bytes.Contains(data, []byte("hunter2-secret")) || bytes.Contains(data, []byte("admin")) {
		t.Error("secret stored in plain text")
	}
	for _, f := range []string{"secrets.enc", "secret.key"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, err %v", f, st.Mode().Perm(), err)
		}
	}

	// A new Store (e.g. after a restart) reads it back with the same key file.
	s2, _ := Open(dir, "")
	var got cred
	ok, err := s2.Get("unifi", &got)
	if !ok || err != nil || got.Password != "hunter2-secret" {
		t.Errorf("get = %+v, %v, %v", got, ok, err)
	}
	if ok, _ := s2.Get("missing", &got); ok {
		t.Error("missing secret found")
	}
	if err := s2.Delete("unifi"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s2.Get("unifi", &got); ok {
		t.Error("deleted secret still there")
	}
}

func TestEnvKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "a long passphrase from the environment")
	s.Set("x", cred{Password: "p"})
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("key file written although HLM_SECRET_KEY was set")
	}
	// The wrong key can't read it, and says so rather than returning garbage.
	wrong, _ := Open(dir, "another passphrase")
	var c cred
	if _, err := wrong.Get("x", &c); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("wrong key: err = %v", err)
	}
	right, _ := Open(dir, "a long passphrase from the environment")
	if ok, err := right.Get("x", &c); !ok || err != nil || c.Password != "p" {
		t.Errorf("right key: %+v %v %v", c, ok, err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "")
	s.Set("x", cred{Password: "p"})
	p := filepath.Join(dir, "secrets.enc")
	data, _ := os.ReadFile(p)
	data[len(data)-1] ^= 1
	os.WriteFile(p, data, 0o600)
	var c cred
	if _, err := s.Get("x", &c); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("tampered file: err = %v", err)
	}
}
