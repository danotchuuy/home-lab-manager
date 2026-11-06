package mods

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/mods/modrinthtest"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// fakeServer is one server, its mods folder kept in memory.
type fakeServer struct {
	mu      sync.Mutex
	env     []string
	running bool
	started int
	files   map[string][]byte
	dirs    map[string]bool
	// crashWith makes the server stop as soon as it starts while a file
	// of that name is in the folder.
	crashWith     string
	starts, stops int
	removed       []string
}

func newFake(env ...string) *fakeServer {
	return &fakeServer{env: env, running: true, files: map[string][]byte{}, dirs: map[string]bool{"/data": true}}
}

func (f *fakeServer) put(p string, content string) {
	f.dirs[path.Dir(p)] = true
	f.files[p] = []byte(content)
}

func (f *fakeServer) names(dir string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.files {
		if path.Dir(p) == dir {
			out = append(out, path.Base(p))
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeServer) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info docker.ContainerJSON
	info.ID, info.Name = "c1", "/mc"
	info.Config.Image = "itzg/minecraft-server:java21"
	info.Config.Env = f.env
	info.State.Running = f.running
	info.State.StartedAt = fmt.Sprint(f.started)
	info.Mounts = []docker.MountPoint{{Type: "volume", Destination: "/data", RW: true}}
	return info, nil
}

func (f *fakeServer) ListFiles(ctx context.Context, id, dir string) (manager.FileListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[dir] {
		return manager.FileListing{}, files.ErrNotFound
	}
	l := manager.FileListing{}
	for p, c := range f.files {
		if path.Dir(p) == dir {
			l.Entries = append(l.Entries, files.Entry{Name: path.Base(p), Type: "file", Size: int64(len(c)), Mtime: time.Unix(int64(len(c)), 0)})
		}
	}
	return l, nil
}

func (f *fakeServer) ReadFile(ctx context.Context, id, p string) (files.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	if !ok {
		return files.File{}, files.ErrNotFound
	}
	return files.File{Path: p, Content: string(c)}, nil
}

func (f *fakeServer) ArchiveFolders(ctx context.Context, id string, folders, exclude []string, tw *tar.Writer, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st files.ArchiveStats
	for _, d := range folders {
		if !f.dirs[d] {
			return st, fmt.Errorf("%s: %w", d, files.ErrNotFound)
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(d, "/") + "/", Mode: 0o755, Uid: 1000, Gid: 1000}); err != nil {
			return st, err
		}
		var ps []string
		for p := range f.files {
			if path.Dir(p) == d {
				ps = append(ps, p)
			}
		}
		slices.Sort(ps)
		for _, p := range ps {
			c := f.files[p]
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(p, "/"), Mode: 0o600, Size: int64(len(c)), Uid: 1000, Gid: 1000}); err != nil {
				return st, err
			}
			if _, err := tw.Write(c); err != nil {
				return st, err
			}
			st.Files++
		}
	}
	return st, nil
}

func (f *fakeServer) RestoreArchive(ctx context.Context, id string, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	var st files.ArchiveStats
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return st, nil
		}
		if err != nil {
			return st, err
		}
		p := "/" + strings.TrimSuffix(hdr.Name, "/")
		if hdr.Typeflag == tar.TypeDir {
			f.mu.Lock()
			f.dirs[p] = true
			f.mu.Unlock()
			continue
		}
		if hdr.Uid != 1000 {
			return st, fmt.Errorf("%s written as %d", p, hdr.Uid)
		}
		c, err := io.ReadAll(tr)
		if err != nil {
			return st, err
		}
		f.mu.Lock()
		f.files[p] = c
		f.mu.Unlock()
		st.Files++
	}
}

func (f *fakeServer) RemoveFiles(ctx context.Context, id string, paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range paths {
		delete(f.files, p)
		f.removed = append(f.removed, p)
	}
	return nil
}

func (f *fakeServer) MakeDir(ctx context.Context, id, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return nil
}

func (f *fakeServer) Start(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.started++
	f.running = true
	if f.crashWith != "" {
		for p := range f.files {
			if path.Base(p) == f.crashWith {
				f.running = false
			}
		}
	}
	return nil
}

func (f *fakeServer) Stop(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.running = false
	return nil
}

func (f *fakeServer) Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error) {
	return &catalog.App{}, deploy.Request{Name: id}, nil
}

func (f *fakeServer) Reconfigure(ctx context.Context, id string, req deploy.Request, emit func(manager.Event)) (manager.Server, error) {
	return manager.Server{}, errors.New("not in this test")
}

func (f *fakeServer) StartJob(job manager.Job, emit func(manager.Event)) (func(manager.Event), func(error), error) {
	return emit, func(error) {}, nil
}

type notes struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *notes) Notify(e notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
}
func (n *notes) Clear(string) {}

var day = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

// The fake's mods: Sodium Extra needs Sodium; Broken crashes the server.
func projects() []*modrinthtest.Project {
	return []*modrinthtest.Project{
		{ID: "AANobbMI", Slug: "sodium", Title: "Sodium", Versions: []*modrinthtest.Version{
			{ID: "sod1", Number: "0.5.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "sodium-0.5.0.jar", Content: []byte("sodium 0.5.0")},
			{ID: "sod2", Number: "0.6.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day.AddDate(0, 1, 0), File: "sodium-0.6.0.jar", Content: []byte("sodium 0.6.0")},
			{ID: "sod3", Number: "0.7.0", GameVersions: []string{"1.21.5"}, Loaders: []string{"fabric"}, Published: day.AddDate(0, 2, 0), File: "sodium-0.7.0.jar", Content: []byte("sodium 0.7.0")},
			{ID: "sodb", Number: "0.6.1-beta", Type: "beta", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day.AddDate(0, 1, 5), File: "sodium-0.6.1b.jar", Content: []byte("sodium beta")},
		}},
		{ID: "PtjYWJkn", Slug: "sodium-extra", Title: "Sodium Extra", Versions: []*modrinthtest.Version{
			{ID: "sx1", Number: "1.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "sodium-extra-1.0.jar", Content: []byte("sodium extra"), Requires: []string{"AANobbMI"}},
		}},
		{ID: "BrokenID", Slug: "broken", Title: "Broken", Versions: []*modrinthtest.Version{
			{ID: "br1", Number: "1.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "broken.jar", Content: []byte("crash")},
		}},
		{ID: "BadHashID", Slug: "badhash", Title: "Bad Hash", Versions: []*modrinthtest.Version{
			{ID: "bh1", Number: "1.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "badhash.jar", Content: []byte("x"), BadHash: true},
		}},
		{ID: "EvilID", Slug: "evil", Title: "Evil", Versions: []*modrinthtest.Version{
			{ID: "ev1", Number: "1.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "../evil.jar", Content: []byte("x")},
		}},
		{ID: "ClientID", Slug: "minimap", Title: "Minimap", ServerSide: "unsupported", Versions: []*modrinthtest.Version{
			{ID: "mm1", Number: "1.0", GameVersions: []string{"1.21.4"}, Loaders: []string{"fabric"}, Published: day, File: "minimap.jar", Content: []byte("map")},
		}},
	}
}

func setup(t *testing.T, srv *fakeServer) (*Service, *notes, *modrinthtest.Server) {
	t.Helper()
	mr := modrinthtest.New(projects()...)
	t.Cleanup(mr.Close)
	client := NewModrinth("hlm-test")
	client.API, client.CDN, client.AllowHTTP = mr.URL, []string{mr.Host()}, true
	n := &notes{}
	s := New(Options{
		Servers: srv, Modrinth: client, Dir: t.TempDir(), Notifier: n,
		Ready: func(ctx context.Context, server string) (bool, bool) {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			return srv.running, true
		},
		Poll: time.Millisecond, Settle: time.Millisecond, StartTimeout: time.Second,
	})
	return s, n, mr
}

func record(t *testing.T, srv *fakeServer) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal(srv.files["/data/mods/"+RecordFile], &r); err != nil {
		t.Fatalf("record: %v", err)
	}
	return r
}

func noEvents(manager.Event) {}

func TestUnsupported(t *testing.T) {
	s, _, _ := setup(t, newFake("VERSION=1.21.4"))
	_, err := s.Overview(context.Background(), "mc")
	var u *ErrUnsupported
	if !errors.As(err, &u) || !strings.Contains(u.Why, "as VANILLA, which doesn't load mods") || !strings.Contains(u.Why, "FABRIC") {
		t.Fatalf("vanilla: %v", err)
	}
}

func TestOverview(t *testing.T) {
	srv := newFake("TYPE=fabric", "VERSION=LATEST", "MODRINTH_PROJECTS=fabric-api")
	srv.put("/data/mods/sodium-0.5.0.jar", "sodium 0.5.0") // put there by hand
	srv.put("/data/mods/mystery.jar", "who knows")
	srv.put("/data/mods/notes.txt", "not a mod")
	s, _, _ := setup(t, srv)
	s.opts.GameVersion = func(string) string { return "1.21.4" }
	ov, err := s.Overview(context.Background(), "mc")
	if err != nil {
		t.Fatal(err)
	}
	if ov.Loader != "FABRIC" || ov.Fit.GameVersion != "1.21.4" || !slices.Equal(ov.Fit.Loaders, []string{"fabric"}) || ov.Dir != "/data/mods" {
		t.Fatalf("overview: %+v", ov)
	}
	if len(ov.Warnings) != 1 || !strings.Contains(ov.Warnings[0], "MODRINTH_PROJECTS") {
		t.Errorf("warnings: %v", ov.Warnings)
	}
	if len(ov.Mods) != 2 {
		t.Fatalf("mods: %+v", ov.Mods)
	}
	byFile := map[string]Installed{}
	for _, m := range ov.Mods {
		byFile[m.File] = m
	}
	if m := byFile["sodium-0.5.0.jar"]; m.Source != "modrinth" || m.Title != "Sodium" || m.VersionNumber != "0.5.0" {
		t.Errorf("sodium: %+v", m)
	}
	if m := byFile["mystery.jar"]; m.Source != "unknown" || m.Title != "" {
		t.Errorf("mystery: %+v", m)
	}

	// Without a known game version, nothing can be added.
	s.opts.GameVersion = func(string) string { return "" }
	ov, _ = s.Overview(context.Background(), "mc")
	if !strings.Contains(ov.FitError, "VERSION variable") {
		t.Errorf("fit error: %q", ov.FitError)
	}
	if _, err := s.Plan(context.Background(), "mc", []Change{{Op: OpAdd, Project: "sodium"}}); err == nil {
		t.Error("planned without a game version")
	}
}

func TestPlan(t *testing.T) {
	srv := newFake("TYPE=QUILT", "VERSION=1.21.4")
	s, _, _ := setup(t, srv)
	ctx := context.Background()

	// A dependency comes along, with the newest release (not the beta).
	p, err := s.Plan(ctx, "mc", []Change{{Op: OpAdd, Project: "sodium-extra"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 2 || p.Steps[0].New.File != "sodium-extra-1.0.jar" || p.Steps[1].New.File != "sodium-0.6.0.jar" ||
		p.Steps[1].Why != "needed by Sodium Extra" {
		t.Fatalf("steps: %+v", p.Steps)
	}
	if want := []Change{{Op: OpAdd, Project: "PtjYWJkn", Version: "sx1"}, {Op: OpAdd, Project: "AANobbMI", Version: "sod2", Why: "needed by Sodium Extra"}}; !slices.Equal(p.Changes, want) {
		t.Errorf("changes: %+v", p.Changes)
	}
	// Planning the changes again gives the same plan.
	p2, err := s.Plan(ctx, "mc", p.Changes)
	if err != nil || len(p2.Steps) != 2 || !slices.Equal(p2.Changes, p.Changes) {
		t.Errorf("again: %+v, %v", p2, err)
	}

	// Already installed: the dependency isn't added again.
	srv.put("/data/mods/sodium-0.5.0.jar", "sodium 0.5.0")
	p, err = s.Plan(ctx, "mc", []Change{{Op: OpAdd, Project: "sodium-extra"}})
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("installed dependency: %+v, %v", p, err)
	}

	for _, tt := range []struct {
		changes []Change
		want    string
	}{
		{[]Change{{Op: OpAdd, Project: "sodium"}}, "already installed"},
		{[]Change{{Op: OpAdd, Project: "evil"}}, "isn't a file name"},
		{[]Change{{Op: OpAdd, Project: "minimap"}}, "only runs in the game itself"},
		{[]Change{{Op: OpAdd, Project: "sodium-extra", Version: "sod1"}}, "isn't a version of"},
		{[]Change{{Op: OpUpdate, File: "sodium-0.5.0.jar", Version: "sod3"}}, "doesn't run on this server"},
		{[]Change{{Op: OpRemove, File: "nope.jar"}}, "isn't in the folder"},
		{[]Change{{Op: OpRemove, File: "sodium-0.5.0.jar"}, {Op: OpDisable, File: "sodium-0.5.0.jar"}}, "changed twice"},
		{[]Change{{Op: OpEnable, File: "sodium-0.5.0.jar"}}, "already on"},
		{[]Change{{Op: "chmod"}}, "unknown change"},
		{nil, "nothing to change"},
	} {
		if _, err := s.Plan(ctx, "mc", tt.changes); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v: %v, want %q", tt.changes, err, tt.want)
		}
	}
}

func TestApplyAddUpdateRemove(t *testing.T) {
	srv := newFake("TYPE=FABRIC", "VERSION=1.21.4")
	s, n, _ := setup(t, srv)
	ctx := context.Background()
	var stages []string
	emit := func(e manager.Event) { stages = append(stages, e.Stage) }

	// No mods folder yet: it's made.
	res, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpAdd, Project: "sodium-extra"}}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if res.RolledBack || res.Warning != "" {
		t.Errorf("result: %+v", res)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{RecordFile, "sodium-0.6.0.jar", "sodium-extra-1.0.jar"}) {
		t.Fatalf("folder: %v", got)
	}
	if string(srv.files["/data/mods/sodium-0.6.0.jar"]) != "sodium 0.6.0" {
		t.Error("content")
	}
	r := record(t, srv)
	if e := r.Mods["sodium-0.6.0.jar"]; e.Project != "AANobbMI" || e.Version != "sod2" || e.By != "dan" || e.SHA512 != modrinthtest.Hash([]byte("sodium 0.6.0")) {
		t.Errorf("record: %+v", r)
	}
	if srv.stops != 1 || srv.starts != 1 || !srv.running {
		t.Errorf("stops %d, starts %d", srv.stops, srv.starts)
	}
	if !slices.Equal(stages, []string{"folder", "snapshot", "download", "download", "stop", "apply", "start", "wait"}) {
		t.Errorf("stages: %v", stages)
	}

	ov, err := s.Overview(ctx, "mc")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ov.Mods {
		if m.Source != "hlm" || m.By != "dan" || m.Title == "" {
			t.Errorf("installed: %+v", m)
		}
	}

	// Turn one off, then on: the file is renamed, keeping its content.
	if _, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpDisable, File: "sodium-extra-1.0.jar"}}, noEvents); err != nil {
		t.Fatal(err)
	}
	if got := srv.names("/data/mods"); !slices.Contains(got, "sodium-extra-1.0.jar.disabled") || slices.Contains(got, "sodium-extra-1.0.jar") {
		t.Fatalf("disabled: %v", got)
	}
	if string(srv.files["/data/mods/sodium-extra-1.0.jar.disabled"]) != "sodium extra" {
		t.Error("disabled content")
	}
	ov, _ = s.Overview(ctx, "mc")
	if i := slices.IndexFunc(ov.Mods, func(m Installed) bool { return m.Disabled }); i < 0 || ov.Mods[i].Source != "hlm" {
		t.Errorf("disabled mod: %+v", ov.Mods)
	}
	if _, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpEnable, File: "sodium-extra-1.0.jar.disabled"}}, noEvents); err != nil {
		t.Fatal(err)
	}

	// Remove one.
	if _, err := s.Apply(ctx, "mc", "sam", []Change{{Op: OpRemove, File: "sodium-extra-1.0.jar"}}, noEvents); err != nil {
		t.Fatal(err)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{RecordFile, "sodium-0.6.0.jar"}) {
		t.Fatalf("after removing: %v", got)
	}
	if _, ok := record(t, srv).Mods["sodium-extra-1.0.jar"]; ok {
		t.Error("removed mod still recorded")
	}
	if len(n.events) != 0 {
		t.Errorf("notified: %+v", n.events)
	}
}

func TestUpdates(t *testing.T) {
	srv := newFake("TYPE=FABRIC", "VERSION=1.21.4")
	srv.put("/data/mods/sodium-0.5.0.jar", "sodium 0.5.0")
	s, n, _ := setup(t, srv)
	ctx := context.Background()
	ov, err := s.CheckUpdates(ctx, "mc")
	if err != nil {
		t.Fatal(err)
	}
	if u := ov.Mods[0].Update; u == nil || u.VersionNumber != "0.6.0" || u.Blocked != "" {
		t.Fatalf("update: %+v", ov.Mods[0])
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindModUpdates || !strings.Contains(n.events[0].Message, "Sodium 0.6.0") {
		t.Errorf("notified: %+v", n.events)
	}
	// Checking again doesn't tell again.
	if _, err := s.CheckUpdates(ctx, "mc"); err != nil || len(n.events) != 1 {
		t.Errorf("again: %v, %d", err, len(n.events))
	}

	// Updating replaces the file.
	if _, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpUpdate, File: "sodium-0.5.0.jar"}}, noEvents); err != nil {
		t.Fatal(err)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{RecordFile, "sodium-0.6.0.jar"}) {
		t.Fatalf("after updating: %v", got)
	}

	// The next version needs a newer Minecraft.
	ov, err = s.CheckUpdates(ctx, "mc")
	if err != nil {
		t.Fatal(err)
	}
	if u := ov.Mods[0].Update; u == nil || u.VersionNumber != "0.7.0" || u.Blocked != "needs Minecraft (Java) 1.21.5" {
		t.Fatalf("blocked: %+v", ov.Mods[0].Update)
	}
	if _, err := s.Plan(ctx, "mc", []Change{{Op: OpUpdate, File: "sodium-0.6.0.jar"}}); err == nil || !strings.Contains(err.Error(), "already at 0.6.0") {
		t.Errorf("update to blocked: %v", err)
	}
}

func TestRollback(t *testing.T) {
	srv := newFake("TYPE=FABRIC", "VERSION=1.21.4")
	srv.put("/data/mods/sodium-0.5.0.jar", "sodium 0.5.0")
	srv.put("/data/mods/other.jar", "other")
	srv.crashWith = "broken.jar"
	s, n, _ := setup(t, srv)
	ctx := context.Background()
	var stages []string
	res, err := s.Apply(ctx, "mc", "dan", []Change{
		{Op: OpAdd, Project: "broken"},
		{Op: OpUpdate, File: "sodium-0.5.0.jar"},
		{Op: OpRemove, File: "other.jar"},
	}, func(e manager.Event) { stages = append(stages, e.Stage) })
	if err == nil || !res.RolledBack || !strings.Contains(err.Error(), "put the mods back as they were") || !strings.Contains(err.Error(), "it stopped") {
		t.Fatalf("apply: %+v, %v", res, err)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{"other.jar", "sodium-0.5.0.jar"}) {
		t.Fatalf("folder after rollback: %v", got)
	}
	if string(srv.files["/data/mods/other.jar"]) != "other" {
		t.Error("other.jar's content")
	}
	if !srv.running || srv.starts != 2 {
		t.Errorf("running %v, starts %d", srv.running, srv.starts)
	}
	if !slices.Contains(stages, "rollback") {
		t.Errorf("stages: %v", stages)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindModsFailed || !strings.Contains(n.events[0].Fields["Change"], "add Broken 1.0") {
		t.Errorf("notified: %+v", n.events)
	}

	// A server that never answers is rolled back too.
	srv.crashWith = ""
	s.opts.Ready = func(context.Context, string) (bool, bool) { return false, true }
	_, err = s.Apply(ctx, "mc", "dan", []Change{{Op: OpAdd, Project: "sodium-extra"}}, noEvents)
	if err == nil || !strings.Contains(err.Error(), "didn't answer its status query within 1s") ||
		!strings.Contains(err.Error(), "doesn't start with them either") {
		t.Fatalf("never answers: %v", err)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{"other.jar", "sodium-0.5.0.jar"}) {
		t.Fatalf("folder after second rollback: %v", got)
	}
}

func TestApplyStopsEarly(t *testing.T) {
	srv := newFake("TYPE=FABRIC", "VERSION=1.21.4")
	srv.put("/data/mods/other.jar", "other")
	s, _, _ := setup(t, srv)
	ctx := context.Background()

	// A file that doesn't match its hash is never installed, and the
	// server isn't touched.
	_, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpAdd, Project: "badhash"}}, noEvents)
	if err == nil || !strings.Contains(err.Error(), "doesn't match its SHA-512") {
		t.Fatalf("bad hash: %v", err)
	}
	if srv.stops != 0 || !slices.Equal(srv.names("/data/mods"), []string{"other.jar"}) {
		t.Errorf("stops %d, folder %v", srv.stops, srv.names("/data/mods"))
	}

	// Busy: nothing happens.
	s.opts.Busy = func(string) string { return "a backup is running" }
	if _, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpRemove, File: "other.jar"}}, noEvents); err == nil || err.Error() != "a backup is running" {
		t.Errorf("busy: %v", err)
	}
	s.opts.Busy = func(string) string { return "" }

	// A stopped server is changed and left stopped.
	srv.running = false
	res, err := s.Apply(ctx, "mc", "dan", []Change{{Op: OpRemove, File: "other.jar"}}, noEvents)
	if err != nil || !strings.Contains(res.Warning, "next starts") {
		t.Fatalf("stopped: %+v, %v", res, err)
	}
	if srv.running || srv.starts != 0 {
		t.Error("started a stopped server")
	}
}

func TestPlugins(t *testing.T) {
	srv := newFake("TYPE=PAPER", "VERSION=1.21.4")
	s, _, _ := setup(t, srv)
	ov, err := s.Overview(context.Background(), "mc")
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Plugins || ov.Dir != "/data/plugins" || !slices.Equal(ov.Fit.Loaders, []string{"paper", "spigot", "bukkit"}) {
		t.Errorf("paper: %+v", ov)
	}
}

func TestBest(t *testing.T) {
	f := []VFile{{Filename: "a.jar"}}
	vs := []Version{
		{ID: "beta", VersionType: "beta", Published: day.AddDate(0, 0, 2), Files: f},
		{ID: "old", VersionType: "release", Published: day, Files: f},
		{ID: "new", VersionType: "release", Published: day.AddDate(0, 0, 1), Files: f},
		{ID: "nofile", VersionType: "release", Published: day.AddDate(0, 0, 3)},
	}
	if v, _ := Best(vs); v.ID != "new" {
		t.Errorf("best: %s", v.ID)
	}
	if v, _ := Best(vs[:1]); v.ID != "beta" {
		t.Errorf("only a beta: %s", v.ID)
	}
}

func TestCheckURL(t *testing.T) {
	m := NewModrinth("x")
	for u, ok := range map[string]bool{
		"https://cdn.modrinth.com/data/x/a.jar": true,
		"http://cdn.modrinth.com/data/x/a.jar":  false,
		"https://evil.example/a.jar":            false,
		"https://cdn.modrinth.com.evil/a.jar":   false,
	} {
		if err := m.checkURL(u); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// A server that answers once and then crashes isn't up.
func TestAnswersThenCrashes(t *testing.T) {
	srv := newFake("TYPE=FABRIC", "VERSION=1.21.4")
	srv.put("/data/mods/other.jar", "other")
	s, _, _ := setup(t, srv)
	answered := false
	s.opts.Ready = func(context.Context, string) (bool, bool) {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if !answered && srv.running {
			answered = true
			srv.running = false // right after answering
			return true, true
		}
		return srv.running, true
	}
	res, err := s.Apply(context.Background(), "mc", "dan", []Change{{Op: OpRemove, File: "other.jar"}}, noEvents)
	if err == nil || !res.RolledBack || !strings.Contains(err.Error(), "it answered, then stopped") {
		t.Fatalf("apply: %+v, %v", res, err)
	}
	if got := srv.names("/data/mods"); !slices.Equal(got, []string{"other.jar"}) {
		t.Errorf("folder: %v", got)
	}
}
