package gamequery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type world struct {
	mu        sync.Mutex
	servers   map[string]docker.ContainerJSON
	apps      map[string]*catalog.App
	answering map[string]Result // address → answer
	asked     []string
	console   map[string]string // server → player list output
	events    []notify.Event
	cleared   []string
}

func (w *world) List(context.Context) ([]manager.Server, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []manager.Server
	for name, c := range w.servers {
		state := "exited"
		if c.State.Running {
			state = "running"
		}
		out = append(out, manager.Server{ID: c.ID, Name: name, State: state})
	}
	return out, nil
}
func (w *world) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.servers[id]
	if !ok {
		return c, manager.ErrNotFound
	}
	return c, nil
}
func (w *world) Config(_ context.Context, id string) (*catalog.App, deploy.Request, error) {
	return w.apps[id], deploy.Request{}, nil
}
func (w *world) Replies(_ context.Context, id string) (string, bool) {
	_, ok := w.console[id]
	return "", ok
}
func (w *world) Query(_ context.Context, id, cmd string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, "console "+id+" "+cmd)
	return w.console[id], nil
}
func (w *world) Notify(e notify.Event) { w.mu.Lock(); w.events = append(w.events, e); w.mu.Unlock() }
func (w *world) Clear(k string)        { w.mu.Lock(); w.cleared = append(w.cleared, k); w.mu.Unlock() }

func (w *world) probe(proto string) func(context.Context, string) (Result, error) {
	return func(_ context.Context, addr string) (Result, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.asked = append(w.asked, proto+" "+addr)
		if r, ok := w.answering[proto+" "+addr]; ok {
			r.Protocol, r.Address = proto, addr
			return r, nil
		}
		return Result{}, errors.New("read udp: i/o timeout")
	}
}

func ctr(name, image, ip string, startedAt time.Time) docker.ContainerJSON {
	var c docker.ContainerJSON
	c.ID, c.Name = "id-"+name, "/"+name
	c.Config.Image = image
	c.State.Running = true
	c.State.StartedAt = startedAt.Format(time.RFC3339Nano)
	c.Config.ExposedPorts = map[string]struct{}{}
	c.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	c.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: ip}}
	return c
}

type rig struct {
	s   *Service
	w   *world
	now *time.Time
}

func newRig(t *testing.T) rig {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{servers: map[string]docker.ContainerJSON{}, apps: map[string]*catalog.App{},
		answering: map[string]Result{}, console: map[string]string{}}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := rig{w: w, now: &now}
	r.s = New(Options{Secrets: store, Servers: w, Console: w, Notifier: w,
		Probes: map[string]func(context.Context, string) (Result, error){
			ProtoA2S: w.probe(ProtoA2S), ProtoMinecraft: w.probe(ProtoMinecraft), ProtoBedrock: w.probe(ProtoBedrock), ProtoTCP: w.probe(ProtoTCP),
		},
		Now: func() time.Time { return *r.now }})
	return r
}

// step moves the clock on and polls whatever is due, waiting for it.
func (r rig) step(d time.Duration) {
	*r.now = r.now.Add(d)
	var wg sync.WaitGroup
	r.s.tick(context.Background(), func(name string) {
		wg.Add(1)
		go func() { defer wg.Done(); r.s.poll(context.Background(), name) }()
	})
	wg.Wait()
}

func TestDetectAndPoll(t *testing.T) {
	r := newRig(t)
	start := r.now.Add(time.Minute) // after hlm started
	c := ctr("valheim", "ghcr.io/ich777/steamcmd:valheim", "192.168.11.20", start)
	r.w.servers["valheim"] = c
	r.w.apps["valheim"] = &catalog.App{Config: []catalog.Config{
		{Type: catalog.TypePort, Name: "Game Port", Target: "2456", Mode: "udp"},
	}}

	// Still downloading: nothing answers. Starting.
	r.step(2 * time.Minute)
	if st := r.s.Status("valheim", true); st.Status != StatusStarting || st.Method != nil {
		t.Fatalf("while starting: %+v", st)
	}
	// Detection tries Valheim's query port first.
	if r.w.asked[0] != "a2s 192.168.11.20:2457" {
		t.Errorf("asked %q", r.w.asked)
	}

	// It comes up, answering on 2457.
	r.w.answering["a2s 192.168.11.20:2457"] = Result{Name: "My world", Players: 2, MaxPlayers: 10, HasPlayers: true, PlayerNames: []string{"alex", "sam"}}
	r.step(redetect)
	st := r.s.Status("valheim", true)
	if st.Status != StatusOnline || st.Method == nil || st.Method.Port != 2457 || st.Result.Players != 2 || len(st.History) != 1 {
		t.Fatalf("online: %+v", st)
	}
	if len(r.w.events) != 1 || r.w.events[0].Kind != notify.KindServerOnline || !strings.Contains(r.w.events[0].Message, "2 of 10") {
		t.Errorf("events = %+v", r.w.events)
	}
	if n, ok := r.s.Players("valheim"); !ok || n != 2 {
		t.Errorf("Players = %d, %v", n, ok)
	}
	// Viewers don't get names.
	if st := r.s.Status("valheim", false); st.Result.PlayerNames != nil {
		t.Errorf("names shown without permission: %+v", st.Result)
	}
	sums := r.s.Summaries(func(string) bool { return true })
	if sums["valheim"] != (Summary{Status: StatusOnline, Players: 2, MaxPlayers: 10, HasPlayers: true}) {
		t.Errorf("summary = %+v", sums)
	}

	// It hangs: two failures are tolerated, the third makes it not responding.
	delete(r.w.answering, "a2s 192.168.11.20:2457")
	r.step(pollOnline)
	r.step(pollStarting)
	if st := r.s.Status("valheim", true); st.Status != StatusOnline {
		t.Errorf("after 2 failures: %s", st.Status)
	}
	r.step(pollStarting)
	if st := r.s.Status("valheim", true); st.Status != StatusNotResponding {
		t.Errorf("after 3 failures: %s", st.Status)
	}
	if _, ok := r.s.Players("valheim"); ok {
		t.Error("player count known while not responding")
	}
	if len(r.w.events) != 2 || r.w.events[1].Kind != notify.KindNotResponding {
		t.Errorf("events = %+v", r.w.events)
	}
	// It's back (found again by detection): online, the alert cleared.
	r.w.answering["a2s 192.168.11.20:2457"] = Result{Players: 0, MaxPlayers: 10, HasPlayers: true}
	r.step(redetect)
	if st := r.s.Status("valheim", true); st.Status != StatusOnline {
		t.Errorf("back: %+v", st)
	}
	if len(r.w.cleared) == 0 || len(r.w.events) != 2 {
		t.Errorf("cleared %v, events %d", r.w.cleared, len(r.w.events))
	}

	// Stopped: forgotten.
	c.State.Running = false
	r.w.servers["valheim"] = c
	r.step(time.Second)
	if st := r.s.Status("valheim", true); st.Status != StatusStopped {
		t.Errorf("stopped: %+v", st)
	}
}

func TestAlreadyUpWhenHlmStarts(t *testing.T) {
	r := newRig(t)
	r.w.servers["mc"] = ctr("mc", "itzg/minecraft-server", "172.17.0.5", r.now.Add(-time.Hour))
	r.w.answering["minecraft 172.17.0.5:25565"] = Result{Players: 1, MaxPlayers: 20, HasPlayers: true}
	r.step(time.Second)
	if st := r.s.Status("mc", true); st.Status != StatusOnline || st.Method.Protocol != ProtoMinecraft {
		t.Fatalf("%+v", st)
	}
	if len(r.w.events) != 0 {
		t.Errorf("a server already up when hlm started was reported as ready: %+v", r.w.events)
	}
}

func TestNeverComesUp(t *testing.T) {
	r := newRig(t)
	r.w.servers["x"] = ctr("x", "some/game", "10.0.0.5", r.now.Add(time.Second))
	r.w.apps["x"] = &catalog.App{Config: []catalog.Config{{Type: catalog.TypePort, Target: "7777", Mode: "udp"}}}
	r.step(time.Minute)
	if st := r.s.Status("x", true); st.Status != StatusStarting {
		t.Fatalf("%+v", st)
	}
	// Detection keeps failing: after the grace period, unknown (no alert:
	// it may just not have a query hlm knows).
	r.step(startGrace)
	if st := r.s.Status("x", true); st.Status != StatusUnknown || !strings.Contains(st.Error, "no way to ask") {
		t.Errorf("%+v", st)
	}
	if len(r.w.events) != 0 {
		t.Errorf("events = %+v", r.w.events)
	}
	// With a chosen protocol that never answers, it's not responding, and
	// that's reported.
	if _, err := r.s.SetSettings("x", Settings{Protocol: ProtoA2S, Port: 27015}); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if st := r.s.Status("x", true); st.Status != StatusNotResponding || len(r.w.events) != 1 {
		t.Errorf("chosen protocol: %+v, events %d", st, len(r.w.events))
	}
}

func TestConsolePlayers(t *testing.T) {
	r := newRig(t)
	r.w.servers["palworld"] = ctr("palworld", "ghcr.io/ich777/steamcmd:palworld", "192.168.11.14", *r.now)
	r.w.console["palworld"] = "name,playeruid,steamid\nAlice,1,2\n"
	r.step(time.Second)
	st := r.s.Status("palworld", true)
	// No A2S answers; the console's player list does.
	if st.Status != StatusOnline || st.Method.Protocol != ProtoRCON || st.Result.Players != 1 || !slices.Equal(st.Result.PlayerNames, []string{"Alice"}) {
		t.Fatalf("%+v", st)
	}
	r.w.asked = nil
	r.step(pollOnline) // console queries are less frequent
	if len(r.w.asked) != 0 {
		t.Errorf("queried the console again after 30s: %q", r.w.asked)
	}
	r.step(pollConsole)
	if !slices.Equal(r.w.asked, []string{"console palworld ShowPlayers"}) {
		t.Errorf("asked %q", r.w.asked)
	}
}

func TestSettingsAndTest(t *testing.T) {
	r := newRig(t)
	r.w.servers["mc"] = ctr("mc", "itzg/minecraft-server", "172.17.0.5", *r.now)
	for _, bad := range []Settings{{Protocol: "gamespy"}, {Protocol: ProtoA2S}, {Protocol: ProtoTCP, Port: 70000}} {
		if _, err := r.s.SetSettings("mc", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	got, _ := r.s.SetSettings("mc", Settings{Protocol: ProtoRCON, Port: 9})
	if got.Port != 0 {
		t.Errorf("port kept for rcon: %+v", got)
	}
	r.w.answering["tcp 172.17.0.5:25575"] = Result{}
	set, res, err := r.s.Test(context.Background(), "mc", Settings{Protocol: ProtoTCP, Port: 25575})
	if err != nil || res.Protocol != ProtoTCP || set.Port != 25575 {
		t.Errorf("test: %+v %+v %v", set, res, err)
	}
	r.w.answering["minecraft 172.17.0.5:25565"] = Result{HasPlayers: true}
	set, _, err = r.s.Test(context.Background(), "mc", Settings{})
	if err != nil || set.Protocol != ProtoMinecraft || set.Port != 25565 {
		t.Errorf("auto test: %+v %v", set, err)
	}
	// Off: not queried at all.
	r.s.SetSettings("mc", Settings{Protocol: ProtoOff})
	r.w.asked = nil
	r.step(time.Second)
	if st := r.s.Status("mc", true); st.Status != StatusOff || len(r.w.asked) != 0 {
		t.Errorf("off: %+v, asked %q", st, r.w.asked)
	}
	r.s.ForgetServer("mc")
	if all, _ := r.s.allSettings(); len(all) != 0 {
		t.Errorf("settings left: %+v", all)
	}
}

func TestCandidates(t *testing.T) {
	c := ctr("x", "ghcr.io/ich777/steamcmd:enshrouded", "1.2.3.4", time.Now())
	c.Config.ExposedPorts["15636/udp"] = struct{}{}
	app := &catalog.App{Config: []catalog.Config{
		{Type: catalog.TypePort, Name: "Steam Query Port", Target: "15637", Mode: "udp"},
		{Type: catalog.TypePort, Name: "Game Port", Target: "15636", Mode: "udp"},
	}}
	got := candidates(c, app, false)
	want := []candidate{{ProtoA2S, 15637}, {ProtoA2S, 15636}}
	if !slices.Equal(got[:2], want) {
		t.Errorf("enshrouded candidates = %+v", got)
	}
	bed := candidates(ctr("b", "itzg/minecraft-bedrock-server", "", time.Now()), nil, false)
	if len(bed) == 0 || bed[0] != (candidate{ProtoBedrock, 19132}) {
		t.Errorf("bedrock = %+v", bed)
	}
	if got := candidates(ctr("p", "plexinc/pms-docker", "", time.Now()), nil, true); len(got) != 0 {
		t.Errorf("plex = %+v", got)
	}
	if got := words("ghcr.io/ich777/steamcmd:7dtd"); !slices.Contains(got, "7dtd") {
		t.Errorf("words = %q", got)
	}
}

// A server that hangs right after its settings are reset still raises the
// alert, and is seen again as soon as it answers the way it did before.
func TestHangAfterSettingsChange(t *testing.T) {
	r := newRig(t)
	r.w.servers["mc"] = ctr("mc", "itzg/minecraft-server", "172.17.0.5", r.now.Add(time.Second))
	r.w.answering["minecraft 172.17.0.5:25565"] = Result{HasPlayers: true, MaxPlayers: 20}
	r.step(5 * time.Second)
	if st := r.s.Status("mc", true); st.Status != StatusOnline {
		t.Fatalf("%+v", st)
	}
	r.s.SetSettings("mc", Settings{}) // e.g. saved as automatic again
	delete(r.w.answering, "minecraft 172.17.0.5:25565")
	for range failsAllowed {
		r.step(pollStarting)
	}
	if st := r.s.Status("mc", true); st.Status != StatusNotResponding {
		t.Fatalf("after hanging: %+v", st)
	}
	var kinds []string
	for _, e := range r.w.events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []string{notify.KindServerOnline, notify.KindNotResponding}) {
		t.Errorf("events = %q", kinds)
	}
	// Back, well before the next detection round: noticed on the next poll.
	r.w.answering["minecraft 172.17.0.5:25565"] = Result{HasPlayers: true, MaxPlayers: 20}
	r.step(pollStarting)
	if st := r.s.Status("mc", true); st.Status != StatusOnline {
		t.Errorf("after coming back: %+v", st)
	}
}

// A server reachable only through its published port: once found there, it's
// asked there first, not after its container address times out again.
func TestPrefersWorkingAddress(t *testing.T) {
	r := newRig(t)
	c := ctr("mc", "itzg/minecraft-server", "172.17.0.9", r.now.Add(time.Second))
	c.HostConfig.PortBindings["25565/tcp"] = []docker.PortBinding{{HostPort: "25565"}}
	r.w.servers["mc"] = c
	r.w.answering["minecraft 127.0.0.1:25565"] = Result{HasPlayers: true}
	r.step(5 * time.Second)
	if st := r.s.Status("mc", true); st.Status != StatusOnline || st.Result.Address != "127.0.0.1:25565" {
		t.Fatalf("%+v", st)
	}
	r.w.asked = nil
	r.step(pollOnline)
	if !slices.Equal(r.w.asked, []string{"minecraft 127.0.0.1:25565"}) {
		t.Errorf("asked %q, want only the address that worked", r.w.asked)
	}
}
