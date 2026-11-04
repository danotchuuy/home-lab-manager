// Package registry keeps the logins hlm uses to pull images from private
// registries (a private Docker Hub repository, GHCR, a self-hosted registry),
// and picks the one that matches an image.
//
// Passwords and tokens stay in the encrypted secrets store and are only ever
// sent to the Docker daemon, for the registry they belong to; the API never
// returns them.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "registries"

// DockerHub is the host Docker Hub images are matched under.
const DockerHub = "docker.io"

// Entry is a login for one registry host.
type Entry struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
	// Deployers lets confined installs (by deployer keys and members who
	// deploy) pull with this login. Admins' installs and updates of existing
	// servers always may.
	Deployers bool `json:"deployers,omitempty"`
}

// Public is an entry without its password.
type Public struct {
	Host      string `json:"host"`
	Username  string `json:"username"`
	Deployers bool   `json:"deployers"`
}

// Store holds the registry logins.
type Store struct {
	secrets *secrets.Store
	mu      sync.Mutex
}

// New returns the store kept in s.
func New(s *secrets.Store) *Store { return &Store{secrets: s} }

func (s *Store) load() ([]Entry, error) {
	var out []Entry
	if s == nil || s.secrets == nil {
		return nil, nil
	}
	if _, err := s.secrets.Get(secretName, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns the logins, without passwords, sorted by host.
func (s *Store) List() ([]Public, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Public, 0, len(all))
	for _, e := range all {
		out = append(out, Public{Host: e.Host, Username: e.Username, Deployers: e.Deployers})
	}
	slices.SortFunc(out, func(a, b Public) int { return strings.Compare(a.Host, b.Host) })
	return out, nil
}

// ErrNotFound is returned for a host without a login.
var ErrNotFound = errors.New("no login for that registry")

// ErrExists is returned when adding a host that already has a login.
var ErrExists = errors.New("that registry already has a login; change it instead")

// Add saves a new login.
func (s *Store) Add(e Entry) (Public, error) {
	e, err := clean(e)
	if err != nil {
		return Public{}, err
	}
	if e.Password == "" {
		return Public{}, errors.New("a password or access token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Public{}, err
	}
	if slices.ContainsFunc(all, func(x Entry) bool { return x.Host == e.Host }) {
		return Public{}, ErrExists
	}
	if err := s.secrets.Set(secretName, append(all, e)); err != nil {
		return Public{}, err
	}
	return Public{Host: e.Host, Username: e.Username, Deployers: e.Deployers}, nil
}

// Update changes host's login. A blank password keeps the saved one.
func (s *Store) Update(host string, e Entry) (Public, error) {
	host, err := NormalizeHost(host)
	if err != nil {
		return Public{}, err
	}
	if e.Host == "" {
		e.Host = host
	}
	e, err = clean(e)
	if err != nil {
		return Public{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Public{}, err
	}
	i := slices.IndexFunc(all, func(x Entry) bool { return x.Host == host })
	if i < 0 {
		return Public{}, ErrNotFound
	}
	if e.Host != host && slices.ContainsFunc(all, func(x Entry) bool { return x.Host == e.Host }) {
		return Public{}, ErrExists
	}
	if e.Password == "" {
		// A password typed for one registry isn't sent to another.
		if e.Host != host {
			return Public{}, errors.New("enter the password again to move a login to another registry")
		}
		e.Password = all[i].Password
	}
	all[i] = e
	if err := s.secrets.Set(secretName, all); err != nil {
		return Public{}, err
	}
	return Public{Host: e.Host, Username: e.Username, Deployers: e.Deployers}, nil
}

// Delete removes host's login.
func (s *Store) Delete(host string) error {
	host, err := NormalizeHost(host)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(x Entry) bool { return x.Host == host })
	if i < 0 {
		return ErrNotFound
	}
	return s.secrets.Set(secretName, slices.Delete(all, i, i+1))
}

// Get returns host's login, with its password, for testing it.
func (s *Store) Get(host string) (Entry, error) {
	host, err := NormalizeHost(host)
	if err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range all {
		if e.Host == host {
			return e, nil
		}
	}
	return Entry{}, ErrNotFound
}

func clean(e Entry) (Entry, error) {
	host, err := NormalizeHost(e.Host)
	if err != nil {
		return e, err
	}
	e.Host = host
	e.Username = strings.TrimSpace(e.Username)
	if e.Username == "" {
		return e, errors.New("a username is required")
	}
	if len(e.Username) > 256 || len(e.Password) > 8192 {
		return e, errors.New("the username or password is too long")
	}
	return e, nil
}

// NormalizeHost turns what someone typed for a registry (a host, a URL,
// Docker Hub's old names) into the host images are matched by.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/") // https://index.docker.io/v1/
	switch h {
	case "":
		return "", errors.New("a registry host is required, e.g. ghcr.io or registry.lan:5000")
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com", "hub.docker.com":
		return DockerHub, nil
	}
	name, port, err := net.SplitHostPort(h)
	if err != nil {
		name, port = h, ""
	}
	if name == "" || strings.ContainsAny(name, " @?#") {
		return "", fmt.Errorf("%q isn't a registry host", h)
	}
	if port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				return "", fmt.Errorf("%q isn't a registry host", h)
			}
		}
	}
	return h, nil
}

// HostOf is the registry an image reference is pulled from: the first part
// of its name if that looks like a host (has a dot or a port, or is
// localhost), otherwise Docker Hub.
func HostOf(ref string) string {
	name, _ := docker.SplitImageRef(strings.TrimSpace(ref))
	first, _, found := strings.Cut(name, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return DockerHub
	}
	h, err := NormalizeHost(first)
	if err != nil {
		return DockerHub
	}
	return h
}

// ServerAddress is the address Docker's login uses for a host.
func ServerAddress(host string) string {
	if host == DockerHub {
		return "https://index.docker.io/v1/"
	}
	return host
}

type confinedKey struct{}

// Confined marks a context as a confined install's: only logins an admin
// allowed for deployers are used.
func Confined(ctx context.Context) context.Context {
	return context.WithValue(ctx, confinedKey{}, true)
}

// For returns the login to pull ref with, if there is one (and, in a
// confined install's context, it's allowed for deployers). It's what the
// Docker client asks before each pull and registry lookup.
func (s *Store) For(ctx context.Context, ref string) (docker.RegistryAuth, bool) {
	if s == nil {
		return docker.RegistryAuth{}, false
	}
	host := HostOf(ref)
	s.mu.Lock()
	all, err := s.load()
	s.mu.Unlock()
	if err != nil {
		return docker.RegistryAuth{}, false
	}
	confined, _ := ctx.Value(confinedKey{}).(bool)
	for _, e := range all {
		if e.Host == host && (!confined || e.Deployers) {
			return docker.RegistryAuth{Username: e.Username, Password: e.Password, ServerAddress: ServerAddress(host)}, true
		}
	}
	return docker.RegistryAuth{}, false
}
