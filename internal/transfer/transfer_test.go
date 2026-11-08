package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/autoupdate"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/configfiles"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
)

func valheimTemplate() *catalog.App {
	return &catalog.App{ID: "valheim", Name: "Valheim", Repository: "lloesche/valheim-server", Network: "bridge",
		Config: []catalog.Config{
			{ID: "port", Name: "Game ports", Type: catalog.TypePort, Target: "2456-2458", Mode: "udp", Value: "2456-2458"},
			{ID: "web", Name: "Status page", Type: catalog.TypePort, Target: "80", Mode: "tcp", Value: "8080"},
			{ID: "config", Name: "Config", Type: catalog.TypePath, Target: "/config", Value: "/mnt/user/appdata/valheim/config"},
			{ID: "worlds", Name: "Worlds", Type: catalog.TypePath, Target: "/opt/valheim", Value: "/mnt/user/appdata/valheim/server"},
			{ID: "pw", Name: "Server password", Type: catalog.TypeVariable, Target: "SERVER_PASS"},
			{ID: "sname", Name: "Server name", Type: catalog.TypeVariable, Target: "SERVER_NAME", Value: "My server"},
		}}
}

// fakeManager is the server manager, with servers by name.
type fakeManager struct {
	mu       sync.Mutex
	root     string
	cat      *fakeCatalog
	servers  map[string]docker.ContainerJSON
	reqs     map[string]deploy.Request
	used     map[string]bool
	nets     []manager.NetworkInfo
	netsOn   map[string][]manager.NetworkInfo // by host, when set
	calls    []string
	copyFail error
}

func (m *fakeManager) call(c string) { m.mu.Lock(); m.calls = append(m.calls, c); m.mu.Unlock() }

func (m *fakeManager) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	if c, ok := m.servers[id]; ok {
		return c, nil
	}
	return docker.ContainerJSON{}, manager.ErrNotFound
}
func (m *fakeManager) List(context.Context) ([]manager.Server, error) {
	var out []manager.Server
	for name, c := range m.servers {
		out = append(out, manager.Server{ID: name, Name: name, State: c.State.Status})
	}
	return out, nil
}
func (m *fakeManager) Config(_ context.Context, id string) (*catalog.App, deploy.Request, error) {
	app, _ := m.AppOn(m.reqs[id].TemplateID, manager.HostOf(id))
	return app, m.reqs[id], nil
}
func (m *fakeManager) AppOn(id, host string) (*catalog.App, bool) {
	a, ok := m.cat.Get(id)
	if !ok {
		return nil, false
	}
	return a.Localize(m.AppdataRootOf(host)), true
}

// The other host, "nas", keeps data in /nas/appdata.
func (m *fakeManager) AppdataRootOf(host string) string {
	if host == "nas" {
		return "/nas/appdata"
	}
	return m.root
}
func (m *fakeManager) KnownHost(host string) bool { return host == manager.LocalHost || host == "nas" }
func (m *fakeManager) add(req deploy.Request) (manager.Server, error) {
	if _, ok := m.servers[req.Name]; ok {
		return manager.Server{}, errors.New("exists")
	}
	var c docker.ContainerJSON
	c.ID, c.Name = "id-"+req.Name, "/"+req.Name
	m.servers[req.Name], m.reqs[req.Name] = c, req
	return manager.Server{ID: c.ID, Name: req.Name}, nil
}
func (m *fakeManager) Install(_ context.Context, req deploy.Request, emit func(manager.Event)) (manager.Server, error) {
	m.call("install " + req.Name)
	return m.add(req)
}
func (m *fakeManager) Create(_ context.Context, req deploy.Request, emit func(manager.Event)) (manager.Server, error) {
	m.call("create " + req.Name)
	return m.add(req)
}
func (m *fakeManager) Start(_ context.Context, id string) error { m.call("start " + id); return nil }
func (m *fakeManager) Stop(_ context.Context, id string) error  { m.call("stop " + id); return nil }
func (m *fakeManager) Remove(_ context.Context, id string) error {
	m.call("remove " + id)
	delete(m.servers, strings.TrimPrefix(id, "id-"))
	return nil
}
func (m *fakeManager) CopyData(_ context.Context, from, to string, folders []string, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	m.call("copy " + from + " " + to + " " + strings.Join(folders, ","))
	return files.ArchiveStats{Files: 3, Bytes: 2048}, m.copyFail
}
func (m *fakeManager) DataSize(context.Context, string, []string) (int64, error) { return 4096, nil }
func (m *fakeManager) HostPortsInUse(_ context.Context, host string) (map[string]bool, error) {
	if host != manager.LocalHost {
		return map[string]bool{}, nil
	}
	return m.used, nil
}
func (m *fakeManager) Networks(_ context.Context, host string) ([]manager.NetworkInfo, error) {
	if nets, ok := m.netsOn[host]; ok {
		return nets, nil
	}
	return m.nets, nil
}

type fakeCatalog struct{ apps map[string]*catalog.App }

func (c *fakeCatalog) Get(id string) (*catalog.App, bool) { a, ok := c.apps[id]; return a, ok }
func (c *fakeCatalog) AddCustom(a *catalog.App) error     { c.apps[a.ID] = a; return nil }

// fakeSettings keeps every kind of per-server setting.
type fakeSettings struct {
	plans    map[string]backup.Plan
	tasks    map[string][]schedule.Task
	policies map[string]autoupdate.Policy
	queries  map[string]gamequery.Settings
	consoles map[string]console.Settings
	pins     map[string][]configfiles.File
	health   map[string]health.Settings
	restored []string
}

func newSettings() *fakeSettings {
	return &fakeSettings{plans: map[string]backup.Plan{}, tasks: map[string][]schedule.Task{}, policies: map[string]autoupdate.Policy{},
		queries: map[string]gamequery.Settings{}, consoles: map[string]console.Settings{}, pins: map[string][]configfiles.File{},
		health: map[string]health.Settings{}}
}

func (f *fakeSettings) Plans() (map[string]backup.Plan, error) { return f.plans, nil }
func (f *fakeSettings) SetPlan(_ context.Context, id string, p backup.Plan) (backup.Plan, error) {
	f.plans[id] = p
	return p, nil
}
func (f *fakeSettings) RestoreInto(_ context.Context, from, key, to string, _ func(files.ArchiveStats)) (backup.Run, error) {
	f.restored = append(f.restored, from+" "+key+" "+to)
	return backup.Run{Files: 7}, nil
}
func (f *fakeSettings) IsBackupOf(name, key string) bool { return strings.HasPrefix(key, "k") }
func (f *fakeSettings) Tasks(server string, _ bool) (schedule.ServerTasks, error) {
	return schedule.ServerTasks{Tasks: f.tasks[server]}, nil
}
func (f *fakeSettings) Save(server string, t schedule.Task) (schedule.Task, error) {
	if t.ID != "" {
		return t, errors.New("new tasks have no ID")
	}
	t.ID = "new"
	f.tasks[server] = append(f.tasks[server], t)
	return t, nil
}
func (f *fakeSettings) Status(server string) (autoupdate.ServerStatus, error) {
	return autoupdate.ServerStatus{Policy: f.policies[server]}, nil
}
func (f *fakeSettings) SetServerPolicy(server string, p autoupdate.Policy) (autoupdate.Policy, error) {
	f.policies[server] = p
	return p, nil
}

type fakeQuery struct{ f *fakeSettings }

func (q fakeQuery) Status(name string, _ bool) gamequery.Status {
	return gamequery.Status{Settings: q.f.queries[name]}
}
func (q fakeQuery) SetSettings(name string, set gamequery.Settings) (gamequery.Settings, error) {
	q.f.queries[name] = set
	return set, nil
}

type fakeConsole struct{ f *fakeSettings }

func (c fakeConsole) Settings(name string) (console.Settings, error) { return c.f.consoles[name], nil }
func (c fakeConsole) Save(_ context.Context, id string, in console.Input) (console.Status, error) {
	c.f.consoles[id] = in.Settings
	return console.Status{}, nil
}

type fakePins struct{ f *fakeSettings }

func (p fakePins) Pinned(s string) ([]configfiles.File, error) { return p.f.pins[s], nil }
func (p fakePins) SetPinned(s string, in []configfiles.File) ([]configfiles.File, error) {
	p.f.pins[s] = in
	return in, nil
}

type fakeHealth struct{ f *fakeSettings }

func (h fakeHealth) Get(s string) (health.Settings, error) { return h.f.health[s], nil }
func (h fakeHealth) Set(s string, set health.Settings) (health.Settings, error) {
	h.f.health[s] = set
	return set, nil
}

// setup is an hlm with a valheim server whose appdata is under root.
func setup(t *testing.T, root string) (*Service, *fakeManager, *fakeSettings) {
	t.Helper()
	cat := &fakeCatalog{apps: map[string]*catalog.App{"valheim": valheimTemplate()}}
	m := &fakeManager{root: root, cat: cat, servers: map[string]docker.ContainerJSON{}, reqs: map[string]deploy.Request{},
		used: map[string]bool{}}
	f := newSettings()
	s := New(Options{Manager: m, Catalog: cat, Backups: f, Tasks: f, Updates: f, Query: fakeQuery{f}, Console: fakeConsole{f},
		ConfigFiles: fakePins{f}, Health: fakeHealth{f}, Version: "v1.2.3"})
	return s, m, f
}

func addValheim(m *fakeManager, f *fakeSettings, root string) {
	req := deploy.Request{TemplateID: "valheim", Name: "valheim",
		Values: map[string]string{"pw": "hunter2", "sname": "Vikings", "worlds": root + "/valheim/server"},
		Extra: []deploy.ExtraConfig{{Type: "Path", Name: "Mods", Target: "/mods", Value: root + "/valheim/mods"},
			{Type: "Port", Name: "RCON", Target: "2459", Mode: "tcp", Value: "2459"}}}
	m.add(req)
	c := m.servers["valheim"]
	c.HostConfig.RestartPolicy.Name = "always"
	c.State.Running = true
	c.Mounts = []docker.MountPoint{
		{Type: "bind", Source: root + "/valheim/config", Destination: "/config", RW: true},
		{Type: "bind", Source: root + "/valheim/server", Destination: "/opt/valheim", RW: true},
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"},
		{Type: "volume", Name: strings.Repeat("ab", 32), Destination: "/var/lib/cache", RW: true},
	}
	m.servers["valheim"] = c
	m.used = map[string]bool{"2456/udp": true, "2457/udp": true, "2458/udp": true, "8080/tcp": true, "2461/udp": true}
	f.plans["valheim"] = backup.Plan{Folders: []string{"/config"}, IntervalHours: 24, Keep: 7, Last: &backup.Run{Key: "x"}}
	f.tasks["valheim"] = []schedule.Task{{ID: "t1", Name: "Nightly restart", Enabled: true,
		When: schedule.When{Kind: "daily", Time: "04:00"}, Steps: []schedule.Step{{Kind: "restart"}}, Runs: []schedule.Run{{Result: "ok"}}}}
	f.policies["valheim"] = autoupdate.Policy{Mode: "auto", Backup: true}
	f.queries["valheim"] = gamequery.Settings{Protocol: "a2s", Port: 2457}
	f.consoles["valheim"] = console.Settings{Mode: console.ModeRCON, Password: "rcon-secret", Port: 25575}
	f.pins["valheim"] = []configfiles.File{{Path: "/config/adminlist.txt"}}
	f.health["valheim"] = health.Settings{RestartAfter: 10}
}

func TestExportAndImport(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	ctx := context.Background()

	exp, err := s.Export(ctx, "valheim", false)
	if err != nil {
		t.Fatal(err)
	}
	// Through a file and back.
	data, _ := json.Marshal(exp)
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "rcon-secret") {
		t.Errorf("secrets in an export without them: %s", data)
	}
	var back Export
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	req := back.Request
	if req.Extra[0].Value != "${APPDATA}/valheim/mods" || req.Values["extra:0"] != "" {
		t.Errorf("extra path = %+v, values %v", req.Extra, req.Values)
	}
	if req.Values["worlds"] != "${APPDATA}/valheim/server" || req.Values["config"] != "${APPDATA}/valheim/config" ||
		req.RestartPolicy != "always" || req.Values["sname"] != "Vikings" || !slices.Equal(back.Omitted, []string{"pw"}) {
		t.Errorf("exported request = %+v, omitted %v", req, back.Omitted)
	}
	set := back.Settings
	if set.BackupPlan == nil || set.BackupPlan.Keep != 7 || len(set.Tasks) != 1 || set.Tasks[0].ID != "" || set.Tasks[0].Runs != nil ||
		set.UpdatePolicy == nil || set.Query == nil || set.Console == nil || set.Console.Password != "" || len(set.ConfigFiles) != 1 ||
		set.Health == nil || back.Template == nil || back.HLMVersion != "v1.2.3" {
		t.Errorf("exported settings = %+v", set)
	}

	// Into another hlm, with another appdata root and without the template.
	s2, m2, f2 := setup(t, "/mnt/games")
	delete(m2.cat.apps, "valheim")
	p, err := s2.PreviewImport(ctx, back, manager.LocalHost)
	if err != nil {
		t.Fatal(err)
	}
	if !p.TemplateMissing || p.Request.Values["worlds"] != "/mnt/games/valheim/server" || p.Request.Extra[0].Value != "/mnt/games/valheim/mods" || p.App.Config[2].Value != "/mnt/games/valheim/config" ||
		!slices.Equal(p.Omitted, []string{"Server password"}) || len(p.Settings) != 7 {
		t.Errorf("preview = %+v", p)
	}
	warnings := strings.Join(p.Warnings, "\n")
	if !strings.Contains(warnings, "Server password") || !strings.Contains(warnings, "console's password") || !strings.Contains(warnings, "custom template") {
		t.Errorf("warnings = %s", warnings)
	}
	r := p.Request
	r.Values["pw"] = "new-pass"
	var events []string
	srv, err := s2.Import(ctx, back, r, func(e manager.Event) { events = append(events, e.Stage+": "+e.Message) })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.cat.apps["valheim"]; !ok || srv.Name != "valheim" || m2.reqs["valheim"].Values["pw"] != "new-pass" {
		t.Errorf("imported %+v; template added: %v", m2.reqs["valheim"], ok)
	}
	if len(f2.tasks["valheim"]) != 1 || f2.plans["valheim"].Keep != 7 || f2.policies["valheim"].Mode != "auto" ||
		f2.consoles["valheim"].Mode != console.ModeRCON || f2.health["valheim"].RestartAfter != 10 || len(f2.pins["valheim"]) != 1 {
		t.Errorf("settings not applied: %+v", f2)
	}
	if !strings.Contains(strings.Join(events, "\n"), "Copied its backup plan, 1 scheduled task") {
		t.Errorf("events = %q", events)
	}

	// On the same hlm, the folders are the original's.
	p, _ = s.PreviewImport(ctx, back, manager.LocalHost)
	if w := strings.Join(p.Warnings, "\n"); !strings.Contains(w, "/srv/appdata/valheim/server) is already valheim's") ||
		!strings.Contains(w, "named valheim already exists") {
		t.Errorf("same-host warnings = %s", w)
	}

	// With secrets, they're there.
	exp, _ = s.Export(ctx, "valheim", true)
	if exp.Request.Values["pw"] != "hunter2" || exp.Settings.Console.Password != "rcon-secret" || len(exp.Omitted) != 0 {
		t.Errorf("export with secrets = %+v", exp)
	}

	// Files hlm can't read.
	for _, bad := range []Export{{}, {HLMExport: Version + 1, Request: back.Request}, {HLMExport: 1}} {
		if _, err := s2.PreviewImport(ctx, bad, manager.LocalHost); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestClonePath(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"/srv/appdata/valheim/config", "/srv/appdata/valheim-test/config"},
		{"/srv/appdata/valheim-server/data", "/srv/appdata/valheim-test-server/data"},
		{"/srv/appdata/shared-saves", "/srv/appdata/valheim-test/shared-saves"},
		{"/srv/appdata", "/srv/appdata/valheim-test"},
		{"/mnt/games/valheim/worlds", "/mnt/games/valheim-test/worlds"},
	} {
		if got, ok := clonePath(tt.in, "/srv/appdata", "valheim", "valheim-test"); !ok || got != tt.want {
			t.Errorf("clonePath(%s) = %s, %v; want %s", tt.in, got, ok, tt.want)
		}
	}
	if got, ok := clonePath("/mnt/games/worlds", "/srv/appdata", "valheim", "valheim-test"); ok || got != "/mnt/games/worlds" {
		t.Errorf("unrelated path moved: %s", got)
	}
}

func TestPreviewClone(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	r := m.reqs["valheim"]
	r.Network, r.IPAddress = "br0", "192.168.1.50"
	m.reqs["valheim"] = r
	ctx := context.Background()

	if _, err := s.PreviewClone(ctx, "valheim", "valheim"); err == nil {
		t.Error("cloned onto an existing name")
	}
	if _, err := s.PreviewClone(ctx, "valheim", "-bad name"); err == nil {
		t.Error("accepted a bad name")
	}
	p, err := s.PreviewClone(ctx, "valheim", "valheim-test")
	if err != nil {
		t.Fatal(err)
	}
	v := p.Request.Values
	// 2456-2458 moved by 1 or 2 overlaps the original, and by 3, 4 or 5
	// takes in 2461: by 6.
	if v["port"] != "2462-2464" || v["web"] != "8086" {
		t.Errorf("ports = %s, %s (want the smallest step that frees all)", v["port"], v["web"])
	}
	if e := p.Request.Extra; e[0].Value != "/srv/appdata/valheim-test/mods" || e[1].Value != "2465" || v["extra:0"] != "" || v["extra:1"] != "" {
		t.Errorf("extra items = %+v, values %v", e, v)
	}
	if v["worlds"] != "/srv/appdata/valheim-test/server" || v["config"] != "/srv/appdata/valheim-test/config" ||
		p.Request.IPAddress != "" || p.Request.Name != "valheim-test" || v["pw"] != "hunter2" {
		t.Errorf("request = %+v", p.Request)
	}
	want := []Copy{
		{Target: "/config", From: "/srv/appdata/valheim/config", To: "/srv/appdata/valheim-test/config"},
		{Target: "/opt/valheim", From: "/srv/appdata/valheim/server", To: "/srv/appdata/valheim-test/server"},
		{Target: "/var/lib/cache", From: "Docker volume", To: "a new Docker volume"},
	}
	if !slices.Equal(p.Copies, want) {
		t.Errorf("copies = %+v", p.Copies)
	}
	if w := strings.Join(p.Warnings, "\n"); !strings.Contains(w, "192.168.1.50") || !strings.Contains(w, "moved up by 6") {
		t.Errorf("warnings = %s", w)
	}

	// Its own LAN address: no ports to move.
	m.nets = []manager.NetworkInfo{{Name: "br0", DirectIP: true}}
	p, _ = s.PreviewClone(ctx, "valheim", "valheim-test")
	if p.Request.Values["port"] != "" {
		t.Errorf("ports moved on a macvlan network: %v", p.Request.Values["port"])
	}
}

// A clone on another host keeps its name and ports, takes that host's data
// folder, and copies every folder: the same path there is another folder.
func TestCloneToOtherHost(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	ctx := context.Background()

	if _, err := s.PreviewClone(ctx, "valheim", "valheim@nowhere"); err == nil {
		t.Error("cloned onto a host that doesn't exist")
	}
	p, err := s.PreviewClone(ctx, "valheim", CloneName("valheim", "nas", "valheim"))
	if err != nil {
		t.Fatal(err)
	}
	v := p.Request.Values
	if p.Request.Name != "valheim@nas" || v["port"] != "" || v["worlds"] != "/nas/appdata/valheim/server" ||
		v["config"] != "/nas/appdata/valheim/config" || p.Request.Extra[0].Value != "/nas/appdata/valheim/mods" {
		t.Errorf("request = %+v", p.Request)
	}
	want := []Copy{
		{Target: "/config", From: "/srv/appdata/valheim/config", To: "/nas/appdata/valheim/config"},
		{Target: "/opt/valheim", From: "/srv/appdata/valheim/server", To: "/nas/appdata/valheim/server"},
		{Target: "/var/lib/cache", From: "Docker volume", To: "a new Docker volume"},
	}
	if !slices.Equal(p.Copies, want) {
		t.Errorf("copies = %+v", p.Copies)
	}

	srv, err := s.Clone(ctx, "valheim", CloneOptions{Name: "valheim", Host: "nas", CopyData: CopyNow}, func(manager.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Name != "valheim@nas" || f.consoles["valheim@nas"].Password != "rcon-secret" ||
		!slices.Contains(m.calls, "copy id-valheim id-valheim@nas /config,/opt/valheim,/var/lib/cache") {
		t.Errorf("server %+v, calls %q", srv, m.calls)
	}

	// Exported from the other host, it's named without the host and its
	// paths are relative to that host's data folder.
	e, err := s.Export(ctx, "valheim@nas", false)
	if err != nil {
		t.Fatal(err)
	}
	if e.Server != "valheim" || e.Request.Name != "valheim" || e.Request.Values["worlds"] != AppdataVar+"/valheim/server" {
		t.Errorf("export = %s %+v", e.Server, e.Request)
	}
}

func TestClone(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	ctx := context.Background()

	if _, err := s.Clone(ctx, "valheim", CloneOptions{Name: "test", CopyData: CopyBackup}, func(manager.Event) {}); err == nil {
		t.Error("cloned from a backup without choosing one")
	}

	srv, err := s.Clone(ctx, "valheim", CloneOptions{Name: "valheim-test", CopyData: CopyNow, StopSource: true, Start: true}, func(manager.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create valheim-test", "stop id-valheim", "copy id-valheim id-valheim-test /config,/opt/valheim,/var/lib/cache",
		"start id-valheim", "start id-valheim-test"}
	if !slices.Equal(m.calls, want) || srv.Name != "valheim-test" {
		t.Errorf("calls = %q", m.calls)
	}
	if f.consoles["valheim-test"].Password != "rcon-secret" || len(f.tasks["valheim-test"]) != 1 {
		t.Errorf("settings not cloned: %+v", f.consoles["valheim-test"])
	}

	// Not one of its backups: nothing is made.
	m.calls = nil
	if _, err := s.Clone(ctx, "valheim", CloneOptions{Name: "valheim-x", CopyData: CopyBackup, BackupKey: "other/x"}, func(manager.Event) {}); err == nil || len(m.calls) != 0 {
		t.Errorf("bad key: %v, calls %q", err, m.calls)
	}

	// From a backup, with the form's settings.
	m.calls = nil
	r := m.reqs["valheim-test"]
	r.Values["sname"] = "Test world"
	if _, err := s.Clone(ctx, "valheim", CloneOptions{Name: "valheim-b", Request: &r, CopyData: CopyBackup, BackupKey: "k1"},
		func(manager.Event) {}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.restored, []string{"valheim k1 id-valheim-b"}) || m.reqs["valheim-b"].Values["sname"] != "Test world" ||
		m.reqs["valheim-b"].Name != "valheim-b" || slices.Contains(m.calls, "start id-valheim-b") {
		t.Errorf("restored %q, request %+v, calls %q", f.restored, m.reqs["valheim-b"], m.calls)
	}

	// A failed copy removes the half-made copy, and starts the original
	// again.
	m.calls, m.copyFail = nil, errors.New("disk full")
	if _, err := s.Clone(ctx, "valheim", CloneOptions{Name: "valheim-c", CopyData: CopyNow, StopSource: true}, func(manager.Event) {}); err == nil ||
		!strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v", err)
	}
	if !slices.Equal(m.calls[len(m.calls)-2:], []string{"start id-valheim", "remove id-valheim-c"}) {
		t.Errorf("calls = %q", m.calls)
	}
}
