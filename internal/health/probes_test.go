package health

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

func TestProbesValidate(t *testing.T) {
	var p Probes
	if err := json.Unmarshal([]byte(`{
		"startupProbe": {"tcpSocket": {"port": "8080"}, "failureThreshold": 30},
		"livenessProbe": {"httpGet": {"port": 8080, "scheme": "https"}},
		"readinessProbe": {"exec": {"command": ["sh", "-c", "true"]}, "successThreshold": 2}
	}`), &p); err != nil {
		t.Fatal(err)
	}
	got, err := Settings{Probes: p}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	s, l, r := got.Probes.Startup, got.Probes.Liveness, got.Probes.Readiness
	if s.TCPSocket.Port != 8080 || s.FailureThreshold != 30 || s.PeriodSeconds != 10 || s.TimeoutSeconds != 1 || s.SuccessThreshold != 1 {
		t.Errorf("startup %+v", s)
	}
	if l.HTTPGet.Path != "/" || l.HTTPGet.Scheme != "HTTPS" || l.FailureThreshold != 3 {
		t.Errorf("liveness %+v %+v", l, l.HTTPGet)
	}
	if r.SuccessThreshold != 2 {
		t.Errorf("readiness %+v", r)
	}

	for in, want := range map[string]string{
		`{"livenessProbe": {}}`: "exactly one",
		`{"livenessProbe": {"tcpSocket": {"port": 1}, "exec": {"command": ["x"]}}}`: "exactly one",
		`{"livenessProbe": {"tcpSocket": {"port": 1}, "successThreshold": 2}}`:      "successThreshold must be 1",
		`{"readinessProbe": {"httpGet": {"port": 0}}}`:                              "needs the port",
		`{"readinessProbe": {"httpGet": {"port": 80, "path": "x"}}}`:                "starts with /",
		`{"readinessProbe": {"exec": {"command": []}}}`:                             "needs a command",
		`{"readinessProbe": {"tcpSocket": {"port": 1}, "periodSeconds": 99999}}`:    "periodSeconds",
	} {
		var p Probes
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatal(in, err)
		}
		if _, err := (Settings{Probes: p}).Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
	if err := json.Unmarshal([]byte(`{"livenessProbe": {"tcpSocket": {"port": "http"}}}`), &p); err == nil || !strings.Contains(err.Error(), "named ports") {
		t.Errorf("named port: %v", err)
	}
}

func TestProbeRun(t *testing.T) {
	codes := map[string]int{"/ok": 200, "/redirect": 302, "/bad": 503}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "t" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "http://example.invalid/")
		}
		w.WriteHeader(codes[r.URL.Path])
		w.Write([]byte("down for maintenance\nmore"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	ctx := context.Background()
	var info docker.ContainerJSON
	env := ProbeEnv{}

	for path, want := range map[string]bool{"/ok": true, "/redirect": true, "/bad": false} {
		pr := Probe{HTTPGet: &HTTPGetAction{Path: path, Port: Port(p), Host: host, Scheme: "HTTP",
			HTTPHeaders: []HTTPHeader{{Name: "X-Token", Value: "t"}}}, TimeoutSeconds: 2}
		ok, msg := pr.Run(ctx, info, env)
		if ok != want {
			t.Errorf("%s: %v (%s)", path, ok, msg)
		}
		if path == "/bad" && msg != "HTTP 503 from "+srv.Listener.Addr().String()+": down for maintenance" {
			t.Errorf("message %q", msg)
		}
	}
	if ok, _ := (Probe{TCPSocket: &TCPSocketAction{Port: Port(p), Host: host}, TimeoutSeconds: 1}).Run(ctx, info, env); !ok {
		t.Error("tcp to a listening port failed")
	}
	srv.Close()
	if ok, msg := (Probe{TCPSocket: &TCPSocketAction{Port: Port(p), Host: host}, TimeoutSeconds: 1}).Run(ctx, info, env); ok || !strings.Contains(msg, "nothing is listening") {
		t.Errorf("tcp to a closed port: %v %s", ok, msg)
	}
	// Without a host: the container's published port, on its Docker host.
	info.HostConfig.PortBindings = map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8088"}}}
	var dialed string
	env = ProbeEnv{HostAddresses: []string{"10.0.0.5"}, Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}}
	if ok, _ := (Probe{TCPSocket: &TCPSocketAction{Port: 80}, TimeoutSeconds: 1}).Run(ctx, info, env); !ok || dialed != "10.0.0.5:8088" {
		t.Errorf("published port: %v %s", ok, dialed)
	}
}

// probeServers is a running container whose exec probes answer from
// results, by the command's last word.
type probeServers struct {
	fakeServers
	started string
	results map[string]bool
}

func (f *probeServers) Inspect(context.Context, string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info docker.ContainerJSON
	info.ID = "a"
	info.State.Running = true
	info.State.StartedAt = f.started
	return info, nil
}

func (f *probeServers) RunCheck(_ context.Context, _ string, hc docker.Healthcheck) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results[hc.Test[len(hc.Test)-1]] {
		return 0, "", nil
	}
	return 1, "nope", nil
}

func (f *probeServers) RestartUnhealthy(ctx context.Context, name, reason string) (bool, error) {
	f.fakeServers.RestartUnhealthy(ctx, name, reason)
	f.mu.Lock()
	f.started = "2026-11-09T12:01:30Z" // just before the clock
	f.mu.Unlock()
	return false, nil
}

func (f *probeServers) set(k string, v bool) {
	f.mu.Lock()
	f.results[k] = v
	f.mu.Unlock()
}

type safeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (f *safeNotifier) Notify(e notify.Event) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}
func (f *safeNotifier) Clear(string) {}

func TestProber(t *testing.T) {
	st := testStore(t)
	cmd := func(c string) *ExecAction { return &ExecAction{Command: []string{"check", c}} }
	if _, err := st.Set("app", Settings{Probes: Probes{
		Startup:   &Probe{Exec: cmd("startup"), PeriodSeconds: 5, FailureThreshold: 4, InitialDelaySeconds: 20},
		Liveness:  &Probe{Exec: cmd("live"), PeriodSeconds: 10, FailureThreshold: 3},
		Readiness: &Probe{Exec: cmd("ready"), PeriodSeconds: 10, FailureThreshold: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	srvs := &probeServers{started: "2026-11-09T12:00:00Z", results: map[string]bool{"live": true}}
	srvs.jobs = map[string]bool{}
	srvs.list = []manager.Server{{ID: "a", Name: "app", State: "running"}, {ID: "b", Name: "other", State: "running"}}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	n := &safeNotifier{}
	p := NewProber(ProberOptions{Store: st, Servers: srvs, Notifier: n, Now: func() time.Time { return now }})
	ctx := context.Background()
	step := func(d time.Duration) ProbeStatus {
		t.Helper()
		now = now.Add(d)
		p.Tick(ctx)
		p.wg.Wait()
		s, _ := p.Status("app")
		return s
	}

	// Nothing runs before the initial delay.
	if s := step(5 * time.Second); s.Probes[ProbeStartup].Result != "" {
		t.Fatalf("probed during the initial delay: %+v", s)
	}
	if _, ok := p.Status("other"); ok {
		t.Error("a server without probes is probed")
	}
	// The startup probe fails, holding the others back.
	s := step(16 * time.Second)
	if s.Started || s.Probes[ProbeStartup].Result != ResultFailure || s.Probes[ProbeLiveness].Result != "" {
		t.Fatalf("startup failing: %+v", s)
	}
	srvs.set("startup", true)
	if s = step(5 * time.Second); !s.Started || s.Ready {
		t.Fatalf("started: %+v", s)
	}
	// The others start right after; not ready until readiness succeeds.
	s = step(time.Second)
	if s.Probes[ProbeLiveness].Result != ResultSuccess || s.Probes[ProbeReadiness].Result != ResultFailure || s.Ready {
		t.Fatalf("after startup: %+v", s)
	}
	srvs.set("ready", true)
	if s = step(10 * time.Second); !s.Ready {
		t.Fatalf("not ready: %+v", s)
	}
	// Readiness failing twice: not ready, and said once.
	srvs.set("ready", false)
	step(10 * time.Second)
	if s = step(10 * time.Second); s.Ready || len(n.events) != 1 || !strings.Contains(n.events[0].Title, "isn't ready") {
		t.Fatalf("readiness failing: %+v %+v", s, n.events)
	}
	// Liveness failing three times: restarted, but not during a backup.
	srvs.set("live", false)
	srvs.jobs["app"] = true
	for range 3 {
		step(10 * time.Second)
	}
	if len(srvs.restarted) != 0 {
		t.Fatalf("restarted during a job: %v", srvs.restarted)
	}
	srvs.mu.Lock()
	srvs.jobs["app"] = false
	srvs.mu.Unlock()
	step(10 * time.Second)
	if len(srvs.restarted) != 1 || !strings.Contains(srvs.restarted[0], "liveness probe failed 4 times") {
		t.Fatalf("restarts %v", srvs.restarted)
	}
	// The new start begins again with the startup probe, after its delay.
	srvs.set("live", true)
	if s = step(time.Second); s.Started || s.Probes[ProbeStartup].Result != "" {
		t.Fatalf("after the restart: %+v", s)
	}
	// The startup probe never succeeding restarts it too.
	srvs.set("startup", false)
	for range 7 {
		step(5 * time.Second)
	}
	if s = step(5 * time.Second); len(srvs.restarted) != 2 || !strings.Contains(srvs.restarted[1], "startup probe failed 4 times") {
		t.Fatalf("startup never passing: %v %+v", srvs.restarted, s)
	}
	// Probes taken away: it's forgotten.
	st.Set("app", Settings{})
	step(time.Second)
	if _, ok := p.Status("app"); ok {
		t.Error("still probed")
	}
}
