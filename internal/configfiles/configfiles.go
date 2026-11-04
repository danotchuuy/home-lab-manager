// Package configfiles keeps each server's pinned settings files, for opening
// them in one click, and suggests the usual ones for games it knows.
package configfiles

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// File is a settings file of a server.
type File struct {
	// Path is the file's absolute path in the container.
	Path string `json:"path"`
	// Label names it in the UI; empty uses the file name.
	Label string `json:"label,omitempty"`
}

// List is a server's pinned files and suggested ones.
type List struct {
	Pinned    []File `json:"pinned"`
	Suggested []File `json:"suggested"`
}

// Servers is what the service needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	SearchFiles(ctx context.Context, id, dir, query string) ([]files.Match, bool, error)
}

// Service keeps pinned files and finds suggestions.
type Service struct {
	secrets *secrets.Store
	servers Servers
	saveMu  sync.Mutex

	mu    sync.Mutex
	cache map[string]cached // suggestions by container ID
	now   func() time.Time
}

type cached struct {
	files []File
	at    time.Time
}

// suggestFor is how long suggestions are kept before looking again.
const suggestFor = 10 * time.Minute

const secretName = "configfiles"

// Limits.
const (
	maxPinned   = 20
	maxLabelLen = 64
)

// New creates the service.
func New(store *secrets.Store, servers Servers) *Service {
	return &Service{secrets: store, servers: servers, cache: map[string]cached{}, now: time.Now}
}

func (s *Service) all() (map[string][]File, error) {
	m := map[string][]File{}
	_, err := s.secrets.Get(secretName, &m)
	return m, err
}

// Pinned returns a server's pinned files.
func (s *Service) Pinned(server string) ([]File, error) {
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	out := all[server]
	if out == nil {
		out = []File{}
	}
	return out, nil
}

// SetPinned replaces a server's pinned files.
func (s *Service) SetPinned(server string, in []File) ([]File, error) {
	var out []File
	for _, f := range in {
		f.Path = strings.TrimSpace(f.Path)
		f.Label = strings.TrimSpace(f.Label)
		if !strings.HasPrefix(f.Path, "/") || strings.ContainsAny(f.Path, "\x00\n") {
			return nil, fmt.Errorf("%q isn't an absolute path in the container", f.Path)
		}
		f.Path = path.Clean(f.Path)
		if len(f.Label) > maxLabelLen {
			return nil, fmt.Errorf("labels can be at most %d characters", maxLabelLen)
		}
		if f.Label == path.Base(f.Path) {
			f.Label = ""
		}
		if !slices.ContainsFunc(out, func(x File) bool { return x.Path == f.Path }) {
			out = append(out, f)
		}
	}
	if len(out) > maxPinned {
		return nil, fmt.Errorf("a server can have at most %d pinned files", maxPinned)
	}
	if out == nil {
		out = []File{}
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		delete(all, server)
	} else {
		all[server] = out
	}
	return out, s.secrets.Set(secretName, all)
}

// ForgetServer removes a deleted server's pinned files.
func (s *Service) ForgetServer(server string) error {
	_, err := s.SetPinned(server, nil)
	return err
}

// known are games' usual settings files, by a word in the image name (its
// tag included, as ich777's images name the game there). prefer, if set,
// picks among several files of that name.
var known = []struct {
	word   string
	names  []string
	prefer string
}{
	{"palworld", []string{"PalWorldSettings.ini"}, "/Saved/Config/"},
	{"minecraft", []string{"server.properties"}, ""},
	{"ark", []string{"GameUserSettings.ini", "Game.ini"}, "/Saved/Config/"},
	{"arkse", []string{"GameUserSettings.ini", "Game.ini"}, "/Saved/Config/"},
	{"enshrouded", []string{"enshrouded_server.json"}, ""},
	{"vrising", []string{"ServerHostSettings.json", "ServerGameSettings.json"}, "/Settings/"},
	{"7dtd", []string{"serverconfig.xml"}, ""},
	{"7daystodie", []string{"serverconfig.xml"}, ""},
	{"terraria", []string{"serverconfig.txt"}, ""},
	{"tshock", []string{"config.json"}, ""},
	{"factorio", []string{"server-settings.json"}, ""},
	{"cs2", []string{"server.cfg"}, ""},
	{"rust", []string{"server.cfg"}, ""},
	{"zomboid", []string{"servertest.ini"}, ""},
	{"conan", []string{"ServerSettings.ini"}, "/Saved/Config/"},
	{"satisfactory", []string{"ServerSettings.ini", "Game.ini"}, "/Saved/Config/"},
	{"nightingale", []string{"ServerSettings.ini"}, ""},
	{"soulmask", []string{"GameXishu.json"}, ""},
}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

func words(image string) []string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return wordRE.FindAllString(strings.ToLower(image), -1)
}

// List returns a server's pinned files, and suggested ones that aren't
// pinned.
func (s *Service) List(ctx context.Context, id string) (List, error) {
	info, err := s.servers.Inspect(ctx, id)
	if err != nil {
		return List{}, err
	}
	pinned, err := s.Pinned(strings.TrimPrefix(info.Name, "/"))
	if err != nil {
		return List{}, err
	}
	out := List{Pinned: pinned, Suggested: []File{}}
	for _, f := range s.suggest(ctx, info) {
		if !slices.ContainsFunc(pinned, func(p File) bool { return p.Path == f.Path }) {
			out.Suggested = append(out.Suggested, f)
		}
	}
	return out, nil
}

// suggest finds the usual settings files of a server's game in its data
// folders.
func (s *Service) suggest(ctx context.Context, info docker.ContainerJSON) []File {
	s.mu.Lock()
	if c, ok := s.cache[info.ID]; ok && s.now().Sub(c.at) < suggestFor {
		s.mu.Unlock()
		return c.files
	}
	s.mu.Unlock()

	ws := words(info.Config.Image)
	var names []string
	prefer := map[string]string{}
	for _, k := range known {
		if slices.Contains(ws, k.word) {
			for _, n := range k.names {
				if !slices.Contains(names, n) {
					names = append(names, n)
					prefer[n] = k.prefer
				}
			}
		}
	}
	found := []File{}
	if len(names) > 0 {
		var dirs []string
		for _, m := range info.Mounts {
			if m.Type != "tmpfs" && m.Destination != "" && m.Destination != "/" && !slices.Contains(dirs, m.Destination) {
				dirs = append(dirs, m.Destination)
			}
		}
		slices.Sort(dirs)
		for _, name := range names {
			var matches []string
			for _, d := range dirs {
				ms, _, err := s.servers.SearchFiles(ctx, info.ID, d, name)
				if err != nil {
					continue
				}
				for _, m := range ms {
					// Search matches names containing the query; only the
					// exact name counts (not DefaultPalWorldSettings.ini).
					if strings.EqualFold(path.Base(m.Path), name) && !slices.Contains(matches, m.Path) {
						matches = append(matches, m.Path)
					}
				}
			}
			// Where a game keeps several, the one under its usual folder.
			if p := prefer[name]; p != "" && len(matches) > 1 {
				var preferred []string
				for _, m := range matches {
					if strings.Contains(m, p) {
						preferred = append(preferred, m)
					}
				}
				if len(preferred) > 0 {
					matches = preferred
				}
			}
			for _, m := range matches {
				if len(found) < 6 {
					found = append(found, File{Path: m})
				}
			}
		}
	}
	s.mu.Lock()
	s.cache[info.ID] = cached{found, s.now()}
	s.mu.Unlock()
	return found
}
