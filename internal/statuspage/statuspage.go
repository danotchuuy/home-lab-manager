// Package statuspage is the public status page: for the servers an admin
// chooses, whether they're up, who's on, and how to connect, for people
// without an hlm account. Nothing else about hlm or the servers is in it:
// the response is built from its own types, field by field.
package statuspage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "statuspage"

// CacheFor is how long a built page is reused.
const CacheFor = 15 * time.Second

// ErrDisabled is returned while the page is off (and for servers that
// aren't on it).
var ErrDisabled = errors.New("not found")

// Entry is a server on the page, as the admin set it up.
type Entry struct {
	// Server is the server's (internal) name; it's never shown.
	Server string `json:"server"`
	// DisplayName is what the page calls it; the app's name if empty.
	DisplayName string `json:"displayName,omitempty"`
	ShowPlayers bool   `json:"showPlayers"`
	// ShowNames lists who's playing (with ShowPlayers).
	ShowNames bool `json:"showNames"`
	// ShowVersion shows the game's version and map, if it reports them.
	ShowVersion bool `json:"showVersion"`
	// Address is how to connect, as players should type it.
	Address string `json:"address,omitempty"`
	// Hint is a note next to the address, such as where to find the
	// password: never the password itself.
	Hint string `json:"hint,omitempty"`
	// Slug names the server in badge URLs; made from the display name.
	Slug string `json:"slug"`
}

// Settings are the page's settings.
type Settings struct {
	Enabled bool    `json:"enabled"`
	Title   string  `json:"title,omitempty"`
	Message string  `json:"message,omitempty"`
	Servers []Entry `json:"servers"`
	// Hostname, if set, is a name that only reaches the status page: the
	// rest of hlm answers 404 there (for a tunnel or proxy route).
	Hostname string `json:"hostname,omitempty"`
	// AllowFrom, if set, limits the page to these addresses and ranges.
	AllowFrom []string `json:"allowFrom,omitempty"`
}

// DefaultTitle is the page's title when none is set.
const DefaultTitle = "Game servers"

// Public states.
const (
	StateOnline   = "online"   // the game answers
	StateStarting = "starting" // running, not answering yet
	StateRunning  = "running"  // running; hlm can't ask the game
	StateOffline  = "offline"
)

// Page is the public page. Only what's here is ever sent.
type Page struct {
	Title   string    `json:"title"`
	Message string    `json:"message,omitempty"`
	Servers []Server  `json:"servers"`
	Updated time.Time `json:"updated"`
}

// Server is one server on the public page.
type Server struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Game        string   `json:"game,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	State       string   `json:"state"`
	Players     *int     `json:"players,omitempty"`
	MaxPlayers  *int     `json:"maxPlayers,omitempty"`
	PlayerNames []string `json:"playerNames,omitempty"`
	Version     string   `json:"version,omitempty"`
	Map         string   `json:"map,omitempty"`
	Address     string   `json:"address,omitempty"`
	Hint        string   `json:"hint,omitempty"`
	// Uptime is the share of the last 7 and 30 days it was up (0–1), when
	// there's history.
	Uptime7d  *float64 `json:"uptime7d,omitempty"`
	Uptime30d *float64 `json:"uptime30d,omitempty"`
}

// Servers is what the page needs from the manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
}

// Query is what it needs from the game query.
type Query interface {
	Status(name string, names bool) gamequery.Status
}

// Options configure a Service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	Query   Query
	// Uptime is the share of rng a server was up, if there's history.
	Uptime func(server string, rng time.Duration) (float64, bool)
	// Suggest offers addresses players could use to reach a server, for
	// the settings form.
	Suggest func(ctx context.Context, server string) []Suggestion
}

// Suggestion is an address the settings form offers.
type Suggestion struct {
	Address string `json:"address"`
	// Source is where it comes from: "proxy" or "unifi".
	Source string `json:"source"`
}

// Service keeps the settings and builds the page.
type Service struct {
	opts Options

	mu       sync.Mutex
	settings *Settings // cached; see current
	page     *Page
	built    time.Time
}

// New creates the service.
func New(o Options) *Service { return &Service{opts: o} }

// Settings returns the settings.
func (s *Service) Settings() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current()
}

func (s *Service) current() (Settings, error) {
	if s.settings != nil {
		return *s.settings, nil
	}
	var set Settings
	if _, err := s.opts.Secrets.Get(secretName, &set); err != nil {
		return Settings{}, err
	}
	if set.Servers == nil {
		set.Servers = []Entry{}
	}
	s.settings = &set
	return set, nil
}

const (
	maxServers = 50
	maxText    = 200
	maxMessage = 1000
)

var hostnamePattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Save checks and stores the settings. parseAllowFrom checks AllowFrom (as
// the API keys' allow-from lists are checked).
func (s *Service) Save(in Settings, parseAllowFrom func([]string) ([]string, error)) (Settings, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Message = strings.TrimSpace(in.Message)
	in.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Hostname), "."))
	switch {
	case len(in.Title) > maxText:
		return Settings{}, fmt.Errorf("the title is up to %d characters", maxText)
	case len(in.Message) > maxMessage:
		return Settings{}, fmt.Errorf("the message is up to %d characters", maxMessage)
	case len(in.Servers) > maxServers:
		return Settings{}, fmt.Errorf("up to %d servers", maxServers)
	case in.Hostname != "" && (!hostnamePattern.MatchString(in.Hostname) || net.ParseIP(in.Hostname) != nil):
		return Settings{}, errors.New("the hostname is a name like status.example.com")
	}
	if parseAllowFrom != nil {
		list, err := parseAllowFrom(in.AllowFrom)
		if err != nil {
			return Settings{}, err
		}
		in.AllowFrom = list
	}
	seen := map[string]bool{}
	slugs := map[string]bool{}
	out := []Entry{}
	for _, e := range in.Servers {
		e.Server = strings.TrimSpace(e.Server)
		if e.Server == "" || seen[e.Server] {
			continue
		}
		seen[e.Server] = true
		e.DisplayName, e.Address, e.Hint = strings.TrimSpace(e.DisplayName), strings.TrimSpace(e.Address), strings.TrimSpace(e.Hint)
		if len(e.DisplayName) > maxText || len(e.Address) > maxText || len(e.Hint) > maxText {
			return Settings{}, fmt.Errorf("%s: names, addresses and hints are up to %d characters", e.Server, maxText)
		}
		if !e.ShowPlayers {
			e.ShowNames = false
		}
		// A badge URL's name: from the display name (or the server's own
		// name, without its host), once.
		base := slug(e.DisplayName)
		if base == "" {
			_, bare := manager.SplitID(e.Server)
			base = slug(bare)
		}
		if base == "" {
			base = "server"
		}
		e.Slug = base
		for n := 2; slugs[e.Slug]; n++ {
			e.Slug = fmt.Sprintf("%s-%d", base, n)
		}
		slugs[e.Slug] = true
		out = append(out, e)
	}
	in.Servers = out
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.opts.Secrets.Set(secretName, in); err != nil {
		return Settings{}, err
	}
	s.settings, s.page = &in, nil
	return in, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// ForgetServer takes a deleted server off the page.
func (s *Service) ForgetServer(name string) error {
	set, err := s.Settings()
	if err != nil {
		return err
	}
	keep := slices.DeleteFunc(slices.Clone(set.Servers), func(e Entry) bool { return e.Server == name })
	if len(keep) == len(set.Servers) {
		return nil
	}
	set.Servers = keep
	_, err = s.Save(set, nil)
	return err
}

// Page builds the public page, or reuses one built in the last CacheFor.
// It's ErrDisabled while the page is off.
func (s *Service) Page(ctx context.Context, now time.Time) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.current()
	if err != nil {
		return Page{}, err
	}
	if !set.Enabled {
		return Page{}, ErrDisabled
	}
	if s.page != nil && now.Sub(s.built) < CacheFor {
		return *s.page, nil
	}
	list, err := s.opts.Servers.List(ctx)
	if err != nil {
		return Page{}, err
	}
	byName := map[string]manager.Server{}
	for _, srv := range list {
		byName[srv.Name] = srv
	}
	p := Page{Title: set.Title, Message: set.Message, Servers: []Server{}, Updated: now.UTC()}
	if p.Title == "" {
		p.Title = DefaultTitle
	}
	for _, e := range set.Servers {
		srv, exists := byName[e.Server]
		p.Servers = append(p.Servers, s.server(e, srv, exists))
	}
	s.page, s.built = &p, now
	return p, nil
}

// server is one entry's public view: only allow-listed values, copied one
// by one.
func (s *Service) server(e Entry, srv manager.Server, exists bool) Server {
	out := Server{Slug: e.Slug, Name: e.DisplayName, Address: e.Address, Hint: e.Hint, State: StateOffline}
	if exists {
		out.Game, out.Icon = srv.App, srv.Icon
	}
	if out.Name == "" {
		out.Name = out.Game
	}
	if out.Name == "" {
		_, out.Name = manager.SplitID(e.Server)
	}
	running := exists && (srv.State == "running" || srv.State == "restarting")
	var st gamequery.Status
	if s.opts.Query != nil && running {
		st = s.opts.Query.Status(e.Server, e.ShowNames)
	}
	switch {
	case !running:
		out.State = StateOffline
	case st.Status == gamequery.StatusOnline:
		out.State = StateOnline
	case st.Status == gamequery.StatusStarting:
		out.State = StateStarting
	case st.Status == gamequery.StatusNotResponding:
		out.State = StateOffline
	default:
		out.State = StateRunning
	}
	if r := st.Result; r != nil && out.State == StateOnline {
		if e.ShowPlayers && r.HasPlayers {
			n := r.Players
			out.Players = &n
			if r.MaxPlayers > 0 {
				m := r.MaxPlayers
				out.MaxPlayers = &m
			}
			if e.ShowNames && len(r.PlayerNames) > 0 {
				out.PlayerNames = slices.Clone(r.PlayerNames)
			}
		}
		if e.ShowVersion {
			out.Version, out.Map = r.Version, r.Map
		}
	}
	if s.opts.Uptime != nil {
		if u, ok := s.opts.Uptime(e.Server, 7*24*time.Hour); ok {
			out.Uptime7d = &u
		}
		if u, ok := s.opts.Uptime(e.Server, 30*24*time.Hour); ok {
			out.Uptime30d = &u
		}
	}
	return out
}

// ServerBySlug returns one server of the page, for its JSON and badge.
func (s *Service) ServerBySlug(ctx context.Context, slug string, now time.Time) (Server, error) {
	p, err := s.Page(ctx, now)
	if err != nil {
		return Server{}, err
	}
	for _, srv := range p.Servers {
		if srv.Slug == slug {
			return srv, nil
		}
	}
	return Server{}, ErrDisabled
}

// Suggestions offers addresses for the servers, for the settings form.
func (s *Service) Suggestions(ctx context.Context, servers []string) map[string][]Suggestion {
	out := map[string][]Suggestion{}
	if s.opts.Suggest == nil {
		return out
	}
	for _, name := range servers {
		if list := s.opts.Suggest(ctx, name); len(list) > 0 {
			out[name] = list
		}
	}
	return out
}
