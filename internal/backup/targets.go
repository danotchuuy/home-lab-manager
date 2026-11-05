package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
)

// Backups can go to more than one place (a target): the primary one, where
// every backup goes, and others that plans can also copy backups to (an
// off-site bucket, say). Each can encrypt what it stores.

const (
	secretTargets = "backup-targets"
	// secretKeyPrefix + a target's ID holds its encryption key.
	secretKeyPrefix = "backup-key-"
	// PrimaryID is the primary target's ID.
	PrimaryID = "primary"
)

// Target is a place backups are stored.
type Target struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Settings
	// Encrypt encrypts backups before they're stored (see crypt.go); the
	// key is kept in the secrets store.
	Encrypt bool `json:"encrypt,omitempty"`
}

// PublicTarget is a target without its secret key, for the UI.
type PublicTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	PublicSettings
	Encrypt bool `json:"encrypt"`
	// HasKey says an encryption key exists (so encrypted backups there can
	// be read, even with encryption since turned off).
	HasKey bool `json:"hasKey"`
	// NewKey is the encryption key, only in the answer to the save that
	// made it, to keep somewhere safe.
	NewKey string `json:"newKey,omitempty"`
}

// targets returns the targets, the primary first. Settings saved before
// targets existed are the primary.
func (s *Service) targets() ([]Target, error) {
	var ts []Target
	ok, err := s.secrets.Get(secretTargets, &ts)
	if err != nil {
		return nil, err
	}
	if ok {
		return ts, nil
	}
	c, ok, err := s.legacySettings()
	if err != nil || !ok {
		return nil, err
	}
	return []Target{{ID: PrimaryID, Name: "Primary", Settings: c}}, nil
}

func (s *Service) legacySettings() (Settings, bool, error) {
	var c Settings
	ok, err := s.secrets.Get(secretSettings, &c)
	return c, ok, err
}

func (s *Service) saveTargets(ts []Target) error {
	if len(ts) == 0 {
		if err := s.secrets.Delete(secretTargets); err != nil {
			return err
		}
	} else if err := s.secrets.Set(secretTargets, ts); err != nil {
		return err
	}
	// Moved into the list.
	return s.secrets.Delete(secretSettings)
}

// target returns the target with the ID ("" is the primary).
func (s *Service) target(id string) (Target, error) {
	ts, err := s.targets()
	if err != nil {
		return Target{}, err
	}
	if len(ts) == 0 {
		return Target{}, ErrNotConfigured
	}
	if id == "" {
		return ts[0], nil
	}
	for _, t := range ts {
		if t.ID == id {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("there's no backup storage %q", id)
}

func (s *Service) key(id string) ([]byte, error) {
	var k string
	ok, err := s.secrets.Get(secretKeyPrefix+id, &k)
	if err != nil || !ok {
		return nil, err
	}
	return ParseKey(k)
}

// store is a target, ready to use.
type store struct {
	t   Target
	cl  *s3.Client
	key []byte // nil when it has none
}

func (s *Service) store(id string) (*store, error) {
	t, err := s.target(id)
	if err != nil {
		return nil, err
	}
	cl, err := s3.New(t.Settings.Settings)
	if err != nil {
		return nil, err
	}
	key, err := s.key(t.ID)
	if err != nil {
		return nil, fmt.Errorf("read the encryption key of %s: %w", t.Name, err)
	}
	if t.Encrypt && key == nil {
		return nil, fmt.Errorf("%s encrypts backups, but its key is missing: save its settings again to make one", t.Name)
	}
	return &store{t: t, cl: cl, key: key}, nil
}

// client returns the primary target's client and settings.
func (s *Service) client() (*s3.Client, Settings, error) {
	st, err := s.store("")
	if err != nil {
		return nil, Settings{}, err
	}
	return st.cl, st.t.Settings, nil
}

// sumSuffix names the object holding a backup's SHA-256 checksum, in
// sha256sum's format.
const sumSuffix = ".sha256"

// put stores what r reads as key (encrypted, if the target encrypts), with
// its SHA-256 (of the stored bytes) beside it. It returns the stored size
// and checksum.
func (st *store) put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, string, error) {
	if st.t.Encrypt {
		r = encryptReader(r, st.key)
	}
	h := sha256.New()
	size, err := st.cl.Upload(ctx, key, io.TeeReader(r, h), "application/gzip", meta)
	if err != nil {
		return 0, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	base := key[strings.LastIndex(key, "/")+1:]
	if err := st.cl.Put(ctx, key+sumSuffix, []byte(sum+"  "+base+"\n"), "text/plain"); err != nil {
		return size, sum, &sumError{err}
	}
	return size, sum, nil
}

// sumError is a backup that was stored, but whose checksum wasn't.
type sumError struct{ err error }

func (e *sumError) Error() string {
	return "the backup was stored, but its checksum wasn't: " + e.err.Error()
}

// open reads a backup, decrypting it if it's encrypted. size is the stored
// size.
func (st *store) open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	rc, size, err := st.cl.Get(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	r, err := st.plain(rc)
	if err != nil {
		rc.Close()
		return nil, 0, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, rc}, size, nil
}

// plain returns the plain archive from a stored backup's bytes.
func (st *store) plain(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(len(encMagic))
	if !isEncrypted(head) {
		return br, nil
	}
	if st.key == nil {
		return nil, ErrNoKey
	}
	return newDecryptReader(br, st.key)
}

// sum returns a backup's stored checksum, or "" if it has none (backups
// made before checksums were kept).
func (st *store) sum(ctx context.Context, key string) (string, error) {
	rc, _, err := st.cl.Get(ctx, key+sumSuffix)
	if s3.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", err
	}
	sum, _, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	return sum, nil
}

// remove deletes a backup and its checksum.
func (st *store) remove(ctx context.Context, key string) error {
	if err := st.cl.Delete(ctx, key); err != nil {
		return err
	}
	return st.cl.Delete(ctx, key+sumSuffix)
}

// --- Managing targets ---

// Targets lists the targets without secrets, the primary first.
func (s *Service) Targets() ([]PublicTarget, error) {
	ts, err := s.targets()
	if err != nil {
		return nil, err
	}
	out := make([]PublicTarget, 0, len(ts))
	for _, t := range ts {
		out = append(out, s.public(t))
	}
	return out, nil
}

func (s *Service) public(t Target) PublicTarget {
	k, _ := s.key(t.ID)
	return PublicTarget{ID: t.ID, Name: t.Name, PublicSettings: publicSettings(t.Settings), Encrypt: t.Encrypt, HasKey: k != nil}
}

var targetIDRE = regexp.MustCompile(`[^a-z0-9]+`)

// SaveTarget adds a target (with an empty ID) or changes one, after
// checking that it works. A blank secret key keeps the saved one. Turning
// on encryption makes a key, returned once as NewKey.
func (s *Service) SaveTarget(ctx context.Context, in Target) (PublicTarget, error) {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	ts, err := s.targets()
	if err != nil {
		return PublicTarget{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	i := -1
	if in.ID != "" {
		if i = slices.IndexFunc(ts, func(t Target) bool { return t.ID == in.ID }); i < 0 {
			return PublicTarget{}, fmt.Errorf("there's no backup storage %q", in.ID)
		}
	} else {
		if in.Name == "" {
			return PublicTarget{}, errors.New("name the backup storage, e.g. Off-site")
		}
		id := strings.Trim(targetIDRE.ReplaceAllString(strings.ToLower(in.Name), "-"), "-")
		if id == "" || id == PrimaryID {
			id = "storage"
		}
		base := id
		for n := 2; slices.ContainsFunc(ts, func(t Target) bool { return t.ID == id }); n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		if len(ts) == 0 {
			id = PrimaryID
		}
		in.ID = id
	}
	var saved Settings
	if i >= 0 {
		saved = ts[i].Settings
		if in.Name == "" {
			in.Name = ts[i].Name
		}
	}
	in.Settings, err = mergeSettings(in.Settings, saved)
	if err != nil {
		return PublicTarget{}, err
	}
	if _, err := test(ctx, in.Settings); err != nil {
		return PublicTarget{}, err
	}
	newKey := ""
	if in.Encrypt {
		if k, err := s.key(in.ID); err != nil {
			return PublicTarget{}, err
		} else if k == nil {
			newKey = NewKey()
			if err := s.secrets.Set(secretKeyPrefix+in.ID, newKey); err != nil {
				return PublicTarget{}, err
			}
		}
	}
	if i >= 0 {
		ts[i] = in
	} else {
		ts = append(ts, in)
	}
	if err := s.saveTargets(ts); err != nil {
		return PublicTarget{}, err
	}
	out := s.public(in)
	out.NewKey = newKey
	return out, nil
}

// DeleteTarget forgets a target (not the backups in it). The primary can
// only go once it's the only one; plans copying to a target stop.
func (s *Service) DeleteTarget(id string) error {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	ts, err := s.targets()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ts, func(t Target) bool { return t.ID == id })
	if i < 0 {
		return fmt.Errorf("there's no backup storage %q", id)
	}
	if i == 0 && len(ts) > 1 {
		return errors.New("remove the other backup storage first: the primary is where every backup goes")
	}
	return s.saveTargets(slices.Delete(ts, i, i+1))
}

// Key returns a target's encryption key, for keeping it safe.
func (s *Service) Key(id string) (string, error) {
	var k string
	ok, err := s.secrets.Get(secretKeyPrefix+id, &k)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("this backup storage has no encryption key")
	}
	return k, nil
}
