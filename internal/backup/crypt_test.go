package backup

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	key := NewKey()
	k, err := ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		plain := make([]byte, size)
		rand.Read(plain)
		enc, err := io.ReadAll(encryptReader(bytes.NewReader(plain), k))
		if err != nil {
			t.Fatal(err)
		}
		if !isEncrypted(enc) || bytes.Contains(enc, plain[:min(size, 64)]) && size > 16 {
			t.Errorf("size %d: not encrypted", size)
		}
		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(enc), key); err != nil || !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("size %d: round trip failed: %v", size, err)
		}
		// Cut short at a chunk boundary: the missing last chunk is noticed.
		if size > chunkSize {
			cut := enc[:len(encMagic)+prefixSize+chunkSize+16]
			if err := Decrypt(io.Discard, bytes.NewReader(cut), key); err == nil {
				t.Errorf("size %d: a file cut short decrypted", size)
			}
		}
		// A changed byte fails.
		if size > 0 {
			bad := bytes.Clone(enc)
			bad[len(bad)-1] ^= 1
			if err := Decrypt(io.Discard, bytes.NewReader(bad), key); err == nil {
				t.Errorf("size %d: a damaged file decrypted", size)
			}
		}
	}
	// The wrong key fails.
	enc, _ := io.ReadAll(encryptReader(strings.NewReader("hello"), k))
	if err := Decrypt(io.Discard, bytes.NewReader(enc), NewKey()); err == nil {
		t.Error("decrypted with the wrong key")
	}
	if _, err := ParseKey("short"); err == nil {
		t.Error("accepted a bad key")
	}
}
