package manager

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// labelDocker serves container listing (with label filters), inspect,
// start/stop/restart and image pulls.
type labelDocker struct {
	mu         sync.Mutex
	containers []docker.ContainerJSON
	actions    []string
	pullFails  bool
}

func (f *labelDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := apiPrefix.ReplaceAllString(r.URL.Path, "")
	switch {
	case p == "/_ping":
		w.Header().Set("API-Version", "1.45")
	case p == "/containers/json":
		var filters struct {
			Label []string `json:"label"`
			ID    []string `json:"id"`
		}
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		list := []docker.ContainerSummary{}
	next:
		for _, c := range f.containers {
			if len(filters.ID) > 0 && c.ID != filters.ID[0] {
				continue
			}
			for _, l := range filters.Label {
				k, v, hasValue := strings.Cut(l, "=")
				if got, ok := c.Config.Labels[k]; !ok || (hasValue && got != v) {
					continue next
				}
			}
			list = append(list, docker.ContainerSummary{ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels, State: c.State.Status, Image: c.Config.Image})
		}
		json.NewEncoder(w).Encode(list)
	case p == "/images/create":
		if f.pullFails {
			http.Error(w, `{"message":"registry unreachable"}`, http.StatusInternalServerError)
			return
		}
		f.actions = append(f.actions, "pull "+r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
		w.Write([]byte(`{"status":"Status: Image is up to date"}` + "\n"))
	case strings.HasPrefix(p, "/containers/"):
		rest := strings.TrimPrefix(p, "/containers/")
		id, action, _ := strings.Cut(rest, "/")
		for _, c := range f.containers {
			if c.ID != id && c.Name != "/"+id {
				continue
			}
			if action == "json" {
				json.NewEncoder(w).Encode(c)
			} else {
				f.actions = append(f.actions, action+" "+c.ID)
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	default:
		http.Error(w, `{"message":"not implemented: `+r.Method+` `+p+`"}`, http.StatusNotImplemented)
	}
}

func TestConnections(t *testing.T) {
	var wg, game docker.ContainerJSON
	wg.ID, wg.Name, wg.State.Status, wg.Image = "w1", "/hlm-wireguard", "running", "sha256:old"
	wg.Config.Image = "lscr.io/linuxserver/wireguard:latest"
	wg.Config.Labels = map[string]string{"io.hlm.wireguard": "home"}
	wg.HostConfig.RestartPolicy.Name = "unless-stopped"
	game = gameServer("g1", "valheim")
	fd := &labelDocker{containers: []docker.ContainerJSON{wg, game}}
	srv := httptest.NewServer(fd)
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	ctx := context.Background()
	if conns, err := m.Connections(ctx); err != nil || len(conns) != 0 {
		t.Errorf("before any are registered: %+v, %v", conns, err)
	}
	var recreated []string
	m.UseConnection("io.hlm.wireguard", ConnectionKind{Kind: "wireguard", Title: "WireGuard tunnel", Page: "/proxy",
		Recreate: func(_ context.Context, step, _ func(string)) error {
			step("Creating the hlm-wireguard container")
			recreated = append(recreated, "wireguard")
			return nil
		}})
	m.UseConnection("io.hlm.cloudflared", ConnectionKind{Kind: "cloudflared", Title: "Cloudflare Tunnel", Page: "/tunnel"})

	conns, err := m.Connections(ctx)
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %+v, %v", conns, err)
	}
	c := conns[0]
	if c.Name != "hlm-wireguard" || c.Kind != "wireguard" || c.Page != "/proxy" || c.Image != wg.Config.Image || c.RestartPolicy != "unless-stopped" || c.State != "running" {
		t.Errorf("connection = %+v", c)
	}

	// Looked up by name, for its own page; game servers aren't connections.
	if got, err := m.Connection(ctx, "hlm-wireguard"); err != nil || got.ID != "w1" || got.Kind != "wireguard" {
		t.Errorf("connection by name = %+v, %v", got, err)
	}
	if _, err := m.Connection(ctx, "valheim"); !errors.Is(err, ErrNotManaged) {
		t.Errorf("game server as a connection: %v", err)
	}
	if _, err := m.ConnectionLogs(ctx, "valheim", docker.LogsOptions{}); !errors.Is(err, ErrNotManaged) {
		t.Errorf("game server's logs as a connection's: %v", err)
	}

	if err := m.ConnectionAction(ctx, "hlm-wireguard", "restart"); err != nil {
		t.Error(err)
	}
	// A game server isn't a connection, and vice versa.
	if err := m.ConnectionAction(ctx, "valheim", "stop"); !errors.Is(err, ErrNotManaged) {
		t.Errorf("game server as a connection: %v", err)
	}
	if _, err := m.Get(ctx, "hlm-wireguard"); !errors.Is(err, ErrNotManaged) {
		t.Errorf("connection as a game server: %v", err)
	}
	if err := m.ConnectionAction(ctx, "gone", "stop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing container: %v", err)
	}

	var events []Event
	if err := m.UpdateConnection(ctx, "hlm-wireguard", func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(recreated, ",") != "wireguard" {
		t.Errorf("recreated = %v", recreated)
	}
	if got := strings.Join(fd.actions, ","); got != "restart w1,pull lscr.io/linuxserver/wireguard:latest" {
		t.Errorf("docker calls = %s", got)
	}
	if last := events[len(events)-1]; last.Stage != "done" {
		t.Errorf("last event = %+v", last)
	}
	jobs := m.Jobs()
	if len(jobs) != 1 || jobs[0].Server != "hlm-wireguard" || jobs[0].Kind != "update" || jobs[0].Finished == nil || jobs[0].Error != "" {
		t.Errorf("jobs = %+v", jobs)
	}

	// An unreachable registry fails the update without recreating anything.
	fd.pullFails = true
	if err := m.UpdateConnection(ctx, "hlm-wireguard", func(Event) {}); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("update with a failing pull: %v", err)
	}
	if len(recreated) != 1 {
		t.Errorf("recreated after a failed pull: %v", recreated)
	}
}

func TestSelfConnection(t *testing.T) {
	var self docker.ContainerJSON
	self.ID, self.Name, self.State.Status, self.State.Running = "s1", "/home-lab-manager", "running", true
	self.Config.Image = "danotchuuy/home-lab-manager:latest"
	fd := &labelDocker{containers: []docker.ContainerJSON{self, gameServer("g1", "valheim")}}
	srv := httptest.NewServer(fd)
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	ctx := context.Background()

	// Not in a container, or in one on another Docker host: nothing shows.
	for _, id := range []string{"", "elsewhere"} {
		m.UseSelf(id, ConnectionKind{Kind: "hlm", Title: "Home Lab Manager", Page: "/system"})
		if conns, err := m.Connections(ctx); err != nil || len(conns) != 0 {
			t.Errorf("self %q: %+v, %v", id, conns, err)
		}
	}

	m.UseSelf("s1", ConnectionKind{Kind: "hlm", Title: "Home Lab Manager", Page: "/system"})
	conns, err := m.Connections(ctx)
	if err != nil || len(conns) != 1 || !conns[0].Self || conns[0].Name != "home-lab-manager" || conns[0].Kind != "hlm" {
		t.Fatalf("connections = %+v, %v", conns, err)
	}
	if c, err := m.Connection(ctx, "home-lab-manager"); err != nil || !c.Self {
		t.Errorf("by name: %+v, %v", c, err)
	}
	// It can be restarted but not stopped, and updates itself elsewhere.
	if err := m.ConnectionAction(ctx, "home-lab-manager", "stop"); !errors.Is(err, ErrStopSelf) {
		t.Errorf("stop: %v", err)
	}
	if err := m.ConnectionAction(ctx, "home-lab-manager", "restart"); err != nil {
		t.Error(err)
	}
	if err := m.UpdateConnection(ctx, "home-lab-manager", func(Event) {}); err == nil || !strings.Contains(err.Error(), "System page") {
		t.Errorf("update: %v", err)
	}
	if got := strings.Join(fd.actions, ","); got != "restart s1" {
		t.Errorf("docker calls = %s", got)
	}
}
