// Package mods installs, updates and removes a game server's mods (or
// plugins) from Modrinth. Every change is a transaction: hlm snapshots the
// mods folder, makes the change, restarts the server and waits for it to
// answer its status query; if it doesn't, the folder is put back the way
// it was.
//
// What hlm installed is recorded in a file beside the mods (RecordFile),
// so it travels with the server's data; files put there some other way
// are recognised by their hash where Modrinth knows them.
package mods

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// RecordFile is hlm's record of the mods it installed, in the mods folder.
// The games load only .jar files, so it's left alone.
const RecordFile = ".hlm-mods.json"

// disabledSuffix marks a mod that's kept but not loaded.
const disabledSuffix = ".disabled"

// Servers is what the service needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	ListFiles(ctx context.Context, id, path string) (manager.FileListing, error)
	ReadFile(ctx context.Context, id, path string) (files.File, error)
	ArchiveFolders(ctx context.Context, id string, folders, exclude []string, tw *tar.Writer, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	RestoreArchive(ctx context.Context, id string, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error)
	RemoveFiles(ctx context.Context, id string, paths []string) error
	MakeDir(ctx context.Context, id, path string) error
	Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error)
	Reconfigure(ctx context.Context, id string, req deploy.Request, emit func(manager.Event)) (manager.Server, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	StartJob(job manager.Job, emit func(manager.Event)) (func(manager.Event), func(error), error)
}

// Options configures a Service.
type Options struct {
	Servers  Servers
	Modrinth *Modrinth
	// Steam reads Workshop items' details.
	Steam *Steam
	// Dir is where downloads and snapshots are kept while a change runs.
	Dir string
	// Ready asks a server's game whether it's up; known is false when it
	// can't be asked (querying is off).
	Ready func(ctx context.Context, server string) (up, known bool)
	// GameVersion is the version the server last reported to its status
	// query, if any.
	GameVersion func(server string) string
	// Busy says why a server's mods can't change now (a backup is
	// running), or "".
	Busy     func(server string) string
	Notifier notify.Notifier
	Log      *slog.Logger
	Now      func() time.Time
	// StartTimeout, if set, replaces the profiles' start timeouts.
	StartTimeout time.Duration
	// Settle is how long a server that answered has to keep running to
	// count as up (10s).
	Settle time.Duration
	// Poll is how often a restarted server is checked (5s).
	Poll time.Duration
	// Unqueryable is how long a server that can't be queried has to keep
	// running to count as started (a minute).
	Unqueryable time.Duration
}

// Service manages mods.
type Service struct {
	opts Options

	mu       sync.Mutex
	hashes   map[string]string            // server|path|size|mtime → SHA-512
	byHash   map[string]*Version          // SHA-512 → its version, nil if Modrinth doesn't know it
	projects map[string]cachedProject     // project ID → project
	updates  map[string]map[string]Update // server → file → update
	checked  map[string]time.Time         // server → last update check
}

type cachedProject struct {
	p       Project
	fetched time.Time
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}
	if o.Settle == 0 {
		o.Settle = 10 * time.Second
	}
	if o.Unqueryable == 0 {
		o.Unqueryable = time.Minute
	}
	if o.Busy == nil {
		o.Busy = func(string) string { return "" }
	}
	if o.GameVersion == nil {
		o.GameVersion = func(string) string { return "" }
	}
	return &Service{opts: o, hashes: map[string]string{}, byHash: map[string]*Version{},
		projects: map[string]cachedProject{}, updates: map[string]map[string]Update{}, checked: map[string]time.Time{}}
}

// ErrUnsupported is a server hlm can't manage mods for; the message says
// why.
type ErrUnsupported struct{ Why string }

func (e *ErrUnsupported) Error() string { return e.Why }

// Overview is a server's mods.
type Overview struct {
	// Plugins says the server calls them plugins.
	Plugins bool   `json:"plugins,omitempty"`
	Loader  string `json:"loader"`
	Fit     Fit    `json:"fit"`
	// FitError says why mods can't be added (the game version isn't
	// known).
	FitError string      `json:"fitError,omitempty"`
	Dir      string      `json:"dir"`
	Running  bool        `json:"running"`
	Warnings []string    `json:"warnings,omitempty"`
	Mods     []Installed `json:"mods"`
	// Checked is when updates were last looked for.
	Checked *time.Time `json:"checked,omitempty"`
	// Timeout is how long the server gets to come back after a change.
	Timeout time.Duration `json:"timeoutNs"`
}

// Installed is a file in the mods folder.
type Installed struct {
	File     string    `json:"file"`
	Disabled bool      `json:"disabled,omitempty"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Source is hlm (installed through hlm), modrinth (put there some
	// other way, and recognised) or unknown.
	Source        string    `json:"source"`
	Project       string    `json:"project,omitempty"`
	Slug          string    `json:"slug,omitempty"`
	Title         string    `json:"title,omitempty"`
	IconURL       string    `json:"iconUrl,omitempty"`
	Version       string    `json:"version,omitempty"`
	VersionNumber string    `json:"versionNumber,omitempty"`
	By            string    `json:"by,omitempty"`
	Installed     time.Time `json:"installed,omitzero"`
	Update        *Update   `json:"update,omitempty"`
	sha512        string
}

// Update is a newer version of an installed mod.
type Update struct {
	Version       string `json:"version"`
	VersionNumber string `json:"versionNumber"`
	// Blocked says why it can't be installed (it needs a newer game
	// version).
	Blocked string `json:"blocked,omitempty"`
}

// Record is RecordFile's contents.
type Record struct {
	Mods map[string]RecordEntry `json:"mods"`
}

// RecordEntry is a mod hlm installed, by its file name (without
// disabledSuffix).
type RecordEntry struct {
	Project       string    `json:"project"`
	Version       string    `json:"version"`
	VersionNumber string    `json:"versionNumber,omitempty"`
	SHA512        string    `json:"sha512"`
	Size          int64     `json:"size"`
	Installed     time.Time `json:"installed"`
	By            string    `json:"by,omitempty"`
}

// state is what a server's mods look like now.
type state struct {
	info    docker.ContainerJSON
	name    string
	profile *gameprofile.Profile
	loader  gameprofile.ModLoader
	ov      Overview
	record  Record
	// exists says the mods folder is there.
	exists bool
}

// load reads a server's mods folder.
func (s *Service) load(ctx context.Context, server string, identify bool) (*state, error) {
	info, err := s.opts.Servers.Inspect(ctx, server)
	if err != nil {
		return nil, err
	}
	name := server
	p := gameprofile.Match(info.Config.Image)
	if p == nil {
		return nil, &ErrUnsupported{"hlm doesn't know which game this server runs, so it can't manage its mods"}
	}
	if p.Mods == nil || p.Mods.Modrinth == nil {
		return nil, &ErrUnsupported{fmt.Sprintf("hlm doesn't know how %s takes mods", p.Name)}
	}
	env := envMap(info.Config.Env)
	mr := p.Mods.Modrinth
	lname, loader, ok := mr.Loader(env)
	if !ok {
		var names []string
		for k := range mr.Loaders {
			names = append(names, k)
		}
		slices.Sort(names)
		return nil, &ErrUnsupported{fmt.Sprintf("This server runs %s as %s, which doesn't load mods. Set its %s variable to one of %s to use them",
			p.Name, cmpOr(lname, "plain"), mr.LoaderEnv, strings.Join(names, ", "))}
	}
	st := &state{info: info, name: name, profile: p, loader: loader, record: Record{Mods: map[string]RecordEntry{}}}
	st.ov = Overview{Plugins: loader.Plugins, Loader: lname, Dir: loader.Dir, Running: info.State.Running,
		Mods: []Installed{}, Timeout: p.Mods.Timeout()}
	if s.opts.StartTimeout > 0 {
		st.ov.Timeout = s.opts.StartTimeout
	}
	st.ov.Fit = Fit{Loaders: loader.Modrinth, GameVersion: s.gameVersion(name, mr, env)}
	if st.ov.Fit.GameVersion == "" {
		st.ov.FitError = fmt.Sprintf("hlm doesn't know which version of %s this server runs. Start it, so hlm can ask it, or set its %s variable",
			p.Name, mr.VersionEnv)
	}
	for _, e := range mr.WarnEnv {
		if v := env[e]; v != "" && !strings.EqualFold(v, "false") {
			st.ov.Warnings = append(st.ov.Warnings, fmt.Sprintf(
				"The server's %s variable is set: the image installs or removes %s itself when it starts, which can undo changes made here.", e, st.kind()))
		}
	}

	listing, err := s.opts.Servers.ListFiles(ctx, server, loader.Dir)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return st, nil // no mods yet
	case err != nil:
		return nil, err
	}
	st.exists = true
	if f, err := s.opts.Servers.ReadFile(ctx, server, path.Join(loader.Dir, RecordFile)); err == nil {
		if err := json.Unmarshal([]byte(f.Content), &st.record); err != nil {
			st.ov.Warnings = append(st.ov.Warnings, fmt.Sprintf("hlm's record of what it installed (%s) is damaged, so it's starting a new one: %v", RecordFile, err))
		}
		if st.record.Mods == nil {
			st.record.Mods = map[string]RecordEntry{}
		}
	}
	for _, e := range listing.Entries {
		base, disabled := strings.CutSuffix(e.Name, disabledSuffix)
		if e.Type != "file" || !strings.HasSuffix(strings.ToLower(base), ".jar") {
			continue
		}
		m := Installed{File: e.Name, Disabled: disabled, Size: e.Size, Modified: e.Mtime, Source: "unknown"}
		if r, ok := st.record.Mods[base]; ok && r.Size == e.Size {
			m.Source, m.Project, m.Version, m.VersionNumber, m.By, m.Installed, m.sha512 =
				"hlm", r.Project, r.Version, r.VersionNumber, r.By, r.Installed, r.SHA512
		}
		st.ov.Mods = append(st.ov.Mods, m)
	}
	// Forget files that are gone.
	for k := range st.record.Mods {
		if !slices.ContainsFunc(st.ov.Mods, func(m Installed) bool { return strings.TrimSuffix(m.File, disabledSuffix) == k }) {
			delete(st.record.Mods, k)
		}
	}
	if identify {
		if err := s.identify(ctx, st); err != nil {
			st.ov.Warnings = append(st.ov.Warnings, "Couldn't recognise the mods put there outside hlm: "+err.Error())
		}
		s.describe(ctx, st)
	}
	return st, nil
}

func (st *state) kind() string {
	if st.loader.Plugins {
		return "plugins"
	}
	return "mods"
}

// gameVersion is the server's game version: its variable, or what it told
// its status query.
func (s *Service) gameVersion(name string, mr *gameprofile.ModrinthMods, env map[string]string) string {
	if mr.VersionEnv != "" {
		if v := strings.TrimSpace(env[mr.VersionEnv]); versionRE.MatchString(v) {
			return v
		}
	}
	return versionRE.FindString(s.opts.GameVersion(name))
}

// versionRE finds a game version such as 1.21.4 (in "Paper 1.21.4").
var versionRE = regexp.MustCompile(`\b\d+\.\d+(?:\.\d+)?(?:-(?:pre|rc)\d+)?\b`)

// identify recognises files hlm didn't install, by asking Modrinth about
// their hashes.
func (s *Service) identify(ctx context.Context, st *state) error {
	var need []int
	for i, m := range st.ov.Mods {
		if m.sha512 == "" {
			need = append(need, i)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if err := s.hashFiles(ctx, st, need); err != nil {
		return err
	}
	var ask []string
	s.mu.Lock()
	for _, i := range need {
		if h := st.ov.Mods[i].sha512; h != "" {
			if _, known := s.byHash[h]; !known {
				ask = append(ask, h)
			}
		}
	}
	s.mu.Unlock()
	if len(ask) > 0 {
		found, err := s.opts.Modrinth.ByHash(ctx, ask)
		if err != nil {
			return err
		}
		s.mu.Lock()
		for _, h := range ask {
			if v, ok := found[h]; ok {
				s.byHash[h] = &v
			} else {
				s.byHash[h] = nil
			}
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range need {
		m := &st.ov.Mods[i]
		if v := s.byHash[m.sha512]; v != nil {
			m.Source, m.Project, m.Version, m.VersionNumber = "modrinth", v.ProjectID, v.ID, v.VersionNumber
		}
	}
	return nil
}

// describe fills in the mods' titles and icons, and known updates.
func (s *Service) describe(ctx context.Context, st *state) {
	var ids []string
	s.mu.Lock()
	for _, m := range st.ov.Mods {
		if c, ok := s.projects[m.Project]; m.Project != "" && (!ok || s.opts.Now().Sub(c.fetched) > time.Hour) && !slices.Contains(ids, m.Project) {
			ids = append(ids, m.Project)
		}
	}
	s.mu.Unlock()
	if len(ids) > 0 {
		ps, err := s.opts.Modrinth.Projects(ctx, ids)
		if err != nil {
			st.ov.Warnings = append(st.ov.Warnings, "Couldn't get the mods' names from Modrinth: "+err.Error())
		}
		s.mu.Lock()
		for _, p := range ps {
			s.projects[p.ID] = cachedProject{p: p, fetched: s.opts.Now()}
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ups := s.updates[st.name]
	for i := range st.ov.Mods {
		m := &st.ov.Mods[i]
		if c, ok := s.projects[m.Project]; ok {
			m.Title, m.Slug, m.IconURL = c.p.Title, c.p.Slug, c.p.IconURL
		}
		if u, ok := ups[m.File]; ok && u.Version != m.Version {
			m.Update = &u
		}
	}
	if t, ok := s.checked[st.name]; ok {
		st.ov.Checked = &t
	}
}

// Overview lists a server's mods.
func (s *Service) Overview(ctx context.Context, server string) (Overview, error) {
	st, err := s.load(ctx, server, true)
	if err != nil {
		return Overview{}, err
	}
	return st.ov, nil
}

// Search finds mods for a server on Modrinth.
func (s *Service) Search(ctx context.Context, server, query string) ([]SearchHit, error) {
	st, err := s.load(ctx, server, false)
	if err != nil {
		return nil, err
	}
	if st.ov.FitError != "" {
		return nil, errors.New(st.ov.FitError)
	}
	if len(query) > 100 {
		return nil, errors.New("search for something shorter")
	}
	return s.opts.Modrinth.Search(ctx, query, st.ov.Fit, 20)
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
