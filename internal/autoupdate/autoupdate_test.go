package autoupdate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
)

type fake struct {
	mu          sync.Mutex
	servers     []manager.Server
	stillThere  bool // CheckUpdate still finds the update
	updateErr   error
	backupErr   error
	plans       map[string]bool
	jobs        map[string]bool
	busy        string // manager.Busy
	did         []string
	slept       time.Duration
	self        selfupdate.Status
	selfErr     error
	notified    []notify.Event
	taskRunning map[string]bool
}

func (f *fake) log(s string) {
	f.mu.Lock()
	f.did = append(f.did, s)
	f.mu.Unlock()
}
func (f *fake) List(context.Context) ([]manager.Server, error) { return f.servers, nil }
func (f *fake) CheckUpdate(_ context.Context, id string) (manager.UpdateStatus, error) {
	f.log("check " + id)
	if f.stillThere {
		return manager.UpdateStatus{State: manager.UpdateAvailable}, nil
	}
	return manager.UpdateStatus{State: manager.UpdateCurrent}, nil
}
func (f *fake) Update(_ context.Context, id string, _ func(manager.Event)) (manager.Server, error) {
	f.log("update " + id)
	return manager.Server{}, f.updateErr
}
func (f *fake) JobRunning(name string) bool { return f.jobs[name] }
func (f *fake) Busy() string                { return f.busy }

// backups is the fake's backup side (its Busy differs from the manager's).
type backups struct{ *fake }

func (b backups) Busy(string) bool       { return false }
func (f *fake) HasPlan(name string) bool { return f.plans[name] }
func (f *fake) BackupNow(_ context.Context, id, trigger string) (backup.Run, error) {
	f.log("backup " + id + " " + trigger)
	return backup.Run{Key: "k"}, f.backupErr
}
func (f *fake) Send(_ context.Context, id, who, cmd string) (console.Result, error) {
	f.log("command " + id + " " + who + " " + cmd)
	return console.Result{}, nil
}
func (f *fake) Running(server string) bool { return f.taskRunning[server] }
func (f *fake) Status() selfupdate.Status  { return f.self }
func (f *fake) Apply(context.Context, func(selfupdate.Event)) error {
	f.log("self-update")
	return f.selfErr
}
func (f *fake) Notify(e notify.Event) { f.notified = append(f.notified, e) }
func (f *fake) Clear(string)          {}

func server(name string, update bool) manager.Server {
	s := manager.Server{Name: name, State: "running"}
	if update {
		s.Update = &manager.UpdateStatus{State: manager.UpdateAvailable}
	}
	return s
}

func setup(t *testing.T) (*Service, *fake, *time.Time) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{stillThere: true, plans: map[string]bool{}, jobs: map[string]bool{}, taskRunning: map[string]bool{}}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s := New(Options{Secrets: store, Servers: f, Backups: backups{f}, Console: f, Tasks: f, Self: f, Notifier: f,
		Location: func() *time.Location { return time.UTC },
		Now:      func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			f.slept += d
			return nil
		}})
	return s, f, &now
}

func TestManualDoesNothing(t *testing.T) {
	s, f, _ := setup(t)
	f.servers = []manager.Server{server("valheim", true)}
	s.tick(context.Background())
	if len(f.did) != 0 {
		t.Errorf("manual servers were touched: %q", f.did)
	}
	if st, _ := s.Status("valheim"); st.Effective.Mode != ModeManual || st.State != nil {
		t.Errorf("status = %+v", st)
	}
}

func TestAutoUpdate(t *testing.T) {
	s, f, now := setup(t)
	f.servers = []manager.Server{server("valheim", true), server("plex", false)}
	f.plans["valheim"] = true
	if _, err := s.SetServerPolicy("valheim", Policy{Mode: ModeAuto, WindowStart: "3:00", WindowEnd: "06:00",
		Warn: "say Updating in 5 minutes", WarnMinutes: 5, Backup: true}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 12:00: outside the window.
	s.tick(ctx)
	st, _ := s.Status("valheim")
	if len(f.did) != 0 || st.State == nil || st.State.State != StateWaiting || st.State.Next.Format("01-02 15:04") != "11-05 03:00" {
		t.Fatalf("outside the window: did %q, state %+v", f.did, st.State)
	}

	// 03:30: in it. Check, warn, wait, back up, update.
	*now = time.Date(2026, 11, 5, 3, 30, 0, 0, time.UTC)
	s.tick(ctx)
	want := []string{"check valheim", "command valheim auto-update say Updating in 5 minutes", "backup valheim update", "update valheim"}
	if !slices.Equal(f.did, want) || f.slept != 5*time.Minute {
		t.Errorf("did %q (slept %v), want %q", f.did, f.slept, want)
	}
	st, _ = s.Status("valheim")
	if st.State != nil || st.Last == nil || !st.Last.OK || len(st.Last.Steps) != 3 || st.Policy.WindowStart != "03:00" {
		t.Errorf("after updating: %+v, last %+v", st, st.Last)
	}
}

func TestBackupFailureAndRetry(t *testing.T) {
	s, f, now := setup(t)
	f.servers = []manager.Server{server("valheim", true)}
	f.plans["valheim"] = true
	f.backupErr = errors.New("bucket gone")
	s.SetServerPolicy("valheim", Policy{Mode: ModeAuto, Backup: true})
	ctx := context.Background()
	s.tick(ctx)
	if slices.Contains(f.did, "update valheim") {
		t.Fatal("updated although the backup failed")
	}
	st, _ := s.Status("valheim")
	if st.Last == nil || st.Last.OK || !strings.Contains(st.Last.Error, "bucket gone") {
		t.Errorf("last = %+v", st.Last)
	}
	// Not tried again for a day.
	f.did = nil
	*now = now.Add(time.Hour)
	s.tick(ctx)
	st, _ = s.Status("valheim")
	if len(f.did) != 0 || st.State == nil || st.State.State != StateFailed {
		t.Errorf("an hour later: did %q, state %+v", f.did, st.State)
	}
	f.backupErr = nil
	*now = now.Add(24 * time.Hour)
	s.tick(ctx)
	if !slices.Contains(f.did, "update valheim") {
		t.Errorf("a day later: did %q", f.did)
	}
	// Without a backup plan, there's nothing to back up: it just updates.
	f.did = nil
	f.plans["valheim"] = false
	s.tick(ctx)
	if slices.Contains(f.did, "backup valheim update") || !slices.Contains(f.did, "update valheim") {
		t.Errorf("without a plan: did %q", f.did)
	}
}

func TestWaitsAndQueues(t *testing.T) {
	s, f, _ := setup(t)
	f.servers = []manager.Server{server("b", true), server("a", true), server("c", true)}
	s.SetGeneral(General{Default: Policy{Mode: ModeAuto}})
	f.jobs["c"] = true
	s.tick(context.Background())
	// One at a time: a first; b queued; c busy.
	if !slices.Equal(f.did, []string{"check a", "update a"}) {
		t.Errorf("did %q", f.did)
	}
	states := s.States()
	if states["c"].State != StateBusy {
		t.Errorf("states = %+v", states)
	}
	// The update vanished (e.g. updated by hand): nothing done, no failure.
	f.did, f.stillThere = nil, false
	f.servers = []manager.Server{server("b", true)}
	s.tick(context.Background())
	st, _ := s.Status("b")
	if !slices.Equal(f.did, []string{"check b"}) || st.Last != nil {
		t.Errorf("vanished update: did %q, last %+v", f.did, st.Last)
	}
	if len(s.States()) != 0 {
		t.Errorf("states left over: %+v", s.States())
	}
	// A server can opt out of the automatic default.
	f.did, f.stillThere = nil, true
	s.SetServerPolicy("b", Policy{Mode: ModeManual})
	s.tick(context.Background())
	if len(f.did) != 0 {
		t.Errorf("opted-out server updated: %q", f.did)
	}
	// A running task makes it wait.
	s.SetServerPolicy("b", Policy{Mode: ModeDefault})
	f.taskRunning["b"] = true
	s.tick(context.Background())
	if len(f.did) != 0 || s.States()["b"].State != StateBusy {
		t.Errorf("during a task: did %q, %+v", f.did, s.States())
	}
}

func TestSelfUpdate(t *testing.T) {
	s, f, now := setup(t)
	f.self = selfupdate.Status{Current: "v1.0.0", Available: true, CanUpdate: true, Latest: &selfupdate.Release{Version: "v1.1.0"}}
	ctx := context.Background()
	s.tick(ctx)
	if len(f.did) != 0 {
		t.Fatalf("updated hlm without being asked: %q", f.did)
	}
	s.SetGeneral(General{Default: Policy{Mode: ModeManual}, Self: SelfPolicy{Auto: true, WindowStart: "02:00", WindowEnd: "04:00"}})
	s.tick(ctx) // 12:00: outside its window
	if len(f.did) != 0 {
		t.Fatalf("updated hlm outside its window: %q", f.did)
	}
	*now = time.Date(2026, 11, 5, 2, 30, 0, 0, time.UTC)
	f.busy = "wait for the update of valheim to finish first"
	s.tick(ctx)
	if len(f.did) != 0 {
		t.Fatalf("updated hlm while a server was updating: %q", f.did)
	}
	f.busy = ""
	f.selfErr = errors.New("checksum mismatch")
	s.tick(ctx)
	if !slices.Equal(f.did, []string{"self-update"}) || len(f.notified) != 1 || f.notified[0].Kind != notify.KindHLMUpdateFailed {
		t.Errorf("did %q, notified %+v", f.did, f.notified)
	}
	s.tick(ctx) // not retried straight away
	if len(f.did) != 1 {
		t.Errorf("retried hlm's update at once: %q", f.did)
	}
	// Servers' updates go first.
	*now = now.Add(25 * time.Hour)
	f.did = nil
	f.servers = []manager.Server{server("valheim", true)}
	s.SetServerPolicy("valheim", Policy{Mode: ModeAuto})
	s.tick(ctx)
	if !slices.Equal(f.did, []string{"check valheim", "update valheim"}) {
		t.Errorf("did %q", f.did)
	}
}

func TestWindows(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 4, h, m, 0, 0, time.UTC) }
	tests := []struct {
		start, end string
		now        time.Time
		in         bool
		next       string
	}{
		{"", "", at(12, 0), true, ""},
		{"03:00", "06:00", at(3, 0), true, ""},
		{"03:00", "06:00", at(6, 0), false, "11-05 03:00"},
		{"03:00", "06:00", at(2, 59), false, "11-04 03:00"},
		{"22:00", "02:00", at(23, 30), true, ""},
		{"22:00", "02:00", at(1, 59), true, ""},
		{"22:00", "02:00", at(12, 0), false, "11-04 22:00"},
	}
	for _, tt := range tests {
		in, next := open(tt.start, tt.end, tt.now, time.UTC)
		got := ""
		if !next.IsZero() {
			got = next.Format("01-02 15:04")
		}
		if in != tt.in || got != tt.next {
			t.Errorf("%s-%s at %s: in %v next %q, want %v %q", tt.start, tt.end, tt.now.Format("15:04"), in, got, tt.in, tt.next)
		}
	}
}

func TestSettings(t *testing.T) {
	s, _, _ := setup(t)
	for _, bad := range []Policy{
		{Mode: "sometimes"},
		{Mode: ModeAuto, WindowStart: "03:00"},
		{Mode: ModeAuto, WindowStart: "03:00", WindowEnd: "03:00"},
		{Mode: ModeAuto, WindowStart: "25:00", WindowEnd: "03:00"},
		{Mode: ModeAuto, Warn: "say hi", WarnMinutes: 31},
	} {
		if _, err := s.SetServerPolicy("x", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := s.SetGeneral(General{CheckHours: 200}); err == nil {
		t.Error("accepted checking every 200 hours")
	}
	if s.CheckInterval() != 12*time.Hour {
		t.Errorf("default interval = %v", s.CheckInterval())
	}
	if err := s.SetGeneral(General{CheckHours: 3}); err != nil {
		t.Fatal(err)
	}
	set, _ := s.Settings()
	if s.CheckInterval() != 3*time.Hour || set.Default.Mode != ModeManual {
		t.Errorf("interval %v, default %+v", s.CheckInterval(), set.Default)
	}
	p, _ := s.SetServerPolicy("x", Policy{Mode: ModeAuto, WarnMinutes: 9})
	if p.WarnMinutes != 0 {
		t.Errorf("warning minutes kept without a warning: %+v", p)
	}
	s.ForgetServer("x")
	if st, _ := s.Status("x"); st.Policy.Mode != ModeDefault {
		t.Errorf("policy of a forgotten server: %+v", st.Policy)
	}
}

func TestOnlyWhenEmpty(t *testing.T) {
	s, f, _ := setup(t)
	f.servers = []manager.Server{server("valheim", true)}
	players, known := 3, true
	s.opts.Players = func(string) (int, bool) { return players, known }
	s.SetServerPolicy("valheim", Policy{Mode: ModeAuto, OnlyWhenEmpty: true})
	s.tick(context.Background())
	st, _ := s.Status("valheim")
	if len(f.did) != 0 || st.State == nil || st.State.State != StatePlayers || !strings.Contains(st.State.Message, "3 players online") {
		t.Fatalf("with players on: did %q, state %+v", f.did, st.State)
	}
	players = 0
	s.tick(context.Background())
	if !slices.Contains(f.did, "update valheim") {
		t.Errorf("once empty: did %q", f.did)
	}
	// Unknown counts don't hold it up.
	f.did, players, known = nil, 5, false
	s.tick(context.Background())
	if !slices.Contains(f.did, "update valheim") {
		t.Errorf("unknown count: did %q", f.did)
	}
}
