// Package ups watches hosts' UPSes on a Network UPS Tools server: it shows
// their state, sends notifications when they go on battery or come back,
// and stops a host's servers and stacks when its battery runs low, starting
// them again once the power has been back for a while.
package ups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/nut"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Secret names in the store.
const (
	secretSettings = "ups"
	secretStopped  = "ups-stopped"
)

// Settings are the NUT server and which UPS each host is on.
type Settings struct {
	// Address is the NUT server, nas.lan or nas.lan:3493.
	Address  string `json:"address"`
	Username string `json:"username,omitempty"`
	// Password is only ever sent to hlm: it's kept when left empty, and
	// HasPassword says there is one.
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	// Hosts are the UPS (its name on the NUT server) each host is on, by
	// host ID ("local" for hlm's own host). Hosts not listed aren't watched.
	Hosts map[string]string `json:"hosts"`

	// StopOnLow stops a host's running servers and stacks when its UPS
	// says the battery is low, or when the charge or runtime drop below
	// the limits below (0 leaves a limit out) while on battery.
	StopOnLow        bool `json:"stopOnLow"`
	StopBelowCharge  int  `json:"stopBelowCharge"`  // percent
	StopBelowRuntime int  `json:"stopBelowRuntime"` // minutes
	// Shutdown are the hosts (with hlm's agent) hlm then shuts down, by ID.
	Shutdown []string `json:"shutdown"`
	// ShutdownIdle shuts those hosts down sooner, once they've been on
	// battery for ShutdownIdleAfter minutes with none of hlm's servers or
	// stacks running, leaving the battery to hosts that have some.
	ShutdownIdle      bool `json:"shutdownIdle"`
	ShutdownIdleAfter int  `json:"shutdownIdleAfter"`
	// RestartOnPower starts what was stopped again once the UPS has had
	// mains power for RestartAfter minutes.
	RestartOnPower bool `json:"restartOnPower"`
	RestartAfter   int  `json:"restartAfter"`
}

// Defaults for new settings.
const (
	defaultStopBelowCharge  = 30
	defaultStopBelowRuntime = 5
	defaultRestartAfter     = 5
	// A minute on battery, so a flicker doesn't switch off a host that may
	// not come back on by itself.
	defaultShutdownIdleAfter = 1
)

func defaults() Settings {
	return Settings{Hosts: map[string]string{}, Shutdown: []string{}, StopOnLow: true, StopBelowCharge: defaultStopBelowCharge,
		StopBelowRuntime: defaultStopBelowRuntime, RestartOnPower: true, RestartAfter: defaultRestartAfter,
		ShutdownIdle: true, ShutdownIdleAfter: defaultShutdownIdleAfter}
}

// Enabled reports whether there is a NUT server and a host to watch.
func (s Settings) Enabled() bool { return s.Address != "" && len(s.Hosts) > 0 }

func (s Settings) query(ups string) nut.Query {
	return nut.Query{Address: s.Address, UPS: ups, Username: s.Username, Password: s.Password}
}

// Reader reads a UPS as seen from a host (through its agent, if it has one).
type Reader interface {
	UPS(ctx context.Context, host string, q nut.Query) (nut.Status, error)
	UPSList(ctx context.Context, host string, q nut.Query) ([]nut.UPS, error)
}

// Servers is what the watcher needs from the server manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
}

// Shutdowner shuts hosts down (through their agents).
type Shutdowner interface {
	CanShutdown(host string) bool
	Shutdown(ctx context.Context, host string) error
}

// Stacks starts and stops Compose stacks.
type Stacks interface {
	Act(ctx context.Context, name, action string) error
}

// Options configures a Service.
type Options struct {
	Store   *secrets.Store
	Reader  Reader
	Servers Servers
	Stacks  Stacks // optional
	// Hosts shuts hosts down; without it, none can be.
	Hosts    Shutdowner
	Notifier notify.Notifier
	Audit    audit.Recorder
	// HostName names a host in messages.
	HostName func(id string) string
	Log      *slog.Logger
	// Now is the clock (for tests).
	Now func() time.Time
}

// Service keeps the settings and watches the UPSes.
type Service struct {
	opts Options

	mu       sync.Mutex
	settings *Settings
	state    map[string]*hostState // by host ID
	stopped  map[string]Stopped    // by host ID, kept in the store
	actions  sync.WaitGroup        // stops and starts under way
	acting   map[string]bool       // hosts with a stop or start under way
}

// hostState is what the watcher remembers about a host's UPS.
type hostState struct {
	status    nut.Status
	err       string
	failures  int
	onBattery bool // as last notified
	lowDone   bool // the low battery was acted on, until mains power is back
	// batterySince is when it went on battery (or hlm first saw it on
	// battery); idleTried that an idle shutdown was tried since.
	batterySince time.Time
	idleTried    bool
	powerBack    time.Time // when mains power came back, while waiting to start servers
}

// Stopped is what hlm stopped on a host because its battery was low.
type Stopped struct {
	Servers []string  `json:"servers"`
	Stacks  []string  `json:"stacks"`
	At      time.Time `json:"at"`
	// ShutDown says hlm then told the host to shut down, and
	// ShutdownError why that failed.
	ShutDown      bool   `json:"shutDown,omitempty"`
	ShutdownError string `json:"shutdownError,omitempty"`
}

// HostUPS is a host's UPS, for the UI.
type HostUPS struct {
	Host   string      `json:"host"`
	UPS    string      `json:"ups"`
	Status *nut.Status `json:"status,omitempty"`
	Error  string      `json:"error,omitempty"`
	// Stopped is what hlm stopped on the host while the battery was low,
	// which it starts again when the power is back (if set to).
	Stopped *Stopped `json:"stopped,omitempty"`
	// RestartAt is when it will, once the power is back.
	RestartAt *time.Time `json:"restartAt,omitempty"`
}

// New returns the service; Run watches the UPSes.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HostName == nil {
		o.HostName = func(id string) string { return id }
	}
	s := &Service{opts: o, state: map[string]*hostState{}, stopped: map[string]Stopped{}, acting: map[string]bool{}}
	if o.Store != nil {
		_, _ = o.Store.Get(secretStopped, &s.stopped)
		if s.stopped == nil {
			s.stopped = map[string]Stopped{}
		}
	}
	return s
}

// Settings returns the settings, without the password.
func (s *Service) Settings() Settings {
	set := s.get()
	set.HasPassword, set.Password = set.Password != "", ""
	set.Hosts = maps.Clone(set.Hosts)
	set.Shutdown = slices.Clone(set.Shutdown)
	return set
}

func (s *Service) get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		set := defaults()
		if s.opts.Store != nil {
			_, _ = s.opts.Store.Get(secretSettings, &set)
		}
		if set.Hosts == nil {
			set.Hosts = map[string]string{}
		}
		if set.Shutdown == nil {
			set.Shutdown = []string{}
		}
		s.settings = &set
	}
	return *s.settings
}

// SetSettings checks and saves the settings. An empty password keeps the
// one saved before, unless the username is cleared too.
func (s *Service) SetSettings(in Settings) (Settings, error) {
	in.Address = strings.TrimSpace(in.Address)
	in.Username = strings.TrimSpace(in.Username)
	if in.Address != "" {
		addr, err := nut.Address(in.Address)
		if err != nil {
			return Settings{}, err
		}
		in.Address = addr
	}
	hosts := map[string]string{}
	for host, name := range in.Hosts {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, " \t\r\n\"") {
			return Settings{}, fmt.Errorf("the UPS name %q can't contain spaces or quotes", name)
		}
		hosts[host] = name
	}
	in.Hosts = hosts
	shutdown := []string{}
	for _, host := range in.Shutdown {
		if slices.Contains(shutdown, host) {
			continue
		}
		if _, ok := hosts[host]; !ok {
			return Settings{}, fmt.Errorf("choose a UPS for %s before shutting it down when the battery runs low", s.opts.HostName(host))
		}
		if s.opts.Hosts == nil || !s.opts.Hosts.CanShutdown(host) {
			return Settings{}, fmt.Errorf("hlm can't shut %s down: only hosts with hlm's agent can be", s.opts.HostName(host))
		}
		shutdown = append(shutdown, host)
	}
	sort.Strings(shutdown)
	in.Shutdown = shutdown
	if len(shutdown) > 0 && !in.StopOnLow {
		return Settings{}, errors.New("shutting hosts down needs stopping servers on: hlm shuts a host down after stopping its servers")
	}
	if len(hosts) > 0 && in.Address == "" {
		return Settings{}, errors.New("enter the NUT server's address")
	}
	switch {
	case in.StopBelowCharge < 0 || in.StopBelowCharge > 95:
		return Settings{}, errors.New("the charge to stop servers at must be between 0 and 95%")
	case in.StopBelowRuntime < 0 || in.StopBelowRuntime > 120:
		return Settings{}, errors.New("the runtime to stop servers at must be between 0 and 120 minutes")
	case in.ShutdownIdleAfter < 0 || in.ShutdownIdleAfter > 120:
		return Settings{}, errors.New("the time on battery before shutting idle hosts down must be between 0 and 120 minutes")
	case in.RestartAfter < 0 || in.RestartAfter > 24*60:
		return Settings{}, errors.New("the wait before starting servers again must be between 0 minutes and a day")
	}
	old := s.get()
	if in.Password == "" && in.Username != "" {
		in.Password = old.Password
	}
	if in.Username == "" {
		in.Password = ""
	}
	in.HasPassword = false
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.Store != nil {
		if err := s.opts.Store.Set(secretSettings, in); err != nil {
			return Settings{}, err
		}
	}
	s.settings = &in
	// Forget hosts no longer watched.
	for host := range s.state {
		if _, ok := hosts[host]; !ok {
			delete(s.state, host)
		}
	}
	out := in
	out.HasPassword, out.Password = in.Password != "", ""
	out.Hosts = maps.Clone(in.Hosts)
	out.Shutdown = slices.Clone(in.Shutdown)
	return out, nil
}

// List lists the NUT server's UPSes as a host sees it, with settings that
// may not be saved yet (an empty password uses the saved one).
func (s *Service) List(ctx context.Context, host string, in Settings) ([]nut.UPS, error) {
	if s.opts.Reader == nil {
		return nil, errors.New("UPSes can't be read")
	}
	addr, err := nut.Address(in.Address)
	if err != nil {
		return nil, err
	}
	in.Address = addr
	// The saved password only goes to the saved server, for its user.
	if saved := s.get(); in.Password == "" && in.Username != "" && in.Username == saved.Username && addr == saved.Address {
		in.Password = saved.Password
	}
	return s.opts.Reader.UPSList(ctx, host, in.query(""))
}

// Status returns each watched host's UPS, by host ID.
func (s *Service) Status() []HostUPS {
	set := s.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []HostUPS{}
	ids := slices.Sorted(maps.Keys(set.Hosts))
	for _, host := range ids {
		h := HostUPS{Host: host, UPS: set.Hosts[host]}
		if st, ok := s.state[host]; ok {
			if st.status.Available {
				cp := st.status
				h.Status = &cp
			}
			h.Error = st.err
			if !st.powerBack.IsZero() && set.RestartOnPower {
				at := st.powerBack.Add(time.Duration(set.RestartAfter) * time.Minute)
				h.RestartAt = &at
			}
		}
		if st, ok := s.stopped[host]; ok {
			cp := st
			h.Stopped = &cp
		}
		out = append(out, h)
	}
	return out
}

// Run checks the UPSes every interval until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Check(ctx)
		select {
		case <-ctx.Done():
			s.actions.Wait()
			return
		case <-t.C:
		}
	}
}

// failuresBeforeNotify is how many reads in a row have to fail before
// hlm sends a notification about it.
const failuresBeforeNotify = 4

// Check reads every watched host's UPS once, acting on what it finds.
func (s *Service) Check(ctx context.Context) {
	set := s.get()
	if !set.Enabled() || s.opts.Reader == nil {
		return
	}
	var wg sync.WaitGroup
	for host, ups := range set.Hosts {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			st, err := s.opts.Reader.UPS(cctx, host, set.query(ups))
			if ctx.Err() != nil {
				return
			}
			s.observe(ctx, set, host, st, err)
		})
	}
	wg.Wait()
}

// observe acts on a read of a host's UPS.
func (s *Service) observe(ctx context.Context, set Settings, host string, st nut.Status, err error) {
	now := s.opts.Now()
	name := s.opts.HostName(host)
	s.mu.Lock()
	hs := s.state[host]
	if hs == nil {
		hs = &hostState{}
		s.state[host] = hs
	}
	if err != nil {
		hs.err = err.Error()
		hs.status.Available = false
		hs.failures++
		failures := hs.failures
		// It's off because hlm shut it down.
		off := s.stopped[host].ShutDown
		s.mu.Unlock()
		if failures == failuresBeforeNotify && !off {
			s.notify(notify.KindUPSUnreachable, host, fmt.Sprintf("Can't read %s's UPS", name),
				fmt.Sprintf("Reading the UPS %s from the NUT server failed: %v", set.Hosts[host], err), nil)
		}
		return
	}
	if hs.failures >= failuresBeforeNotify && s.opts.Notifier != nil {
		s.opts.Notifier.Clear(key(notify.KindUPSUnreachable, host))
	}
	hs.err, hs.failures, hs.status = "", 0, st
	wasOnBattery := hs.onBattery
	hs.onBattery = st.OnBattery
	busy := s.acting[host]
	switch {
	case st.OnBattery && hs.batterySince.IsZero():
		hs.batterySince = now
	case !st.OnBattery:
		hs.batterySince, hs.idleTried = time.Time{}, false
	}
	if !st.OnBattery && !busy {
		hs.lowDone = false
		// Nothing will start what was stopped: let it go.
		if _, ok := s.stopped[host]; ok && !set.RestartOnPower {
			delete(s.stopped, host)
			s.saveStopped()
		}
	}
	_, stopped := s.stopped[host]
	low := st.OnBattery && lowBattery(set, st) && !hs.lowDone && !busy
	if low {
		hs.lowDone = true
	}
	var restart bool
	switch {
	case st.OnBattery:
		hs.powerBack = time.Time{}
	case stopped && hs.powerBack.IsZero():
		hs.powerBack = now
	}
	if stopped && !st.OnBattery && set.RestartOnPower && !busy && !st.LowBattery &&
		!now.Before(hs.powerBack.Add(time.Duration(set.RestartAfter)*time.Minute)) {
		restart = true
	}
	stop := low && set.StopOnLow
	// Idle on battery: shut it down now, if none of hlm's servers run there.
	idle := !stop && !low && st.OnBattery && !busy && !hs.lowDone && !hs.idleTried && set.StopOnLow && set.ShutdownIdle &&
		slices.Contains(set.Shutdown, host) && s.opts.Hosts != nil && !s.stopped[host].ShutDown &&
		!now.Before(hs.batterySince.Add(time.Duration(set.ShutdownIdleAfter)*time.Minute))
	if stop || restart || idle {
		s.acting[host] = true
	}
	s.mu.Unlock()

	fields := statusFields(st)
	switch {
	case st.OnBattery && !wasOnBattery:
		s.notify(notify.KindPowerOnBattery, host, fmt.Sprintf("%s is on battery", name),
			fmt.Sprintf("The UPS %s lost mains power.", set.Hosts[host]), fields)
	case !st.OnBattery && wasOnBattery:
		msg := fmt.Sprintf("The UPS %s has mains power again.", set.Hosts[host])
		if rec := s.Stopped(host); rec != nil && describe(*rec) != "" && set.RestartOnPower {
			msg += fmt.Sprintf(" hlm will start the servers it stopped in %d minutes, if the power stays on.", set.RestartAfter)
		}
		s.notify(notify.KindPowerRestored, host, fmt.Sprintf("%s has mains power again", name), msg, fields)
	}
	switch {
	case stop:
		s.actions.Go(func() { s.stopHost(ctx, host, name, set, st) })
	case low:
		s.notify(notify.KindPowerLow, host, fmt.Sprintf("%s's UPS battery is low", name),
			fmt.Sprintf("The UPS %s's battery is low. hlm isn't set to stop servers.", set.Hosts[host]), fields)
	case restart:
		s.actions.Go(func() { s.startHost(ctx, host, name) })
	case idle:
		s.actions.Go(func() { s.idleShutdown(ctx, host, name, set, st) })
	}
}

// idleShutdown shuts a host on battery down if none of hlm's servers or
// stacks are running there; otherwise it's checked again next time.
func (s *Service) idleShutdown(ctx context.Context, host, name string, set Settings, st nut.Status) {
	defer s.done(host)
	ctx = context.WithoutCancel(ctx)
	servers, stacks, err := s.running(ctx, host)
	if err != nil {
		s.opts.Log.Warn("UPS on battery: list the host's servers", "host", host, "err", err)
		return
	}
	if len(servers)+len(stacks) > 0 {
		return
	}
	err = s.opts.Hosts.Shutdown(ctx, host)
	s.recordShutdown(host, err)
	s.mu.Lock()
	if hs := s.state[host]; hs != nil {
		hs.idleTried = true
	}
	rec, ok := s.stopped[host]
	if !ok {
		rec = Stopped{At: s.opts.Now().UTC(), Servers: []string{}, Stacks: []string{}}
	}
	if err != nil {
		rec.ShutdownError = err.Error()
	} else {
		rec.ShutDown, rec.ShutdownError = true, ""
	}
	s.stopped[host] = rec
	s.saveStopped()
	s.mu.Unlock()
	title := fmt.Sprintf("Shut %s down to save the battery", name)
	msg := fmt.Sprintf("%s is on battery (the UPS %s) with none of hlm's servers running, so hlm shut it down to leave the battery to the others.", name, set.Hosts[host])
	if err != nil {
		title = fmt.Sprintf("Couldn't shut %s down", name)
		msg = fmt.Sprintf("%s is on battery (the UPS %s) with none of hlm's servers running, but shutting it down failed: %v.", name, set.Hosts[host], err)
		s.opts.Log.Warn("UPS on battery: shut the idle host down", "host", host, "err", err)
	}
	s.notify(notify.KindPowerShutdown, host, title, msg, statusFields(st))
}

// lowBattery reports whether a UPS on battery is low enough to stop the
// host's servers.
func lowBattery(set Settings, st nut.Status) bool {
	if st.LowBattery {
		return true
	}
	if set.StopBelowCharge > 0 && st.Charge != nil && *st.Charge < float64(set.StopBelowCharge) {
		return true
	}
	return set.StopBelowRuntime > 0 && st.Runtime != nil && *st.Runtime < float64(set.StopBelowRuntime*60)
}

// stopHost stops a host's running servers and stacks, remembering them
// to start again.
func (s *Service) stopHost(ctx context.Context, host, name string, set Settings, st nut.Status) {
	defer s.done(host)
	// Stopping has to finish even if hlm is shutting down.
	ctx = context.WithoutCancel(ctx)
	servers, stacks, err := s.running(ctx, host)
	if err != nil {
		s.opts.Log.Warn("UPS low: list the host's servers", "host", host, "err", err)
	}
	// Stopped since the power went (with hlm restarting meanwhile, or the
	// power back too briefly to start them), and now.
	s.mu.Lock()
	prev, hadPrev := s.stopped[host]
	s.mu.Unlock()
	rec := Stopped{At: s.opts.Now().UTC(), Servers: []string{}, Stacks: []string{}}
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	stopOne := func(what, id string, stop func() error) {
		err := stop()
		s.record("ups.stop", what, id, host, err)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", id, err))
			return
		}
		if what == "stack" {
			rec.Stacks = append(rec.Stacks, id)
		} else {
			rec.Servers = append(rec.Servers, id)
		}
	}
	// In parallel: the battery is running out.
	for _, id := range servers {
		wg.Go(func() { stopOne("server", id, func() error { return s.opts.Servers.Stop(ctx, id) }) })
	}
	for _, stk := range stacks {
		wg.Go(func() { stopOne("stack", stk, func() error { return s.opts.Stacks.Act(ctx, stk, "stop") }) })
	}
	wg.Wait()
	now := rec
	if hadPrev {
		rec.At = prev.At
		rec.Servers = union(prev.Servers, rec.Servers)
		rec.Stacks = union(prev.Stacks, rec.Stacks)
	}
	shutdown := slices.Contains(set.Shutdown, host) && s.opts.Hosts != nil
	if shutdown {
		err := s.opts.Hosts.Shutdown(ctx, host)
		s.recordShutdown(host, err)
		if err != nil {
			s.opts.Log.Warn("UPS low: shut the host down", "host", host, "err", err)
			rec.ShutdownError = err.Error()
		} else {
			rec.ShutDown = true
		}
	}
	sort.Strings(rec.Servers)
	sort.Strings(rec.Stacks)
	s.mu.Lock()
	s.stopped[host] = rec
	s.saveStopped()
	s.mu.Unlock()
	if hadPrev && len(now.Servers)+len(now.Stacks)+len(failed) == 0 && !shutdown {
		return // already said
	}

	what := describe(rec)
	msg := fmt.Sprintf("The UPS %s's battery is low, so hlm stopped %s.", set.Hosts[host], what)
	if what == "" {
		msg = fmt.Sprintf("The UPS %s's battery is low. Nothing was running for hlm to stop.", set.Hosts[host])
	}
	if len(failed) > 0 {
		msg += " Stopping these failed: " + strings.Join(failed, ", ") + "."
	}
	switch {
	case rec.ShutDown:
		msg += fmt.Sprintf(" Then it told %s to shut down.", name)
	case rec.ShutdownError != "":
		msg += fmt.Sprintf(" Shutting %s down failed: %s.", name, rec.ShutdownError)
	}
	if set.RestartOnPower && what != "" {
		msg += " They'll be started again once the power has been back for a while"
		if rec.ShutDown {
			msg += " and the host is up again"
		}
		msg += "."
	}
	s.notify(notify.KindPowerLow, host, fmt.Sprintf("%s's UPS battery is low", name), msg, statusFields(st))
}

// startHost starts what stopHost stopped.
func (s *Service) startHost(ctx context.Context, host, name string) {
	defer s.done(host)
	s.mu.Lock()
	rec := s.stopped[host]
	s.mu.Unlock()
	var failed []string
	for _, stk := range rec.Stacks {
		err := s.opts.Stacks.Act(ctx, stk, "start")
		s.record("ups.start", "stack", stk, host, err)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", stk, err))
		}
	}
	for _, id := range rec.Servers {
		err := s.opts.Servers.Start(ctx, id)
		s.record("ups.start", "server", id, host, err)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", id, err))
		}
	}
	if ctx.Err() != nil {
		return // hlm is stopping: try again when it's back
	}
	s.mu.Lock()
	delete(s.stopped, host)
	if hs := s.state[host]; hs != nil {
		hs.powerBack = time.Time{}
	}
	s.saveStopped()
	s.mu.Unlock()
	if what := describe(rec); what != "" {
		msg := fmt.Sprintf("The power stayed on, so hlm started %s again.", what)
		if len(failed) > 0 {
			msg += " Starting these failed: " + strings.Join(failed, ", ") + "."
		}
		s.notify(notify.KindPowerRestored, host, fmt.Sprintf("Started %s's servers again", name), msg, nil)
	}
}

// Stopped is what hlm stopped on a host, if anything.
func (s *Service) Stopped(host string) *Stopped {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.stopped[host]
	if !ok {
		return nil
	}
	return &rec
}

// Forget drops what hlm stopped on a host, so it isn't started again.
func (s *Service) Forget(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stopped, host)
	if hs := s.state[host]; hs != nil {
		hs.powerBack = time.Time{}
	}
	s.saveStopped()
}

// running lists a host's running servers that aren't in a stack, and its
// stacks with a running service.
func (s *Service) running(ctx context.Context, host string) (servers, stacks []string, err error) {
	list, err := s.opts.Servers.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, srv := range list {
		if srv.Host != host || srv.State != "running" {
			continue
		}
		if srv.Stack != "" {
			if s.opts.Stacks != nil && !seen[srv.Stack] {
				seen[srv.Stack] = true
				stacks = append(stacks, srv.Stack)
			}
			continue
		}
		servers = append(servers, srv.ID)
	}
	return servers, stacks, nil
}

func (s *Service) done(host string) {
	s.mu.Lock()
	delete(s.acting, host)
	s.mu.Unlock()
}

// saveStopped keeps what was stopped, so it's started again after hlm
// restarts (with the power, say). s.mu is held.
func (s *Service) saveStopped() {
	if s.opts.Store == nil {
		return
	}
	if err := s.opts.Store.Set(secretStopped, s.stopped); err != nil {
		s.opts.Log.Warn("save the servers stopped for a low UPS battery", "err", err)
	}
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func describe(rec Stopped) string {
	var parts []string
	if n := len(rec.Servers); n > 0 {
		parts = append(parts, plural(n, "server")+" ("+strings.Join(rec.Servers, ", ")+")")
	}
	if n := len(rec.Stacks); n > 0 {
		parts = append(parts, plural(n, "stack")+" ("+strings.Join(rec.Stacks, ", ")+")")
	}
	return strings.Join(parts, " and ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func statusFields(st nut.Status) map[string]string {
	f := map[string]string{}
	if st.Charge != nil {
		f["Battery"] = fmt.Sprintf("%.0f%%", *st.Charge)
	}
	if st.Runtime != nil {
		f["Runtime"] = (time.Duration(*st.Runtime) * time.Second).Round(time.Second).String()
	}
	if st.Load != nil {
		f["Load"] = fmt.Sprintf("%.0f%%", *st.Load)
	}
	return f
}

func key(kind, host string) string { return kind + ":" + host }

func (s *Service) notify(kind, host, title, msg string, fields map[string]string) {
	if s.opts.Notifier == nil {
		return
	}
	k, _ := notify.KindByName(kind)
	if len(fields) == 0 {
		fields = nil
	}
	s.opts.Notifier.Notify(notify.Event{Kind: kind, Severity: k.Severity, Title: title, Message: msg,
		Link: "/host?host=" + url.QueryEscape(host), Fields: fields, Time: s.opts.Now(), Key: key(kind, host)})
}

// recordShutdown audits shutting a host down.
func (s *Service) recordShutdown(host string, err error) {
	if s.opts.Audit == nil {
		return
	}
	e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: "ups.shutdown", Target: host,
		Result: audit.ResultOK, Details: map[string]string{"host": host, "reason": "UPS battery low"}}
	if err != nil {
		e.Result, e.Error = audit.ResultError, err.Error()
	}
	s.opts.Audit.Record(e)
}

// record audits stopping or starting a server or stack (what).
func (s *Service) record(action, what, target, host string, err error) {
	if s.opts.Audit == nil {
		return
	}
	e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: action, Target: target,
		Result: audit.ResultOK, Details: map[string]string{"host": host, "reason": "UPS battery low"}}
	if action == "ups.start" {
		e.Details["reason"] = "power back"
	}
	if what == "server" {
		e.Server = target
	} else {
		e.Details["stack"] = target
	}
	if err != nil {
		e.Result, e.Error = audit.ResultError, err.Error()
	}
	s.opts.Audit.Record(e)
}
