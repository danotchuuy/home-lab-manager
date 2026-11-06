package restoretest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fake is the manager and backups: a copy that runs as long as exitAfter
// allows, and answers once it has the file "world".
type fake struct {
	mu        sync.Mutex
	host      string
	copies    int // made
	removed   int
	running   bool
	restored  map[string]bool
	exitAfter int // probes before the copy exits; 0: never
	probes    int
	answerAt  int // probes before it answers; -1: never
	backups   []string
	busy      bool
	tar       []byte
	// The probe helper on another host.
	probeArgs  []string
	probeCalls int
}

func newFake(t *testing.T, withWorld bool) *fake {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	name := "data/other"
	if withWorld {
		name = "data/world"
	}
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.Close()
	return &fake{host: manager.LocalHost, restored: map[string]bool{}, answerAt: 1, backups: []string{"b/3", "b/2", "b/1"}, tar: buf.Bytes()}
}

func (f *fake) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	info.ID, info.Name = id, "/"+id
	if f.host != manager.LocalHost {
		info.ID = id + "@" + f.host
	}
	info.Config.Image = "itzg/minecraft-server"
	return info, nil
}
func (f *fake) CreateTestCopy(ctx context.Context, server string) (*manager.TestCopy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies++
	return &manager.TestCopy{Server: server, Host: f.host, Name: "hlm-restore-test-" + server, ID: "copy"}, nil
}
func (f *fake) RestoreTestCopy(ctx context.Context, tc *manager.TestCopy, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	var st files.ArchiveStats
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return st, nil
		}
		if err != nil {
			return st, err
		}
		f.mu.Lock()
		f.restored[h.Name] = true
		f.mu.Unlock()
		st.Files++
		st.Bytes += h.Size
	}
}
func (f *fake) StartTestCopy(ctx context.Context, tc *manager.TestCopy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	return nil
}
func (f *fake) InspectTestCopy(ctx context.Context, tc *manager.TestCopy) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info docker.ContainerJSON
	info.State.Running = f.running
	info.State.ExitCode = 1
	return info, nil
}
func (f *fake) TestCopyLogs(ctx context.Context, tc *manager.TestCopy, lines int) string {
	return "Loading world\nERROR: level.dat missing\npassword=hunter2\n"
}
func (f *fake) RemoveTestCopy(ctx context.Context, tc *manager.TestCopy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed++
	f.running = false
	return nil
}
func (f *fake) RemoveTestCopies(ctx context.Context, server string) error { return nil }
func (f *fake) StartTestProbe(ctx context.Context, tc *manager.TestCopy, cmd string, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeArgs = append([]string{cmd}, args...)
	return nil
}

// TestProbe finishes on its third look: it answered if the backup had the
// world, or gave up.
func (f *fake) TestProbe(ctx context.Context, tc *manager.TestCopy) (string, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCalls++
	if f.probeCalls < 3 {
		return "", 0, false, nil
	}
	if f.restored["data/world"] {
		return "some noise\n" + `{"result":{"protocol":"minecraft","version":"1.21.4","players":1,"maxPlayers":20,"hasPlayers":true}}` + "\n", 0, true, nil
	}
	return `{"error":"minecraft port 25565: nothing is listening on that port"}`, 1, true, nil
}
func (f *fake) StartJob(job manager.Job, emit func(manager.Event)) (func(manager.Event), func(error), error) {
	return emit, func(error) {}, nil
}
func (f *fake) List(ctx context.Context) ([]manager.Server, error) { return nil, nil }

func (f *fake) Overview(ctx context.Context, id string) (backup.Overview, error) {
	ov := backup.Overview{Configured: true}
	for _, k := range f.backups {
		ov.Backups = append(ov.Backups, s3.Object{Key: k})
	}
	return ov, nil
}
func (f *fake) OpenBackup(ctx context.Context, id, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.tar)), nil
}
func (f *fake) Busy(string) bool { return f.busy }

func (f *fake) probe(ctx context.Context, server string, info docker.ContainerJSON) (gamequery.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	if f.exitAfter > 0 && f.probes >= f.exitAfter {
		f.running = false
	}
	if f.answerAt >= 0 && f.probes > f.answerAt && f.restored["data/world"] {
		return gamequery.Result{Protocol: "minecraft", Version: "1.21.4", Players: 0, MaxPlayers: 20, HasPlayers: true}, nil
	}
	return gamequery.Result{}, errors.New("nothing is listening on that port")
}

type notes struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *notes) Notify(e notify.Event) { n.mu.Lock(); n.events = append(n.events, e); n.mu.Unlock() }
func (n *notes) Clear(string)          {}

func setup(t *testing.T, f *fake) (*Service, *notes) {
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	n := &notes{}
	s := New(Options{Secrets: store, Servers: f, Backups: f, Probe: f.probe, Notifier: n,
		Poll: time.Millisecond, Unqueryable: 20 * time.Millisecond})
	return s, n
}

func TestPasses(t *testing.T) {
	f := newFake(t, true)
	s, n := setup(t, f)
	var stages []string
	res, err := s.Test(context.Background(), "mc", "", "manual", func(e manager.Event) { stages = append(stages, e.Stage) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Passed || res.Key != "b/3" || res.Answer != "1.21.4, 0/20 players" || res.Files != 1 {
		t.Fatalf("result: %+v", res)
	}
	if f.copies != 1 || f.removed != 1 {
		t.Errorf("copies %d, removed %d", f.copies, f.removed)
	}
	if strings.Join(stages, ",") != "create,restore,start,wait,done,cleanup" {
		t.Errorf("stages: %v", stages)
	}
	ov, _ := s.Get("mc")
	if len(ov.Results) != 1 || ov.Results[0].Trigger != "manual" {
		t.Errorf("recorded: %+v", ov.Results)
	}
	if len(n.events) != 0 {
		t.Errorf("notified: %+v", n.events)
	}
}

func TestFails(t *testing.T) {
	// A backup without the world: the copy never answers, then exits.
	f := newFake(t, false)
	f.exitAfter = 3
	s, n := setup(t, f)
	res, err := s.Test(context.Background(), "mc", "b/2", "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Failed || !strings.Contains(res.Error, "the copy stopped (exit code 1)") || res.Key != "b/2" {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(res.Log, "level.dat missing") || strings.Contains(res.Log, "hunter2") {
		t.Errorf("log: %q", res.Log)
	}
	if f.removed != 1 {
		t.Errorf("removed %d", f.removed)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindRestoreTestFailed || n.events[0].Fields["Backup"] != "b/2" {
		t.Errorf("notified: %+v", n.events)
	}

	// Never answers, keeps running: the time limit.
	f = newFake(t, true)
	f.answerAt = -1
	s, _ = setup(t, f)
	s.SetSettings("mc", Settings{Every: EveryWeek, TimeoutMinutes: 1})
	s.opts.Poll = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	set, _ := s.Get("mc")
	res = s.run(ctx, "mc", "b/3", 50*time.Millisecond, func(manager.Event) {})
	if res.Outcome != Failed || !strings.Contains(res.Error, "didn't answer its status query within 50ms") {
		t.Errorf("timeout: %+v (settings %+v)", res, set)
	}
	if f.removed != 1 {
		t.Errorf("removed %d", f.removed)
	}
}

func TestQueryOffAndSkips(t *testing.T) {
	f := newFake(t, true)
	f.answerAt = -1
	s, _ := setup(t, f)
	s.opts.QueryOff = func(string) bool { return true }
	res, err := s.Test(context.Background(), "mc", "", "manual", nil)
	if err != nil || res.Outcome != Passed || !strings.Contains(res.Note, "kept running") {
		t.Fatalf("query off: %+v, %v", res, err)
	}

	// Not enough memory: skipped, no copy made.
	f = newFake(t, true)
	s, n := setup(t, f)
	s.opts.Memory = func(context.Context, string) (uint64, uint64, bool) { return 512 << 20, 2048 << 20, true }
	res, err = s.Test(context.Background(), "mc", "", "schedule", nil)
	if err != nil || res.Outcome != Skipped || !strings.Contains(res.Note, "512 MiB free") || f.copies != 0 || len(n.events) != 0 {
		t.Fatalf("memory: %+v, %v, copies %d", res, err, f.copies)
	}

	// Can't run at all.
	f.busy = true
	if _, err := s.Test(context.Background(), "mc", "", "manual", nil); err == nil || !strings.Contains(err.Error(), "backup or restore") {
		t.Errorf("busy: %v", err)
	}
	f.busy, f.backups = false, nil
	if _, err := s.Test(context.Background(), "mc", "", "manual", nil); err == nil || !strings.Contains(err.Error(), "no backups") {
		t.Errorf("no backups: %v", err)
	}
}

// On another host, a probe helper next to the copy asks its game.
func TestRemote(t *testing.T) {
	f := newFake(t, true)
	f.host = "nas"
	s, _ := setup(t, f)
	s.opts.Methods = func(context.Context, string, docker.ContainerJSON) ([]gamequery.Settings, error) {
		return []gamequery.Settings{{Protocol: "minecraft", Port: 25565}}, nil
	}
	s.opts.Probe = func(context.Context, string, docker.ContainerJSON) (gamequery.Result, error) {
		t.Error("asked from hlm, not the probe")
		return gamequery.Result{}, errors.New("no")
	}
	res, err := s.Test(context.Background(), "mc", "", "manual", nil)
	if err != nil || res.Outcome != Passed || res.Answer != "1.21.4, 1/20 players" {
		t.Fatalf("remote: %+v, %v", res, err)
	}
	if strings.Join(f.probeArgs, " ") != "probe-game --host hlm-restore-test-mc --methods minecraft:25565 --timeout 10m0s --interval 1s" {
		t.Errorf("probe args: %v", f.probeArgs)
	}
	if f.removed != 1 {
		t.Errorf("removed %d", f.removed)
	}

	// The probe gives up: failed at once, with its last error.
	f = newFake(t, false)
	f.host = "nas"
	s, n := setup(t, f)
	s.opts.Methods = func(context.Context, string, docker.ContainerJSON) ([]gamequery.Settings, error) {
		return []gamequery.Settings{{Protocol: "minecraft", Port: 25565}}, nil
	}
	res, err = s.Test(context.Background(), "mc", "", "manual", nil)
	if err != nil || res.Outcome != Failed || !strings.Contains(res.Error, "nothing is listening") || len(n.events) != 1 {
		t.Fatalf("gave up: %+v, %v", res, err)
	}
}

func TestSettings(t *testing.T) {
	s, _ := setup(t, newFake(t, true))
	for _, bad := range []Settings{{Every: "daily"}, {Every: EveryBackups}, {Every: EveryBackups, Backups: 101}, {Every: EveryWeek, TimeoutMinutes: 200}} {
		if _, err := s.SetSettings("mc", bad); err == nil {
			t.Errorf("saved %+v", bad)
		}
	}
	if got, err := s.SetSettings("mc", Settings{Every: EveryWeek, Backups: 3}); err != nil || got.Backups != 0 {
		t.Errorf("week: %+v, %v", got, err)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	backups := []string{"b/5", "b/4", "b/3", "b/2"}
	at := func(key, outcome string, ago time.Duration) Result {
		return Result{Key: key, Outcome: outcome, Finished: now.Add(-ago)}
	}
	week := Settings{Every: EveryWeek}
	every2 := Settings{Every: EveryBackups, Backups: 2}
	for _, tt := range []struct {
		name    string
		set     Settings
		results []Result
		backups []string
		want    bool
	}{
		{"never", Settings{}, nil, backups, false},
		{"no backups", week, nil, nil, false},
		{"first", week, nil, backups, true},
		{"newest tested", week, []Result{at("b/5", Passed, 30*24*time.Hour)}, backups, false},
		{"week not up", week, []Result{at("b/4", Passed, 6*24*time.Hour)}, backups, false},
		{"week up", week, []Result{at("b/4", Failed, 8*24*time.Hour)}, backups, true},
		{"one newer of 2", every2, []Result{at("b/4", Passed, time.Hour)}, backups, false},
		{"two newer of 2", every2, []Result{at("b/3", Passed, time.Hour)}, backups, true},
		{"tested one gone", every2, []Result{at("b/0", Passed, time.Hour)}, backups, true},
		{"skipped recently", week, []Result{at("b/5", Skipped, time.Hour)}, backups, false},
		{"skipped yesterday", week, []Result{at("b/5", Skipped, 25*time.Hour), at("b/2", Passed, 9*24*time.Hour)}, backups, true},
	} {
		if got := due(tt.set, tt.results, tt.backups, now); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}
