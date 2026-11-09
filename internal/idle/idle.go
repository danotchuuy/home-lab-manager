// Package idle stops servers nobody is playing on and starts them again
// when someone wants to: on a connection to the server's port (through a
// small wake listener container that takes its place while it's asleep), or
// from the public status page.
package idle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	settingsName = "idle"
	asleepName   = "idle-asleep"

	// MinMinutes and MaxMinutes bound how long a server has to be empty.
	MinMinutes = 5
	MaxMinutes = 24 * 60

	// warnFor is how long players are warned before the server stops.
	warnFor = time.Minute
	// retryListener is how long after a wake listener stops on its own
	// it's started again.
	retryListener = 5 * time.Minute
)

// Settings are a server's idle settings.
type Settings struct {
	// Minutes is how long it has to be empty before it's stopped; 0 is off.
	Minutes int `json:"minutes"`
	// WakeOnConnect starts it when someone connects to one of its ports.
	WakeOnConnect bool `json:"wakeOnConnect"`
	// WakeFromStatusPage shows a Start button for it on the status page.
	WakeFromStatusPage bool `json:"wakeFromStatusPage"`
}

// Asleep is a server hlm stopped because nobody was on it.
type Asleep struct {
	Since   time.Time `json:"since"`
	Minutes int       `json:"minutes"`
	// Listener is the wake listener container's ID, and ListenerError why
	// there isn't one running, if it should be.
	Listener      string `json:"listener,omitempty"`
	ListenerError string `json:"listenerError,omitempty"`
	// Address is the server's own LAN address as it was before it
	// stopped, for one on a macvlan/ipvlan network (Docker forgets an
	// address it chose when the container stops).
	Address *Address `json:"address,omitempty"`
}

// Address is a container's address on a network.
type Address struct {
	Network string `json:"network"`
	IP      string `json:"ip"`
	MAC     string `json:"mac,omitempty"`
}

// seenAddress is a running server's own LAN address, if it has one.
func seenAddress(info docker.ContainerJSON) *Address {
	if !lanAddressed(info) {
		return nil
	}
	n := info.HostConfig.NetworkMode
	ep, ok := info.NetworkSettings.Networks[n]
	if !ok || ep.IPAddress == "" {
		return nil
	}
	return &Address{Network: n, IP: ep.IPAddress, MAC: ep.MacAddress}
}

// Status is a server's idle settings and state.
type Status struct {
	Settings
	// IdleSince is when it was last seen empty with nobody joining since.
	IdleSince *time.Time `json:"idleSince,omitempty"`
	Asleep    *Asleep    `json:"asleep,omitempty"`
	// CanStop says whether it can be stopped when idle: it has to answer
	// its status query with a player count. Why says why not.
	CanStop bool   `json:"canStop"`
	Why     string `json:"why,omitempty"`
	// CanListen says whether it can be woken on a connection, and
	// ListenWhy why not.
	CanListen bool   `json:"canListen"`
	ListenWhy string `json:"listenWhy,omitempty"`
}

// Servers is what the service needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	JobRunning(name string) bool
}

// Docker is what the wake listener needs from a Docker host.
type Docker interface {
	InspectImage(ctx context.Context, ref string) (docker.Image, error)
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	ListContainers(ctx context.Context, labels ...string) ([]docker.ContainerSummary, error)
	CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error)
	CopyTo(ctx context.Context, id, dir string, tarStream io.Reader) error
	StartContainer(ctx context.Context, id string) error
	WaitContainer(ctx context.Context, id string) (int, error)
	Logs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
}

// Console sends servers commands.
type Console interface {
	Send(ctx context.Context, id, who, command string) (console.Result, error)
}

// Tasks says whether a server's scheduled task is running.
type Tasks interface {
	Running(server string) bool
}

// Options configures a Service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	// DockerFor is a Docker host's client, for wake listeners.
	DockerFor func(host string) (Docker, error)
	// Players says how many players are on a server and its limit, if
	// that's known.
	Players  func(server string) (online, max int, known bool)
	Console  Console
	Tasks    Tasks
	Notifier notify.Notifier
	Audit    audit.Recorder
	// Executable is hlm's own binary, copied into wake listeners; it has
	// to be a static Linux one.
	Executable string
	Log        *slog.Logger
	// Now and Sleep are for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Service stops idle servers and wakes them.
type Service struct {
	opts Options
	// listenerProblem says why wake listeners can't be used at all.
	listenerProblem string
	saveMu          sync.Mutex

	mu        sync.Mutex
	runCtx    context.Context
	idleSince map[string]time.Time
	busy      map[string]bool // going to sleep or waking
	watching  map[string]string
	// tried is when each sleeping server's listener was last started
	// again after it couldn't be.
	tried map[string]time.Time
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
	s := &Service{opts: o, runCtx: context.Background(), idleSince: map[string]time.Time{}, busy: map[string]bool{},
		watching: map[string]string{}, tried: map[string]time.Time{}}
	s.listenerProblem = checkExecutable(o.Executable)
	return s
}

// --- stored settings and state ---

func (s *Service) all() (map[string]Settings, error) {
	m := map[string]Settings{}
	_, err := s.opts.Secrets.Get(settingsName, &m)
	return m, err
}

func (s *Service) asleepAll() (map[string]Asleep, error) {
	m := map[string]Asleep{}
	_, err := s.opts.Secrets.Get(asleepName, &m)
	return m, err
}

func (s *Service) setAsleep(name string, a *Asleep) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	m, err := s.asleepAll()
	if err != nil {
		return err
	}
	if a == nil {
		if _, ok := m[name]; !ok {
			return nil
		}
		delete(m, name)
	} else {
		m[name] = *a
	}
	return s.opts.Secrets.Set(asleepName, m)
}

func (s *Service) asleepEntry(name string) (Asleep, bool) {
	m, _ := s.asleepAll()
	a, ok := m[name]
	return a, ok
}

// AsleepInfo says whether a server is asleep, and whether the status page
// may wake it.
func (s *Service) AsleepInfo(name string) (asleep, wakeFromStatusPage bool) {
	m, err := s.asleepAll()
	if err != nil {
		return false, false
	}
	if _, ok := m[name]; !ok {
		return false, false
	}
	set, _ := s.all()
	return true, set[name].WakeFromStatusPage
}

// AsleepSince lists the servers that are asleep, for the server list.
func (s *Service) AsleepSince() map[string]Asleep {
	m, _ := s.asleepAll()
	return m
}

// Status is a server's settings and state.
func (s *Service) Status(ctx context.Context, name string) (Status, error) {
	all, err := s.all()
	if err != nil {
		return Status{}, err
	}
	st := Status{Settings: all[name]}
	s.mu.Lock()
	if t, ok := s.idleSince[name]; ok {
		st.IdleSince = &t
	}
	s.mu.Unlock()
	if m, err := s.asleepAll(); err == nil {
		if a, ok := m[name]; ok {
			st.Asleep = &a
		}
	}
	if _, _, known := s.opts.Players(name); known {
		st.CanStop = true
	} else {
		st.Why = "hlm can only stop a server when it's empty if its status query counts players, and it isn't doing that now (it's stopped, starting, or its query has no player count)."
	}
	st.CanListen, st.ListenWhy = true, ""
	if s.listenerProblem != "" {
		st.CanListen, st.ListenWhy = false, s.listenerProblem
	} else if info, err := s.opts.Servers.Inspect(ctx, name); err == nil {
		var seen *Address
		if a, ok := s.asleepEntry(name); ok {
			seen = a.Address
		}
		if _, err := listenerPlan(info, seen); err != nil {
			st.CanListen, st.ListenWhy = false, err.Error()
		}
	}
	return st, nil
}

// Save changes a server's settings.
func (s *Service) Save(ctx context.Context, name string, in Settings) (Status, error) {
	if in.Minutes != 0 && (in.Minutes < MinMinutes || in.Minutes > MaxMinutes) {
		return Status{}, fmt.Errorf("stop it after %d to %d minutes without players", MinMinutes, MaxMinutes)
	}
	s.saveMu.Lock()
	all, err := s.all()
	if err == nil {
		if in == (Settings{}) {
			delete(all, name)
		} else {
			all[name] = in
		}
		err = s.opts.Secrets.Set(settingsName, all)
	}
	s.saveMu.Unlock()
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	if in.Minutes == 0 {
		delete(s.idleSince, name)
	}
	s.mu.Unlock()
	// Asleep: start or stop its listener to match.
	if m, err := s.asleepAll(); err == nil {
		if a, ok := m[name]; ok {
			if in.WakeOnConnect && a.Listener == "" {
				s.listen(ctx, name)
			} else if !in.WakeOnConnect && a.Listener != "" {
				s.removeListener(ctx, name, a.Listener)
				a.Listener, a.ListenerError = "", ""
				s.setAsleep(name, &a)
			}
		}
	}
	return s.Status(ctx, name)
}

// ForgetServer drops a deleted server's settings, and its listener.
func (s *Service) ForgetServer(name string) error {
	if m, err := s.asleepAll(); err == nil {
		if a, ok := m[name]; ok && a.Listener != "" {
			s.removeListener(context.Background(), name, a.Listener)
		}
	}
	if err := s.setAsleep(name, nil); err != nil {
		return err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all, err := s.all()
	if err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	return s.opts.Secrets.Set(settingsName, all)
}

// --- the loop ---

// Run checks servers every 30 seconds until ctx ends. At start it puts back
// the wake listeners of servers that are asleep.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	s.resume(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// resume watches the listeners of servers asleep since before hlm started,
// starting those that are gone.
func (s *Service) resume(ctx context.Context) {
	asleep, err := s.asleepAll()
	if err != nil {
		s.opts.Log.Warn("read sleeping servers", "err", err)
		return
	}
	all, _ := s.all()
	for name, a := range asleep {
		if !all[name].WakeOnConnect {
			continue
		}
		if a.Listener != "" && s.listenerRunning(ctx, name, a.Listener) {
			s.watch(name, a.Listener)
			continue
		}
		s.listen(ctx, name)
	}
}

// Check looks at each server once: counts how long it's been empty, puts
// it to sleep, or notices it was started by someone.
func (s *Service) Check(ctx context.Context) {
	all, err := s.all()
	if err != nil {
		s.opts.Log.Warn("read idle settings", "err", err)
		return
	}
	asleep, _ := s.asleepAll()
	now := s.opts.Now()
	// Started by someone else (by hand, Docker on boot, an update).
	for name, a := range asleep {
		info, err := s.opts.Servers.Inspect(ctx, name)
		if err != nil {
			if errors.Is(err, manager.ErrNotFound) {
				s.ForgetServer(name)
			}
			continue
		}
		if info.State.Running && !s.isBusy(name) {
			if a.Listener != "" {
				s.removeListener(ctx, name, a.Listener)
			}
			s.setAsleep(name, nil)
			s.opts.Log.Info("sleeping server was started", "server", name)
			continue
		}
		// Its listener couldn't start (its host's agent hadn't connected
		// yet when hlm started, say): try again now and then.
		if !info.State.Running && all[name].WakeOnConnect && a.Listener == "" && a.ListenerError != "" && s.retryDue(name, now) {
			s.listen(ctx, name)
		}
	}
	for name, set := range all {
		if set.Minutes == 0 {
			continue
		}
		if _, ok := asleep[name]; ok {
			continue
		}
		online, _, known := s.opts.Players(name)
		s.mu.Lock()
		since, counting := s.idleSince[name]
		switch {
		case !known || online > 0:
			delete(s.idleSince, name)
			counting = false
		case !counting:
			s.idleSince[name] = now
			since, counting = now, true
		}
		due := counting && now.Sub(since) >= time.Duration(set.Minutes)*time.Minute && !s.busy[name]
		if due {
			s.busy[name] = true
		}
		s.mu.Unlock()
		if due {
			go func() {
				defer s.setBusy(name, false)
				s.sleep(ctx, name, set)
			}()
		}
	}
}

// retryDue says whether it's time to try starting a sleeping server's
// listener again, and if so notes that it's being tried now.
func (s *Service) retryDue(name string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.tried[name]; ok && now.Sub(last) < retryListener {
		return false
	}
	s.tried[name] = now
	return true
}

func (s *Service) isBusy(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[name]
}

func (s *Service) setBusy(name string, b bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b {
		s.busy[name] = true
	} else {
		delete(s.busy, name)
	}
}

// sleep warns players, checks again that nobody's on, and stops the
// server.
func (s *Service) sleep(ctx context.Context, name string, set Settings) {
	if s.opts.Servers.JobRunning(name) || (s.opts.Tasks != nil && s.opts.Tasks.Running(name)) {
		return // tried again on the next check
	}
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil || !info.State.Running {
		return
	}
	if p := gameprofile.Match(info.Config.Image); p != nil && p.Console.Say != "" && s.opts.Console != nil {
		msg := fmt.Sprintf("%s Nobody has played for %d minutes: the server stops in a minute.", p.Console.Say, set.Minutes)
		if _, err := s.opts.Console.Send(ctx, name, "hlm (idle)", msg); err != nil {
			s.opts.Log.Debug("warn players before sleeping", "server", name, "err", err)
		}
	}
	if err := s.opts.Sleep(ctx, warnFor); err != nil {
		return
	}
	if online, _, known := s.opts.Players(name); !known || online > 0 {
		s.mu.Lock()
		delete(s.idleSince, name)
		s.mu.Unlock()
		return
	}
	if s.opts.Servers.JobRunning(name) || (s.opts.Tasks != nil && s.opts.Tasks.Running(name)) {
		return
	}
	// Recorded first, so the stop isn't taken for a crash or a start by
	// someone else.
	a := Asleep{Since: s.opts.Now(), Minutes: set.Minutes, Address: seenAddress(info)}
	if err := s.setAsleep(name, &a); err != nil {
		s.opts.Log.Warn("record sleeping server", "server", name, "err", err)
		return
	}
	if err := s.opts.Servers.Stop(ctx, name); err != nil {
		s.setAsleep(name, nil)
		s.record("servers.idle.sleep", name, nil, err)
		s.opts.Log.Warn("stop idle server", "server", name, "err", err)
		return
	}
	s.mu.Lock()
	delete(s.idleSince, name)
	s.mu.Unlock()
	s.opts.Log.Info("stopped idle server", "server", name, "minutes", set.Minutes)
	s.record("servers.idle.sleep", name, map[string]string{"minutes": fmt.Sprint(set.Minutes)}, nil)
	s.notify(notify.KindServerAsleep, name, name+" is asleep",
		fmt.Sprintf("Nobody played on %s for %d minutes, so hlm stopped it.", name, set.Minutes))
	if set.WakeOnConnect {
		s.listen(ctx, name)
	}
}

// ErrNotAsleep is a wake for a server that isn't asleep.
var ErrNotAsleep = errors.New("the server isn't asleep")

// Wake starts a sleeping server; why says what woke it, for the activity
// log.
func (s *Service) Wake(ctx context.Context, name, why string) error {
	s.mu.Lock()
	if s.busy[name] {
		s.mu.Unlock()
		return nil // already waking (or going to sleep)
	}
	s.busy[name] = true
	s.mu.Unlock()
	defer s.setBusy(name, false)
	m, err := s.asleepAll()
	if err != nil {
		return err
	}
	a, ok := m[name]
	if !ok {
		return ErrNotAsleep
	}
	// The listener has the server's ports; they have to be free first.
	if a.Listener != "" {
		s.removeListener(ctx, name, a.Listener)
	}
	if err := s.opts.Servers.Start(ctx, name); err != nil {
		s.record("servers.idle.wake", name, map[string]string{"by": why}, err)
		s.notify(notify.KindServerWakeFailed, name, name+" didn't wake",
			fmt.Sprintf("%s woke %s, but starting it failed: %v", why, name, err))
		a.Listener = ""
		s.setAsleep(name, &a)
		if all, _ := s.all(); all[name].WakeOnConnect {
			s.listen(ctx, name)
		}
		return err
	}
	s.setAsleep(name, nil)
	s.opts.Log.Info("woke server", "server", name, "by", why)
	s.record("servers.idle.wake", name, map[string]string{"by": why}, nil)
	return nil
}

func (s *Service) record(action, name string, details map[string]string, err error) {
	if s.opts.Audit == nil {
		return
	}
	e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: action, Target: name, Server: name,
		Result: audit.ResultOK, Details: details}
	if err != nil {
		e.Result, e.Error = audit.ResultError, err.Error()
	}
	s.opts.Audit.Record(e)
}

func (s *Service) notify(kind, name, title, msg string) {
	if s.opts.Notifier == nil {
		return
	}
	k, _ := notify.KindByName(kind)
	s.opts.Notifier.Notify(notify.Event{Kind: kind, Severity: k.Severity, Title: title, Message: msg, Server: name,
		Link: "/servers/" + name, Time: s.opts.Now()})
}

// describe says what woke a server, for people.
func describe(c Contact) string {
	what := "a connection"
	if c.About != "" {
		what = c.About
	}
	from := c.From
	if h, _, err := net.SplitHostPort(from); err == nil {
		from = h
	}
	return fmt.Sprintf("%s to port %s from %s", what, c.Port, from)
}

// BeforeStart runs before hlm starts a stopped server (by hand, a
// scheduled task, a restore): a sleeping server is awake from then on, and
// its wake listener has to free its ports first.
func (s *Service) BeforeStart(ctx context.Context, name string) {
	if s.isBusy(name) {
		return // Wake is starting it, and has seen to the listener
	}
	m, err := s.asleepAll()
	if err != nil {
		return
	}
	a, ok := m[name]
	if !ok {
		return
	}
	if a.Listener != "" {
		s.removeListener(ctx, name, a.Listener)
	}
	s.setAsleep(name, nil)
}

// RenameServer moves a server's idle settings to its new name, when it moves to
// another Docker host (its name gains or changes its @host).
// Being asleep was the old container's state, so it's dropped, as when a
// server is deleted.
func (s *Service) RenameServer(from, to string) error {
	if m, err := s.asleepAll(); err == nil {
		if a, ok := m[from]; ok && a.Listener != "" {
			s.removeListener(context.Background(), from, a.Listener)
		}
	}
	if err := s.setAsleep(from, nil); err != nil {
		return err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	_, err := s.opts.Secrets.MoveKey(settingsName, from, to)
	return err
}
