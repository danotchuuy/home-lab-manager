// Package statuspage is the public status page: for the servers an admin
// chooses, whether they're up, who's on, and how to connect, for people
// without an hlm account. Nothing else about hlm or the servers is in it:
// the response is built from its own types, field by field.
package statuspage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// AllowFrom, if set, limits the page to these addresses and ranges.
	AllowFrom []string `json:"allowFrom,omitempty"`
	// ViewHash and DetailsHash are the passwords' hashes (see access.go) on
	// any address that isn't one of the Sites; the API never sends them.
	ViewHash    string `json:"viewHash,omitempty"`
	DetailsHash string `json:"detailsHash,omitempty"`
	// Sites are the page under their own domains, each with some of the
	// servers and its own passwords.
	Sites []Site `json:"sites"`
}

// Site is the status page under its own domain: only the page's servers
// in some server groups, behind its own passwords. A site's domain reaches
// only the status page: the rest of hlm answers 404 there.
type Site struct {
	// ID keeps a site's passwords when its other settings change.
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	// Groups are the IDs of the server groups whose servers it shows;
	// none shows every server on the page.
	Groups []string `json:"groups"`
	// Title and Message replace the page's, if set.
	Title       string `json:"title,omitempty"`
	Message     string `json:"message,omitempty"`
	ViewHash    string `json:"viewHash,omitempty"`
	DetailsHash string `json:"detailsHash,omitempty"`
}

// siteFor is the site a request's Host is for, or nil for the page on any
// other address.
func siteFor(set Settings, host string) *Site {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	for i := range set.Sites {
		if strings.EqualFold(set.Sites[i].Hostname, host) {
			return &set.Sites[i]
		}
	}
	return nil
}

// IsSiteHost reports whether a Host is one of the sites' domains, which
// reach only the status page.
func (s *Service) IsSiteHost(host string) bool {
	set, err := s.Settings()
	return err == nil && siteFor(set, host) != nil
}

// DefaultTitle is the page's title when none is set.
const DefaultTitle = "Game servers"

// Public states.
const (
	StateOnline   = "online"   // the game answers
	StateStarting = "starting" // running, not answering yet
	StateRunning  = "running"  // running; hlm can't ask the game
	StateOffline  = "offline"
	StateAsleep   = "asleep" // stopped by hlm while nobody played
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
	// CanWake says the page may start it while it's asleep.
	CanWake bool `json:"canWake,omitempty"`
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
	// HashPassword and PasswordMatches hash and check the page's passwords
	// (auth.HashPassword and auth.PasswordMatches).
	HashPassword    func(string) string
	PasswordMatches func(hash, password string) bool
	// Groups lists the server groups: their servers by group ID, and
	// "@host" for each Docker host a group holds whole.
	Groups func() (map[string][]string, error)
	// Asleep says whether hlm stopped a server while nobody played, and
	// whether the page may start it.
	Asleep func(server string) (asleep, canWake bool)
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
	names    []string // the server behind each of page.Servers
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
// the API keys' allow-from lists are checked). The passwords' hashes are
// kept as they were (a site's by its ID): change them with SavePasswords.
func (s *Service) Save(in Settings, parseAllowFrom func([]string) ([]string, error)) (Settings, error) {
	return s.SavePasswords(in, nil, nil, nil, parseAllowFrom)
}

// SitePasswords are the password changes for one of the sites.
type SitePasswords struct {
	View, Details PasswordChange
}

// SavePasswords sets, keeps (nil) or removes ("") the passwords along with
// the other settings: view and details for any address that isn't a site,
// and sites[i] for in.Sites[i].
func (s *Service) SavePasswords(in Settings, view, details PasswordChange, sites []SitePasswords, parseAllowFrom func([]string) ([]string, error)) (Settings, error) {
	cur, err := s.Settings()
	if err != nil {
		return Settings{}, err
	}
	in.ViewHash, in.DetailsHash = cur.ViewHash, cur.DetailsHash
	if err := s.applyPasswords(&in.ViewHash, &in.DetailsHash, view, details); err != nil {
		return Settings{}, err
	}
	in.Sites = slices.Clone(in.Sites)
	for i := range in.Sites {
		site := &in.Sites[i]
		site.ViewHash, site.DetailsHash = "", ""
		for _, old := range cur.Sites {
			if site.ID != "" && old.ID == site.ID {
				site.ViewHash, site.DetailsHash = old.ViewHash, old.DetailsHash
			}
		}
		if i < len(sites) {
			if err := s.applyPasswords(&site.ViewHash, &site.DetailsHash, sites[i].View, sites[i].Details); err != nil {
				return Settings{}, fmt.Errorf("%s: %w", site.Hostname, err)
			}
		}
	}
	return s.save(in, parseAllowFrom)
}

func (s *Service) applyPasswords(viewHash, detailsHash *string, view, details PasswordChange) error {
	for _, c := range []struct {
		change PasswordChange
		hash   *string
	}{{view, viewHash}, {details, detailsHash}} {
		switch {
		case c.change == nil:
		case *c.change == "":
			*c.hash = ""
		default:
			if err := checkNewPassword(*c.change); err != nil {
				return err
			}
			if s.opts.HashPassword == nil {
				return errors.New("passwords can't be set here")
			}
			*c.hash = s.opts.HashPassword(*c.change)
		}
	}
	return nil
}

// checkSites tidies and checks the sites, giving new ones IDs.
func (s *Service) checkSites(sites []Site) ([]Site, error) {
	var known map[string][]string
	if s.opts.Groups != nil {
		g, err := s.opts.Groups()
		if err != nil {
			return nil, err
		}
		known = g
	}
	hosts := map[string]bool{}
	out := []Site{}
	for _, site := range sites {
		site.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(site.Hostname), "."))
		site.Title, site.Message = strings.TrimSpace(site.Title), strings.TrimSpace(site.Message)
		switch {
		case site.Hostname == "":
			return nil, errors.New("a site needs its domain, such as status.example.com")
		case !hostnamePattern.MatchString(site.Hostname) || net.ParseIP(site.Hostname) != nil:
			return nil, fmt.Errorf("%q: a site's domain is a name like status.example.com", site.Hostname)
		case hosts[site.Hostname]:
			return nil, fmt.Errorf("%s is there twice", site.Hostname)
		case len(site.Title) > maxText || len(site.Message) > maxMessage:
			return nil, fmt.Errorf("%s: the title is up to %d characters, the message %d", site.Hostname, maxText, maxMessage)
		}
		hosts[site.Hostname] = true
		groups := []string{}
		for _, g := range site.Groups {
			if g == "" || slices.Contains(groups, g) {
				continue
			}
			if known != nil {
				if _, ok := known[g]; !ok {
					return nil, fmt.Errorf("%s: there's no server group %q", site.Hostname, g)
				}
			}
			groups = append(groups, g)
		}
		site.Groups = groups
		if site.ID == "" {
			b := make([]byte, 6)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			site.ID = hex.EncodeToString(b)
		}
		out = append(out, site)
	}
	return out, nil
}

func (s *Service) save(in Settings, parseAllowFrom func([]string) ([]string, error)) (Settings, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Message = strings.TrimSpace(in.Message)
	switch {
	case len(in.Title) > maxText:
		return Settings{}, fmt.Errorf("the title is up to %d characters", maxText)
	case len(in.Message) > maxMessage:
		return Settings{}, fmt.Errorf("the message is up to %d characters", maxMessage)
	case len(in.Servers) > maxServers:
		return Settings{}, fmt.Errorf("up to %d servers", maxServers)
	case len(in.Sites) > maxServers:
		return Settings{}, fmt.Errorf("up to %d sites", maxServers)
	}
	sites, err := s.checkSites(in.Sites)
	if err != nil {
		return Settings{}, err
	}
	in.Sites = sites
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
	_, err = s.save(set, nil)
	return err
}

// Page builds the public page as it is on a host (a site's domain, or any
// other), reusing the servers' states for CacheFor. It's ErrDisabled while
// the page is off.
func (s *Service) Page(ctx context.Context, host string, now time.Time) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, err := s.current()
	if err != nil {
		return Page{}, err
	}
	if !set.Enabled {
		return Page{}, ErrDisabled
	}
	if s.page == nil || now.Sub(s.built) >= CacheFor {
		if err := s.build(ctx, set, now); err != nil {
			return Page{}, err
		}
	}
	p := *s.page
	p.Title, p.Message = set.Title, set.Message
	site := siteFor(set, host)
	if site != nil {
		if site.Title != "" {
			p.Title = site.Title
		}
		if site.Message != "" {
			p.Message = site.Message
		}
		if len(site.Groups) > 0 {
			in := s.groupServers(site.Groups)
			p.Servers = []Server{}
			for i, srv := range s.page.Servers {
				if in(s.names[i]) {
					p.Servers = append(p.Servers, srv)
				}
			}
		}
	}
	if p.Title == "" {
		p.Title = DefaultTitle
	}
	return p, nil
}

// groupServers says whether a server is in any of the groups: by its
// name, or by its Docker host (a group's "@host" entries).
func (s *Service) groupServers(groups []string) func(name string) bool {
	in := map[string]bool{}
	if s.opts.Groups != nil {
		if all, err := s.opts.Groups(); err == nil { // on error, show nothing rather than everything
			for _, g := range groups {
				for _, name := range all[g] {
					in[name] = true
				}
			}
		}
	}
	return func(name string) bool {
		host := "local"
		if i := strings.LastIndexByte(name, '@'); i >= 0 {
			host = name[i+1:]
		}
		return in[name] || in["@"+host]
	}
}

// build works out every server's public view, for every host's page.
func (s *Service) build(ctx context.Context, set Settings, now time.Time) error {
	list, err := s.opts.Servers.List(ctx)
	if err != nil {
		return err
	}
	byName := map[string]manager.Server{}
	for _, srv := range list {
		byName[srv.Name] = srv
	}
	p := Page{Servers: []Server{}, Updated: now.UTC()}
	names := []string{}
	for _, e := range set.Servers {
		srv, exists := byName[e.Server]
		p.Servers = append(p.Servers, s.server(e, srv, exists))
		names = append(names, e.Server)
	}
	s.page, s.names, s.built = &p, names, now
	return nil
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
	asleep, canWake := false, false
	if s.opts.Asleep != nil && exists && !running {
		asleep, canWake = s.opts.Asleep(e.Server)
	}
	switch {
	case asleep:
		out.State, out.CanWake = StateAsleep, canWake
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

// ServerName is the server behind one of the page's entries, as it is on a
// host (a site shows only its groups' servers).
func (s *Service) ServerName(ctx context.Context, host, slug string) (string, error) {
	p, err := s.Page(ctx, host, time.Now())
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range p.Servers {
		if srv.Slug != slug {
			continue
		}
		for i, built := range s.page.Servers {
			if built.Slug == slug {
				return s.names[i], nil
			}
		}
	}
	return "", ErrDisabled
}

// Invalidate makes the next request build the page again, after a change
// visitors should see at once.
func (s *Service) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.page = nil
}

// ServerBySlug returns one server of the page, for its JSON and badge.
func (s *Service) ServerBySlug(ctx context.Context, host, slug string, now time.Time) (Server, error) {
	p, err := s.Page(ctx, host, now)
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
