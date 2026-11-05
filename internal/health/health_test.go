package health

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(s)
}

func TestSettings(t *testing.T) {
	st := testStore(t)

	// Defaults until set.
	if n, w := st.CrashLoopLimit("valheim"); n != manager.DefaultCrashLoopExits || w != manager.DefaultCrashLoopWindow {
		t.Errorf("default limit = %d in %s", n, w)
	}
	if hc := st.Healthcheck("valheim"); hc != nil {
		t.Errorf("default check = %+v", hc)
	}

	set, err := st.Set("valheim", Settings{
		CrashLoop: CrashLoop{Exits: 3, Minutes: 10},
		Check:     Check{Kind: CheckHTTP, Port: 8080, Interval: 60, Retries: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults are kept as 0 and the path filled in.
	if set.CrashLoop.Minutes != 0 || set.Check.Interval != 0 || set.Check.Path != "/" || set.Check.Retries != 5 {
		t.Errorf("tidied = %+v", set)
	}
	if n, w := st.CrashLoopLimit("valheim"); n != 3 || w != 10*time.Minute {
		t.Errorf("limit = %d in %s", n, w)
	}
	hc := st.Healthcheck("valheim")
	if hc == nil || hc.Test[0] != "CMD-SHELL" || !strings.Contains(hc.Test[1], "'http://127.0.0.1:8080/'") ||
		hc.Retries != 5 || hc.Interval != int64(time.Minute) || hc.StartPeriod != int64(10*time.Minute) {
		t.Errorf("check = %+v", hc)
	}

	if _, err := st.Set("valheim", Settings{CrashLoop: CrashLoop{Off: true, Exits: 9}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CrashLoopLimit("valheim"); n != 0 {
		t.Errorf("off limit = %d", n)
	}

	// Back to the defaults forgets the server.
	if _, err := st.Set("valheim", Settings{CrashLoop: CrashLoop{Exits: 5}}); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.All(); len(all) != 0 {
		t.Errorf("defaults stored: %v", all)
	}

	for _, bad := range []Settings{
		{CrashLoop: CrashLoop{Exits: 1}},
		{CrashLoop: CrashLoop{Minutes: 2000}},
		{Check: Check{Kind: CheckTCP}},
		{Check: Check{Kind: CheckHTTP, Port: 80, Path: "/x' ; rm -rf / ; '"}},
		{Check: Check{Kind: CheckHTTP, Port: 80, Path: "no-slash"}},
		{Check: Check{Kind: CheckCommand}},
		{Check: Check{Kind: CheckTCP, Port: 25565, Interval: 1}},
		{Check: Check{Kind: "ping"}},
		{RestartAfter: -1},
		{Signal: "vibes"},
	} {
		if _, err := st.Set("valheim", bad); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

func TestHealthchecks(t *testing.T) {
	if hc := (Check{Kind: CheckNone}).Healthcheck(); hc == nil || len(hc.Test) != 1 || hc.Test[0] != "NONE" {
		t.Errorf("none = %+v", hc)
	}
	hc := (Check{Kind: CheckTCP, Port: 25565, Timeout: 3}).Healthcheck()
	if !strings.Contains(hc.Test[1], "nc -z -w 3 127.0.0.1 25565") || !strings.Contains(hc.Test[1], "/dev/tcp/127.0.0.1/25565") ||
		hc.Timeout != int64(3*time.Second) {
		t.Errorf("tcp = %+v", hc)
	}
	zero := 0
	hc = (Check{Kind: CheckCommand, Command: "pgrep -f valheim_server", StartPeriod: &zero}).Healthcheck()
	if hc.StartPeriod != 0 {
		t.Errorf("start period 0 = %d", hc.StartPeriod)
	}
	if hc.Test[1] != "pgrep -f valheim_server" {
		t.Errorf("command = %+v", hc)
	}
}

type fakeServers struct {
	mu        sync.Mutex
	list      []manager.Server
	jobs      map[string]bool
	restarted []string
	stop      bool // RestartUnhealthy reports a crash loop instead
}

func (f *fakeServers) List(context.Context) ([]manager.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]manager.Server(nil), f.list...), nil
}

func (f *fakeServers) JobRunning(name string) bool { return f.jobs[name] }

func (f *fakeServers) RestartUnhealthy(_ context.Context, name, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, name+": "+reason)
	return f.stop, nil
}

type fakeNotifier struct{ events []notify.Event }

func (f *fakeNotifier) Notify(e notify.Event) { f.events = append(f.events, e) }
func (f *fakeNotifier) Clear(string)          {}

func TestMonitor(t *testing.T) {
	st := testStore(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	srvs := &fakeServers{jobs: map[string]bool{}, list: []manager.Server{
		{ID: "a", Name: "valheim", State: "running", Health: "unhealthy"},
		{ID: "b", Name: "minecraft", State: "running", Health: "healthy"},
		{ID: "c", Name: "palworld", State: "running", Health: "unhealthy"}, // not set to restart
		{ID: "d", Name: "satisfactory", State: "running"},
	}}
	responding := map[string]bool{"satisfactory": false}
	n := &fakeNotifier{}
	m := NewMonitor(Options{
		Store: st, Servers: srvs, Notifier: n, Now: func() time.Time { return now },
		Responding: func(name string) (bool, bool) { r, ok := responding[name]; return r, ok },
	})
	for _, name := range []string{"valheim", "minecraft"} {
		if _, err := st.Set(name, Settings{RestartAfter: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Set("satisfactory", Settings{RestartAfter: 10, Signal: SignalQuery}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	m.Tick(ctx)
	if since, ok := m.UnhealthySince("valheim"); !ok || !since.Equal(now) {
		t.Errorf("valheim unhealthy since = %v %v", since, ok)
	}
	if _, ok := m.UnhealthySince("palworld"); ok {
		t.Error("palworld watched without restarts on")
	}
	now = now.Add(4 * time.Minute)
	m.Tick(ctx)
	if len(srvs.restarted) != 0 {
		t.Fatalf("restarted early: %v", srvs.restarted)
	}

	// Busy with an update: wait.
	now = now.Add(2 * time.Minute)
	srvs.jobs["valheim"] = true
	m.Tick(ctx)
	if len(srvs.restarted) != 0 {
		t.Fatalf("restarted during a job: %v", srvs.restarted)
	}
	srvs.jobs["valheim"] = false
	m.Tick(ctx)
	if len(srvs.restarted) != 1 || srvs.restarted[0] != "valheim: unhealthy for 5 minutes" {
		t.Fatalf("restarted = %v", srvs.restarted)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindServerUnhealthy || !strings.Contains(n.events[0].Title, "restarted") {
		t.Errorf("events = %+v", n.events)
	}
	// It starts over after the restart.
	if _, ok := m.UnhealthySince("valheim"); ok {
		t.Error("still counted after the restart")
	}

	// Recovering resets the clock.
	srvs.list[0].Health = "healthy"
	now = now.Add(time.Minute)
	m.Tick(ctx)
	srvs.list[0].Health = "unhealthy"
	now = now.Add(4 * time.Minute)
	m.Tick(ctx)
	if len(srvs.restarted) != 2 || !strings.HasPrefix(srvs.restarted[1], "satisfactory: not answering") {
		t.Errorf("restarted = %v", srvs.restarted)
	}

	// A restart that's a crash loop is reported by the manager, not here.
	srvs.stop = true
	now = now.Add(10 * time.Minute)
	m.Tick(ctx)
	if len(n.events) != 2 {
		t.Errorf("events = %+v", n.events)
	}
}
