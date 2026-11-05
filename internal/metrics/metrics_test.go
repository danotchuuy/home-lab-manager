package metrics

import (
	"bufio"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/metricstore"
)

type fakeServers struct {
	list  []manager.Server
	stats map[string]docker.Stats
}

func (f *fakeServers) List(context.Context) ([]manager.Server, error) { return f.list, nil }
func (f *fakeServers) StatsOf(context.Context, []manager.Server) map[string]docker.Stats {
	return f.stats
}
func (f *fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	return docker.ContainerJSON{ID: id, RestartCount: 2}, nil
}

type fakePlayers map[string]int

func (f fakePlayers) Players(name string) (int, bool) { n, ok := f[name]; return n, ok }

func TestSampler(t *testing.T) {
	store, err := metricstore.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServers{
		list: []manager.Server{{ID: "c1", Name: "valheim", State: "running"}, {ID: "c2", Name: "mc", State: "exited"}},
		stats: map[string]docker.Stats{"c1": {CPUPercent: 150, MemoryUsage: 2 << 30, NetworkRx: 1000, NetworkTx: 10,
			BlockRead: 0, BlockWrite: 4096}},
	}
	s := New(store, f, fakePlayers{"valheim": 3}, nil)
	s.Sample(context.Background())
	// The next sample: 1 MB more received. Rates come from the difference.
	s.mu.Lock()
	p := s.prev["c1"]
	p.at = p.at.Add(-10 * time.Second)
	s.prev["c1"] = p
	s.mu.Unlock()
	st := f.stats["c1"]
	st.NetworkRx += 1_000_000
	f.stats["c1"] = st
	s.Sample(context.Background())

	h := store.Query(ServerGroup("valheim"), time.Hour).Series
	last := func(name string) float64 { v := h[name].Max; return float64(v[len(v)-1]) }
	if last(CPU) != 150 || last(Memory) != 2<<30 || last(Players) != 3 {
		t.Errorf("cpu %v mem %v players %v", last(CPU), last(Memory), last(Players))
	}
	if r := last(Rx); r < 99_000 || r > 101_000 {
		t.Errorf("rx rate %v, want ~100000", r)
	}
	if _, ok := store.Query(ServerGroup("mc"), time.Hour).Series[CPU]; ok {
		t.Error("stopped server has CPU history")
	}
	l := s.Latest()
	if l["valheim"].Stats == nil || l["valheim"].Restarts != 2 || l["mc"].Stats != nil {
		t.Errorf("latest = %+v", l)
	}

	s.ObserveHost(hostmetrics.Snapshot{Available: true, SampledAt: time.Now(), CPU: hostmetrics.CPU{Percent: 40},
		Disks:   []hostmetrics.Filesystem{{Mount: "/mnt/user", Total: 100, Used: 25}},
		Network: []hostmetrics.Interface{{Name: "eth0", RxRate: 5}, {Name: "eth1", RxRate: 7}}})
	host := store.Query(HostGroup, time.Hour).Series
	if v := host[FSSeries("/mnt/user")].Avg; float64(v[len(v)-1]) != 25 {
		t.Errorf("fs used = %v", v[len(v)-1])
	}
	if v := host[Rx].Avg; float64(v[len(v)-1]) != 12 {
		t.Errorf("host rx = %v", v[len(v)-1])
	}
}

// A sample line: name, optional labels, value.
var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? (NaN|[+-]Inf|-?[0-9.eE+-]+)$`)

// parseExposition checks text against the format and returns its samples'
// first lines by family.
func parseExposition(t *testing.T, text string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	typed := map[string]bool{}
	var current string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			current = strings.Fields(line)[2]
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if f[2] != current || typed[current] || (f[3] != "gauge" && f[3] != "counter") {
				t.Errorf("bad TYPE line %q", line)
			}
			typed[current] = true
		default:
			if !sampleLine.MatchString(line) {
				t.Errorf("bad sample %q", line)
			}
			name, _, _ := strings.Cut(line, "{")
			name, _, _ = strings.Cut(name, " ")
			if name != current {
				t.Errorf("sample %q outside its family %q", line, current)
			}
			out[name] = append(out[name], line)
		}
	}
	return out
}

func TestExposition(t *testing.T) {
	var e Exposition
	e.Gauge("hlm_server_up", "1 if running.", 1, "server", "valheim", "image", `odd"name\with
newline`)
	e.Counter("hlm_server_restarts_total", "Restarts.", 3, "server", "valheim")
	e.Gauge("hlm_server_up", "1 if running.", 0, "server", "mc")
	e.Gauge("hlm_build_info", "Version.", 1, "version", "v1.2.3")
	e.Gauge("hlm_x", "NaN.", nan())
	var b strings.Builder
	e.WriteTo(&b)
	got := parseExposition(t, b.String())
	if len(got["hlm_server_up"]) != 2 || !strings.Contains(got["hlm_server_up"][0], `image="odd\"name\\with\nnewline"`) {
		t.Errorf("server_up = %q", got["hlm_server_up"])
	}
	if got["hlm_server_restarts_total"][0] != `hlm_server_restarts_total{server="valheim"} 3` || got["hlm_x"][0] != "hlm_x NaN" {
		t.Errorf("text:\n%s", b.String())
	}
}

func nan() float64 { var z float64; return z / z }
