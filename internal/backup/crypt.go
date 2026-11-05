package backup

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Encrypted backups are AES-256-GCM in chunks, so they can be streamed:
//
//	"HLMENC1\n" | 7-byte random nonce prefix | chunks
//
// Each chunk is up to chunkSize bytes of the plain archive, sealed with the
// nonce prefix, a 4-byte big-endian chunk counter and a byte that is 1 for
// the last chunk (and 0 before it), so chunks can't be reordered, and a
// file cut short is detected. Every file has a last chunk, maybe empty.

const (
	encMagic   = "HLMENC1\n"
	chunkSize  = 64 << 10
	prefixSize = 7
	keySize    = 32
)

// ErrNoKey is returned for an encrypted backup when the key isn't known.
var ErrNoKey = errors.New("this backup is encrypted, and the key isn't known here")

// NewKey makes a random backup key, as base64.
func NewKey() string {
	k := make([]byte, keySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// ParseKey reads a key made by NewKey.
func ParseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(k) != keySize {
		return nil, errors.New("that isn't a backup key (44 characters of base64)")
	}
	return k, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, n uint32, last bool) []byte {
	out := make([]byte, 12)
	copy(out, prefix)
	binary.BigEndian.PutUint32(out[prefixSize:], n)
	if last {
		out[11] = 1
	}
	return out
}

// encryptWriter encrypts what's written to it into w. Close writes the
// last chunk; it doesn't close w.
type encryptWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	n      uint32
	buf    []byte
	err    error
}

func newEncryptWriter(w io.Writer, key []byte) (*encryptWriter, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, prefixSize)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, encMagic); err != nil {
		return nil, err
	}
	if _, err := w.Write(prefix); err != nil {
		return nil, err
	}
	return &encryptWriter{w: w, aead: aead, prefix: prefix, buf: make([]byte, 0, chunkSize)}, nil
}

func (e *encryptWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	total := len(p)
	for len(p) > 0 {
		// A full chunk is only sealed once more data comes, since the last
		// chunk is sealed differently.
		if len(e.buf) == chunkSize {
			if e.err = e.seal(false); e.err != nil {
				return 0, e.err
			}
		}
		n := copy(e.buf[len(e.buf):chunkSize], p)
		e.buf = e.buf[:len(e.buf)+n]
		p = p[n:]
	}
	return total, nil
}

func (e *encryptWriter) seal(last bool) error {
	if e.n == ^uint32(0) {
		return errors.New("backup too large to encrypt")
	}
	out := e.aead.Seal(nil, nonce(e.prefix, e.n, last), e.buf, nil)
	e.n++
	e.buf = e.buf[:0]
	_, err := e.w.Write(out)
	return err
}

func (e *encryptWriter) Close() error {
	if e.err != nil {
		return e.err
	}
	e.err = e.seal(true)
	if e.err == nil {
		e.err = errors.New("already closed")
		return nil
	}
	return e.err
}

// encryptReader returns r's bytes, encrypted.
func encryptReader(r io.Reader, key []byte) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		ew, err := newEncryptWriter(pw, key)
		if err == nil {
			_, err = io.Copy(ew, r)
		}
		if err == nil {
			err = ew.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

// decryptReader reads an encrypted backup.
type decryptReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	n      uint32
	plain  []byte
	done   bool
	sealed []byte
}

// newDecryptReader reads the header of an encrypted backup from r.
func newDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(r, chunkSize+64)
	head := make([]byte, len(encMagic)+prefixSize)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(encMagic)]) != encMagic {
		return nil, errors.New("not an encrypted backup")
	}
	return &decryptReader{r: br, aead: aead, prefix: head[len(encMagic):], sealed: make([]byte, chunkSize+aead.Overhead())}, nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.done {
			return 0, io.EOF
		}
		n, err := io.ReadFull(d.r, d.sealed)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			if errors.Is(err, io.EOF) {
				return 0, errors.New("the encrypted backup is cut short")
			}
			return 0, err
		}
		// A short chunk is the last; a full one may be too, if nothing
		// follows it.
		last := n < len(d.sealed)
		if !last {
			if _, perr := d.r.Peek(1); perr == io.EOF {
				last = true
			}
		}
		plain, oerr := d.aead.Open(d.plain[:0], nonce(d.prefix, d.n, last), d.sealed[:n], nil)
		if oerr != nil {
			// The first chunk failing is almost always the wrong key.
			if d.n == 0 || last {
				return 0, errors.New("the backup couldn't be decrypted: wrong key, or the file is damaged or cut short")
			}
			return 0, fmt.Errorf("the encrypted backup is damaged (chunk %d)", d.n)
		}
		d.plain, d.done = plain, last
		d.n++
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

// isEncrypted reports whether data starts like an encrypted backup.
func isEncrypted(head []byte) bool { return bytes.HasPrefix(head, []byte(encMagic)) }

// Decrypt copies an encrypted backup from r to w, decrypted with key (from
// NewKey), for `hlm decrypt-backup`.
func Decrypt(w io.Writer, r io.Reader, key string) error {
	k, err := ParseKey(key)
	if err != nil {
		return err
	}
	dr, err := newDecryptReader(r, k)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, dr)
	return err
}
