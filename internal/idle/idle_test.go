package idle

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeServers is one server's container.
type fakeServers struct {
	mu      sync.Mutex
	info    docker.ContainerJSON
	calls   []string
	startFn func() error
}

func (f *fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A copy, as Docker's is: Stop changes the networks.
	info := f.info
	info.NetworkSettings.Networks = maps.Clone(f.info.NetworkSettings.Networks)
	return info, nil
}

func (f *fakeServers) Start(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	if f.startFn != nil {
		if err := f.startFn(); err != nil {
			return err
		}
	}
	f.info.State.Running = true
	return nil
}

func (f *fakeServers) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	f.info.State.Running = false
	// Docker forgets the addresses it chose, and generated MACs.
	for n, ep := range f.info.NetworkSettings.Networks {
		ep.IPAddress, ep.MacAddress = "", ""
		f.info.NetworkSettings.Networks[n] = ep
	}
	return nil
}

func (f *fakeServers) JobRunning(string) bool { return false }

func (f *fakeServers) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeDocker runs wake listeners that exit when told to.
type fakeDocker struct {
	mu      sync.Mutex
	created []docker.CreateRequest
	copied  map[string]string // file name → size, of the last copy
	removed []string
	exit    chan int
	logs    string
	running map[string]bool
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{exit: make(chan int, 1), running: map[string]bool{}}
}

func (f *fakeDocker) InspectImage(context.Context, string) (docker.Image, error) {
	return docker.Image{Os: "linux"}, nil
}

func (f *fakeDocker) InspectContainer(_ context.Context, id string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info docker.ContainerJSON
	info.State.Running = f.running[id]
	return info, nil
}

func (f *fakeDocker) ListContainers(context.Context, ...string) ([]docker.ContainerSummary, error) {
	return nil, nil
}

func (f *fakeDocker) CreateContainer(_ context.Context, name string, req docker.CreateRequest) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return "listener-" + name, nil, nil
}

func (f *fakeDocker) CopyTo(_ context.Context, id, dir string, r io.Reader) error {
	tr := tar.NewReader(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copied = map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(tr)
		f.copied[dir+h.Name] = string(data)
	}
}

func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[id] = true
	return nil
}

func (f *fakeDocker) WaitContainer(ctx context.Context, id string) (int, error) {
	select {
	case code := <-f.exit:
		f.mu.Lock()
		f.running[id] = false
		f.mu.Unlock()
		return code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (f *fakeDocker) Logs(context.Context, string, docker.LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	delete(f.running, id)
	return nil
}

type fakeConsole struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeConsole) Send(_ context.Context, _, _, cmd string) (console.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, cmd)
	return console.Result{}, nil
}

type auditLog struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *auditLog) Record(e audit.Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

type rig struct {
	s       *Service
	servers *fakeServers
	docker  *fakeDocker
	console *fakeConsole
	audit   *auditLog
	now     time.Time
	online  int
	known   bool
	// dockerErr is what reaching the Docker host fails with (an agent
	// that isn't connected), if anything.
	dockerErr error
	mu        sync.Mutex
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "hlm")
	os.WriteFile(exe, []byte("binary"), 0o755)
	r := &rig{servers: &fakeServers{}, docker: newFakeDocker(), console: &fakeConsole{}, audit: &auditLog{},
		now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), known: true}
	r.servers.info.Name = "/mc"
	r.servers.info.State.Running = true
	r.servers.info.Image = "sha256:abc"
	r.servers.info.Config.Image = "itzg/minecraft-server"
	r.servers.info.Config.Env = []string{"SERVER_PORT=25566"}
	r.servers.info.HostConfig.PortBindings = map[string][]docker.PortBinding{
		"25566/tcp": {{HostPort: "25566"}}, "25575/tcp": {{HostIP: "127.0.0.1", HostPort: "25575"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.s = New(Options{
		Secrets: store,
		Servers: r.servers,
		DockerFor: func(string) (Docker, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.dockerErr != nil {
				return nil, r.dockerErr
			}
			return r.docker, nil
		},
		Players: func(string) (int, int, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.online, 20, r.known
		},
		Console:    r.console,
		Audit:      r.audit,
		Executable: exe,
		Now: func() time.Time {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.now
		},
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	})
	r.s.listenerProblem = "" // the test binary isn't a static one
	r.s.runCtx = ctx
	return r
}

func (r *rig) set(online int, known bool, after time.Duration) {
	r.mu.Lock()
	r.online, r.known = online, known
	r.now = r.now.Add(after)
	r.mu.Unlock()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSleepAndWake(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.s.Save(ctx, "mc", Settings{Minutes: 3}); err == nil {
		t.Error("3 minutes accepted")
	}
	st, err := r.s.Save(ctx, "mc", Settings{Minutes: 10, WakeOnConnect: true, WakeFromStatusPage: true})
	if err != nil || !st.CanStop || !st.CanListen {
		t.Fatalf("save: %+v %v", st, err)
	}

	// Empty, then someone joins: the count starts again.
	r.s.Check(ctx)
	r.set(1, true, 8*time.Minute)
	r.s.Check(ctx)
	r.set(0, true, time.Minute)
	r.s.Check(ctx)
	r.set(0, false, 5*time.Minute) // the query failed: not known to be empty
	r.s.Check(ctx)
	r.set(0, true, time.Minute)
	r.s.Check(ctx)
	r.set(0, true, 9*time.Minute)
	r.s.Check(ctx)
	if got := r.servers.did(); len(got) != 0 {
		t.Fatalf("stopped too soon: %v", got)
	}
	r.set(0, true, time.Minute)
	r.s.Check(ctx)
	waitFor(t, "the stop", func() bool { return slices.Equal(r.servers.did(), []string{"stop"}) })
	waitFor(t, "the listener", func() bool { st, _ := r.s.Status(ctx, "mc"); return st.Asleep != nil && st.Asleep.Listener != "" })

	if len(r.console.sent) != 1 || !strings.HasPrefix(r.console.sent[0], "say Nobody has played for 10 minutes") {
		t.Errorf("warning = %q", r.console.sent)
	}
	if asleep, wake := r.s.AsleepInfo("mc"); !asleep || !wake {
		t.Errorf("AsleepInfo = %v %v", asleep, wake)
	}
	req := r.docker.created[0]
	if req.Image != "sha256:abc" || req.User != "65534:65534" || req.Entrypoint[0] != "/hlm-wake" ||
		!slices.Equal(req.HostConfig.CapDrop, []string{"ALL"}) || req.HostConfig.PortBindings["25575/tcp"][0].HostIP != "127.0.0.1" ||
		req.Labels[LabelListener] != "mc" {
		t.Errorf("listener = %+v", req)
	}
	if got := strings.Join(req.Cmd, " "); got != "wake-listener --tcp 25566,25575 --minecraft 25566 --max 20" {
		t.Errorf("args = %q", got)
	}
	if r.docker.copied["/hlm-wake"] != "binary" {
		t.Errorf("copied = %v", r.docker.copied)
	}

	// A player connects.
	r.docker.mu.Lock()
	r.docker.logs = `{"port":"25566/tcp","from":"192.168.1.20:51000","about":"a Minecraft player joining"}` + "\n"
	r.docker.mu.Unlock()
	r.docker.exit <- 0
	waitFor(t, "the start", func() bool { return slices.Equal(r.servers.did(), []string{"stop", "start"}) })
	waitFor(t, "awake", func() bool { a, _ := r.s.AsleepInfo("mc"); return !a })
	var wake audit.Entry
	r.audit.mu.Lock()
	for _, e := range r.audit.entries {
		if e.Action == "servers.idle.wake" {
			wake = e
		}
	}
	r.audit.mu.Unlock()
	if wake.Details["by"] != "a Minecraft player joining to port 25566/tcp from 192.168.1.20" || wake.Result != audit.ResultOK {
		t.Errorf("wake entry = %+v", wake)
	}
	if !slices.Contains(r.docker.removed, "listener-hlm-wake-mc") {
		t.Errorf("listener not removed: %v", r.docker.removed)
	}
}

func TestStartedBySomeoneElse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.s.Save(ctx, "mc", Settings{Minutes: 5, WakeOnConnect: true})
	r.s.Check(ctx)
	r.set(0, true, 5*time.Minute)
	r.s.Check(ctx)
	waitFor(t, "the listener", func() bool { st, _ := r.s.Status(ctx, "mc"); return st.Asleep != nil && st.Asleep.Listener != "" })

	// Started by hand: hlm's start moves the listener out of the way first.
	r.s.BeforeStart(ctx, "mc")
	if a, _ := r.s.AsleepInfo("mc"); a {
		t.Error("still asleep after a start")
	}
	if !slices.Contains(r.docker.removed, "listener-hlm-wake-mc") {
		t.Errorf("listener not removed: %v", r.docker.removed)
	}

	// Started outside hlm: noticed on the next check.
	r.servers.mu.Lock()
	r.servers.info.State.Running = true // the start
	r.servers.mu.Unlock()
	r.s.Check(ctx)
	r.set(0, true, 5*time.Minute)
	r.s.Check(ctx)
	waitFor(t, "asleep again", func() bool { a, _ := r.s.AsleepInfo("mc"); return a })
	r.servers.mu.Lock()
	r.servers.info.State.Running = true
	r.servers.mu.Unlock()
	r.s.Check(ctx)
	if a, _ := r.s.AsleepInfo("mc"); a {
		t.Error("still asleep while running")
	}
}

func TestListenerRetried(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.s.Save(ctx, "mc", Settings{Minutes: 5, WakeOnConnect: true})
	r.mu.Lock()
	r.dockerErr = errors.New("the host's agent isn't connected")
	r.mu.Unlock()
	r.s.Check(ctx)
	r.set(0, true, 5*time.Minute)
	r.s.Check(ctx)
	waitFor(t, "asleep without a listener", func() bool {
		st, _ := r.s.Status(ctx, "mc")
		return st.Asleep != nil && st.Asleep.ListenerError != ""
	})

	// The agent's back: the next checks start it.
	r.mu.Lock()
	r.dockerErr = nil
	r.mu.Unlock()
	r.set(0, true, retryListener)
	r.s.Check(ctx)
	r.s.Check(ctx)
	if st, _ := r.s.Status(ctx, "mc"); st.Asleep == nil || st.Asleep.Listener == "" || st.Asleep.ListenerError != "" {
		t.Errorf("after the agent's back: %+v", st.Asleep)
	}
}

func TestWakeFails(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.s.Save(ctx, "mc", Settings{Minutes: 5})
	r.s.Check(ctx)
	r.set(0, true, 5*time.Minute)
	r.s.Check(ctx)
	waitFor(t, "asleep", func() bool { a, _ := r.s.AsleepInfo("mc"); return a })
	if len(r.docker.created) != 0 {
		t.Error("a listener without wake on connect")
	}
	r.servers.startFn = func() error { return errors.New("port is already allocated") }
	if err := r.s.Wake(ctx, "mc", "the status page"); err == nil {
		t.Fatal("no error")
	}
	if a, _ := r.s.AsleepInfo("mc"); !a {
		t.Error("a failed wake should leave it asleep")
	}
	r.servers.startFn = nil
	if err := r.s.Wake(ctx, "mc", "the status page"); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Wake(ctx, "mc", "the status page"); !errors.Is(err, ErrNotAsleep) {
		t.Errorf("waking an awake server: %v", err)
	}
}

func TestListenerUnavailable(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.servers.info.HostConfig.NetworkMode = "host"
	r.servers.info.HostConfig.PortBindings = nil
	st, _ := r.s.Status(ctx, "mc")
	if st.CanListen || !strings.Contains(st.ListenWhy, "host's network") {
		t.Errorf("status = %+v", st)
	}
	r.set(0, false, 0)
	if st, _ := r.s.Status(ctx, "mc"); st.CanStop || st.Why == "" {
		t.Errorf("status = %+v", st)
	}
	if got := checkExecutable(filepath.Join(t.TempDir(), "missing")); got == "" {
		t.Error("a missing binary is fine?")
	}
}

// onBr0 makes the rig's server an Unraid-style one on br0: its own LAN
// address (chosen by Docker unless fixed), nothing published, and a port
// only in an Unraid variable.
func (r *rig) onBr0(fixed string) {
	info := &r.servers.info
	info.Config.Labels = map[string]string{"io.hlm.directip": "true"}
	info.Config.ExposedPorts = map[string]struct{}{"25566/tcp": {}}
	info.Config.Env = append(info.Config.Env, "UDP_PORT_15637=15637")
	info.HostConfig.NetworkMode = "br0"
	info.HostConfig.PortBindings = nil
	ep := docker.EndpointSettings{IPAddress: "192.168.1.50", MacAddress: "02:00:c0:a8:01:32"}
	if fixed != "" {
		ep.IPAddress = fixed
		ep.IPAMConfig = &docker.EndpointIPAMConfig{IPv4Address: fixed}
	}
	info.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": ep}
}

func TestSleepOnLANAddress(t *testing.T) {
	for _, fixed := range []string{"", "192.168.1.60"} {
		t.Run("fixed="+fixed, func(t *testing.T) {
			r := newRig(t)
			r.onBr0(fixed)
			ctx := context.Background()
			st, err := r.s.Save(ctx, "mc", Settings{Minutes: 5, WakeOnConnect: true})
			if err != nil || !st.CanListen {
				t.Fatalf("save: %+v %v", st, err)
			}
			r.s.Check(ctx)
			r.set(0, true, 5*time.Minute)
			r.s.Check(ctx)
			waitFor(t, "the listener", func() bool { st, _ := r.s.Status(ctx, "mc"); return st.Asleep != nil && st.Asleep.Listener != "" })
			if st, _ := r.s.Status(ctx, "mc"); !st.CanListen {
				t.Errorf("asleep status = %+v", st)
			}
			req := r.docker.created[0]
			want := cmpOr(fixed, "192.168.1.50")
			ep := req.NetworkingConfig.EndpointsConfig["br0"]
			if req.HostConfig.NetworkMode != "br0" || len(req.HostConfig.PortBindings) != 0 || ep.IPAMConfig == nil ||
				ep.IPAMConfig.IPv4Address != want || ep.MacAddress != "02:00:c0:a8:01:32" {
				t.Errorf("listener network = %+v %+v", req.HostConfig, req.NetworkingConfig)
			}
			if got := strings.Join(req.Cmd, " "); got != "wake-listener --tcp 25566 --udp 15637 --minecraft 25566 --max 20" {
				t.Errorf("args = %q", got)
			}
		})
	}
}

func TestLANAddressUnknown(t *testing.T) {
	r := newRig(t)
	r.onBr0("")
	r.servers.Stop(context.Background(), "mc") // stopped before hlm saw its address
	st, _ := r.s.Status(context.Background(), "mc")
	if st.CanListen || !strings.Contains(st.ListenWhy, "fixed") {
		t.Errorf("status = %+v", st)
	}
	// Unraid's own br0 containers, without hlm's label: a fixed address
	// and nothing published.
	r = newRig(t)
	r.onBr0("192.168.1.61")
	r.servers.info.Config.Labels = nil
	r.servers.Stop(context.Background(), "mc")
	if st, _ := r.s.Status(context.Background(), "mc"); !st.CanListen {
		t.Errorf("unlabelled br0 status = %+v", st)
	}
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
