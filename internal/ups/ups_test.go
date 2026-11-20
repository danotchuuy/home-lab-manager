package ups

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/nut"
	"github.com/danotchuuy/home-lab-manager/internal/nut/nuttest"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// direct reads UPSes from hlm, recording which host asked.
type direct struct {
	mu    sync.Mutex
	hosts []string
	fail  map[string]error
}

func (d *direct) UPS(ctx context.Context, host string, q nut.Query) (nut.Status, error) {
	d.mu.Lock()
	d.hosts = append(d.hosts, host)
	err := d.fail[host]
	d.mu.Unlock()
	if err != nil {
		return nut.Status{}, err
	}
	return nut.Fetch(ctx, q)
}

func (d *direct) UPSList(ctx context.Context, host string, q nut.Query) ([]nut.UPS, error) {
	return nut.List(ctx, q)
}

type fakeServers struct {
	mu      sync.Mutex
	list    []manager.Server
	stopped []string
	started []string
	failOn  string
}

func (f *fakeServers) List(context.Context) ([]manager.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), nil
}

func (f *fakeServers) set(id, state string) {
	for i := range f.list {
		if f.list[i].ID == id {
			f.list[i].State = state
		}
	}
}

func (f *fakeServers) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == f.failOn {
		return errors.New("boom")
	}
	f.stopped = append(f.stopped, id)
	f.set(id, "exited")
	return nil
}

func (f *fakeServers) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	f.set(id, "running")
	return nil
}

type fakeStacks struct {
	mu      sync.Mutex
	acts    []string
	servers *fakeServers
}

func (f *fakeStacks) Act(_ context.Context, name, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, action+" "+name)
	f.servers.mu.Lock()
	defer f.servers.mu.Unlock()
	for i, srv := range f.servers.list {
		if srv.Stack == name {
			f.servers.list[i].State = map[string]string{"stop": "exited", "start": "running"}[action]
		}
	}
	return nil
}

type events struct {
	mu   sync.Mutex
	list []notify.Event
}

func (e *events) Notify(ev notify.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}
func (e *events) Clear(string) {}

func (e *events) kinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.list {
		out = append(out, ev.Kind)
	}
	return out
}

func (e *events) last() notify.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.list[len(e.list)-1]
}

type fakeHosts struct {
	mu       sync.Mutex
	agents   map[string]bool
	shutDown []string
	// stoppedFirst records whether the servers were stopped when the
	// host was told to shut down.
	stoppedFirst bool
	servers      *fakeServers
	err          error
}

func (f *fakeHosts) CanShutdown(host string) bool { return f.agents[host] }

func (f *fakeHosts) Shutdown(_ context.Context, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.servers.mu.Lock()
	f.stoppedFirst = len(f.servers.stopped) > 0
	f.servers.mu.Unlock()
	f.shutDown = append(f.shutDown, host)
	return nil
}

type env struct {
	upsd    *nuttest.Server
	reader  *direct
	servers *fakeServers
	stacks  *fakeStacks
	hosts   *fakeHosts
	events  *events
	store   *secrets.Store
	now     time.Time
	svc     *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := secrets.Open(filepath.Join(t.TempDir(), "secrets"), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		upsd:   nuttest.New(t),
		reader: &direct{fail: map[string]error{}},
		servers: &fakeServers{list: []manager.Server{
			{ID: "valheim@nas", Host: "nas", State: "running"},
			{ID: "minecraft@nas", Host: "nas", State: "running"},
			{ID: "palworld@nas", Host: "nas", State: "exited"},
			{ID: "db@nas", Host: "nas", State: "running", Stack: "wiki"},
			{ID: "web@nas", Host: "nas", State: "running", Stack: "wiki"},
			{ID: "factorio", Host: manager.LocalHost, State: "running"},
		}},
		events: &events{},
		store:  store,
		now:    time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC),
	}
	e.stacks = &fakeStacks{servers: e.servers}
	e.hosts = &fakeHosts{agents: map[string]bool{"nas": true}, servers: e.servers}
	e.upsd.Set("rack", map[string]string{"ups.status": "OL", "battery.charge": "100", "battery.runtime": "1800"})
	e.svc = e.service()
	return e
}

func (e *env) service() *Service {
	return New(Options{Store: e.store, Reader: e.reader, Servers: e.servers, Stacks: e.stacks, Hosts: e.hosts, Notifier: e.events,
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return e.now }})
}

// check runs a check and waits for any stop or start it began.
func (e *env) check(t *testing.T) {
	t.Helper()
	e.svc.Check(context.Background())
	e.svc.actions.Wait()
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	got := e.svc.Settings()
	if !got.StopOnLow || got.StopBelowCharge != 30 || !got.RestartOnPower || got.Enabled() {
		t.Errorf("defaults = %+v", got)
	}
	if _, err := e.svc.SetSettings(Settings{Hosts: map[string]string{"nas": "rack"}}); err == nil {
		t.Error("hosts without a server saved")
	}
	if _, err := e.svc.SetSettings(Settings{Address: "nas.lan", Hosts: map[string]string{"nas": "a rack"}}); err == nil {
		t.Error("a UPS name with a space saved")
	}
	out, err := e.svc.SetSettings(Settings{Address: "nas.lan", Username: "mon", Password: "pw",
		Hosts: map[string]string{"nas": " rack ", "local": ""}, StopBelowCharge: 20})
	if err != nil {
		t.Fatal(err)
	}
	if out.Address != "nas.lan:3493" || out.Password != "" || !out.HasPassword || len(out.Hosts) != 1 || out.Hosts["nas"] != "rack" {
		t.Errorf("saved = %+v", out)
	}
	// An empty password keeps the saved one; the store keeps it all.
	if _, err := e.svc.SetSettings(Settings{Address: "nas.lan", Username: "mon", Hosts: map[string]string{"nas": "rack"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.service().get(); got.Password != "pw" || got.Hosts["nas"] != "rack" {
		t.Errorf("reloaded = %+v", got)
	}
	// No username, no password.
	if _, err := e.svc.SetSettings(Settings{Address: "nas.lan", Hosts: map[string]string{"nas": "rack"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.get(); got.Password != "" {
		t.Errorf("password kept without a username")
	}
}

func TestLowBatteryStopsAndRestarts(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"},
		StopOnLow: true, StopBelowCharge: 30, StopBelowRuntime: 5, RestartOnPower: true, RestartAfter: 5}); err != nil {
		t.Fatal(err)
	}
	e.check(t)
	if st := e.svc.Status(); len(st) != 1 || st[0].Status == nil || !st[0].Status.OnLine {
		t.Fatalf("status = %+v", st)
	}
	if len(e.events.kinds()) != 0 {
		t.Errorf("events on mains power: %v", e.events.kinds())
	}

	// On battery, still plenty left.
	e.upsd.Set("rack", map[string]string{"ups.status": "OB DISCHRG", "battery.charge": "80", "battery.runtime": "1500"})
	e.check(t)
	e.check(t)
	if k := e.events.kinds(); !slices.Equal(k, []string{notify.KindPowerOnBattery}) {
		t.Errorf("events = %v", k)
	}
	if len(e.servers.stopped) != 0 {
		t.Errorf("stopped with charge left: %v", e.servers.stopped)
	}

	// The runtime drops under 5 minutes: the host's running servers and
	// stacks stop, not the local host's.
	e.upsd.Set("rack", map[string]string{"battery.charge": "45", "battery.runtime": "250"})
	e.check(t)
	slices.Sort(e.servers.stopped)
	if !slices.Equal(e.servers.stopped, []string{"minecraft@nas", "valheim@nas"}) || !slices.Equal(e.stacks.acts, []string{"stop wiki"}) {
		t.Fatalf("stopped %v, stacks %v", e.servers.stopped, e.stacks.acts)
	}
	ev := e.events.last()
	if ev.Kind != notify.KindPowerLow || !strings.Contains(ev.Message, "2 servers (minecraft@nas, valheim@nas) and 1 stack (wiki)") || ev.Fields["Battery"] != "45%" {
		t.Errorf("low event = %+v", ev)
	}
	// It doesn't stop them again.
	e.check(t)
	if len(e.servers.stopped) != 2 || len(e.stacks.acts) != 1 {
		t.Errorf("stopped again: %v %v", e.servers.stopped, e.stacks.acts)
	}
	if st := e.svc.Status(); st[0].Stopped == nil || len(st[0].Stopped.Servers) != 2 {
		t.Errorf("status = %+v", st[0])
	}

	// Power's back, but not for long enough; hlm restarts meanwhile.
	e.upsd.Set("rack", map[string]string{"ups.status": "OL CHRG", "battery.charge": "46"})
	e.check(t)
	if ev := e.events.last(); ev.Kind != notify.KindPowerRestored || !strings.Contains(ev.Message, "in 5 minutes") {
		t.Errorf("restored event = %+v", ev)
	}
	e.now = e.now.Add(3 * time.Minute)
	e.svc = e.service()
	e.check(t)
	if len(e.servers.started) != 0 {
		t.Fatalf("started early: %v", e.servers.started)
	}
	if st := e.svc.Status(); st[0].Stopped == nil || st[0].RestartAt == nil || !st[0].RestartAt.Equal(e.now.Add(5*time.Minute)) {
		t.Errorf("status after restarting hlm = %+v", st[0])
	}
	e.now = e.now.Add(5 * time.Minute)
	e.check(t)
	slices.Sort(e.servers.started)
	if !slices.Equal(e.servers.started, []string{"minecraft@nas", "valheim@nas"}) || !slices.Equal(e.stacks.acts, []string{"stop wiki", "start wiki"}) {
		t.Errorf("started %v, stacks %v", e.servers.started, e.stacks.acts)
	}
	if ev := e.events.last(); ev.Kind != notify.KindPowerRestored || !strings.Contains(ev.Message, "started 2 servers") {
		t.Errorf("started event = %+v", ev)
	}
	if st := e.svc.Status(); st[0].Stopped != nil || st[0].RestartAt != nil {
		t.Errorf("status after starting = %+v", st[0])
	}
	if got := e.service().Status(); got[0].Stopped != nil {
		t.Errorf("stopped list kept: %+v", got[0])
	}
}

func TestPowerFlickerWaits(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"},
		StopOnLow: true, RestartOnPower: true, RestartAfter: 5}); err != nil {
		t.Fatal(err)
	}
	// The UPS's own low-battery flag stops them, with no limits set.
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB", "battery.charge": "90"})
	e.check(t)
	if len(e.servers.stopped) != 2 {
		t.Fatalf("stopped = %v", e.servers.stopped)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	e.now = e.now.Add(4 * time.Minute)
	// Off again before the wait is over: the wait starts afresh.
	e.upsd.Set("rack", map[string]string{"ups.status": "OB"})
	e.check(t)
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	e.now = e.now.Add(4 * time.Minute)
	e.check(t)
	if len(e.servers.started) != 0 {
		t.Fatalf("started after a flicker: %v", e.servers.started)
	}
	e.now = e.now.Add(time.Minute)
	e.check(t)
	if len(e.servers.started) != 2 {
		t.Errorf("started = %v", e.servers.started)
	}
}

func TestNoStopWhenOff(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"}, StopBelowCharge: 30}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB", "battery.charge": "10"})
	e.check(t)
	e.check(t)
	if len(e.servers.stopped) != 0 || len(e.stacks.acts) != 0 {
		t.Errorf("stopped with stopping off: %v %v", e.servers.stopped, e.stacks.acts)
	}
	// Still said once.
	if k := e.events.kinds(); !slices.Equal(k, []string{notify.KindPowerOnBattery, notify.KindPowerLow}) {
		t.Errorf("events = %v", k)
	}
}

func TestEachOutageStopsWithoutRestart(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"}, StopOnLow: true}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	if len(e.servers.stopped) != 2 {
		t.Fatalf("stopped = %v", e.servers.stopped)
	}
	// Back on mains: nothing will start them, so the list goes.
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	if st := e.svc.Status(); st[0].Stopped != nil {
		t.Errorf("kept with restarting off: %+v", st[0].Stopped)
	}
	// Someone starts one by hand; the next outage stops it.
	e.servers.Start(context.Background(), "valheim@nas")
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	if len(e.servers.stopped) != 3 || e.servers.stopped[2] != "valheim@nas" {
		t.Errorf("second outage stopped %v", e.servers.stopped)
	}
}

func TestStopMergesAfterHLMRestarts(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"},
		StopOnLow: true, RestartOnPower: true, RestartAfter: 5}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	lows := func() int {
		n := 0
		for _, k := range e.events.kinds() {
			if k == notify.KindPowerLow {
				n++
			}
		}
		return n
	}
	// hlm restarts, still on battery: nothing new to stop or say.
	e.svc = e.service()
	e.check(t)
	if lows() != 1 {
		t.Errorf("low events = %d", lows())
	}
	// It restarts again after someone started a server: that one stops
	// too, and both lists are kept.
	e.servers.Start(context.Background(), "palworld@nas")
	e.svc = e.service()
	e.check(t)
	st := e.svc.Status()[0].Stopped
	if st == nil || !slices.Equal(st.Servers, []string{"minecraft@nas", "palworld@nas", "valheim@nas"}) || !slices.Equal(st.Stacks, []string{"wiki"}) {
		t.Errorf("stopped = %+v", st)
	}
	if lows() != 2 {
		t.Errorf("low events = %d", lows())
	}
}

func TestUnreachable(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack", "local": "missing"}}); err != nil {
		t.Fatal(err)
	}
	e.reader.fail["nas"] = errors.New("agent not connected")
	for range failuresBeforeNotify + 2 {
		e.check(t)
	}
	var unreachable []notify.Event
	for _, ev := range e.events.list {
		if ev.Kind == notify.KindUPSUnreachable {
			unreachable = append(unreachable, ev)
		}
	}
	if len(unreachable) != 2 {
		t.Fatalf("unreachable events = %+v", unreachable)
	}
	st := e.svc.Status()
	if len(st) != 2 || st[0].Host != "local" || !strings.Contains(st[0].Error, `no UPS called "missing"`) || st[1].Error != "agent not connected" || st[1].Status != nil {
		t.Errorf("status = %+v", st)
	}
	// Both hosts were read, each as itself.
	slices.Sort(e.reader.hosts)
	if e.reader.hosts[0] != "local" || e.reader.hosts[len(e.reader.hosts)-1] != "nas" {
		t.Errorf("read as %v", e.reader.hosts)
	}
}

func TestListKeepsPasswordToItsServer(t *testing.T) {
	e := newEnv(t)
	e.upsd.Username, e.upsd.Password = "mon", "pw"
	other := nuttest.New(t)
	other.Username, other.Password = "mon", "pw"
	other.Set("desk", nil)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Username: "mon", Password: "pw", Hosts: map[string]string{"nas": "rack"}}); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(context.Background(), "nas", Settings{Address: e.upsd.Addr, Username: "mon"})
	if err != nil || len(list) != 1 || list[0].Name != "rack" {
		t.Errorf("saved server: %+v %v", list, err)
	}
	if _, err := e.svc.List(context.Background(), "nas", Settings{Address: other.Addr, Username: "mon"}); err == nil {
		t.Error("the saved password was sent to another server")
	}
}

func TestShutdownSettings(t *testing.T) {
	e := newEnv(t)
	base := func() Settings {
		return Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack", "local": "rack"}, StopOnLow: true}
	}
	in := base()
	in.Shutdown = []string{"local"}
	if _, err := e.svc.SetSettings(in); err == nil || !strings.Contains(err.Error(), "only hosts with hlm's agent") {
		t.Errorf("local host: %v", err)
	}
	in = base()
	in.Shutdown = []string{"other"}
	if _, err := e.svc.SetSettings(in); err == nil || !strings.Contains(err.Error(), "choose a UPS") {
		t.Errorf("host without a UPS: %v", err)
	}
	in = base()
	in.Shutdown, in.StopOnLow = []string{"nas"}, false
	if _, err := e.svc.SetSettings(in); err == nil || !strings.Contains(err.Error(), "needs stopping servers") {
		t.Errorf("without stopping: %v", err)
	}
	in = base()
	in.Shutdown = []string{"nas", "nas"}
	out, err := e.svc.SetSettings(in)
	if err != nil || !slices.Equal(out.Shutdown, []string{"nas"}) {
		t.Errorf("saved %v, %v", out.Shutdown, err)
	}
	if got := e.service().Settings(); !slices.Equal(got.Shutdown, []string{"nas"}) {
		t.Errorf("reloaded %v", got.Shutdown)
	}
}

func TestShutdownAfterStopping(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"},
		StopOnLow: true, Shutdown: []string{"nas"}, RestartOnPower: true, RestartAfter: 5}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	if !slices.Equal(e.hosts.shutDown, []string{"nas"}) || !e.hosts.stoppedFirst {
		t.Fatalf("shut down %v (servers stopped first: %v)", e.hosts.shutDown, e.hosts.stoppedFirst)
	}
	if ev := e.events.last(); ev.Kind != notify.KindPowerLow || !strings.Contains(ev.Message, "Then it told nas to shut down") {
		t.Errorf("low event = %+v", ev)
	}
	if st := e.svc.Status()[0].Stopped; st == nil || !st.ShutDown {
		t.Errorf("stopped = %+v", st)
	}
	// The host is off: its agent can't read the UPS, which is expected.
	e.reader.fail["nas"] = errors.New("agent not connected")
	for range failuresBeforeNotify + 1 {
		e.check(t)
	}
	if slices.Contains(e.events.kinds(), notify.KindUPSUnreachable) {
		t.Error("unreachable while hlm had shut the host down")
	}
	// It's back, with the power: the servers start once it's been on a while.
	delete(e.reader.fail, "nas")
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	e.now = e.now.Add(5 * time.Minute)
	e.check(t)
	if len(e.servers.started) != 2 || len(e.hosts.shutDown) != 1 {
		t.Errorf("started %v, shut down %v", e.servers.started, e.hosts.shutDown)
	}
}

func TestShutdownFails(t *testing.T) {
	e := newEnv(t)
	e.hosts.err = errors.New("the host's agent is too old for this")
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"},
		StopOnLow: true, Shutdown: []string{"nas"}}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	if ev := e.events.last(); !strings.Contains(ev.Message, "Shutting nas down failed: the host's agent is too old") {
		t.Errorf("low event = %+v", ev)
	}
	if st := e.svc.Status()[0].Stopped; st == nil || st.ShutDown || st.ShutdownError == "" {
		t.Errorf("stopped = %+v", st)
	}
}

func TestIdleHostShutsDownOnBattery(t *testing.T) {
	e := newEnv(t)
	set := Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"}, StopOnLow: true, StopBelowCharge: 30,
		Shutdown: []string{"nas"}, ShutdownIdle: true, ShutdownIdleAfter: 1, RestartOnPower: true, RestartAfter: 5}
	if _, err := e.svc.SetSettings(set); err != nil {
		t.Fatal(err)
	}
	// Servers are running: on battery, nothing happens.
	e.upsd.Set("rack", map[string]string{"ups.status": "OB", "battery.charge": "90"})
	e.check(t)
	e.now = e.now.Add(2 * time.Minute)
	e.check(t)
	if len(e.hosts.shutDown) != 0 {
		t.Fatalf("shut down with servers running: %v", e.hosts.shutDown)
	}
	// They stop (someone, or idle sleep) while still on battery: it goes.
	e.servers.Stop(context.Background(), "valheim@nas")
	e.servers.Stop(context.Background(), "minecraft@nas")
	e.stacks.Act(context.Background(), "wiki", "stop")
	e.check(t)
	if !slices.Equal(e.hosts.shutDown, []string{"nas"}) {
		t.Fatalf("shut down = %v", e.hosts.shutDown)
	}
	ev := e.events.last()
	if ev.Kind != notify.KindPowerShutdown || !strings.Contains(ev.Message, "leave the battery to the others") {
		t.Errorf("event = %+v", ev)
	}
	if st := e.svc.Status()[0].Stopped; st == nil || !st.ShutDown || len(st.Servers) != 0 {
		t.Errorf("stopped = %+v", st)
	}
	// Back up with the power: nothing to start, and the record goes.
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	if ev := e.events.last(); ev.Kind != notify.KindPowerRestored || strings.Contains(ev.Message, "will start") {
		t.Errorf("restored = %+v", ev)
	}
	e.now = e.now.Add(5 * time.Minute)
	e.check(t)
	if st := e.svc.Status()[0]; st.Stopped != nil || len(e.servers.started) != 0 {
		t.Errorf("after power back: %+v, started %v", st, e.servers.started)
	}
}

func TestIdleShutdownWaits(t *testing.T) {
	e := newEnv(t)
	e.servers.list = []manager.Server{{ID: "palworld@nas", Host: "nas", State: "exited"}}
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"}, StopOnLow: true,
		Shutdown: []string{"nas"}, ShutdownIdle: true, ShutdownIdleAfter: 1}); err != nil {
		t.Fatal(err)
	}
	// A flicker shorter than the wait: nothing.
	e.upsd.Set("rack", map[string]string{"ups.status": "OB"})
	e.check(t)
	e.now = e.now.Add(30 * time.Second)
	e.upsd.Set("rack", map[string]string{"ups.status": "OL"})
	e.check(t)
	e.upsd.Set("rack", map[string]string{"ups.status": "OB"})
	e.check(t)
	e.now = e.now.Add(45 * time.Second)
	e.check(t)
	if len(e.hosts.shutDown) != 0 {
		t.Fatalf("shut down after a flicker: %v", e.hosts.shutDown)
	}
	e.now = e.now.Add(15 * time.Second)
	e.check(t)
	if len(e.hosts.shutDown) != 1 {
		t.Fatalf("shut down = %v", e.hosts.shutDown)
	}
}

func TestIdleShutdownOff(t *testing.T) {
	for name, set := range map[string]Settings{
		"idle off":        {StopOnLow: true, Shutdown: []string{"nas"}, ShutdownIdle: false},
		"host not ticked": {StopOnLow: true, ShutdownIdle: true},
	} {
		e := newEnv(t)
		e.servers.list = nil
		set.Address, set.Hosts = e.upsd.Addr, map[string]string{"nas": "rack"}
		if _, err := e.svc.SetSettings(set); err != nil {
			t.Fatal(err)
		}
		e.upsd.Set("rack", map[string]string{"ups.status": "OB"})
		e.check(t)
		e.now = e.now.Add(10 * time.Minute)
		e.check(t)
		if len(e.hosts.shutDown) != 0 {
			t.Errorf("%s: shut down %v", name, e.hosts.shutDown)
		}
	}
}

func TestIdleShutdownFailsOnce(t *testing.T) {
	e := newEnv(t)
	e.servers.list = nil
	e.hosts.err = errors.New("the host's agent is too old for this")
	if _, err := e.svc.SetSettings(Settings{Address: e.upsd.Addr, Hosts: map[string]string{"nas": "rack"}, StopOnLow: true,
		Shutdown: []string{"nas"}, ShutdownIdle: true}); err != nil {
		t.Fatal(err)
	}
	e.upsd.Set("rack", map[string]string{"ups.status": "OB"})
	for range 3 {
		e.check(t)
	}
	n := 0
	for _, k := range e.events.kinds() {
		if k == notify.KindPowerShutdown {
			n++
		}
	}
	if n != 1 || !strings.Contains(e.events.last().Title, "Couldn't shut nas down") {
		t.Errorf("%d shutdown events, last %+v", n, e.events.last())
	}
	// The low battery still stops servers and tries again.
	e.hosts.err = nil
	e.upsd.Set("rack", map[string]string{"ups.status": "OB LB"})
	e.check(t)
	if len(e.hosts.shutDown) != 1 {
		t.Errorf("shut down at low battery: %v", e.hosts.shutDown)
	}
}
