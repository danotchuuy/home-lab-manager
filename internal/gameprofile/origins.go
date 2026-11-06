package gameprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// originsFile records where profiles imported from a link came from, by
// file name. It isn't a .yaml file, so it's never read as a profile.
const originsFile = ".origins.json"

// Origin is where one of your profiles came from.
type Origin struct {
	URL string `json:"url"`
	// SHA256 is the hash of what was fetched and saved.
	SHA256  string    `json:"sha256"`
	Fetched time.Time `json:"fetched"`
	// Index is the index it was listed in, if it came from one.
	Index string `json:"index,omitempty"`
	// Checked is when the link was last fetched again; Update is the hash
	// it serves now, when that's different, and UpdateError why it
	// couldn't be fetched.
	Checked     time.Time `json:"checked,omitzero"`
	Update      string    `json:"update,omitempty"`
	UpdateError string    `json:"updateError,omitempty"`
}

// OriginInfo is an origin as shown with its profile.
type OriginInfo struct {
	Origin
	// ChangedLocally says the file isn't what was fetched any more.
	ChangedLocally bool `json:"changedLocally,omitempty"`
}

// Hash is the SHA-256 of a profile's text, as origins record it.
func Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func (s *Store) readOrigins() (map[string]Origin, error) {
	out := map[string]Origin{}
	data, err := os.ReadFile(filepath.Join(s.dir, originsFile))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) writeOrigins(o map[string]Origin) error {
	if len(o) == 0 {
		err := os.Remove(filepath.Join(s.dir, originsFile))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return writeFile(filepath.Join(s.dir, originsFile), data)
}

// fileOf is the file your own profile with an id is in.
func (s *Store) fileOf(id string) (string, bool) {
	i := slices.IndexFunc(s.custom, func(c *Profile) bool { return c.ID == id })
	if i < 0 {
		return "", false
	}
	return s.custom[i].File, true
}

// Linked is one of your profiles imported from a link.
type Linked struct {
	ID     string
	Origin Origin
}

// Linked lists your profiles that came from a link.
func (s *Store) Linked() ([]Linked, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil, err
	}
	origins, err := s.readOrigins()
	if err != nil {
		return nil, err
	}
	var out []Linked
	for _, p := range s.custom {
		if o, ok := origins[p.File]; ok {
			out = append(out, Linked{ID: p.ID, Origin: o})
		}
	}
	return out, nil
}

// SetChecked records what fetching a profile's link again found: the hash
// it serves now, or why it couldn't be fetched.
func (s *Store) SetChecked(id, hash string, fetchErr error, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	file, ok := s.fileOf(id)
	if !ok {
		return ErrNotFound
	}
	origins, err := s.readOrigins()
	if err != nil {
		return err
	}
	o, ok := origins[file]
	if !ok {
		return ErrNotFound
	}
	o.Checked = now.UTC()
	o.UpdateError = ""
	if fetchErr != nil {
		o.UpdateError = fetchErr.Error()
	} else if hash != o.SHA256 {
		o.Update = hash
	} else {
		o.Update = ""
	}
	origins[file] = o
	return s.writeOrigins(origins)
}

// withOrigins copies profiles, adding your own profiles' origins.
func (s *Store) withOrigins(ps []*Profile) []*Profile {
	origins, _ := s.readOrigins()
	out := make([]*Profile, len(ps))
	for i, p := range ps {
		cp := *p
		if o, ok := origins[p.File]; ok && p.Source == Custom {
			cp.Origin = &OriginInfo{Origin: o, ChangedLocally: Hash(p.raw) != o.SHA256}
		}
		out[i] = &cp
	}
	return out
}
