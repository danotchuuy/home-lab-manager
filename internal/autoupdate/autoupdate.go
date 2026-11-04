// Package autoupdate applies updates by itself: to servers whose update
// policy is automatic, and to hlm, within the time windows they're allowed
// in. Updates are only found by the regular update checks; this package
// decides when to apply them, warns players, backs up first, and keeps to
// one update at a time.
package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
)

// Modes.
const (
	ModeDefault = ""       // a server follows the default policy
	ModeManual  = "manual" // updates wait for someone to click Update
	ModeAuto    = "auto"   // updates are applied in the window
)

// Policy says how a server's updates are applied.
type Policy struct {
	Mode string `json:"mode"`
	// WindowStart and WindowEnd ("03:00", "06:00", in hlm's time zone)
	// limit when an update may start; both empty means any time. The
	// window can run past midnight.
	WindowStart string `json:"windowStart,omitempty"`
	WindowEnd   string `json:"windowEnd,omitempty"`
	// Warn is a console command sent WarnMinutes before updating, such as
	// "say Updating in 5 minutes"; empty sends nothing.
	Warn        string `json:"warn,omitempty"`
	WarnMinutes int    `json:"warnMinutes,omitempty"`
	// Backup runs the server's backup plan first, if it has one; if the
	// backup fails, the update waits for the next window.
	Backup bool `json:"backup"`
	// OnlyWhenEmpty waits while players are online (when the server's
	// status query says how many are; if it doesn't, it doesn't wait).
	OnlyWhenEmpty bool `json:"onlyWhenEmpty,omitempty"`
}

// SelfPolicy says whether hlm updates itself, and when.
type SelfPolicy struct {
	Auto        bool   `json:"auto"`
	WindowStart string `json:"windowStart,omitempty"`
	WindowEnd   string `json:"windowEnd,omitempty"`
}

// Settings are the stored settings.
type Settings struct {
	// Default is the policy of servers that don't have their own; its Mode
	// is manual or auto.
	Default Policy            `json:"default"`
	Servers map[string]Policy `json:"servers,omitempty"`
	// CheckHours is how often to check for updates, in hours (0 means 12);
	// HLM_UPDATE_CHECK, when set, wins.
	CheckHours int        `json:"checkHours,omitempty"`
	Self       SelfPolicy `json:"self"`
	// Results are the last automatic update of each server, and of hlm
	// (under SelfKey).
	Results map[string]Result `json:"results,omitempty"`
}

// SelfKey is hlm's own entry in Settings.Results.
const SelfKey = "_hlm"

// DefaultCheckHours is how often updates are checked for by default.
const DefaultCheckHours = 12

// Result is how an automatic update went.
type Result struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	// Steps describes what was done: warned, backed up, updated.
	Steps []string `json:"steps,omitempty"`
}

const secretName = "autoupdate"

// retryAfterFailure is how long after a failed automatic update the next
// try waits, so a broken image isn't retried over and over.
const retryAfterFailure = 24 * time.Hour

// Servers is what the service needs from the server manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	CheckUpdate(ctx context.Context, id string) (manager.UpdateStatus, error)
	Update(ctx context.Context, id string, emit func(manager.Event)) (manager.Server, error)
	JobRunning(name string) bool
	Busy() string
}

// Backups runs servers' backups.
type Backups interface {
	HasPlan(name string) bool
	Busy(name string) bool
	BackupNow(ctx context.Context, id, trigger string) (backup.Run, error)
}

// Console sends servers commands.
type Console interface {
	Send(ctx context.Context, id, who, command string) (console.Result, error)
}

// Tasks says whether a server's scheduled task is running.
type Tasks interface {
	Running(server string) bool
}

// Self updates hlm.
type Self interface {
	Status() selfupdate.Status
	Apply(ctx context.Context, emit func(selfupdate.Event)) error
}

// Options configures a Service.
type Options struct {
	Secrets  *secrets.Store
	Servers  Servers
	Backups  Backups
	Console  Console
	Tasks    Tasks
	Self     Self
	Notifier notify.Notifier
	// Players says how many players are on a server, if that's known.
	Players func(server string) (int, bool)
	// Audit records each automatic update.
	Audit audit.Recorder
	// Location is hlm's time zone, which windows are in, and ZoneName its
	// name for showing.
	Location func() *time.Location
	ZoneName func() string
	Log      *slog.Logger
	// Now and Sleep are for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Service applies updates automatically.
type Service struct {
	opts   Options
	saveMu sync.Mutex

	mu sync.Mutex
	// state says what's happening with each server's update, by name.
	state map[string]State
}

// States of a server's automatic update.
const (
	StateWaiting  = "waiting"  // for the window
	StateBusy     = "busy"     // the server is busy; tried again shortly
	StateQueued   = "queued"   // another update is going first
	StateUpdating = "updating" // being updated now
	StateFailed   = "failed"   // the last try failed; tried again later
	StatePlayers  = "players"  // waiting for players to leave
)

// State is what's happening with a server's automatic update.
type State struct {
	State   string     `json:"state"`
	Message string     `json:"message,omitempty"`
	Next    *time.Time `json:"next,omitempty"` // when it's next tried
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = func() *time.Location { return time.Local }
	}
	if o.ZoneName == nil {
		o.ZoneName = func() string { return o.Location().String() }
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
	return &Service{opts: o, state: map[string]State{}}
}

func (s *Service) load() (Settings, error) {
	set := Settings{Default: Policy{Mode: ModeManual, Backup: true}}
	_, err := s.opts.Secrets.Get(secretName, &set)
	if set.Default.Mode == ModeDefault {
		set.Default.Mode = ModeManual
	}
	if set.Servers == nil {
		set.Servers = map[string]Policy{}
	}
	if set.Results == nil {
		set.Results = map[string]Result{}
	}
	return set, err
}

func (s *Service) update(fn func(*Settings) error) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	set, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&set); err != nil {
		return err
	}
	return s.opts.Secrets.Set(secretName, set)
}

// Settings returns the stored settings.
func (s *Service) Settings() (Settings, error) { return s.load() }

// CheckInterval is how often to check for updates, from the setting.
func (s *Service) CheckInterval() time.Duration {
	set, err := s.load()
	if err != nil || set.CheckHours <= 0 {
		return DefaultCheckHours * time.Hour
	}
	return time.Duration(set.CheckHours) * time.Hour
}

// effective returns the policy a server follows.
func (set Settings) effective(server string) Policy {
	if p, ok := set.Servers[server]; ok && p.Mode != ModeDefault {
		return p
	}
	return set.Default
}

// parseClock reads "04:30" as minutes since midnight.
func parseClock(s string) (int, bool) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(m) != 2 || len(h) == 0 || len(h) > 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

func checkWindow(start, end string) (string, string, error) {
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)
	if start == "" && end == "" {
		return "", "", nil
	}
	a, ok1 := parseClock(start)
	b, ok2 := parseClock(end)
	if !ok1 || !ok2 {
		return "", "", errors.New("the window's start and end are times like 03:00")
	}
	if a == b {
		return "", "", errors.New("the window starts and ends at the same time")
	}
	return fmt.Sprintf("%02d:%02d", a/60, a%60), fmt.Sprintf("%02d:%02d", b/60, b%60), nil
}

// open reports whether now is in the window, and if not, when it next
// opens. An empty window is always open.
func open(start, end string, now time.Time, loc *time.Location) (bool, time.Time) {
	a, ok1 := parseClock(start)
	b, ok2 := parseClock(end)
	if !ok1 || !ok2 || a == b {
		return true, time.Time{}
	}
	now = now.In(loc)
	m := now.Hour()*60 + now.Minute()
	in := m >= a && m < b
	if a > b { // past midnight
		in = m >= a || m < b
	}
	if in {
		return true, time.Time{}
	}
	c, err := schedule.ParseCron(fmt.Sprintf("%d %d * * *", a%60, a/60))
	if err != nil {
		return false, time.Time{}
	}
	return false, c.Next(now, loc)
}

func (p Policy) check() (Policy, error) {
	switch p.Mode {
	case ModeDefault, ModeManual, ModeAuto:
	default:
		return p, fmt.Errorf("unknown update mode %q", p.Mode)
	}
	var err error
	if p.WindowStart, p.WindowEnd, err = checkWindow(p.WindowStart, p.WindowEnd); err != nil {
		return p, err
	}
	p.Warn = strings.TrimSpace(p.Warn)
	if len(p.Warn) > console.MaxCommand {
		return p, errors.New("the warning command is too long")
	}
	if p.Warn == "" {
		p.WarnMinutes = 0
	} else if p.WarnMinutes < 0 || p.WarnMinutes > 30 {
		return p, errors.New("warn between 0 and 30 minutes before updating")
	}
	return p, nil
}

// SetServerPolicy sets a server's policy; ModeDefault makes it follow the
// default.
func (s *Service) SetServerPolicy(server string, p Policy) (Policy, error) {
	p, err := p.check()
	if err != nil {
		return p, err
	}
	err = s.update(func(set *Settings) error {
		if p.Mode == ModeDefault {
			delete(set.Servers, server)
		} else {
			set.Servers[server] = p
		}
		return nil
	})
	s.clearState(server)
	return p, err
}

// General are the settings on the System page.
type General struct {
	Default    Policy     `json:"default"`
	CheckHours int        `json:"checkHours"`
	Self       SelfPolicy `json:"self"`
}

// SetGeneral saves the default policy, the check interval and hlm's own
// policy.
func (s *Service) SetGeneral(g General) error {
	if g.Default.Mode == ModeDefault {
		g.Default.Mode = ModeManual
	}
	d, err := g.Default.check()
	if err != nil {
		return err
	}
	if g.CheckHours < 0 || g.CheckHours > 168 {
		return errors.New("check for updates every 1 to 168 hours")
	}
	if g.Self.WindowStart, g.Self.WindowEnd, err = checkWindow(g.Self.WindowStart, g.Self.WindowEnd); err != nil {
		return err
	}
	return s.update(func(set *Settings) error {
		set.Default, set.CheckHours, set.Self = d, g.CheckHours, g.Self
		return nil
	})
}

// ForgetServer removes a deleted server's policy and results.
func (s *Service) ForgetServer(server string) error {
	s.clearState(server)
	return s.update(func(set *Settings) error {
		delete(set.Servers, server)
		delete(set.Results, server)
		return nil
	})
}

func (s *Service) setState(server string, st State) {
	s.mu.Lock()
	s.state[server] = st
	s.mu.Unlock()
}

func (s *Service) clearState(server string) {
	s.mu.Lock()
	delete(s.state, server)
	s.mu.Unlock()
}

// ServerStatus is a server's policy and what's happening with its update.
type ServerStatus struct {
	// Policy is the server's own (Mode "" when it follows the default).
	Policy    Policy  `json:"policy"`
	Effective Policy  `json:"effective"`
	Default   Policy  `json:"default"`
	State     *State  `json:"state,omitempty"`
	Last      *Result `json:"last,omitempty"`
	Timezone  string  `json:"timezone"`
}

// Status returns a server's update policy and state.
func (s *Service) Status(server string) (ServerStatus, error) {
	set, err := s.load()
	if err != nil {
		return ServerStatus{}, err
	}
	st := ServerStatus{Policy: set.Servers[server], Effective: set.effective(server), Default: set.Default,
		Timezone: s.opts.ZoneName()}
	if r, ok := set.Results[server]; ok {
		st.Last = &r
	}
	s.mu.Lock()
	if x, ok := s.state[server]; ok {
		st.State = &x
	}
	s.mu.Unlock()
	return st, nil
}

// States returns what's happening with each server's automatic update.
func (s *Service) States() map[string]State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]State, len(s.state))
	for k, v := range s.state {
		out[k] = v
	}
	return out
}

// Run applies updates until ctx ends, looking every minute.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.tick(ctx)
	}
}

// tick applies at most one update: to the server that's waited longest
// whose window is open, or else to hlm.
func (s *Service) tick(ctx context.Context) {
	set, err := s.load()
	if err != nil {
		s.opts.Log.Warn("read update policies", "err", err)
		return
	}
	servers, err := s.opts.Servers.List(ctx)
	if err != nil {
		return // Docker unreachable: try again next time
	}
	now := s.opts.Now()
	loc := s.opts.Location()

	var ready []manager.Server
	live := map[string]bool{}
	for _, srv := range servers {
		p := set.effective(srv.Name)
		if p.Mode != ModeAuto || srv.Update == nil || srv.Update.State != manager.UpdateAvailable {
			continue
		}
		live[srv.Name] = true
		if r, ok := set.Results[srv.Name]; ok && !r.OK && now.Sub(r.Finished) < retryAfterFailure {
			next := r.Finished.Add(retryAfterFailure)
			s.setState(srv.Name, State{State: StateFailed, Message: "the last automatic update failed: " + r.Error, Next: &next})
			continue
		}
		if in, next := open(p.WindowStart, p.WindowEnd, now, loc); !in {
			s.setState(srv.Name, State{State: StateWaiting, Message: "waiting for the update window, " + p.WindowStart + "–" + p.WindowEnd, Next: &next})
			continue
		}
		if why := s.busy(srv.Name); why != "" {
			s.setState(srv.Name, State{State: StateBusy, Message: "waiting: " + why})
			continue
		}
		if p.OnlyWhenEmpty && s.opts.Players != nil {
			if n, known := s.opts.Players(srv.Name); known && n > 0 {
				s.setState(srv.Name, State{State: StatePlayers, Message: fmt.Sprintf("waiting for players to leave (%s online)", plural(n, "player"))})
				continue
			}
		}
		ready = append(ready, srv)
	}
	// Forget servers that no longer have an update waiting.
	s.mu.Lock()
	for name := range s.state {
		if !live[name] {
			delete(s.state, name)
		}
	}
	s.mu.Unlock()

	if len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
		for _, srv := range ready[1:] {
			s.setState(srv.Name, State{State: StateQueued, Message: "waiting for " + ready[0].Name + " to update first"})
		}
		s.updateServer(ctx, ready[0], set.effective(ready[0].Name))
		return
	}
	s.updateSelf(ctx, set, now, loc)
}

// busy says why a server can't be updated now, or "".
func (s *Service) busy(server string) string {
	switch {
	case s.opts.Servers.JobRunning(server):
		return "it's being installed, updated or reconfigured"
	case s.opts.Backups != nil && s.opts.Backups.Busy(server):
		return "it's being backed up or restored"
	case s.opts.Tasks != nil && s.opts.Tasks.Running(server):
		return "one of its scheduled tasks is running"
	}
	return ""
}

// updateServer applies one server's update: warn, back up, update.
func (s *Service) updateServer(ctx context.Context, srv manager.Server, p Policy) {
	name := srv.Name
	res := Result{Started: s.opts.Now()}
	s.setState(name, State{State: StateUpdating, Message: "checking the update"})
	finish := func(err error) {
		res.Finished = s.opts.Now()
		res.OK = err == nil
		if err != nil {
			res.Error = err.Error()
			s.opts.Log.Warn("automatic update failed", "server", name, "err", err)
		} else {
			s.opts.Log.Info("updated automatically", "server", name)
		}
		if err := s.update(func(set *Settings) error {
			set.Results[name] = res
			return nil
		}); err != nil {
			s.opts.Log.Warn("record automatic update", "server", name, "err", err)
		}
		s.audit("servers.auto_update", name, name, res)
		s.clearState(name)
	}

	// The update was found up to hours ago; make sure it's still there.
	upd, err := s.opts.Servers.CheckUpdate(ctx, name)
	if err != nil {
		finish(fmt.Errorf("check for the update: %w", err))
		return
	}
	if upd.State != manager.UpdateAvailable {
		s.clearState(name) // nothing to do after all; not a failure
		return
	}
	if p.Warn != "" && s.opts.Console != nil && srv.State == "running" {
		s.setState(name, State{State: StateUpdating, Message: "warned players; updating in " + plural(p.WarnMinutes, "minute")})
		if _, err := s.opts.Console.Send(ctx, name, "auto-update", p.Warn); err != nil {
			res.Steps = append(res.Steps, "couldn't warn players: "+err.Error())
		} else {
			res.Steps = append(res.Steps, "warned players")
		}
		if p.WarnMinutes > 0 {
			if err := s.opts.Sleep(ctx, time.Duration(p.WarnMinutes)*time.Minute); err != nil {
				finish(errors.New("hlm stopped before the update"))
				return
			}
		}
	}
	if p.Backup && s.opts.Backups != nil && s.opts.Backups.HasPlan(name) {
		s.setState(name, State{State: StateUpdating, Message: "backing up first"})
		run, err := s.opts.Backups.BackupNow(ctx, name, "update")
		if err != nil {
			finish(fmt.Errorf("the backup before updating failed, so the server wasn't updated: %w", err))
			return
		}
		res.Steps = append(res.Steps, "backed up as "+run.Key)
	}
	s.setState(name, State{State: StateUpdating, Message: "updating"})
	if _, err := s.opts.Servers.Update(ctx, name, func(manager.Event) {}); err != nil {
		finish(err)
		return
	}
	res.Steps = append(res.Steps, "updated")
	finish(nil)
}

// updateSelf updates hlm if it should and can now. hlm restarts if it
// works.
func (s *Service) updateSelf(ctx context.Context, set Settings, now time.Time, loc *time.Location) {
	if !set.Self.Auto || s.opts.Self == nil {
		return
	}
	st := s.opts.Self.Status()
	if !st.Available || !st.CanUpdate || st.Updating || st.Latest == nil {
		return
	}
	if r, ok := set.Results[SelfKey]; ok && !r.OK && now.Sub(r.Finished) < retryAfterFailure {
		return
	}
	if in, _ := open(set.Self.WindowStart, set.Self.WindowEnd, now, loc); !in {
		return
	}
	if why := s.opts.Servers.Busy(); why != "" {
		return // e.g. a server's being updated; try again next minute
	}
	s.opts.Log.Info("updating hlm automatically", "to", st.Latest.Version)
	res := Result{Started: now, Steps: []string{"updating to " + st.Latest.Version}}
	// Record the attempt first: if it works, hlm restarts straight after.
	res.OK, res.Finished = true, now
	_ = s.update(func(set *Settings) error { set.Results[SelfKey] = res; return nil })
	s.audit("system.auto_update", st.Latest.Version, "", res)
	err := s.opts.Self.Apply(ctx, func(selfupdate.Event) {})
	if err != nil {
		res.OK, res.Error = false, err.Error()
		s.audit("system.auto_update", st.Latest.Version, "", res)
		res.OK, res.Error, res.Finished = false, err.Error(), s.opts.Now()
		s.opts.Log.Warn("automatic update of hlm failed", "err", err)
		_ = s.update(func(set *Settings) error { set.Results[SelfKey] = res; return nil })
		if s.opts.Notifier != nil {
			s.opts.Notifier.Notify(notify.Event{
				Kind: notify.KindHLMUpdateFailed, Title: "Updating hlm to " + st.Latest.Version + " failed",
				Message: err.Error(), Link: "/system",
			})
		}
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// Visible filters states to the servers keep accepts.
func Visible(states map[string]State, keep func(string) bool) map[string]State {
	out := map[string]State{}
	for k, v := range states {
		if keep(k) {
			out[k] = v
		}
	}
	return out
}

func (s *Service) audit(action, target, server string, r Result) {
	if s.opts.Audit == nil {
		return
	}
	e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: action, Target: target, Server: server,
		Result: audit.ResultOK, Error: r.Error}
	if !r.OK {
		e.Result = audit.ResultError
	}
	if len(r.Steps) > 0 {
		e.Details = map[string]string{"steps": strings.Join(r.Steps, "; ")}
	}
	s.opts.Audit.Record(e)
}
