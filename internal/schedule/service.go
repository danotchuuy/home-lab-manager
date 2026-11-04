package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Servers is what tasks need from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	CheckUpdate(ctx context.Context, id string) (manager.UpdateStatus, error)
	Update(ctx context.Context, id string, emit func(manager.Event)) (manager.Server, error)
	JobRunning(name string) bool
}

// Backups runs servers' backups.
type Backups interface {
	BackupNow(ctx context.Context, id, trigger string) (backup.Run, error)
	Busy(name string) bool
}

// Console sends servers commands.
type Console interface {
	Send(ctx context.Context, id, who, command string) (console.Result, error)
}

// Options configures a Service.
type Options struct {
	Secrets  *secrets.Store
	Servers  Servers
	Backups  Backups
	Console  Console
	Notifier notify.Notifier
	// Players says how many players are on a server, if that's known.
	Players func(server string) (int, bool)
	Log     *slog.Logger
	// Now and Sleep are for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// stored is what's kept in the secrets store (task commands can carry
// passwords).
type stored struct {
	// Timezone is hlm's time zone, e.g. Europe/London; empty means the
	// system's.
	Timezone string            `json:"timezone,omitempty"`
	Servers  map[string][]Task `json:"servers,omitempty"`
}

const secretName = "schedule"

// Progress is a task that's running.
type Progress struct {
	TaskID   string    `json:"taskId"`
	TaskName string    `json:"taskName"`
	Trigger  string    `json:"trigger"`
	Started  time.Time `json:"started"`
	Step     int       `json:"step"` // index of the step running
	Steps    int       `json:"steps"`
}

// Service keeps and runs the tasks.
type Service struct {
	opts   Options
	saveMu sync.Mutex

	mu      sync.Mutex
	running map[string]*Progress // by server name
	// waiting are due tasks held up by the server being busy, by task ID,
	// with when they were first seen due.
	waiting map[string]time.Time
	ctx     context.Context // for runs; set by Run
	wg      sync.WaitGroup
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	return &Service{opts: o, running: map[string]*Progress{}, waiting: map[string]time.Time{}, ctx: context.Background()}
}

func (s *Service) load() (stored, error) {
	var st stored
	_, err := s.opts.Secrets.Get(secretName, &st)
	if st.Servers == nil {
		st.Servers = map[string][]Task{}
	}
	return st, err
}

// update changes the stored tasks under the save lock.
func (s *Service) update(fn func(*stored) error) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return s.opts.Secrets.Set(secretName, st)
}

func (st stored) location() *time.Location {
	if st.Timezone != "" {
		if loc, err := time.LoadLocation(st.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

// zoneName names the time zone in use, for showing.
func zoneName(st stored) string {
	if loc := st.location(); loc != time.Local {
		return loc.String()
	}
	return systemZone()
}

// ZoneName names hlm's time zone, for showing.
func (s *Service) ZoneName() string {
	st, _ := s.load()
	return zoneName(st)
}

// Location is hlm's time zone.
func (s *Service) Location() *time.Location {
	st, err := s.load()
	if err != nil {
		return time.Local
	}
	return st.location()
}

// Timezone returns the configured time zone's name, and the system's.
func (s *Service) Timezone() (configured, system string) {
	st, _ := s.load()
	return st.Timezone, systemZone()
}

// systemZone names the system's time zone, which Go only calls "Local": from
// TZ, the /etc/localtime link, or /etc/timezone.
func systemZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && !strings.HasPrefix(tz, "/") {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			return name
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	if _, offset := time.Now().Zone(); offset == 0 {
		return "UTC"
	}
	return time.Local.String()
}

// SetTimezone sets hlm's time zone ("" for the system's), moving every
// task's next run to match.
func (s *Service) SetTimezone(name string) error {
	name = strings.TrimSpace(name)
	if name != "" {
		if _, err := time.LoadLocation(name); err != nil || strings.EqualFold(name, "local") {
			return fmt.Errorf("unknown time zone %q; use a name like Europe/London or America/New_York", name)
		}
	}
	return s.update(func(st *stored) error {
		st.Timezone = name
		loc := st.location()
		now := s.opts.Now()
		for server, tasks := range st.Servers {
			for i := range tasks {
				tasks[i].schedule(now, loc)
			}
			st.Servers[server] = tasks
		}
		return nil
	})
}

// schedule sets when the task next runs after now.
func (t *Task) schedule(now time.Time, loc *time.Location) {
	if !t.Enabled {
		t.Next = nil
		return
	}
	n := t.When.next(now, t.Anchor, loc)
	if n.IsZero() {
		t.Next = nil
		return
	}
	t.Next = &n
}

// ServerTasks is a server's tasks as the API shows them.
type ServerTasks struct {
	Tasks    []Task    `json:"tasks"`
	Running  *Progress `json:"running,omitempty"`
	Timezone string    `json:"timezone"`
}

// Tasks returns a server's tasks. Without withCommands, console commands
// are left out.
func (s *Service) Tasks(server string, withCommands bool) (ServerTasks, error) {
	st, err := s.load()
	if err != nil {
		return ServerTasks{}, err
	}
	out := ServerTasks{Tasks: []Task{}, Timezone: zoneName(st)}
	for _, t := range st.Servers[server] {
		if !withCommands {
			t.Steps = slices.Clone(t.Steps)
			for i := range t.Steps {
				t.Steps[i].Command = ""
			}
		}
		out.Tasks = append(out.Tasks, t)
	}
	s.mu.Lock()
	if p := s.running[server]; p != nil {
		cp := *p
		out.Running = &cp
	}
	s.mu.Unlock()
	return out, nil
}

// ErrNotFound is returned for a task that doesn't exist.
var ErrNotFound = errors.New("no such task")

// Save adds a task to a server (when in.ID is empty) or changes one.
func (s *Service) Save(server string, in Task) (Task, error) {
	t, err := in.check()
	if err != nil {
		return Task{}, err
	}
	var saved Task
	err = s.update(func(st *stored) error {
		tasks := st.Servers[server]
		now := s.opts.Now()
		i := -1
		if t.ID != "" {
			if i = slices.IndexFunc(tasks, func(x Task) bool { return x.ID == t.ID }); i < 0 {
				return ErrNotFound
			}
			old := tasks[i]
			t.Runs = old.Runs
			t.Anchor = old.Anchor
			if old.When.Kind != t.When.Kind || old.When.Every != t.When.Every {
				t.Anchor = now
			}
		} else {
			if len(tasks) >= maxTasks {
				return fmt.Errorf("a server can have at most %d tasks", maxTasks)
			}
			t.ID, t.Anchor, t.Runs = newID(), now, nil
		}
		t.schedule(now, st.location())
		if i >= 0 {
			tasks[i] = t
		} else {
			tasks = append(tasks, t)
		}
		st.Servers[server] = tasks
		saved = t
		return nil
	})
	return saved, err
}

// Delete removes a task.
func (s *Service) Delete(server, id string) error {
	return s.update(func(st *stored) error {
		tasks := st.Servers[server]
		i := slices.IndexFunc(tasks, func(x Task) bool { return x.ID == id })
		if i < 0 {
			return ErrNotFound
		}
		st.Servers[server] = slices.Delete(tasks, i, i+1)
		if len(st.Servers[server]) == 0 {
			delete(st.Servers, server)
		}
		return nil
	})
}

// ForgetServer removes a deleted server's tasks.
func (s *Service) ForgetServer(server string) error {
	return s.update(func(st *stored) error {
		delete(st.Servers, server)
		return nil
	})
}

// Upcoming is a task's next run.
type Upcoming struct {
	Server   string    `json:"server"`
	TaskID   string    `json:"taskId"`
	TaskName string    `json:"taskName"`
	When     string    `json:"when"`
	Next     time.Time `json:"next"`
}

// Upcoming lists the next runs of the tasks of servers visible accepts,
// soonest first, at most n.
func (s *Service) Upcoming(visible func(server string) bool, n int) ([]Upcoming, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Upcoming{}
	for server, tasks := range st.Servers {
		if !visible(server) {
			continue
		}
		for _, t := range tasks {
			if t.Next != nil {
				out = append(out, Upcoming{server, t.ID, t.Name, t.When.Describe(), *t.Next})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Next.Before(out[j].Next) })
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// ErrBusy is returned when a task can't start because the server is busy.
var ErrBusy = errors.New("busy")

// busy says why a server can't run a task now, or "".
func (s *Service) busy(server string) string {
	s.mu.Lock()
	p := s.running[server]
	s.mu.Unlock()
	switch {
	case p != nil:
		return "the task " + p.TaskName + " is running"
	case s.opts.Servers.JobRunning(server):
		return "it's being installed, updated or reconfigured"
	case s.opts.Backups != nil && s.opts.Backups.Busy(server):
		return "it's being backed up or restored"
	}
	return ""
}

// Running reports whether one of the server's tasks is running.
func (s *Service) Running(server string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[server] != nil
}

// RunNow starts a task straight away.
func (s *Service) RunNow(server, id string) error {
	st, err := s.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(st.Servers[server], func(x Task) bool { return x.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	if why := s.busy(server); why != "" {
		return fmt.Errorf("%w: %s", ErrBusy, why)
	}
	return s.start(server, st.Servers[server][i], "manual")
}

// start runs a task in the background, unless the server is running one.
func (s *Service) start(server string, t Task, trigger string) error {
	s.mu.Lock()
	if p := s.running[server]; p != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: the task %s is running", ErrBusy, p.TaskName)
	}
	prog := &Progress{TaskID: t.ID, TaskName: t.Name, Trigger: trigger, Started: s.opts.Now(), Steps: len(t.Steps)}
	s.running[server] = prog
	ctx := s.ctx
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		run := s.execute(ctx, server, t, prog)
		s.mu.Lock()
		delete(s.running, server)
		s.mu.Unlock()
		s.finish(server, t, run)
	}()
	return nil
}

// finish records a run and reports one that didn't go well.
func (s *Service) finish(server string, t Task, run Run) {
	if err := s.update(func(st *stored) error {
		tasks := st.Servers[server]
		if i := slices.IndexFunc(tasks, func(x Task) bool { return x.ID == t.ID }); i >= 0 {
			tasks[i].Runs = append([]Run{run}, tasks[i].Runs...)
			if len(tasks[i].Runs) > keepRuns {
				tasks[i].Runs = tasks[i].Runs[:keepRuns]
			}
		}
		return nil
	}); err != nil {
		s.opts.Log.Warn("record task run", "server", server, "task", t.Name, "err", err)
	}
	level := slog.LevelInfo
	if run.Result == ResultFailed || run.Result == ResultMissed {
		level = slog.LevelWarn
	}
	s.opts.Log.Log(context.Background(), level, "scheduled task", "server", server, "task", t.Name,
		"trigger", run.Trigger, "result", run.Result, "err", run.Error)
	if s.opts.Notifier == nil {
		return
	}
	key := notify.KindTaskFailed + "/" + server + "/" + t.ID
	switch run.Result {
	case ResultOK:
		s.opts.Notifier.Clear(key)
	case ResultFailed, ResultMissed:
		title := "Task " + t.Name + " on " + server + " failed"
		if run.Result == ResultMissed {
			title = "Task " + t.Name + " on " + server + " was missed"
		}
		s.opts.Notifier.Notify(notify.Event{
			Kind: notify.KindTaskFailed, Title: title, Message: run.Error, Server: server,
			Link: "/servers/" + server + "?tab=schedule", Key: key,
			Fields: map[string]string{"Task": t.Name, "Schedule": t.When.Describe()},
		})
	}
}

// execute runs a task's steps.
func (s *Service) execute(ctx context.Context, server string, t Task, prog *Progress) (run Run) {
	run = Run{Trigger: prog.Trigger, Started: prog.Started, Result: ResultOK}
	// Named, so this applies to whichever run is returned.
	defer func() { run.Finished = s.opts.Now() }()
	info, err := s.opts.Servers.Inspect(ctx, server)
	if err != nil {
		run.Result, run.Error = ResultFailed, err.Error()
		return run
	}
	if t.OnlyIfRunning && !info.State.Running {
		run.Result, run.Error = ResultSkipped, "the server was stopped"
		return run
	}
	for i, step := range t.Steps {
		s.mu.Lock()
		prog.Step = i
		s.mu.Unlock()
		res := s.step(ctx, server, step)
		run.Steps = append(run.Steps, res)
		if step.Kind == StepIfPlayers && !res.OK {
			// The condition wasn't met: the rest of the task doesn't run.
			run.Result, run.Error = ResultSkipped, res.Message
			return run
		}
		if !res.OK && !res.Skipped && (!step.ContinueOnError || ctx.Err() != nil) {
			run.Result = ResultFailed
			run.Error = fmt.Sprintf("step %d (%s): %s", i+1, step.Kind, res.Message)
			if ctx.Err() != nil {
				run.Error = fmt.Sprintf("step %d (%s): hlm stopped", i+1, step.Kind)
			}
			return run
		}
		if !res.OK && !res.Skipped && run.Error == "" {
			run.Error = fmt.Sprintf("step %d (%s) failed, but the task carried on: %s", i+1, step.Kind, res.Message)
		}
	}
	return run
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// step runs one step.
func (s *Service) step(ctx context.Context, server string, st Step) StepResult {
	res := StepResult{Kind: st.Kind, OK: true}
	fail := func(err error) StepResult {
		res.OK, res.Message = false, err.Error()
		return res
	}
	skip := func(why string) StepResult {
		res.OK, res.Skipped, res.Message = false, true, why
		return res
	}
	running := func() (bool, error) {
		info, err := s.opts.Servers.Inspect(ctx, server)
		return info.State.Running, err
	}
	switch st.Kind {
	case StepStart:
		if up, err := running(); err != nil {
			return fail(err)
		} else if up {
			return skip("already running")
		}
		if err := s.opts.Servers.Start(ctx, server); err != nil {
			return fail(err)
		}
	case StepStop:
		if up, err := running(); err != nil {
			return fail(err)
		} else if !up {
			return skip("already stopped")
		}
		if err := s.opts.Servers.Stop(ctx, server); err != nil {
			return fail(err)
		}
	case StepRestart:
		if err := s.opts.Servers.Restart(ctx, server); err != nil {
			return fail(err)
		}
	case StepUpdate:
		upd, err := s.opts.Servers.CheckUpdate(ctx, server)
		if err != nil {
			return fail(err)
		}
		switch upd.State {
		case manager.UpdateAvailable:
		case manager.UpdateError:
			return fail(errors.New("checking for an update failed: " + upd.Reason))
		default:
			return skip("no update available")
		}
		if _, err := s.opts.Servers.Update(ctx, server, func(manager.Event) {}); err != nil {
			return fail(err)
		}
		res.Message = "updated"
	case StepBackup:
		if s.opts.Backups == nil {
			return fail(errors.New("backups aren't available"))
		}
		r, err := s.opts.Backups.BackupNow(ctx, server, "schedule")
		if err != nil {
			return fail(err)
		}
		res.Message = "saved as " + r.Key
	case StepCommand:
		if s.opts.Console == nil {
			return fail(errors.New("the console isn't available"))
		}
		r, err := s.opts.Console.Send(ctx, server, "schedule", st.Command)
		if err != nil {
			return fail(err)
		}
		res.Message = truncate(r.Output, 500)
	case StepWait:
		if err := s.opts.Sleep(ctx, time.Duration(st.Seconds)*time.Second); err != nil {
			return fail(err)
		}
	case StepIfPlayers:
		n, known := 0, false
		if s.opts.Players != nil {
			n, known = s.opts.Players(server)
		}
		switch {
		case !known:
			return skip("the number of players online isn't known (the server isn't answering its status query)")
		case st.Players == PlayersNone && n > 0:
			return skip(fmt.Sprintf("%d %s online", n, map[bool]string{true: "player was", false: "players were"}[n == 1]))
		case st.Players == PlayersSome && n == 0:
			return skip("nobody was online")
		}
		res.Message = fmt.Sprintf("%d online", n)
	default:
		return fail(fmt.Errorf("unknown step %q", st.Kind))
	}
	return res
}

// Run starts tasks as they come due, until ctx ends, then waits for running
// ones to stop.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// tick starts the tasks that are due.
func (s *Service) tick(ctx context.Context) {
	st, err := s.load()
	if err != nil {
		s.opts.Log.Warn("read scheduled tasks", "err", err)
		return
	}
	now := s.opts.Now()
	loc := st.location()
	type due struct {
		server string
		task   Task
	}
	var list []due
	for server, tasks := range st.Servers {
		for _, t := range tasks {
			if t.Enabled && t.Next != nil && !now.Before(*t.Next) {
				list = append(list, due{server, t})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].task.Next.Before(*list[j].task.Next) })
	for _, d := range list {
		if ctx.Err() != nil {
			return
		}
		t := d.task
		scheduled := *t.Next
		s.mu.Lock()
		firstSeen, waited := s.waiting[t.ID]
		if !waited {
			firstSeen = now
		}
		s.mu.Unlock()
		// Due long before it was first seen: hlm wasn't running then.
		if !waited && firstSeen.Sub(scheduled) > missedAfter {
			s.advance(d.server, t.ID, scheduled, now, loc)
			s.finish(d.server, t, Run{Trigger: "schedule", Started: scheduled, Finished: now, Result: ResultMissed,
				Error: "hlm wasn't running at " + scheduled.In(loc).Format("Mon 2 Jan 15:04") + ", so the task didn't run"})
			continue
		}
		if _, err := s.opts.Servers.Inspect(ctx, d.server); err != nil {
			if errors.Is(err, manager.ErrNotFound) || errors.Is(err, manager.ErrNotManaged) {
				s.advance(d.server, t.ID, scheduled, now, loc)
			}
			continue // or Docker is unreachable: try again
		}
		if why := s.busy(d.server); why != "" {
			if now.Sub(scheduled) > busyGiveUp {
				s.forgetWaiting(t.ID)
				s.advance(d.server, t.ID, scheduled, now, loc)
				s.finish(d.server, t, Run{Trigger: "schedule", Started: scheduled, Finished: now, Result: ResultFailed,
					Error: "the server was busy for 10 minutes (" + why + "), so the task didn't run"})
				continue
			}
			s.mu.Lock()
			s.waiting[t.ID] = firstSeen
			s.mu.Unlock()
			continue
		}
		s.forgetWaiting(t.ID)
		s.advance(d.server, t.ID, scheduled, now, loc)
		if err := s.start(d.server, t, "schedule"); err != nil {
			s.opts.Log.Warn("start scheduled task", "server", d.server, "task", t.Name, "err", err)
		}
	}
}

func (s *Service) forgetWaiting(id string) {
	s.mu.Lock()
	delete(s.waiting, id)
	s.mu.Unlock()
}

// advance moves a task's next run past now, if it's still the one that
// was due at scheduled (it may have been changed meanwhile).
func (s *Service) advance(server, id string, scheduled, now time.Time, loc *time.Location) {
	if err := s.update(func(st *stored) error {
		tasks := st.Servers[server]
		i := slices.IndexFunc(tasks, func(x Task) bool { return x.ID == id })
		if i < 0 || tasks[i].Next == nil || !tasks[i].Next.Equal(scheduled) {
			return nil
		}
		tasks[i].schedule(now, loc)
		return nil
	}); err != nil {
		s.opts.Log.Warn("schedule next run", "server", server, "err", err)
	}
}
