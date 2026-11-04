package schedule

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
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeWorld records what tasks do to servers.
type fakeWorld struct {
	mu        sync.Mutex
	running   map[string]bool
	jobs      map[string]bool
	backingUp map[string]bool
	update    string // the update check's state
	failStep  string // a step kind that fails
	did       []string
	events    []notify.Event
	cleared   []string
	slept     time.Duration
}

func (f *fakeWorld) log(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.did = append(f.did, s)
	if strings.HasPrefix(s, f.failStep+" ") || s == f.failStep {
		if f.failStep != "" {
			return errors.New(f.failStep + " went wrong")
		}
	}
	return nil
}

func (f *fakeWorld) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.running[id]
	if !ok {
		return docker.ContainerJSON{}, manager.ErrNotFound
	}
	var c docker.ContainerJSON
	c.Name = "/" + id
	c.State.Running = up
	return c, nil
}
func (f *fakeWorld) Start(_ context.Context, id string) error {
	f.mu.Lock()
	f.running[id] = true
	f.mu.Unlock()
	return f.log("start " + id)
}
func (f *fakeWorld) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	f.running[id] = false
	f.mu.Unlock()
	return f.log("stop " + id)
}
func (f *fakeWorld) Restart(_ context.Context, id string) error { return f.log("restart " + id) }
func (f *fakeWorld) CheckUpdate(context.Context, string) (manager.UpdateStatus, error) {
	return manager.UpdateStatus{State: f.update, Reason: "registry down"}, nil
}
func (f *fakeWorld) Update(_ context.Context, id string, _ func(manager.Event)) (manager.Server, error) {
	return manager.Server{}, f.log("update " + id)
}
func (f *fakeWorld) JobRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[name]
}
func (f *fakeWorld) BackupNow(_ context.Context, id, trigger string) (backup.Run, error) {
	return backup.Run{Key: "hlm/" + id + "/b.tar.gz"}, f.log("backup " + id + " " + trigger)
}
func (f *fakeWorld) Busy(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backingUp[name]
}
func (f *fakeWorld) Send(_ context.Context, id, who, cmd string) (console.Result, error) {
	return console.Result{Output: "ok: " + cmd}, f.log("command " + id + " " + who + " " + cmd)
}
func (f *fakeWorld) Notify(e notify.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}
func (f *fakeWorld) Clear(key string) {
	f.mu.Lock()
	f.cleared = append(f.cleared, key)
	f.mu.Unlock()
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }

func newTest(t *testing.T) (*Service, *fakeWorld, *testClock) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorld{running: map[string]bool{"valheim": true, "mc": false}, jobs: map[string]bool{},
		backingUp: map[string]bool{}, update: manager.UpdateAvailable}
	clk := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Secrets: store, Servers: w, Backups: w, Console: w, Notifier: w, Now: clk.Now,
		Sleep: func(_ context.Context, d time.Duration) error {
			w.mu.Lock()
			w.slept += d
			w.mu.Unlock()
			return nil
		}})
	if err := s.SetTimezone("UTC"); err != nil {
		t.Fatal(err)
	}
	return s, w, clk
}

func nightly() Task {
	return Task{
		Name: "Nightly restart", Enabled: true, OnlyIfRunning: true,
		When: When{Kind: WhenDaily, Time: "4:00"},
		Steps: []Step{
			{Kind: StepCommand, Command: "say Restarting in 5 minutes"},
			{Kind: StepWait, Seconds: 300},
			{Kind: StepCommand, Command: "save"},
			{Kind: StepRestart},
		},
	}
}

func TestValidation(t *testing.T) {
	s, _, _ := newTest(t)
	bad := []Task{
		{Name: "", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "25:00"}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00", Weekdays: []int{7}}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenEvery, Every: 2}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenCron, Cron: "0 0 31 2 *"}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenCron, Cron: "nope"}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: "hourly"}, Steps: []Step{{Kind: StepRestart}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: "reboot"}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: StepCommand}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: StepWait, Seconds: 0}}},
		{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: StepWait, Seconds: 3601}}},
	}
	for _, b := range bad {
		if _, err := s.Save("valheim", b); err == nil {
			t.Errorf("saved %+v", b)
		}
	}
	got, err := s.Save("valheim", Task{Name: " x ", Enabled: true,
		When:  When{Kind: WhenDaily, Time: "4:05", Weekdays: []int{5, 1, 1}, Cron: "junk", Every: 9},
		Steps: []Step{{Kind: StepWait, Seconds: 5, Command: "junk", ContinueOnError: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "x" || got.When.Time != "04:05" || !slices.Equal(got.When.Weekdays, []int{1, 5}) || got.When.Cron != "" ||
		got.When.Every != 0 || got.Steps[0].Command != "" || got.Steps[0].ContinueOnError || got.ID == "" {
		t.Errorf("cleaned task = %+v", got)
	}
	// Next: Friday 6 November 04:05 (it's Wednesday 4 November).
	if got.Next == nil || got.Next.Format("Mon 2006-01-02 15:04") != "Fri 2026-11-06 04:05" {
		t.Errorf("next = %v", got.Next)
	}
	if got.When.Describe() != "Mon, Fri at 04:05" {
		t.Errorf("describe = %q", got.When.Describe())
	}
}

func TestNextRuns(t *testing.T) {
	s, _, clk := newTest(t)
	every, _ := s.Save("valheim", Task{Name: "save", Enabled: true, When: When{Kind: WhenEvery, Every: 90},
		Steps: []Step{{Kind: StepCommand, Command: "save"}}})
	if !every.Next.Equal(clk.Now().Add(90 * time.Minute)) {
		t.Errorf("every 90 minutes: next = %v", every.Next)
	}
	off, _ := s.Save("valheim", Task{Name: "off", Enabled: false, When: When{Kind: WhenCron, Cron: "*/5 * * * *"},
		Steps: []Step{{Kind: StepRestart}}})
	if off.Next != nil {
		t.Errorf("disabled task has a next run: %v", off.Next)
	}
	n, _ := s.Save("valheim", nightly())
	if n.Next.Format("2006-01-02 15:04 MST") != "2026-11-05 04:00 UTC" {
		t.Errorf("nightly next = %v", n.Next)
	}
	// A new time zone moves daily runs to that zone's 04:00.
	if err := s.SetTimezone("America/New_York"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Tasks("valheim", true)
	ny, _ := time.LoadLocation("America/New_York")
	if got := st.Tasks[2].Next.In(ny).Format("2006-01-02 15:04 MST"); got != "2026-11-05 04:00 EST" {
		t.Errorf("nightly in New York = %s", got)
	}
	if st.Timezone != "America/New_York" {
		t.Errorf("time zone = %q", st.Timezone)
	}
	if err := s.SetTimezone("Mars/Olympus"); err == nil {
		t.Error("accepted an unknown time zone")
	}
}

func TestRunSteps(t *testing.T) {
	s, w, _ := newTest(t)
	task, _ := s.Save("valheim", nightly())
	if err := s.RunNow("valheim", task.ID); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	want := []string{"command valheim schedule say Restarting in 5 minutes", "command valheim schedule save", "restart valheim"}
	if !slices.Equal(w.did, want) || w.slept != 5*time.Minute {
		t.Errorf("did %q, slept %v", w.did, w.slept)
	}
	st, _ := s.Tasks("valheim", true)
	run := st.Tasks[0].Runs[0]
	if run.Result != ResultOK || run.Trigger != "manual" || len(run.Steps) != 4 || run.Steps[0].Message != "ok: say Restarting in 5 minutes" ||
		run.Finished.IsZero() || run.Started.IsZero() {
		t.Errorf("run = %+v", run)
	}
	if len(w.cleared) != 1 || len(w.events) != 0 {
		t.Errorf("a good run: cleared %v, events %v", w.cleared, w.events)
	}

	// A failing step stops the task and is reported.
	w.did, w.failStep = nil, "command"
	s.RunNow("valheim", task.ID)
	s.wg.Wait()
	st, _ = s.Tasks("valheim", true)
	run = st.Tasks[0].Runs[0]
	if run.Result != ResultFailed || len(w.did) != 1 || !strings.Contains(run.Error, "step 1 (command): command went wrong") {
		t.Errorf("failed run = %+v, did %q", run, w.did)
	}
	if len(w.events) != 1 || w.events[0].Kind != notify.KindTaskFailed || w.events[0].Server != "valheim" {
		t.Errorf("events = %+v", w.events)
	}

	// Unless it may carry on.
	cont := nightly()
	cont.ID = task.ID
	cont.Steps[0].ContinueOnError = true
	cont.Steps[2].ContinueOnError = true
	s.Save("valheim", cont)
	w.did = nil
	s.RunNow("valheim", task.ID)
	s.wg.Wait()
	st, _ = s.Tasks("valheim", true)
	run = st.Tasks[0].Runs[0]
	if run.Result != ResultOK || len(w.did) != 3 || !strings.Contains(run.Error, "carried on") {
		t.Errorf("carried-on run = %+v, did %q", run, w.did)
	}
	if len(st.Tasks[0].Runs) != 3 {
		t.Errorf("runs kept = %d", len(st.Tasks[0].Runs))
	}

	// Stopped servers are skipped when asked.
	stopped, _ := s.Save("mc", nightly())
	w.did = nil
	s.RunNow("mc", stopped.ID)
	s.wg.Wait()
	st, _ = s.Tasks("mc", true)
	if r := st.Tasks[0].Runs[0]; r.Result != ResultSkipped || len(w.did) != 0 {
		t.Errorf("stopped server: %+v, did %q", r, w.did)
	}
}

func TestStepKinds(t *testing.T) {
	s, w, _ := newTest(t)
	task, _ := s.Save("valheim", Task{Name: "all", Enabled: true, When: When{Kind: WhenDaily, Time: "04:00"},
		Steps: []Step{{Kind: StepStart}, {Kind: StepUpdate}, {Kind: StepBackup}, {Kind: StepStop}, {Kind: StepStop}, {Kind: StepStart}}})
	s.RunNow("valheim", task.ID)
	s.wg.Wait()
	want := []string{"update valheim", "backup valheim schedule", "stop valheim", "start valheim"}
	if !slices.Equal(w.did, want) {
		t.Errorf("did %q, want %q", w.did, want)
	}
	st, _ := s.Tasks("valheim", true)
	steps := st.Tasks[0].Runs[0].Steps
	if !steps[0].Skipped || steps[0].Message != "already running" || !steps[4].Skipped || steps[2].Message != "saved as hlm/valheim/b.tar.gz" {
		t.Errorf("steps = %+v", steps)
	}
	// No update: skipped. A failed check: fails.
	w.did, w.update = nil, manager.UpdateCurrent
	s.RunNow("valheim", task.ID)
	s.wg.Wait()
	st, _ = s.Tasks("valheim", true)
	if r := st.Tasks[0].Runs[0]; !r.Steps[1].Skipped || r.Result != ResultOK || slices.Contains(w.did, "update valheim") {
		t.Errorf("no update: %+v", r)
	}
	w.update = manager.UpdateError
	s.RunNow("valheim", task.ID)
	s.wg.Wait()
	st, _ = s.Tasks("valheim", true)
	if r := st.Tasks[0].Runs[0]; r.Result != ResultFailed || !strings.Contains(r.Error, "registry down") {
		t.Errorf("update check failed: %+v", r)
	}
}

func TestScheduler(t *testing.T) {
	s, w, clk := newTest(t)
	task, _ := s.Save("valheim", Task{Name: "restart", Enabled: true, When: When{Kind: WhenDaily, Time: "04:00"},
		Steps: []Step{{Kind: StepRestart}}})
	ctx := context.Background()

	clk.set(time.Date(2026, 11, 5, 3, 59, 0, 0, time.UTC))
	s.tick(ctx)
	s.wg.Wait()
	if len(w.did) != 0 {
		t.Fatalf("ran early: %q", w.did)
	}
	clk.set(time.Date(2026, 11, 5, 4, 0, 10, 0, time.UTC))
	s.tick(ctx)
	s.wg.Wait()
	s.tick(ctx) // the same occurrence doesn't run twice
	s.wg.Wait()
	if !slices.Equal(w.did, []string{"restart valheim"}) {
		t.Errorf("did %q", w.did)
	}
	st, _ := s.Tasks("valheim", true)
	if st.Tasks[0].Next.Format("2006-01-02 15:04") != "2026-11-06 04:00" || st.Tasks[0].Runs[0].Trigger != "schedule" {
		t.Errorf("after running: next %v, runs %+v", st.Tasks[0].Next, st.Tasks[0].Runs)
	}

	// hlm was down at 04:00 and comes back at 05:00: missed, not run late.
	w.did = nil
	clk.set(time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC))
	s.tick(ctx)
	s.wg.Wait()
	st, _ = s.Tasks("valheim", true)
	if len(w.did) != 0 || st.Tasks[0].Runs[0].Result != ResultMissed || st.Tasks[0].Next.Format("01-02 15:04") != "11-07 04:00" {
		t.Errorf("missed: did %q, run %+v, next %v", w.did, st.Tasks[0].Runs[0], st.Tasks[0].Next)
	}
	if len(w.events) != 1 || !strings.Contains(w.events[0].Title, "was missed") {
		t.Errorf("missed run not reported: %+v", w.events)
	}

	// Busy with an update: it waits, then runs when the update's done.
	w.jobs["valheim"] = true
	clk.set(time.Date(2026, 11, 7, 4, 0, 5, 0, time.UTC))
	s.tick(ctx)
	clk.set(time.Date(2026, 11, 7, 4, 5, 0, 0, time.UTC))
	s.tick(ctx)
	if len(w.did) != 0 {
		t.Fatalf("ran while busy: %q", w.did)
	}
	// Still waiting past the "missed" threshold isn't a missed run.
	w.jobs["valheim"] = false
	clk.set(time.Date(2026, 11, 7, 4, 9, 0, 0, time.UTC))
	s.tick(ctx)
	s.wg.Wait()
	if !slices.Equal(w.did, []string{"restart valheim"}) {
		t.Errorf("after waiting: did %q", w.did)
	}

	// Busy for more than 10 minutes: given up, reported.
	w.did, w.events = nil, nil
	w.backingUp["valheim"] = true
	clk.set(time.Date(2026, 11, 8, 4, 0, 5, 0, time.UTC))
	s.tick(ctx)
	clk.set(time.Date(2026, 11, 8, 4, 11, 0, 0, time.UTC))
	s.tick(ctx)
	st, _ = s.Tasks("valheim", true)
	if len(w.did) != 0 || st.Tasks[0].Runs[0].Result != ResultFailed || !strings.Contains(st.Tasks[0].Runs[0].Error, "backed up") {
		t.Errorf("busy too long: did %q, run %+v", w.did, st.Tasks[0].Runs[0])
	}
	if st.Tasks[0].Next.Format("01-02") != "11-09" || len(w.events) != 1 {
		t.Errorf("after giving up: next %v, events %d", st.Tasks[0].Next, len(w.events))
	}

	// Run now refuses while busy.
	if err := s.RunNow("valheim", task.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("run now while backing up: %v", err)
	}
	if err := s.RunNow("valheim", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("run a missing task: %v", err)
	}
}

func TestViewsAndForgetting(t *testing.T) {
	s, _, _ := newTest(t)
	a, _ := s.Save("valheim", nightly())
	s.Save("mc", Task{Name: "save", Enabled: true, When: When{Kind: WhenEvery, Every: 30}, Steps: []Step{{Kind: StepCommand, Command: "save-all"}}})
	s.Save("hidden", Task{Name: "x", Enabled: true, When: When{Kind: WhenEvery, Every: 5}, Steps: []Step{{Kind: StepRestart}}})

	st, _ := s.Tasks("valheim", false)
	for _, step := range st.Tasks[0].Steps {
		if step.Command != "" {
			t.Errorf("command shown without permission: %+v", step)
		}
	}
	if full, _ := s.Tasks("valheim", true); full.Tasks[0].Steps[0].Command == "" {
		t.Error("command hidden from those who may see it")
	}
	up, _ := s.Upcoming(func(server string) bool { return server != "hidden" }, 10)
	if len(up) != 2 || up[0].Server != "mc" || up[1].Server != "valheim" || up[1].When != "every day at 04:00" {
		t.Errorf("upcoming = %+v", up)
	}

	upd := nightly()
	upd.ID, upd.Name = a.ID, "Renamed"
	if _, err := s.Save("valheim", upd); err != nil {
		t.Fatal(err)
	}
	upd.ID = "missing"
	if _, err := s.Save("valheim", upd); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating a missing task: %v", err)
	}
	if err := s.Delete("valheim", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("valheim", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	if err := s.ForgetServer("mc"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Tasks("mc", true); len(st.Tasks) != 0 {
		t.Errorf("tasks of a forgotten server: %+v", st.Tasks)
	}
}

func TestIfPlayers(t *testing.T) {
	s, w, _ := newTest(t)
	players, known := 0, true
	s.opts.Players = func(string) (int, bool) { return players, known }
	task, err := s.Save("valheim", Task{Name: "restart when empty", Enabled: true, When: When{Kind: WhenDaily, Time: "04:00"},
		Steps: []Step{{Kind: StepIfPlayers, Players: PlayersNone, ContinueOnError: true}, {Kind: StepRestart}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Steps[0].ContinueOnError {
		t.Error("a condition kept continueOnError")
	}
	run := func() Run {
		s.RunNow("valheim", task.ID)
		s.wg.Wait()
		st, _ := s.Tasks("valheim", true)
		return st.Tasks[0].Runs[0]
	}
	if r := run(); r.Result != ResultOK || !slices.Equal(w.did, []string{"restart valheim"}) || r.Steps[0].Message != "0 online" {
		t.Errorf("nobody on: %+v, did %q", r, w.did)
	}
	w.did, players = nil, 2
	if r := run(); r.Result != ResultSkipped || len(w.did) != 0 || r.Error != "2 players were online" {
		t.Errorf("2 on: %+v, did %q", r, w.did)
	}
	w.did, known = nil, false
	if r := run(); r.Result != ResultSkipped || len(w.did) != 0 || !strings.Contains(r.Error, "isn't known") {
		t.Errorf("unknown: %+v, did %q", r, w.did)
	}
	if len(w.events) != 0 {
		t.Errorf("skipped runs were reported as failures: %+v", w.events)
	}
	if _, err := s.Save("valheim", Task{Name: "x", When: When{Kind: WhenDaily, Time: "04:00"}, Steps: []Step{{Kind: StepIfPlayers}}}); err == nil {
		t.Error("a condition without players accepted")
	}
}
