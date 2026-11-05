package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// PortConflict is a host port a server would use that something else on
// the host already uses.
type PortConflict struct {
	Port  int    `json:"port"` // on the host
	Proto string `json:"proto"`
	// Container is the server's container port it maps, e.g. "2456/udp".
	Container string `json:"container"`
	// UsedBy is the other container's name, or empty for another program
	// on the host.
	UsedBy  string `json:"usedBy,omitempty"`
	Running bool   `json:"running"`
	Message string `json:"message"`
}

// hostPort is a port a container holds on the host.
type hostPort struct {
	addr      netip.Addr // unspecified: all addresses
	port      int
	proto     string
	container string // the container port, "2456/udp"
}

func (p hostPort) overlaps(q hostPort) bool {
	if p.port != q.port || p.proto != q.proto {
		return false
	}
	return !p.addr.IsValid() || p.addr.IsUnspecified() || !q.addr.IsValid() || q.addr.IsUnspecified() || p.addr == q.addr
}

// hostPorts lists the ports a container holds on the host: its published
// ports, or with host networking, the ports it exposes. Containers with
// their own LAN address hold none.
func hostPorts(networkMode string, exposed map[string]struct{}, bindings map[string][]docker.PortBinding) []hostPort {
	var out []hostPort
	add := func(key string, addr string, port int) {
		_, proto, _ := strings.Cut(key, "/")
		if proto == "" {
			proto = "tcp"
		}
		a, _ := netip.ParseAddr(addr)
		out = append(out, hostPort{addr: a.Unmap(), port: port, proto: proto, container: key})
	}
	if networkMode == "host" {
		for key := range exposed {
			n, _, _ := strings.Cut(key, "/")
			if port, err := strconv.Atoi(n); err == nil {
				add(key, "", port)
			}
		}
		return out
	}
	for key, bs := range bindings {
		for _, b := range bs {
			// An empty host port means Docker picks a free one.
			if port, err := strconv.Atoi(b.HostPort); err == nil && port > 0 {
				add(key, b.HostIP, port)
			}
		}
	}
	return out
}

// PortConflicts checks the host ports a server's settings would use against
// the other containers on the Docker host (running or not: a stopped one
// takes its ports back when it starts) and, when the host can be read, the
// programs listening on it. self is the server being reconfigured, if any.
// The server is on the host its name (or self) says.
func (m *Manager) PortConflicts(ctx context.Context, req deploy.Request, self string) ([]PortConflict, error) {
	host, bare := SplitID(req.Name)
	if self != "" {
		host = HostOf(self)
	}
	req.Name = bare
	spec, err := m.build(ctx, host, req)
	if err != nil {
		return nil, err
	}
	return m.portConflicts(ctx, host, spec.Create, self)
}

// PortConflictsOf is PortConflicts for a container's create request on the
// local host.
func (m *Manager) PortConflictsOf(ctx context.Context, c docker.CreateRequest, self string) ([]PortConflict, error) {
	return m.portConflicts(ctx, LocalHost, c, self)
}

// PortConflictsOn is PortConflictsOf on a Docker host.
func (m *Manager) PortConflictsOn(ctx context.Context, host string, c docker.CreateRequest, self string) ([]PortConflict, error) {
	return m.portConflicts(ctx, cmp.Or(host, LocalHost), c, self)
}

func (m *Manager) portConflicts(ctx context.Context, host string, c docker.CreateRequest, self string) ([]PortConflict, error) {
	if c.Labels[deploy.LabelDirectIP] == "true" {
		return []PortConflict{}, nil
	}
	wanted := hostPorts(c.HostConfig.NetworkMode, c.ExposedPorts, c.HostConfig.PortBindings)
	out := []PortConflict{}
	if len(wanted) == 0 {
		return out, nil
	}
	list, err := m.docker.ListContainersOn(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	// Ports any container publishes, including this server's own, are held
	// by Docker on the host; they're not "another program".
	published := map[hostPort]bool{}
	reported := map[string]bool{}
	for _, s := range list {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		info, err := m.docker.InspectContainer(ctx, s.ID)
		if err != nil {
			continue // removed meanwhile
		}
		theirs := hostPorts(info.HostConfig.NetworkMode, info.Config.ExposedPorts, info.HostConfig.PortBindings)
		for _, p := range theirs {
			published[hostPort{port: p.port, proto: p.proto}] = true
		}
		if self != "" && (name == self || strings.HasPrefix(s.ID, self)) {
			continue
		}
		for _, w := range wanted {
			for _, p := range theirs {
				key := fmt.Sprintf("%d/%s %s", w.port, w.proto, name)
				if !w.overlaps(p) || reported[key] {
					continue
				}
				reported[key] = true
				msg := fmt.Sprintf("Host port %d/%s is already used by %s.", w.port, w.proto, name)
				if !info.State.Running {
					msg = fmt.Sprintf("Host port %d/%s is already used by %s (stopped), so they couldn't run at the same time.", w.port, w.proto, name)
				}
				out = append(out, PortConflict{Port: w.port, Proto: w.proto, Container: w.container, UsedBy: name, Running: info.State.Running, Message: msg})
			}
		}
	}

	if host == LocalHost && m.host != nil && m.host.Available() {
		if ls, err := m.host.Listeners(); err == nil {
			for _, w := range wanted {
				if published[hostPort{port: w.port, proto: w.proto}] {
					continue
				}
				for _, l := range ls {
					if !w.overlaps(hostPort{addr: l.Addr, port: l.Port, proto: l.Proto}) {
						continue
					}
					key := fmt.Sprintf("%d/%s", w.port, w.proto)
					if !reported[key] {
						reported[key] = true
						out = append(out, PortConflict{Port: w.port, Proto: w.proto, Container: w.container, Running: true,
							Message: fmt.Sprintf("Host port %d/%s is already in use by another program on the host.", w.port, w.proto)})
					}
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out, nil
}

// HostPortsInUse lists the ports ("27015/udp") on a host that containers
// publish (running or not) and, when the host can be read, that programs
// on it listen on.
func (m *Manager) HostPortsInUse(ctx context.Context, host string) (map[string]bool, error) {
	list, err := m.docker.ListContainersOn(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	used := map[string]bool{}
	for _, s := range list {
		info, err := m.docker.InspectContainer(ctx, s.ID)
		if err != nil {
			continue // removed meanwhile
		}
		for _, p := range hostPorts(info.HostConfig.NetworkMode, info.Config.ExposedPorts, info.HostConfig.PortBindings) {
			used[fmt.Sprintf("%d/%s", p.port, p.proto)] = true
		}
	}
	if host == LocalHost && m.host != nil && m.host.Available() {
		if ls, err := m.host.Listeners(); err == nil {
			for _, l := range ls {
				used[fmt.Sprintf("%d/%s", l.Port, l.Proto)] = true
			}
		}
	}
	return used, nil
}

// checkPorts refuses settings whose host ports are already in use.
func (m *Manager) checkPorts(ctx context.Context, host string, spec *deploy.Spec, self string) error {
	conflicts, err := m.portConflicts(ctx, host, spec.Create, self)
	if err != nil {
		return err
	}
	if len(conflicts) == 0 {
		return nil
	}
	msgs := make([]string, len(conflicts))
	for i, c := range conflicts {
		msgs[i] = c.Message
	}
	return errors.New(strings.Join(msgs, " ") + " Choose other host ports.")
}
