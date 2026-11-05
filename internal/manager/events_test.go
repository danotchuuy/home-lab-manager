package manager

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

type fakeNotifier struct {
	mu      sync.Mutex
	events  []notify.Event
	cleared []string
}

func (f *fakeNotifier) Notify(e notify.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeNotifier) Clear(key string) {
	f.mu.Lock()
	f.cleared = append(f.cleared, key)
	f.mu.Unlock()
}

func (f *fakeNotifier) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		out = append(out, e.Kind+":"+e.Server+e.Title)
	}
	return out
}

func eventsManager(t *testing.T) (*Manager, *fakeNotifier) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.47")
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"c1","Config":{"Tty":true}}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Write([]byte("loading world\n\x1b[31mERROR: save is corrupt\x1b[0m\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	n := &fakeNotifier{}
	m.UseNotifier(n)
	m.UseConnection("io.hlm.wireguard", ConnectionKind{Kind: "wireguard", Title: "WireGuard tunnel"})
	m.UseSelf("self", ConnectionKind{Kind: "hlm"})
	return m, n
}

type eventFeed struct {
	m  *Manager
	st *watchState
	t  time.Time
}

func (f *eventFeed) send(action, id string, attrs map[string]string) {
	f.t = f.t.Add(time.Second)
	f.m.handleEvent(context.Background(), f.st, docker.EventMessage{
		Type: "container", Action: action, TimeNano: f.t.UnixNano(),
		Actor: docker.EventActor{ID: id, Attributes: attrs},
	})
}

func server(name string, extra ...string) map[string]string {
	a := map[string]string{"name": name, deploy.LabelManaged: "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

func TestCrashDetection(t *testing.T) {
	m, n := eventsManager(t)
	f := &eventFeed{m: m, st: newWatchState(), t: time.Now()}

	// Stopped by someone: a kill, then exit code 143. Not a crash.
	f.send("kill", "a", server("valheim", "signal", "15"))
	f.send("die", "a", server("valheim", "exitCode", "143"))
	// Exited cleanly on its own (e.g. an in-game stop command). Not a crash.
	f.send("die", "a", server("valheim", "exitCode", "0"))
	if got := n.kinds(); len(got) != 0 {
		t.Fatalf("deliberate stops reported: %v", got)
	}

	// Died with an error, nobody asked.
	f.send("die", "a", server("valheim", "exitCode", "1"))
	// Ran out of memory: oom, then die (even with exit 0 or after a kill).
	f.send("oom", "b", server("minecraft"))
	f.send("die", "b", server("minecraft", "exitCode", "137"))
	got := n.kinds()
	if len(got) != 2 || got[0] != "server.crashed:valheimvalheim crashed" || got[1] != "server.crashed:minecraftminecraft crashed" {
		t.Fatalf("got %v", got)
	}
	if msg := n.events[0].Message; !strings.Contains(msg, "exit code 1") || !strings.Contains(msg, "ERROR: save is corrupt") ||
		strings.Contains(msg, "\x1b") {
		t.Errorf("crash message = %q", msg)
	}
	if !strings.Contains(n.events[1].Message, "ran out of memory") {
		t.Errorf("oom message = %q", n.events[1].Message)
	}

	// A kill long before the exit doesn't excuse it.
	f.send("kill", "a", server("valheim"))
	f.t = f.t.Add(expectedStopWindow + time.Minute)
	f.send("die", "a", server("valheim", "exitCode", "2"))
	if len(n.kinds()) != 3 {
		t.Errorf("exit long after a kill wasn't reported: %v", n.kinds())
	}

	// Events already handled (replayed after a reconnect) are skipped.
	f.t = f.t.Add(-time.Second)
	f.send("die", "a", server("valheim", "exitCode", "2"))
	if len(n.kinds()) != 3 {
		t.Errorf("replayed event reported again: %v", n.kinds())
	}
}

func TestHealthAndConnections(t *testing.T) {
	m, n := eventsManager(t)
	f := &eventFeed{m: m, st: newWatchState(), t: time.Now()}

	f.send("health_status: unhealthy", "a", server("valheim"))
	f.send("health_status: unhealthy", "a", server("valheim")) // still unhealthy
	f.send("health_status: healthy", "a", server("valheim"))
	f.send("health_status: unhealthy", "a", server("valheim"))
	if got := n.kinds(); len(got) != 2 || got[0] != "server.unhealthy:valheimvalheim is unhealthy" {
		t.Errorf("health alerts = %v", got)
	}
	if len(n.cleared) != 1 || n.cleared[0] != notify.DedupeKey(notify.KindServerUnhealthy, "valheim") {
		t.Errorf("recovering didn't clear the alert: %v", n.cleared)
	}

	// The WireGuard container stopping on its own.
	f.send("die", "w", map[string]string{"name": "hlm-wireguard", "io.hlm.wireguard": "home", "exitCode": "0"})
	// hlm's own container and unrelated containers are ignored.
	f.send("die", "self", map[string]string{"name": "home-lab-manager", "exitCode": "1"})
	f.send("die", "x", map[string]string{"name": "plex", "exitCode": "1"})
	got := n.kinds()
	if len(got) != 3 || got[2] != "tunnel.down:WireGuard tunnel stopped" {
		t.Errorf("got %v", got)
	}
}

func TestNoCrashAlertDuringJobs(t *testing.T) {
	m, n := eventsManager(t)
	f := &eventFeed{m: m, st: newWatchState(), t: time.Now()}
	_, finish, err := m.jobs.start(Job{Kind: "update", Server: "valheim"}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	f.send("die", "a", server("valheim", "exitCode", "1"))
	finish(nil)
	if got := n.kinds(); len(got) != 0 {
		t.Errorf("crash during an update reported: %v", got)
	}
}

func TestUpdateNotifications(t *testing.T) {
	m, n := eventsManager(t)
	m.notifyUpdated(Job{Server: "valheim", Image: "lloesche/valheim-server"}, nil)
	m.notifyUpdated(Job{Server: "valheim", Image: "lloesche/valheim-server"}, context.DeadlineExceeded)
	m.notifyUpdateAvailable(Server{Name: "valheim", Image: "lloesche/valheim-server", imageID: "sha256:1"})
	got := n.kinds()
	if len(got) != 3 || !strings.HasPrefix(got[0], notify.KindServerUpdated) || !strings.HasPrefix(got[1], notify.KindServerUpdateFail) {
		t.Fatalf("got %v", got)
	}
	if n.events[2].Key != "server.update_available/valheim/sha256:1" {
		t.Errorf("update key = %q", n.events[2].Key)
	}
}

type limitPolicy struct {
	exits  int
	window time.Duration
}

func (p limitPolicy) CrashLoopLimit(string) (int, time.Duration) { return p.exits, p.window }
func (p limitPolicy) Healthcheck(string) *docker.Healthcheck    { return nil }

// waitFor waits for the notifier to have an event of kind.
func (f *fakeNotifier) waitFor(t *testing.T, kind string) notify.Event {
	t.Helper()
	return f.waitForWithin(t, kind, 5*time.Second)
}

func (f *fakeNotifier) waitForWithin(t *testing.T, kind string, d time.Duration) notify.Event {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, e := range f.events {
			if e.Kind == kind {
				f.mu.Unlock()
				return e
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event", kind)
	return notify.Event{}
}

func (f *fakeNotifier) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestCrashLoop(t *testing.T) {
	m, n := eventsManager(t)
	path := t.TempDir() + "/crash-loops.json"
	if err := m.UseCrashLoopStore(path); err != nil {
		t.Fatal(err)
	}
	f := &eventFeed{m: m, st: newWatchState(), t: time.Now()}

	// Exits spread out don't add up: 4 in the window, then one after the
	// first has left it.
	for range 4 {
		f.send("die", "a", server("valheim", "exitCode", "1"))
		f.t = f.t.Add(2 * time.Minute)
	}
	f.t = f.t.Add(3 * time.Minute)
	f.send("die", "a", server("valheim", "exitCode", "1"))
	if _, ok := m.CrashLoopOf("valheim"); ok || n.count(notify.KindServerCrashLoop) != 0 {
		t.Fatal("stopped for exits outside the window")
	}
	// Stops asked for, and exits during jobs, don't count.
	for range 3 {
		f.send("kill", "a", server("valheim"))
		f.send("die", "a", server("valheim", "exitCode", "143"))
	}
	_, finish, _ := m.jobs.start(Job{Kind: "update", Server: "valheim"}, func(Event) {})
	for range 3 {
		f.send("die", "a", server("valheim", "exitCode", "1"))
	}
	finish(nil)
	if _, ok := m.CrashLoopOf("valheim"); ok {
		t.Fatal("stopped for deliberate stops")
	}

	// The fifth within 10 minutes (even a clean exit: Docker restarts
	// those too) stops it, once.
	f.send("die", "a", server("valheim", "exitCode", "0"))
	e := n.waitFor(t, notify.KindServerCrashLoop)
	if !strings.Contains(e.Message, "exited (or had to be restarted) 5 times in 10 minutes") || !strings.Contains(e.Message, "ERROR: save is corrupt") {
		t.Errorf("message = %q", e.Message)
	}
	waitCrashLoop(t, m, "valheim")
	f.send("die", "a", server("valheim", "exitCode", "1"))
	time.Sleep(50 * time.Millisecond)
	if c := n.count(notify.KindServerCrashLoop); c != 1 {
		t.Errorf("%d crash-loop alerts", c)
	}
	srv := Server{Name: "valheim"}
	m.fillCrashLoop(&srv, docker.ContainerJSON{})
	if srv.CrashLoop == nil || srv.CrashLoop.Exits != 5 || srv.CrashLoop.ExitCode != "0" {
		t.Errorf("server crash loop = %+v", srv.CrashLoop)
	}

	// It's remembered across a restart of hlm.
	m2, _ := eventsManager(t)
	if err := m2.UseCrashLoopStore(path); err != nil {
		t.Fatal(err)
	}
	if cl, ok := m2.CrashLoopOf("valheim"); !ok || cl.Exits != 5 {
		t.Errorf("reloaded = %+v %v", cl, ok)
	}
	// Started outside hlm since: forgotten.
	var info docker.ContainerJSON
	info.State.Running = true
	info.State.StartedAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	srv = Server{Name: "valheim"}
	m2.fillCrashLoop(&srv, info)
	if _, ok := m2.CrashLoopOf("valheim"); ok || srv.CrashLoop != nil {
		t.Error("crash loop kept after a start")
	}

	// Starting it in hlm forgets it too, with the exits counted so far.
	m.ClearCrashLoop("valheim")
	if _, ok := m.CrashLoopOf("valheim"); ok || m.RecentExits("valheim") != 0 {
		t.Error("not cleared")
	}

	// Turned off for a server.
	m.UseHealthPolicy(limitPolicy{exits: 0})
	for range 10 {
		f.send("die", "b", server("minecraft", "exitCode", "1"))
	}
	time.Sleep(50 * time.Millisecond)
	if _, ok := m.CrashLoopOf("minecraft"); ok {
		t.Error("stopped with protection off")
	}
	// A lower limit.
	m.UseHealthPolicy(limitPolicy{exits: 2, window: time.Minute})
	f.send("die", "c", server("palworld", "exitCode", "1"))
	f.send("die", "c", server("palworld", "exitCode", "1"))
	waitCrashLoop(t, m, "palworld")
}

func waitCrashLoop(t *testing.T, m *Manager, name string) {
	t.Helper()
	for range 500 {
		if _, ok := m.CrashLoopOf(name); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not stopped for a crash loop", name)
}
