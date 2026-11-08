// Package secrets stores credentials encrypted at rest.
//
// Values are kept in one file, sealed with AES-256-GCM. The key comes from
// HLM_SECRET_KEY when it is set (so it never sits next to the data), or else
// from a random key file created on first use. Both files are readable only
// by the user hlm runs as.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileVersion is written before the nonce so the format can change later.
const fileVersion = 1

// Store holds named secrets, each a JSON-encodable value.
type Store struct {
	path string
	aead cipher.AEAD
	mu   sync.Mutex
	// cache is the decrypted file as of cachedAt (its modification time and
	// size), so reading doesn't decrypt the file every time. Another process
	// writing it (hlm reset-password) changes those.
	cache    map[string]json.RawMessage
	cachedAt fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// Open opens (or prepares to create) the store in dir. envKey, if not empty,
// is used as the encryption key: either base64 for 32 bytes, or any
// passphrase, which is hashed.
func Open(dir, envKey string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var key []byte
	if envKey != "" {
		key = deriveKey(envKey)
	} else {
		var err error
		key, err = loadOrCreateKey(filepath.Join(dir, "secret.key"))
		if err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "secrets.enc"), aead: aead}, nil
}

func deriveKey(s string) []byte {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b
	}
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a valid key", path)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL: never overwrite a key that appeared meanwhile, which would make
	// existing secrets unreadable.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key)); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

// ErrUndecryptable means the secrets file can't be read with the current key
// (e.g. HLM_SECRET_KEY changed or the key file was replaced).
var ErrUndecryptable = errors.New("stored secrets can't be decrypted with the current key")

func (s *Store) readAll() (map[string]json.RawMessage, error) {
	st, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	stamp := fileStamp{st.ModTime(), st.Size()}
	if s.cache != nil && s.cachedAt == stamp {
		return maps.Clone(s.cache), nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	ns := s.aead.NonceSize()
	if len(data) < 1+ns || data[0] != fileVersion {
		return nil, ErrUndecryptable
	}
	plain, err := s.aead.Open(nil, data[1:1+ns], data[1+ns:], []byte{fileVersion})
	if err != nil {
		return nil, ErrUndecryptable
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, ErrUndecryptable
	}
	s.cache, s.cachedAt = out, stamp
	return maps.Clone(out), nil
}

func (s *Store) writeAll(all map[string]json.RawMessage) error {
	plain, err := json.Marshal(all)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append([]byte{fileVersion}, nonce...)
	out = s.aead.Seal(out, nonce, plain, []byte{fileVersion})
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	s.cache = nil
	return os.Rename(tmp, s.path)
}

// Get decodes the secret called name into v. It reports false if there is
// no such secret.
func (s *Store) Get(name string, v any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return false, err
	}
	raw, ok := all[name]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// Set stores v under name.
func (s *Store) Set(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	all[name] = raw
	return s.writeAll(all)
}

// MoveKey moves the entry from to the key to in the secret called name, a
// JSON object such as a map of servers' settings, replacing any entry to
// had. It reports whether there was an entry to move.
func (s *Store) MoveKey(name, from, to string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return false, err
	}
	raw, ok := all[name]
	if !ok || from == to {
		return false, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, fmt.Errorf("%s isn't an object: %w", name, err)
	}
	v, ok := m[from]
	if !ok {
		return false, nil
	}
	m[to] = v
	delete(m, from)
	if all[name], err = json.Marshal(m); err != nil {
		return false, err
	}
	return true, s.writeAll(all)
}

// Delete removes the secret called name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return err
	}
	delete(all, name)
	return s.writeAll(all)
}
