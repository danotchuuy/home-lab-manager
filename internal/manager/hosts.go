package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// LocalHost is the Docker host hlm was started with (DOCKER_HOST or the
// local socket). Its servers keep their plain container names as IDs.
const LocalHost = "local"

// Servers on other hosts are named <container>@<host>: Docker doesn't
// allow @ in container names, so the two can't be confused, and the name is
// safe in URLs, file names and object keys.
const hostSep = "@"

// SplitID splits a server ID or name into its host and the container's own
// ID or name on that host.
func SplitID(id string) (host, bare string) {
	if i := strings.LastIndex(id, hostSep); i >= 0 {
		return id[i+1:], id[:i]
	}
	return LocalHost, id
}

// JoinID names a container on a host the way server IDs do.
func JoinID(host, bare string) string {
	if host == "" || host == LocalHost || bare == "" {
		return bare
	}
	return bare + hostSep + host
}

// HostOf returns the host a server (by ID or name) is on.
func HostOf(id string) string {
	h, _ := SplitID(id)
	return h
}

// RemoteHost is another Docker host whose servers hlm manages.
type RemoteHost struct {
	ID     string
	Name   string
	Docker *docker.Client
	// AppdataRoot is where servers' data folders go on that host.
	AppdataRoot string
	// Address is where the host's published ports are reached, for Web UI
	// links and game queries.
	Address string
}

// HostStatus says whether a host could be reached the last time hlm
// listed its containers.
type HostStatus struct {
	ID      string    `json:"id"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked"`
}

// router sends Docker calls to the host a container is on. Container IDs
// and names going in are server IDs (with @host for other hosts); those
// coming out are qualified the same way.
type router struct {
	local *docker.Client

	mu      sync.RWMutex
	remotes map[string]RemoteHost
	status  map[string]HostStatus
	// probing marks the hosts being tried again in the background.
	probing map[string]bool
}

func newRouter(local *docker.Client) *router {
	return &router{local: local, remotes: map[string]RemoteHost{}, status: map[string]HostStatus{}, probing: map[string]bool{}}
}

// retryDown is how often a host that couldn't be reached is tried again, in
// the background. Until it answers, listings leave it out straight away
// rather than wait for it: one host that's off shouldn't slow every page.
var retryDown = 15 * time.Second

// remoteTimeout bounds a listing's calls to another host, so one that's slow
// to answer doesn't hold up the rest.
var remoteTimeout = 10 * time.Second

// ErrHostDown is returned when listing a host that couldn't be reached the
// last time it was tried.
var ErrHostDown = errors.New("the Docker host can't be reached")

// down reports whether a remote host failed the last time it was tried, and
// why; it starts trying it again in the background when that's due.
func (r *router) down(host string) (bool, string) {
	if host == LocalHost || host == "" {
		return false, ""
	}
	r.mu.Lock()
	st, ok := r.status[host]
	if !ok || st.OK {
		r.mu.Unlock()
		return false, ""
	}
	due := time.Since(st.Checked) >= retryDown && !r.probing[host]
	if due {
		r.probing[host] = true
	}
	r.mu.Unlock()
	if due {
		go r.probe(host)
	}
	return true, st.Error
}

// probe tries a down host again.
func (r *router) probe(host string) {
	defer func() {
		r.mu.Lock()
		delete(r.probing, host)
		r.mu.Unlock()
	}()
	c, err := r.client(host)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	_, err = c.Version(ctx)
	r.setStatus(host, err)
}

// downError is ErrHostDown for a host, with why.
func downError(host, why string) error {
	if why == "" {
		return fmt.Errorf("%w: %s", ErrHostDown, host)
	}
	return fmt.Errorf("%w: %s (%s); hlm tries it again every %s", ErrHostDown, host, why, retryDown)
}

// errNoHost is returned for servers on a host hlm no longer knows.
func errNoHost(host string) error {
	return fmt.Errorf("%w: there is no Docker host %q", ErrNotFound, host)
}

func (r *router) client(host string) (*docker.Client, error) {
	if host == LocalHost || host == "" {
		return r.local, nil
	}
	r.mu.RLock()
	h, ok := r.remotes[host]
	r.mu.RUnlock()
	if !ok || h.Docker == nil {
		return nil, errNoHost(host)
	}
	return h.Docker, nil
}

// resolve finds the client for a server ID and the container's own ID.
func (r *router) resolve(id string) (*docker.Client, string, string, error) {
	host, bare := SplitID(id)
	c, err := r.client(host)
	return c, host, bare, err
}

// hosts lists the host IDs, local first.
func (r *router) hosts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []string{LocalHost}
	for id := range r.remotes {
		out = append(out, id)
	}
	sort.Strings(out[1:])
	return out
}

func (r *router) remote(host string) (RemoteHost, bool) {
	if r == nil {
		return RemoteHost{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.remotes[host]
	return h, ok
}

func (r *router) setRemotes(list []RemoteHost) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.remotes
	r.remotes = map[string]RemoteHost{}
	for _, h := range list {
		r.remotes[h.ID] = h
		// A host whose connection changed is tried afresh.
		if o, ok := old[h.ID]; ok && o.Docker != h.Docker {
			delete(r.status, h.ID)
		}
	}
	for id := range r.status {
		if _, ok := r.remotes[id]; !ok && id != LocalHost {
			delete(r.status, id)
		}
	}
}

func (r *router) setStatus(host string, err error) {
	st := HostStatus{ID: host, OK: err == nil, Checked: time.Now()}
	// The request (Get "http://docker/...") says nothing useful.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if err != nil {
		st.Error = err.Error()
	}
	r.mu.Lock()
	r.status[host] = st
	r.mu.Unlock()
}

func qualifySummary(host string, c *docker.ContainerSummary) {
	c.ID = JoinID(host, c.ID)
	for i, n := range c.Names {
		c.Names[i] = "/" + JoinID(host, strings.TrimPrefix(n, "/"))
	}
}

func qualifyInfo(host string, info *docker.ContainerJSON) {
	info.ID = JoinID(host, info.ID)
	info.Name = "/" + JoinID(host, strings.TrimPrefix(info.Name, "/"))
}

// ListContainers lists containers on every host. A host that can't be
// reached is left out (see HostStatus), unless it's the local one.
func (r *router) ListContainers(ctx context.Context, labels ...string) ([]docker.ContainerSummary, error) {
	return r.listAll(ctx, func(ctx context.Context, c *docker.Client) ([]docker.ContainerSummary, error) {
		return c.ListContainers(ctx, labels...)
	})
}

// ListContainersWithSize is ListContainers with sizes.
func (r *router) ListContainersWithSize(ctx context.Context, labels ...string) ([]docker.ContainerSummary, error) {
	return r.listAll(ctx, func(ctx context.Context, c *docker.Client) ([]docker.ContainerSummary, error) {
		return c.ListContainersWithSize(ctx, labels...)
	})
}

func (r *router) listAll(ctx context.Context, list func(context.Context, *docker.Client) ([]docker.ContainerSummary, error)) ([]docker.ContainerSummary, error) {
	hosts := r.hosts()
	results := make([][]docker.ContainerSummary, len(hosts))
	errs := make([]error, len(hosts))
	skipped := make([]bool, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		if down, why := r.down(h); down {
			// Its status stays as it was; the probe updates it.
			errs[i], skipped[i] = downError(h, why), true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := r.client(h)
			if err == nil {
				lctx := ctx
				if h != LocalHost {
					// One slow host shouldn't hold up the server list.
					var cancel context.CancelFunc
					lctx, cancel = context.WithTimeout(ctx, remoteTimeout)
					defer cancel()
				}
				var l []docker.ContainerSummary
				l, err = list(lctx, c)
				for j := range l {
					qualifySummary(h, &l[j])
				}
				results[i] = l
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	var out []docker.ContainerSummary
	for i, h := range hosts {
		if h == LocalHost && errs[i] != nil {
			return nil, errs[i]
		}
		if h != LocalHost && ctx.Err() == nil && !skipped[i] {
			r.setStatus(h, errs[i])
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

// ListContainersOn lists the containers on one host.
func (r *router) ListContainersOn(ctx context.Context, host string, labels ...string) ([]docker.ContainerSummary, error) {
	c, err := r.client(host)
	if err != nil {
		return nil, err
	}
	if down, why := r.down(host); down {
		return nil, downError(host, why)
	}
	l, err := c.ListContainers(ctx, labels...)
	for i := range l {
		qualifySummary(host, &l[i])
	}
	return l, err
}

// ListContainersWithSizeOn is ListContainersOn with sizes.
func (r *router) ListContainersWithSizeOn(ctx context.Context, host string, labels ...string) ([]docker.ContainerSummary, error) {
	c, err := r.client(host)
	if err != nil {
		return nil, err
	}
	if down, why := r.down(host); down {
		return nil, downError(host, why)
	}
	l, err := c.ListContainersWithSize(ctx, labels...)
	for i := range l {
		qualifySummary(host, &l[i])
	}
	return l, err
}

// ListContainerByID lists one container by ID.
func (r *router) ListContainerByID(ctx context.Context, id string) ([]docker.ContainerSummary, error) {
	c, host, bare, err := r.resolve(id)
	if err != nil {
		return nil, err
	}
	l, err := c.ListContainerByID(ctx, bare)
	for i := range l {
		qualifySummary(host, &l[i])
	}
	return l, err
}

// InspectContainer inspects a container by server ID or name.
func (r *router) InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error) {
	c, host, bare, err := r.resolve(id)
	if err != nil {
		return docker.ContainerJSON{}, err
	}
	info, err := c.InspectContainer(ctx, bare)
	if err == nil {
		qualifyInfo(host, &info)
	}
	return info, err
}

// CreateContainer creates a container named name (with @host for another
// host) and returns its server ID.
func (r *router) CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error) {
	c, host, bare, err := r.resolve(name)
	if err != nil {
		return "", nil, err
	}
	id, warnings, err := c.CreateContainer(ctx, bare, req)
	return JoinID(host, id), warnings, err
}

// CreateContainerOn creates a container on a host, unnamed if name is "".
func (r *router) CreateContainerOn(ctx context.Context, host, name string, req docker.CreateRequest) (string, []string, error) {
	c, err := r.client(host)
	if err != nil {
		return "", nil, err
	}
	id, warnings, err := c.CreateContainer(ctx, name, req)
	return JoinID(host, id), warnings, err
}

// RenameContainer renames a container; name is its new name on its host.
func (r *router) RenameContainer(ctx context.Context, id, name string) error {
	c, host, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	if h, n := SplitID(name); h != LocalHost {
		if h != host {
			return errors.New("can't move a container to another host by renaming it")
		}
		name = n
	}
	return c.RenameContainer(ctx, bare, name)
}

func (r *router) StartContainer(ctx context.Context, id string) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.StartContainer(ctx, bare)
}

func (r *router) StopContainer(ctx context.Context, id string, timeout int) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.StopContainer(ctx, bare, timeout)
}

func (r *router) RestartContainer(ctx context.Context, id string, timeout int) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.RestartContainer(ctx, bare, timeout)
}

func (r *router) RemoveContainer(ctx context.Context, id string, force bool) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.RemoveContainer(ctx, bare, force)
}

func (r *router) WaitContainer(ctx context.Context, id string) (int, error) {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return 0, err
	}
	return c.WaitContainer(ctx, bare)
}

func (r *router) SetRestartPolicy(ctx context.Context, id, policy string) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.SetRestartPolicy(ctx, bare, policy)
}

func (r *router) Logs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error) {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return nil, err
	}
	return c.Logs(ctx, bare, opts)
}

func (r *router) ContainerStats(ctx context.Context, id string) (docker.Stats, error) {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return docker.Stats{}, err
	}
	return c.ContainerStats(ctx, bare)
}

// Exec runs a command in a container.
func (r *router) Exec(ctx context.Context, id, user string, cmd []string, maxOutput int) (docker.ExecResult, error) {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return docker.ExecResult{}, err
	}
	return c.Exec(ctx, bare, user, cmd, maxOutput)
}

// WriteStdin writes to a container's input.
func (r *router) WriteStdin(ctx context.Context, id string, data []byte) error {
	c, _, bare, err := r.resolve(id)
	if err != nil {
		return err
	}
	return c.WriteStdin(ctx, bare, data)
}

// --- per host: images, networks, volumes ---

func (r *router) PullImage(ctx context.Context, host, ref string, progress func(docker.PullProgress)) error {
	c, err := r.client(host)
	if err != nil {
		return err
	}
	return c.PullImage(ctx, ref, progress)
}

func (r *router) InspectImage(ctx context.Context, host, ref string) (docker.Image, error) {
	c, err := r.client(host)
	if err != nil {
		return docker.Image{}, err
	}
	return c.InspectImage(ctx, ref)
}

func (r *router) RemoteDigest(ctx context.Context, host, ref string) (string, error) {
	c, err := r.client(host)
	if err != nil {
		return "", err
	}
	return c.RemoteDigest(ctx, ref)
}

func (r *router) ListNetworks(ctx context.Context, host string) ([]docker.Network, error) {
	c, err := r.client(host)
	if err != nil {
		return nil, err
	}
	return c.ListNetworks(ctx)
}

func (r *router) InspectNetwork(ctx context.Context, host, id string) (docker.Network, error) {
	c, err := r.client(host)
	if err != nil {
		return docker.Network{}, err
	}
	return c.InspectNetwork(ctx, id)
}

func (r *router) CreateNetwork(ctx context.Context, host string, req docker.CreateNetworkRequest) (string, error) {
	c, err := r.client(host)
	if err != nil {
		return "", err
	}
	return c.CreateNetwork(ctx, req)
}

func (r *router) RemoveNetwork(ctx context.Context, host, id string) error {
	c, err := r.client(host)
	if err != nil {
		return err
	}
	return c.RemoveNetwork(ctx, id)
}

func (r *router) RemoveVolume(ctx context.Context, host, name string) error {
	c, err := r.client(host)
	if err != nil {
		return err
	}
	return c.RemoveVolume(ctx, name)
}

func (r *router) SystemDiskUsage(ctx context.Context, host string) (docker.DiskUsage, error) {
	c, err := r.client(host)
	if err != nil {
		return docker.DiskUsage{}, err
	}
	return c.SystemDiskUsage(ctx)
}

// --- Manager ---

// SetHosts sets the other Docker hosts whose servers hlm manages.
func (m *Manager) SetHosts(list []RemoteHost) {
	m.docker.setRemotes(list)
	m.hostsChanged()
}

// HostStatuses reports which other hosts answered when servers were last
// listed.
func (m *Manager) HostStatuses() map[string]HostStatus {
	m.docker.mu.RLock()
	defer m.docker.mu.RUnlock()
	out := make(map[string]HostStatus, len(m.docker.status))
	for k, v := range m.docker.status {
		out[k] = v
	}
	return out
}

// HostIDs lists the hosts servers can be on, the local one first.
func (m *Manager) HostIDs() []string { return m.docker.hosts() }

// KnownHost reports whether servers can be created on host.
func (m *Manager) KnownHost(host string) bool {
	_, err := m.docker.client(host)
	return err == nil
}

// DockerFor returns the Docker client for a host.
func (m *Manager) DockerFor(host string) (*docker.Client, error) { return m.docker.client(host) }

// appdataFor is where data folders go on a host.
func (m *Manager) appdataFor(host string) string {
	if h, ok := m.docker.remote(host); ok {
		return h.AppdataRoot
	}
	return m.appdataRoot
}

// HostAddress is where a server's published ports are reached from hlm
// and the LAN, for servers on other hosts; "" for local servers.
func (m *Manager) HostAddress(id string) string {
	if h, ok := m.docker.remote(HostOf(id)); ok {
		return h.Address
	}
	return ""
}

// HostName is a host's display name.
func (m *Manager) HostName(host string) string {
	if h, ok := m.docker.remote(host); ok && h.Name != "" {
		return h.Name
	}
	return host
}

// Docker sends container commands (Exec, WriteStdin) to the host a server
// is on, by its server ID; for the console.
func (m *Manager) Docker() interface {
	Exec(ctx context.Context, id, user string, cmd []string, maxOutput int) (docker.ExecResult, error)
	WriteStdin(ctx context.Context, id string, data []byte) error
} {
	return m.docker
}
