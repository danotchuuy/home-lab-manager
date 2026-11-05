package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// memDocker is an in-memory Docker host: enough for a stack's life.
type memDocker struct {
	mu         sync.Mutex
	containers map[string]docker.ContainerSummary // by ID
	networks   []docker.Network
	n          int
}

func newMemDocker() *memDocker { return &memDocker{containers: map[string]docker.ContainerSummary{}} }

func (m *memDocker) find(idOrName string) (string, bool) {
	for id, c := range m.containers {
		if id == idOrName || (len(c.Names) > 0 && c.Names[0] == "/"+idOrName) {
			return id, true
		}
	}
	return "", false
}

func (m *memDocker) ListContainers(_ context.Context, labels ...string) ([]docker.ContainerSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []docker.ContainerSummary
	for _, c := range m.containers {
		ok := true
		for _, l := range labels {
			k, v, _ := strings.Cut(l, "=")
			if c.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memDocker) InspectContainer(_ context.Context, id string) (docker.ContainerJSON, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.find(id)
	if !ok {
		return docker.ContainerJSON{}, &docker.APIError{Status: 404, Message: "No such container: " + id}
	}
	var info docker.ContainerJSON
	info.ID, info.Name = key, m.containers[key].Names[0]
	info.State.Status = m.containers[key].State
	info.State.Running = info.State.Status == "running"
	return info, nil
}

func (m *memDocker) CreateContainer(_ context.Context, name string, req docker.CreateRequest) (string, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.find(name); ok {
		return "", nil, fmt.Errorf("conflict: %s exists", name)
	}
	m.n++
	id := fmt.Sprintf("c%d", m.n)
	m.containers[id] = docker.ContainerSummary{ID: id, Names: []string{"/" + name}, Image: req.Image, State: "created", Labels: req.Labels}
	return id, nil, nil
}

func (m *memDocker) setState(id, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.find(id)
	if !ok {
		return errors.New("no such container")
	}
	c := m.containers[key]
	c.State = state
	m.containers[key] = c
	return nil
}

func (m *memDocker) StartContainer(_ context.Context, id string) error {
	return m.setState(id, "running")
}
func (m *memDocker) StopContainer(_ context.Context, id string, _ int) error {
	return m.setState(id, "exited")
}

func (m *memDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, _ := m.find(id)
	delete(m.containers, key)
	return nil
}

func (m *memDocker) RenameContainer(_ context.Context, id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, _ := m.find(id)
	c := m.containers[key]
	c.Names = []string{"/" + name}
	m.containers[key] = c
	return nil
}

func (m *memDocker) ConnectNetwork(context.Context, string, string, any) error { return nil }

func (m *memDocker) ListNetworks(context.Context) ([]docker.Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]docker.Network(nil), m.networks...), nil
}

func (m *memDocker) CreateNetwork(_ context.Context, req docker.CreateNetworkRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.networks = append(m.networks, docker.Network{ID: req.Name, Name: req.Name})
	return req.Name, nil
}

func (m *memDocker) RemoveNetwork(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, n := range m.networks {
		if n.ID == id {
			m.networks = append(m.networks[:i], m.networks[i+1:]...)
			break
		}
	}
	return nil
}

func (m *memDocker) PullImage(context.Context, string, func(docker.PullProgress)) error { return nil }
func (m *memDocker) InspectImage(context.Context, string) (docker.Image, error) {
	return docker.Image{ID: "sha256:img"}, nil
}

// hostsManager records what the stack service asked of the manager.
type hostsManager struct {
	hosts   map[string]*memDocker
	removed []string
	roots   []string
	ports   []string
}

func (h *hostsManager) PortConflictsOn(_ context.Context, host string, _ docker.CreateRequest, _ string) ([]manager.PortConflict, error) {
	h.ports = append(h.ports, host)
	return nil, nil
}

func (h *hostsManager) Remove(ctx context.Context, id string) error {
	h.removed = append(h.removed, id)
	host, bare := manager.SplitID(id)
	return h.hosts[host].RemoveContainer(ctx, bare, true)
}

func (h *hostsManager) AppdataRootOf(host string) string {
	h.roots = append(h.roots, host)
	return "/data/" + host
}

const twoServices = `
services:
  web:
    image: nginx:1
    ports: ["8080:80"]
  db:
    image: postgres:16
    volumes: ["./db:/var/lib/postgresql/data"]
`

// A stack on another host: everything happens on that host's Docker, its
// services are named name@host, and it stays there.
func TestStackOnAnotherHost(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	local, nas := newMemDocker(), newMemDocker()
	mgr := &hostsManager{hosts: map[string]*memDocker{manager.LocalHost: local, "nas": nas}}
	s := New(store, func(host string) (Docker, error) {
		if d, ok := mgr.hosts[host]; ok {
			return d, nil
		}
		return nil, fmt.Errorf("there's no Docker host %q", host)
	}, mgr, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	emit := func(Event) {}

	if _, err := s.Preview(ctx, "app", twoServices, "", false, "nope"); err == nil || !strings.Contains(err.Error(), "no Docker host") {
		t.Errorf("unknown host: %v", err)
	}
	plan, err := s.Preview(ctx, "app", twoServices, "", false, "nas")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Services[1].Mounts, " "), "/data/nas/") && !strings.Contains(strings.Join(plan.Services[0].Mounts, " "), "/data/nas/") {
		t.Errorf("mounts not under nas's data folder: %+v", plan.Services)
	}
	if err := s.Apply(ctx, "app", twoServices, "", false, "nas", emit); err != nil {
		t.Fatal(err)
	}
	if len(nas.containers) != 2 || len(local.containers) != 0 || len(nas.networks) == 0 || len(local.networks) != 0 {
		t.Fatalf("nas %d containers %d networks, local %d %d", len(nas.containers), len(nas.networks), len(local.containers), len(local.networks))
	}
	for _, h := range mgr.ports {
		if h != "nas" {
			t.Errorf("ports checked on %s", h)
		}
	}
	st, err := s.Get(ctx, "app")
	if err != nil || st.Host != "nas" || len(st.Services) != 2 {
		t.Fatalf("stack %+v %v", st, err)
	}
	for _, m := range st.Services {
		if m.Server != m.Container+"@nas" || m.State != "running" {
			t.Errorf("member %+v", m)
		}
	}
	// It stays on its host: applying again with another host changes nothing.
	if err := s.Apply(ctx, "app", strings.Replace(twoServices, "nginx:1", "nginx:2", 1), "", true, manager.LocalHost, emit); err != nil {
		t.Fatal(err)
	}
	if len(local.containers) != 0 || len(nas.containers) != 2 {
		t.Errorf("moved: local %d, nas %d", len(local.containers), len(nas.containers))
	}
	if err := s.Act(ctx, "app", "stop", emit); err != nil {
		t.Fatal(err)
	}
	for _, c := range nas.containers {
		if c.State != "exited" {
			t.Errorf("not stopped: %+v", c)
		}
	}
	if err := s.Delete(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if len(nas.containers) != 0 || len(mgr.removed) == 0 {
		t.Errorf("left %d, removed %v", len(nas.containers), mgr.removed)
	}
	for _, id := range mgr.removed {
		if !strings.HasSuffix(id, "@nas") {
			t.Errorf("removed %s, not a nas server ID", id)
		}
	}

	// A local stack is as before.
	if err := s.Apply(ctx, "here", twoServices, "", false, "", emit); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Get(ctx, "here"); st.Host != manager.LocalHost || st.Services[0].Server != st.Services[0].Container || len(local.containers) != 2 {
		t.Errorf("local stack %+v", st)
	}
}
