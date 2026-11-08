// Package prefs keeps small per-server and per-user preferences for the
// server list: servers' tags, and each user's favourite servers. They're in
// the secrets store, so configuration backups include them.
package prefs

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretTags       = "server-tags"
	secretFavourites = "favourites"
)

// MaxTags is how many tags a server can have.
const MaxTags = 10

var tagPattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)

// Store keeps tags and favourites.
type Store struct {
	secrets *secrets.Store
	mu      sync.Mutex
}

// New creates a store.
func New(s *secrets.Store) *Store { return &Store{secrets: s} }

func (s *Store) load(name string) (map[string][]string, error) {
	m := map[string][]string{}
	_, err := s.secrets.Get(name, &m)
	if m == nil {
		m = map[string][]string{}
	}
	return m, err
}

// Tags returns every server's tags, by server name.
func (s *Store) Tags() (map[string][]string, error) {
	return s.load(secretTags)
}

// CleanTags trims, checks and de-duplicates (ignoring case) tags, keeping
// their order.
func CleanTags(tags []string) ([]string, error) {
	out := []string{}
	for _, t := range tags {
		t = strings.Join(strings.Fields(t), " ")
		if t == "" {
			continue
		}
		if !tagPattern.MatchString(t) {
			return nil, fmt.Errorf("%q isn't a tag: up to 32 letters, digits, spaces, dots, dashes and underscores", t)
		}
		if slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, t) }) {
			continue
		}
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, fmt.Errorf("a server can have up to %d tags", MaxTags)
	}
	return out, nil
}

// SetTags sets a server's tags; none removes them.
func (s *Store) SetTags(server string, tags []string) ([]string, error) {
	tags, err := CleanTags(tags)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(secretTags)
	if err != nil {
		return nil, err
	}
	if len(tags) == 0 {
		delete(m, server)
	} else {
		m[server] = tags
	}
	return tags, s.secrets.Set(secretTags, m)
}

// Favourites returns a user's favourite servers.
func (s *Store) Favourites(user string) ([]string, error) {
	m, err := s.load(secretFavourites)
	return m[user], err
}

// SetFavourite adds a server to a user's favourites, or takes it out.
func (s *Store) SetFavourite(user, server string, on bool) ([]string, error) {
	if user == "" {
		return nil, errors.New("no user")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load(secretFavourites)
	if err != nil {
		return nil, err
	}
	list := slices.DeleteFunc(m[user], func(n string) bool { return n == server })
	if on {
		list = append(list, server)
		sort.Strings(list)
	}
	if len(list) == 0 {
		delete(m, user)
	} else {
		m[user] = list
	}
	return list, s.secrets.Set(secretFavourites, m)
}

// ForgetServer drops a deleted server's tags and takes it out of everyone's
// favourites, so a new server with its name starts without them.
func (s *Store) ForgetServer(server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tags, err := s.load(secretTags)
	if err != nil {
		return err
	}
	if _, ok := tags[server]; ok {
		delete(tags, server)
		if err := s.secrets.Set(secretTags, tags); err != nil {
			return err
		}
	}
	favs, err := s.load(secretFavourites)
	if err != nil {
		return err
	}
	changed := false
	for user, list := range favs {
		if i := slices.Index(list, server); i >= 0 {
			favs[user] = slices.Delete(list, i, i+1)
			if len(favs[user]) == 0 {
				delete(favs, user)
			}
			changed = true
		}
	}
	if changed {
		return s.secrets.Set(secretFavourites, favs)
	}
	return nil
}

// RenameServer moves a server's tags, and its place in users' favourites to its new name, when it moves to
// another Docker host (its name gains or changes its @host).
func (s *Store) RenameServer(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.secrets.MoveKey(secretTags, from, to); err != nil {
		return err
	}
	favs, err := s.load(secretFavourites)
	if err != nil {
		return err
	}
	moved := false
	for user, list := range favs {
		if i := slices.Index(list, from); i >= 0 {
			list = slices.Clone(list)
			list[i] = to
			favs[user] = list
			moved = true
		}
	}
	if !moved {
		return nil
	}
	return s.secrets.Set(secretFavourites, favs)
}
