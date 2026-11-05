package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Besides game servers, hlm runs containers for connections, such as the
// WireGuard tunnel's home end or the Cloudflare Tunnel's connector. The
// services that set them up register them here by label, so they get the
// same resource usage and update checks as servers.

// ConnectionKind describes the containers carrying one label.
type ConnectionKind struct {
	Kind  string // e.g. "wireguard"
	Title string // e.g. "WireGuard tunnel"
	// Page is where the web UI manages it, e.g. "/proxy".
	Page string
	// Recreate replaces the container on a Docker host with one on a freshly
	// pulled image, keeping its settings, reporting steps and console output
	// as it goes.
	Recreate func(ctx context.Context, host string, step, output func(string)) error
}

// UseConnection registers the containers labelled label.
func (m *Manager) UseConnection(label string, k ConnectionKind) {
	if m.connections == nil {
		m.connections = map[string]ConnectionKind{}
	}
	m.connections[label] = k
}

// UseSelf shows the container hlm itself runs in (id; "" when it doesn't
// run in a container) with the connections, for its resource usage and
// console. It can't be stopped from hlm, which would take hlm down with it,
// and hlm updates itself through package selfupdate, not Recreate.
func (m *Manager) UseSelf(id string, k ConnectionKind) {
	m.selfID, m.self = id, k
}

// Connection is a container hlm runs for a connection.
type Connection struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Kind          string        `json:"kind"`
	Title         string        `json:"title"`
	Page          string        `json:"page"`
	Image         string        `json:"image"`
	State         string        `json:"state"`
	Status        string        `json:"status"`
	Health        string        `json:"health,omitempty"`
	Created       time.Time     `json:"created"`
	Network       string        `json:"network"`
	RestartPolicy string        `json:"restartPolicy"`
	Update        *UpdateStatus `json:"update,omitempty"`
	// Self is the container hlm itself runs in.
	Self    bool `json:"self,omitempty"`
	imageID string
}

// Connections lists the connection containers, by name.
func (m *Manager) Connections(ctx context.Context) ([]Connection, error) {
	out := []Connection{}
	for label, k := range m.connections {
		// Each Docker host can have its own (a tunnel's end, cloudflared).
		var list []docker.ContainerSummary
		for _, host := range m.docker.hosts() {
			l, err := m.docker.ListContainersOn(ctx, host, label)
			if err != nil {
				if host == LocalHost {
					return nil, err
				}
				continue // a host that can't be reached
			}
			list = append(list, l...)
		}
		for _, c := range list {
			info, err := m.docker.InspectContainer(ctx, c.ID)
			if docker.IsNotFound(err) {
				continue // removed since the list call
			}
			if err != nil {
				return nil, err
			}
			out = append(out, m.connection(c, info, k))
		}
	}
	if m.selfID != "" {
		// hlm may manage another Docker host, where its container isn't.
		list, err := m.docker.ListContainerByID(ctx, m.selfID)
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			info, err := m.docker.InspectContainer(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			conn := m.connection(c, info, m.self)
			conn.Self = true
			out = append(out, conn)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) connection(c docker.ContainerSummary, info docker.ContainerJSON, k ConnectionKind) Connection {
	conn := Connection{
		ID:            c.ID,
		Name:          strings.TrimPrefix(firstOr(c.Names, ""), "/"),
		Kind:          k.Kind,
		Title:         k.Title,
		Page:          k.Page,
		Image:         info.Config.Image,
		State:         c.State,
		Status:        c.Status,
		Created:       time.Unix(c.Created, 0),
		Network:       c.HostConfig.NetworkMode,
		RestartPolicy: info.HostConfig.RestartPolicy.Name,
		imageID:       info.Image,
	}
	if conn.Image == "" {
		conn.Image = c.Image
	}
	if info.State.Health != nil {
		conn.Health = info.State.Health.Status
	}
	if st, ok := m.updates.get(conn.ID); ok {
		conn.Update = &st
	}
	return conn
}

// connectionKind looks up a connection container by name or ID and checks
// it's one hlm runs.
func (m *Manager) connectionKind(ctx context.Context, id string) (docker.ContainerJSON, ConnectionKind, error) {
	info, err := m.docker.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return info, ConnectionKind{}, ErrNotFound
	}
	if err != nil {
		return info, ConnectionKind{}, err
	}
	if m.selfID != "" && info.ID == m.selfID {
		return info, m.self, nil
	}
	for label, k := range m.connections {
		if _, ok := info.Config.Labels[label]; ok {
			return info, k, nil
		}
	}
	return info, ConnectionKind{}, ErrNotManaged
}

// Connection looks up one connection container by name or ID.
func (m *Manager) Connection(ctx context.Context, id string) (Connection, error) {
	info, _, err := m.connectionKind(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	conns, err := m.Connections(ctx)
	if err != nil {
		return Connection{}, err
	}
	for _, c := range conns {
		if c.ID == info.ID {
			return c, nil
		}
	}
	return Connection{}, ErrNotFound
}

// ConnectionStats returns a resource usage sample for a running connection
// container.
func (m *Manager) ConnectionStats(ctx context.Context, id string) (docker.Stats, error) {
	info, _, err := m.connectionKind(ctx, id)
	if err != nil || !info.State.Running {
		return docker.Stats{}, err
	}
	return m.docker.ContainerStats(ctx, info.ID)
}

// ConnectionLogs returns a connection container's console output.
func (m *Manager) ConnectionLogs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error) {
	info, _, err := m.connectionKind(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.docker.Logs(ctx, info.ID, opts)
}

// ConnectionAction starts, stops or restarts a connection container.
func (m *Manager) ConnectionAction(ctx context.Context, id, action string) error {
	info, _, err := m.connectionKind(ctx, id)
	if err != nil {
		return err
	}
	if info.ID == m.selfID && action == "stop" {
		return ErrStopSelf
	}
	switch action {
	case "start":
		return m.docker.StartContainer(ctx, info.ID)
	case "stop":
		return m.docker.StopContainer(ctx, info.ID, 10)
	case "restart":
		return m.docker.RestartContainer(ctx, info.ID, 10)
	}
	return fmt.Errorf("unknown action %q", action)
}

// UpdateConnection recreates a connection container on the newest image,
// as a tracked job, through the service that set it up.
func (m *Manager) UpdateConnection(ctx context.Context, id string, emit func(Event)) (err error) {
	info, k, err := m.connectionKind(ctx, id)
	if err != nil {
		return err
	}
	if info.ID == m.selfID {
		return errors.New("hlm updates itself from the System page")
	}
	if k.Recreate == nil {
		return errors.New(k.Title + " can't be updated from here")
	}
	name := strings.TrimPrefix(info.Name, "/")
	emit, finish, err := m.jobs.start(Job{Kind: "update", Server: name, App: k.Title, Image: info.Config.Image}, emit)
	if err != nil {
		return err
	}
	defer func() { finish(err) }()

	// Pull first, so a registry that can't be reached fails the update
	// rather than recreating the container on the image it already has.
	host := HostOf(info.ID)
	emit(Event{Stage: "pull", Message: "Pulling " + info.Config.Image})
	if err := m.docker.PullImage(ctx, host, info.Config.Image, func(p docker.PullProgress) {
		emit(Event{Stage: "pull", Progress: &p})
	}); err != nil {
		return fmt.Errorf("pull %s: %w", info.Config.Image, err)
	}
	step := func(msg string) { emit(Event{Stage: "create", Message: msg}) }
	if err := k.Recreate(ctx, host, step, func(string) {}); err != nil {
		return err
	}
	m.updates.forget(info.ID)
	if n, err := m.docker.InspectContainer(ctx, name); err == nil {
		imageID, st := m.checkImage(ctx, host, n.Config.Image)
		if st.State == UpdateCurrent && imageID != "" && imageID != n.Image {
			st = UpdateStatus{State: UpdateAvailable, Reason: "the container still runs an older image"}
		}
		st.CheckedAt = time.Now()
		m.updates.set(n.ID, st)
	}
	m.log.Info("updated connection container", "container", name)
	emit(Event{Stage: "done", Message: "Updated " + name})
	return nil
}
