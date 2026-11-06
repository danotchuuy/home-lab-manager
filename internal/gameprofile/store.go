package gameprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// ErrNotFound is a profile that isn't there, or isn't one of your own.
var ErrNotFound = errors.New("no such game profile")

// Store keeps your own profiles: *.yaml files in a directory, which take the
// place of built-in profiles with the same id. Files that can't be used are
// reported, not fatal: the rest still are.
type Store struct {
	dir string

	mu     sync.Mutex
	custom []*Profile
	errs   []LoadError
}

// NewStore keeps profiles in dir. Call Reload to read them.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir is where the profiles are kept.
func (s *Store) Dir() string { return s.dir }

// Reload reads the directory again and puts its profiles in use. It fails
// only if the directory can't be read; it's fine for it not to exist.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reload()
}

func (s *Store) reload() error {
	ps, errs, err := read(os.DirFS(s.dir), ".", Custom)
	if errors.Is(err, os.ErrNotExist) {
		ps, errs, err = nil, nil, nil
	}
	if err != nil {
		return err
	}
	s.custom, s.errs = ps, errs
	Use(Merge(Default(), ps))
	return nil
}

// State is every profile in use, and the files that couldn't be used.
type State struct {
	Dir      string      `json:"dir"`
	Profiles []*Profile  `json:"profiles"`
	Errors   []LoadError `json:"errors"`
}

// State says which profiles are in use.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Dir: s.dir, Profiles: s.withOrigins(Current().All()), Errors: slices.Clone(s.errs)}
	if st.Errors == nil {
		st.Errors = []LoadError{}
	}
	return st
}

// Import checks a profile and saves it, replacing your own profile with the
// same id (or taking a built-in one's place). If the profile came from a
// link, it keeps its origin, and shows as changed locally.
func (s *Store) Import(data []byte) (*Profile, error) { return s.ImportFrom(data, nil) }

// ImportFrom is Import for a profile fetched from a link: origin says where
// from (nil keeps the origin it had, if any).
func (s *Store) ImportFrom(data []byte, origin *Origin) (*Profile, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("a profile can be at most %d KiB", MaxSize>>10)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil, err
	}
	name := p.ID + ".yaml"
	if i := slices.IndexFunc(s.custom, func(c *Profile) bool { return c.ID == p.ID }); i >= 0 {
		name = s.custom[i].File
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(s.dir, name), data); err != nil {
		return nil, err
	}
	if origin != nil {
		origins, err := s.readOrigins()
		if err != nil {
			return nil, err
		}
		origins[name] = *origin
		if err := s.writeOrigins(origins); err != nil {
			return nil, err
		}
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	if got := Current().ByID(p.ID); got != nil && got.Source == Custom {
		return s.withOrigins([]*Profile{got})[0], nil
	}
	// Another file with the same id, earlier by name, is kept instead.
	for _, e := range s.errs {
		if e.File == name {
			return nil, fmt.Errorf("saved as %s, but it isn't used: %s", name, e.Error)
		}
	}
	return nil, fmt.Errorf("saved as %s, but it isn't used", name)
}

// Delete removes your own profile; a built-in one with the same id is used
// again.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return err
	}
	i := slices.IndexFunc(s.custom, func(c *Profile) bool { return c.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	file := s.custom[i].File
	if err := os.Remove(filepath.Join(s.dir, file)); err != nil {
		return err
	}
	if origins, err := s.readOrigins(); err == nil {
		if _, ok := origins[file]; ok {
			delete(origins, file)
			if err := s.writeOrigins(origins); err != nil {
				return err
			}
		}
	}
	return s.reload()
}

// Source is a profile in use as it was written: a starting point for your
// own.
func (s *Store) Source(id string) ([]byte, error) {
	if p := Current().ByID(id); p != nil {
		return p.YAML(), nil
	}
	return nil, ErrNotFound
}

// Builtin is a built-in profile as it was written, also when your own
// takes its place.
func Builtin(id string) ([]byte, error) {
	if p := Default().ByID(id); p != nil {
		return p.YAML(), nil
	}
	return nil, ErrNotFound
}

// writeFile replaces a file all at once, readable only by hlm.
func writeFile(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".profile-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// Files are your own profiles' files, by name, for exporting them.
func (s *Store) Files() (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, p := range s.custom {
		data, err := os.ReadFile(filepath.Join(s.dir, p.File))
		if err != nil {
			return nil, err
		}
		out[p.File] = data
	}
	return out, nil
}
