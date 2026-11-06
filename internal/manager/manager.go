// Package manager runs game servers as Docker containers built from catalog
// templates. All state lives in Docker: each container carries labels saying
// which template it came from and the values it was created with.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// ErrNotManaged is returned for containers that exist but were not created
// by this program.
var ErrNotManaged = errors.New("container is not managed by home-lab-manager")

// ErrStopSelf is returned for stopping the container hlm itself runs in.
var ErrStopSelf = errors.New("hlm can't stop its own container, since this page would go down with it; stop it with Docker")

// ErrNotFound is returned when there is no such server.
var ErrNotFound = errors.New("server not found")

// Manager creates and controls game server containers.
type Manager struct {
	docker      *router
	catalog     *catalog.Catalog
	appdataRoot string
	// remoteHost is the Docker host's name when it is reached over TCP. Game
	// server ports are published there, so Web UI links point to it.
	remoteHost string
	log        *slog.Logger
	updates    *updateTracker
	jobs       *jobTracker
	storage    *storageTracker
	host       *hostmetrics.Collector
	checkIP    IPChecker
	deleted    *deletedStore // see UseDeletedStore
	// connections are the containers hlm runs for connections, by label;
	// see UseConnection.
	connections map[string]ConnectionKind
	// selfID is the container hlm runs in, shown with the connections as
	// self; see UseSelf.
	selfID string
	// executable is hlm's own binary, for helpers that run it in
	// containers (UseExecutable).
	executable string
	self       ConnectionKind
	// notifier gets crashes, updates and the like; see UseNotifier.
	notifier notify.Notifier
	// crashes counts unexpected exits; see health.go.
	crashes      *crashTracker
	healthPolicy HealthPolicy
	audit        audit.Recorder
	// beforeStop saves a running server's world before hlm stops it; see
	// UseBeforeStop.
	beforeStop  func(ctx context.Context, id string)
	beforeStart func(ctx context.Context, name string)
	// removeOldImages says whether to delete a server's old image after it's
	// recreated with another; see UseOldImageRemoval.
	removeOldImages func() bool
	// Event watchers for other hosts; see WatchEvents.
	watchMu  sync.Mutex
	watchCtx context.Context
	watchers map[string]watcher
}

// UseBeforeStop sets what runs before hlm stops, restarts or recreates a
// running server, such as saving a game that doesn't save when it's told
// to stop. It should give up by itself after a short while.
func (m *Manager) UseBeforeStop(f func(ctx context.Context, id string)) { m.beforeStop = f }

// UseBeforeStart sets what runs before hlm starts a stopped server, by its
// name: putting a sleeping server's wake listener out of its ports' way.
func (m *Manager) UseBeforeStart(f func(ctx context.Context, name string)) { m.beforeStart = f }

func (m *Manager) prepareStart(ctx context.Context, info docker.ContainerJSON) {
	if m.beforeStart != nil && !info.State.Running {
		m.beforeStart(ctx, strings.TrimPrefix(info.Name, "/"))
	}
}

func (m *Manager) prepareStop(ctx context.Context, info docker.ContainerJSON) {
	if m.beforeStop != nil && info.State.Running {
		m.beforeStop(ctx, info.ID)
	}
}

// IPChecker asks the LAN's router whether ip is free. It returns a
// description of the conflict if another device uses it ("" if free), and a
// warning about risks that don't block it. Devices with a MAC in ignore (the
// server's own container) don't count.
type IPChecker func(ctx context.Context, ip string, ignore []string) (conflict, warning string, err error)

// UseIPChecker makes installs and reconfigurations check fixed LAN
// addresses with the router (e.g. UniFi) before using them.
func (m *Manager) UseIPChecker(c IPChecker) {
	m.checkIP = c
}

// checkLANAddress checks a fixed address on a LAN (macvlan/ipvlan) network
// with the router. Addresses on private bridge networks aren't on the LAN,
// so they're left to Docker's own checks.
func (m *Manager) checkLANAddress(ctx context.Context, spec *deploy.Spec, req deploy.Request, ignore []string, emit func(Event)) error {
	if m.checkIP == nil || req.IPAddress == "" || spec.Create.Labels[deploy.LabelDirectIP] != "true" {
		return nil
	}
	conflict, warning, err := m.checkIP(ctx, req.IPAddress, ignore)
	if err != nil {
		emit(Event{Stage: "warning", Message: "couldn't check " + req.IPAddress + " with UniFi: " + err.Error()})
		return nil
	}
	if conflict != "" {
		return fmt.Errorf("%s, according to UniFi; choose another address", conflict)
	}
	if warning != "" {
		emit(Event{Stage: "warning", Message: warning})
	}
	return nil
}

// New creates a manager. Template paths under Unraid's appdata share are
// placed under appdataRoot instead.
func New(d *docker.Client, c *catalog.Catalog, appdataRoot string, log *slog.Logger) *Manager {
	m := &Manager{docker: newRouter(d), catalog: c, appdataRoot: appdataRoot, log: log, updates: newUpdateTracker(), jobs: newJobTracker(), storage: &storageTracker{},
		crashes: newCrashTracker()}
	if u, err := url.Parse(d.Host()); err == nil && u.Scheme != "unix" {
		m.remoteHost = u.Hostname()
	}
	return m
}

// App returns a catalog app with its paths localized for the local host.
func (m *Manager) App(id string) (*catalog.App, bool) {
	return m.AppOn(id, LocalHost)
}

// AppOn returns a catalog app with its paths localized for a host.
func (m *Manager) AppOn(id, host string) (*catalog.App, bool) {
	app, ok := m.catalog.Get(id)
	if !ok {
		return nil, false
	}
	return app.Localize(m.appdataFor(host)), true
}

// Server is a managed game server container.
type Server struct {
	ID string `json:"id"`
	// Host is the Docker host the server is on: LocalHost, or another
	// host's ID (which ID and Name end in, after an @).
	Host       string               `json:"host"`
	Name       string               `json:"name"`
	App        string               `json:"app"`
	TemplateID string               `json:"templateId"`
	Image      string               `json:"image"`
	Icon       string               `json:"icon,omitempty"`
	WebUI      string               `json:"webui,omitempty"`
	State      string               `json:"state"` // created, running, paused, restarting, exited, dead
	Status     string               `json:"status"`
	Health     string               `json:"health,omitempty"`
	Created    time.Time            `json:"created"`
	Ports      []docker.PortSummary `json:"ports"`
	Network    string               `json:"network"`
	// IPAddress is the container's address on Network. Docker only reports
	// it while the container is running.
	IPAddress string `json:"ipAddress,omitempty"`
	// RestartPolicy is Docker's restart policy; Autostart reports whether it
	// starts the server when Docker starts (on boot).
	RestartPolicy string          `json:"restartPolicy"`
	Autostart     bool            `json:"autostart"`
	Update        *UpdateStatus   `json:"update,omitempty"`
	Request       *deploy.Request `json:"request,omitempty"`
	// Access is the caller's role on the server, and Groups the server
	// groups they can see it in. The API fills these in per caller.
	Access string   `json:"access,omitempty"`
	Groups []string `json:"groups,omitempty"`
	// Hidden lists the settings left out of Request for this caller, as
	// deploy.HideSecrets does.
	Hidden []string `json:"hidden,omitempty"`
	// Stack and StackService say which Compose stack (and service in it)
	// the server is, if it's part of one.
	Stack        string `json:"stack,omitempty"`
	StackService string `json:"stackService,omitempty"`
	// Tags are the server's labels, and Favourite whether the caller pinned
	// it; the API fills these in.
	Tags      []string `json:"tags,omitempty"`
	Favourite bool     `json:"favourite,omitempty"`
	// CrashLoop is set when hlm stopped the server because it kept exiting.
	CrashLoop *CrashLoop `json:"crashLoop,omitempty"`
	// GameProfile is the game profile its image matches, if any; the API
	// fills it in.
	GameProfile *gameprofile.Ref `json:"gameProfile,omitempty"`
	// Asleep is set while hlm has it stopped because nobody played; the API
	// fills it in.
	Asleep  *Asleep `json:"asleep,omitempty"`
	imageID string
}

// Asleep says since when hlm has had a server stopped, and after how many
// minutes without players.
type Asleep struct {
	Since   time.Time `json:"since"`
	Minutes int       `json:"minutes"`
}

// startsOnBoot reports whether a restart policy starts the container when the
// Docker daemon starts.
func startsOnBoot(policy string) bool {
	return policy == "always" || policy == "unless-stopped"
}

// enrich fills in the details that only container inspect has.
func (m *Manager) enrich(srv *Server, info docker.ContainerJSON) {
	srv.RestartPolicy = info.HostConfig.RestartPolicy.Name
	if srv.RestartPolicy == "" {
		srv.RestartPolicy = "no"
	}
	srv.Autostart = startsOnBoot(srv.RestartPolicy)
	// The list endpoint shows an image ID once the tag has moved to a newer
	// image; the config keeps the reference the server was created from.
	if info.Config.Image != "" {
		srv.Image = info.Config.Image
	}
	srv.imageID = info.Image
	if info.State.Health != nil {
		srv.Health = info.State.Health.Status
	}
	if st, ok := m.updates.get(srv.ID); ok {
		srv.Update = &st
	}
	m.fillWebUI(srv, info.Config.Labels)
	m.fillCrashLoop(srv, info)
}

// fillWebUI finishes a server's Web UI link. Servers created without one
// take it from the catalog, using the ports they actually publish.
func (m *Manager) fillWebUI(srv *Server, labels map[string]string) {
	if srv.WebUI == "" && m.catalog != nil {
		ports := map[string]int{}
		for _, p := range srv.Ports {
			if p.PublicPort != 0 {
				ports[strconv.Itoa(p.PrivatePort)] = p.PublicPort
			}
		}
		srv.WebUI = deploy.RenderWebUI(m.catalog.WebUI(srv.TemplateID, srv.Image), ports)
	}
	switch {
	case labels[deploy.LabelDirectIP] == "true" && srv.IPAddress != "":
		// macvlan/ipvlan containers have their own address on the LAN.
		srv.WebUI = strings.ReplaceAll(srv.WebUI, "[IP]", srv.IPAddress)
	case m.HostAddress(srv.ID) != "":
		srv.WebUI = strings.ReplaceAll(srv.WebUI, "[IP]", m.HostAddress(srv.ID))
	case m.remoteHost != "" && HostOf(srv.ID) == LocalHost:
		srv.WebUI = strings.ReplaceAll(srv.WebUI, "[IP]", m.remoteHost)
	}
}

func serverFromSummary(s docker.ContainerSummary) Server {
	srv := Server{
		ID:         s.ID,
		Host:       HostOf(s.ID),
		Name:       strings.TrimPrefix(firstOr(s.Names, ""), "/"),
		App:        s.Labels[deploy.LabelApp],
		TemplateID: s.Labels[deploy.LabelTemplate],
		Image:      s.Image,
		Icon:       s.Labels["net.unraid.docker.icon"],
		WebUI:      s.Labels[deploy.LabelWebUI],
		State:      s.State,
		Status:     s.Status,
		Created:    time.Unix(s.Created, 0),
		Ports:      dedupePorts(s.Ports),
		Network:    s.HostConfig.NetworkMode,
	}
	if ep, ok := s.NetworkSettings.Networks[srv.Network]; ok {
		srv.IPAddress = ep.IPAddress
	}
	if r, ok := deploy.RequestFromLabels(s.Labels); ok {
		srv.Request = &r
	}
	srv.Stack, srv.StackService = s.Labels[LabelStack], s.Labels[LabelStackService]
	return srv
}

// dedupePorts drops the duplicate IPv6 entries Docker reports for each
// published port.
func dedupePorts(ports []docker.PortSummary) []docker.PortSummary {
	seen := map[string]bool{}
	out := []docker.PortSummary{}
	for _, p := range ports {
		key := strconv.Itoa(p.PrivatePort) + "/" + p.Type + ":" + strconv.Itoa(p.PublicPort)
		if seen[key] {
			continue
		}
		seen[key] = true
		p.IP = ""
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PrivatePort < out[j].PrivatePort })
	return out
}

func firstOr(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}

// List returns all managed servers, sorted by name.
func (m *Manager) List(ctx context.Context) ([]Server, error) {
	list, err := m.docker.ListContainers(ctx, deploy.LabelManaged+"=true")
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(list))
	for _, c := range list {
		srv := serverFromSummary(c)
		info, err := m.docker.InspectContainer(ctx, c.ID)
		if docker.IsNotFound(err) {
			continue // removed since the list call
		}
		if err != nil {
			return nil, err
		}
		m.enrich(&srv, info)
		out = append(out, srv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one managed server by container ID or name.
func (m *Manager) Get(ctx context.Context, id string) (Server, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return Server{}, err
	}
	// The list endpoint has the friendly status text and port summary, so
	// look the container up there.
	list, err := m.docker.ListContainersOn(ctx, HostOf(info.ID), deploy.LabelManaged+"=true")
	if err != nil {
		return Server{}, err
	}
	for _, c := range list {
		if c.ID == info.ID {
			srv := serverFromSummary(c)
			m.enrich(&srv, info)
			return srv, nil
		}
	}
	return Server{}, ErrNotFound
}

// Inspect returns the container details of a managed server.
func (m *Manager) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	return m.inspect(ctx, id)
}

// inspect looks up a container and checks that it is one of ours.
func (m *Manager) inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	info, err := m.docker.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return info, ErrNotFound
	}
	if err != nil {
		return info, err
	}
	if info.Config.Labels[deploy.LabelManaged] != "true" {
		return info, ErrNotManaged
	}
	return info, nil
}

// Event reports progress during an install or update.
type Event struct {
	// Stage is one of pull, stop, create, start, done, warning or error; an
	// update of several servers adds begin, failed (one server) and summary.
	Stage   string `json:"stage"`
	Message string `json:"message,omitempty"`
	// Target names the server an event is about, during an update of
	// several servers.
	Target   string               `json:"target,omitempty"`
	Progress *docker.PullProgress `json:"progress,omitempty"`
	Server   *Server              `json:"server,omitempty"`
}

// Install pulls the image for req and creates and starts the container.
func (m *Manager) Install(ctx context.Context, req deploy.Request, emit func(Event)) (Server, error) {
	return m.installJob(ctx, req, false, true, emit)
}

// InstallConfined creates and starts a server that can't reach the host (see
// deploy.Confine), or refuses to if the template needs more than that. It
// stays confined whenever it's recreated.
func (m *Manager) InstallConfined(ctx context.Context, req deploy.Request, emit func(Event)) (Server, error) {
	return m.installJob(ctx, req, true, true, emit)
}

// Create installs a server without starting it, for a clone whose data is
// copied in first.
func (m *Manager) Create(ctx context.Context, req deploy.Request, emit func(Event)) (Server, error) {
	return m.installJob(ctx, req, false, false, emit)
}

func (m *Manager) installJob(ctx context.Context, req deploy.Request, confined, start bool, emit func(Event)) (srv Server, err error) {
	job := Job{Kind: "install", Server: req.Name}
	if app, ok := m.AppOn(req.TemplateID, HostOf(req.Name)); ok {
		job.App, job.Icon, job.Image = app.Name, app.Icon, app.Repository
	}
	emit, finish, err := m.jobs.start(job, emit)
	if err != nil {
		return Server{}, err
	}
	defer func() { finish(err) }()
	return m.install(ctx, req, confined, start, emit)
}

// install creates a server; req.Name ends in @host for another host.
func (m *Manager) install(ctx context.Context, req deploy.Request, confined, start bool, emit func(Event)) (Server, error) {
	host, bare := SplitID(req.Name)
	if !m.KnownHost(host) {
		return Server{}, errNoHost(host)
	}
	req.Name = bare
	root := m.appdataFor(host)
	if confined {
		if app, ok := m.AppOn(req.TemplateID, host); ok {
			req = deploy.ConfinePaths(app, req, root)
		}
	}
	spec, err := m.build(ctx, host, req)
	if err != nil {
		return Server{}, err
	}
	name := JoinID(host, spec.Name)
	if confined {
		if bad := deploy.Confine(spec, root); len(bad) > 0 {
			return Server{}, errors.New("not allowed without an admin: " + strings.Join(bad, "; "))
		}
		if err := m.checkFreshData(ctx, name); err != nil {
			return Server{}, err
		}
	}
	if err := m.checkLANAddress(ctx, spec, req, nil, emit); err != nil {
		return Server{}, err
	}
	if _, err := m.docker.InspectContainer(ctx, name); err == nil {
		return Server{}, fmt.Errorf("a container named %q already exists", name)
	}
	if err := m.checkPorts(ctx, host, spec, ""); err != nil {
		return Server{}, err
	}
	for _, w := range spec.Warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	if err := m.pull(ctx, host, spec.Image, emit); err != nil {
		return Server{}, err
	}

	emit(Event{Stage: "create", Message: "Creating container " + name})
	id, warnings, err := m.docker.CreateContainer(ctx, name, spec.Create)
	if err != nil {
		return Server{}, fmt.Errorf("create container: %w", err)
	}
	for _, w := range warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	if !start {
		srv, err := m.Get(ctx, id)
		if err == nil {
			emit(Event{Stage: "created", Message: "Created " + name + " (not started yet)", Server: &srv})
		}
		return srv, err
	}
	srv, err := m.startNew(ctx, id, emit)
	if err != nil {
		// Don't leave a half-installed container behind: it would block
		// installing again under the same name.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if rerr := m.docker.RemoveContainer(rctx, id, true); rerr != nil {
			m.log.Warn("remove container after failed start", "container", id, "err", rerr)
		}
		return Server{}, err
	}
	return srv, nil
}

// AppdataRoot is where servers' data folders go on the local host.
func (m *Manager) AppdataRoot() string { return m.appdataRoot }

// AppdataRootOf is where servers' data folders go on a host.
func (m *Manager) AppdataRootOf(host string) string { return m.appdataFor(host) }

// Reconfigure recreates a server with new settings. The old container is
// kept, renamed, until the new one has been created, so a bad configuration
// leaves the server as it was.
func (m *Manager) Reconfigure(ctx context.Context, id string, req deploy.Request, emit func(Event)) (Server, error) {
	return m.recreateJob(ctx, "reconfigure", id, &req, false, emit)
}

// ReconfigureAsAdmin is Reconfigure for an admin, who may take a server out of
// confinement by giving it what a confined server can't have.
func (m *Manager) ReconfigureAsAdmin(ctx context.Context, id string, req deploy.Request, emit func(Event)) (Server, error) {
	return m.recreateJob(ctx, "reconfigure", id, &req, true, emit)
}

// Update pulls the latest image for a server and recreates it with the same
// settings.
func (m *Manager) Update(ctx context.Context, id string, emit func(Event)) (Server, error) {
	return m.recreateJob(ctx, "update", id, nil, false, emit)
}

// recreateJob runs recreate as a tracked job.
func (m *Manager) recreateJob(ctx context.Context, kind, id string, req *deploy.Request, unconfine bool, emit func(Event)) (srv Server, err error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return Server{}, err
	}
	job := Job{
		Kind:   kind,
		Server: strings.TrimPrefix(info.Name, "/"),
		App:    info.Config.Labels[deploy.LabelApp],
		Icon:   info.Config.Labels["net.unraid.docker.icon"],
		Image:  info.Config.Image,
	}
	emit, finish, err := m.jobs.start(job, emit)
	if err != nil {
		return Server{}, err
	}
	defer func() {
		finish(err)
		if kind == "update" {
			m.notifyUpdated(job, err)
		}
	}()
	return m.recreate(ctx, info.ID, req, unconfine, emit)
}

// notifyUpdated reports how a server's update went.
func (m *Manager) notifyUpdated(job Job, err error) {
	e := notify.Event{
		Kind: notify.KindServerUpdated, Title: job.Server + " was updated",
		Message: fmt.Sprintf("%s now runs the latest %s image.", job.Server, job.Image),
		Server:  job.Server, Link: "/servers/" + job.Server, Fields: map[string]string{"Image": job.Image},
	}
	if err != nil {
		e.Kind, e.Title = notify.KindServerUpdateFail, "Updating "+job.Server+" failed"
		e.Message = err.Error()
	}
	m.notify(e)
}

func (m *Manager) recreate(ctx context.Context, id string, req *deploy.Request, unconfine bool, emit func(Event)) (Server, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return Server{}, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	host, bare := SplitID(name)
	if stack := info.Config.Labels[LabelStack]; stack != "" {
		return Server{}, fmt.Errorf("%s is part of the stack %s: update or change it from the stack", name, stack)
	}
	if req == nil {
		r, ok := deploy.RequestFromLabels(info.Config.Labels)
		if !ok {
			return Server{}, errors.New("server has no saved settings to recreate it from")
		}
		req = &r
	}
	if req.TemplateID == "" {
		req.TemplateID = info.Config.Labels[deploy.LabelTemplate]
	}
	// Renaming a server is not supported; it would orphan its data paths in
	// surprising ways. Keep the container name.
	req.Name = bare
	// The live restart policy wins: it may have been changed with the
	// start-on-boot switch since the server was created.
	if p := info.HostConfig.RestartPolicy.Name; p != "" {
		req.RestartPolicy = p
	}

	spec, err := m.build(ctx, host, *req)
	if err != nil {
		return Server{}, err
	}
	// A template can change after a server is created, so this isn't only
	// about what the request asks for.
	if info.Config.Labels[deploy.LabelConfined] == "true" {
		if bad := deploy.Confine(spec, m.appdataFor(host)); len(bad) > 0 {
			if !unconfine {
				return Server{}, errors.New("this server has to stay confined, so an admin has to change this: " + strings.Join(bad, "; "))
			}
			emit(Event{Stage: "warning", Message: "This server is no longer confined: " + strings.Join(bad, "; ")})
		}
	}
	// The server itself may already hold the address; don't count it, by
	// its current MAC or the stable one it gets on LAN networks.
	ownMACs := []string{deploy.StableMAC(bare)}
	for _, ep := range info.NetworkSettings.Networks {
		ownMACs = append(ownMACs, ep.MacAddress)
	}
	if err := m.checkLANAddress(ctx, spec, *req, ownMACs, emit); err != nil {
		return Server{}, err
	}
	if err := m.checkPorts(ctx, host, spec, info.ID); err != nil {
		return Server{}, err
	}
	for _, w := range spec.Warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	if err := m.pull(ctx, host, spec.Image, emit); err != nil {
		return Server{}, err
	}

	wasRunning := info.State.Running
	if wasRunning && m.beforeStop != nil {
		emit(Event{Stage: "stop", Message: "Saving " + name})
		m.prepareStop(ctx, info)
	}
	emit(Event{Stage: "stop", Message: "Stopping " + name})
	if err := m.docker.StopContainer(ctx, info.ID, -1); err != nil {
		return Server{}, fmt.Errorf("stop old container: %w", err)
	}
	backup := fmt.Sprintf("%s-old-%d", bare, time.Now().Unix())
	if err := m.docker.RenameContainer(ctx, info.ID, backup); err != nil {
		return Server{}, fmt.Errorf("rename old container: %w", err)
	}

	restore := func(cause error) error {
		// Use a fresh context: the request context may be what failed.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.docker.RenameContainer(rctx, info.ID, name); err != nil {
			m.log.Error("restore old container name", "container", info.ID, "err", err)
		} else if wasRunning {
			_ = m.docker.StartContainer(rctx, info.ID)
		}
		return cause
	}

	emit(Event{Stage: "create", Message: "Creating container " + name})
	newID, warnings, err := m.docker.CreateContainer(ctx, name, spec.Create)
	if err != nil {
		return Server{}, restore(fmt.Errorf("create container: %w", err))
	}
	for _, w := range warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	var srv Server
	if wasRunning {
		srv, err = m.startNew(ctx, newID, emit)
	} else {
		// Leave a stopped server stopped.
		srv, err = m.Get(ctx, newID)
		if err == nil {
			emit(Event{Stage: "done", Message: "Server recreated; it was stopped, so it was not started", Server: &srv})
		}
	}
	if err != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = m.docker.RemoveContainer(rctx, newID, true)
		cancel()
		return Server{}, restore(err)
	}
	if err := m.docker.RemoveContainer(ctx, info.ID, true); err != nil {
		emit(Event{Stage: "warning", Message: "could not remove old container " + backup + ": " + err.Error()})
	} else {
		m.removeOldImage(ctx, host, info.Image, srv.imageID, emit)
	}
	// The image was just pulled, so the new container is up to date.
	current := UpdateStatus{State: UpdateCurrent, CheckedAt: time.Now()}
	m.updates.forget(info.ID)
	m.updates.set(srv.ID, current)
	srv.Update = &current
	return srv, nil
}

// build works out a server's container on a host; req.Name is its name
// there.
func (m *Manager) build(ctx context.Context, host string, req deploy.Request) (*deploy.Spec, error) {
	app, ok := m.AppOn(req.TemplateID, host)
	if !ok {
		return nil, fmt.Errorf("template %q not found in catalog", req.TemplateID)
	}
	networks, err := m.inspectNetworks(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	spec, err := deploy.Build(app, req, deploy.Env{Networks: networks})
	if err != nil {
		return nil, err
	}
	m.applyHealthcheck(spec)
	return spec, nil
}

// inspectNetworks returns a host's networks by name, with the containers
// attached to each.
func (m *Manager) inspectNetworks(ctx context.Context, host string) (map[string]docker.Network, error) {
	list, err := m.docker.ListNetworks(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make(map[string]docker.Network, len(list))
	for _, n := range list {
		full, err := m.docker.InspectNetwork(ctx, host, n.ID)
		if err != nil {
			if docker.IsNotFound(err) {
				continue // removed since the list call
			}
			return nil, err
		}
		out[full.Name] = full
	}
	return out, nil
}

// NetworkInfo describes a network a server can be attached to.
type NetworkInfo struct {
	Name    string   `json:"name"`
	Driver  string   `json:"driver"`
	Subnets []string `json:"subnets"`
	Gateway []string `json:"gateways"`
	// StaticIP reports whether servers on this network can have a fixed address.
	StaticIP bool `json:"staticIp"`
	// DirectIP reports whether containers get their own address on the LAN
	// (macvlan/ipvlan), rather than publishing ports on the host.
	DirectIP bool `json:"directIp"`
	// UsedIPs lists addresses taken by running containers.
	UsedIPs []NetworkIP `json:"usedIps"`
	// Parent is the host interface of a macvlan/ipvlan network.
	Parent string `json:"parent,omitempty"`
	// Managed is set for networks created in hlm, which it may delete.
	Managed bool `json:"managed"`
}

// NetworkIP is an address in use on a network.
type NetworkIP struct {
	IP        string `json:"ip"`
	Container string `json:"container"`
}

// Networks lists the networks servers on a host can use. Docker's
// internal-only and swarm-scoped networks are left out.
func (m *Manager) Networks(ctx context.Context, host string) ([]NetworkInfo, error) {
	networks, err := m.inspectNetworks(ctx, host)
	if err != nil {
		return nil, err
	}
	out := []NetworkInfo{}
	for _, n := range networks {
		if n.Scope == "swarm" || n.Driver == "overlay" {
			continue
		}
		info := NetworkInfo{
			Name:     n.Name,
			Driver:   n.Driver,
			Subnets:  []string{},
			Gateway:  []string{},
			DirectIP: n.Driver == "macvlan" || n.Driver == "ipvlan",
			UsedIPs:  []NetworkIP{},
			Parent:   n.Options["parent"],
			Managed:  n.Labels[deploy.LabelManaged] == "true",
		}
		for _, cfg := range n.IPAM.Config {
			if cfg.Subnet != "" {
				info.Subnets = append(info.Subnets, cfg.Subnet)
			}
			if cfg.Gateway != "" {
				info.Gateway = append(info.Gateway, cfg.Gateway)
			}
		}
		// Docker refuses static addresses on its default networks and on
		// networks created without an explicit subnet.
		builtin := n.Name == "bridge" || n.Name == "host" || n.Name == "none"
		info.StaticIP = !builtin && len(info.Subnets) > 0
		for _, ep := range n.Containers {
			for _, addr := range []string{ep.IPv4Address, ep.IPv6Address} {
				if ip, _, ok := strings.Cut(addr, "/"); ok && ip != "" {
					info.UsedIPs = append(info.UsedIPs, NetworkIP{IP: ip, Container: ep.Name})
				}
			}
		}
		sort.Slice(info.UsedIPs, func(i, j int) bool { return info.UsedIPs[i].IP < info.UsedIPs[j].IP })
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) pull(ctx context.Context, host, image string, emit func(Event)) error {
	msg := "Pulling " + image
	if host != LocalHost {
		msg += " on " + m.HostName(host)
	}
	emit(Event{Stage: "pull", Message: msg})
	err := m.docker.PullImage(ctx, host, image, func(p docker.PullProgress) {
		emit(Event{Stage: "pull", Progress: &p})
	})
	if err != nil {
		if docker.IsAuthError(err) {
			return fmt.Errorf("pull %s: %w. %s", image, err, LoginHint)
		}
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// LoginHint follows errors from registries that refused hlm for want of a
// login.
const LoginHint = "This image needs a registry login: an admin can add one under System → Registries."

func (m *Manager) startNew(ctx context.Context, id string, emit func(Event)) (Server, error) {
	emit(Event{Stage: "start", Message: "Starting container"})
	if err := m.docker.StartContainer(ctx, id); err != nil {
		return Server{}, fmt.Errorf("start container: %w", err)
	}
	srv, err := m.Get(ctx, id)
	if err != nil {
		return Server{}, err
	}
	emit(Event{Stage: "done", Server: &srv})
	return srv, nil
}

// SetAutostart sets whether a server starts when Docker starts (on boot), by
// changing its restart policy in place. Turning it on uses "always", so the
// server starts on boot even if it was stopped by hand beforehand.
func (m *Manager) SetAutostart(ctx context.Context, id string, on bool) (Server, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return Server{}, err
	}
	policy := "no"
	if on {
		policy = "always"
	}
	if err := m.docker.SetRestartPolicy(ctx, info.ID, policy); err != nil {
		return Server{}, err
	}
	return m.Get(ctx, info.ID)
}

// Start starts a server. A crash loop it was stopped for is forgotten.
func (m *Manager) Start(ctx context.Context, id string) error {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return err
	}
	m.ClearCrashLoop(strings.TrimPrefix(info.Name, "/"))
	m.prepareStart(ctx, info)
	return m.docker.StartContainer(ctx, info.ID)
}

// Stop stops a server, giving it the template's stop timeout to shut down
// cleanly.
func (m *Manager) Stop(ctx context.Context, id string) error {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return err
	}
	m.prepareStop(ctx, info)
	return m.docker.StopContainer(ctx, info.ID, -1)
}

// Restart restarts a server.
func (m *Manager) Restart(ctx context.Context, id string) error {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return err
	}
	m.ClearCrashLoop(strings.TrimPrefix(info.Name, "/"))
	m.prepareStop(ctx, info)
	m.prepareStart(ctx, info)
	return m.docker.RestartContainer(ctx, info.ID, -1)
}

// Remove deletes a server's container. Its data directories on the host are
// left alone.
func (m *Manager) Remove(ctx context.Context, id string) error {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return err
	}
	// The server's data stays on disk; remember where, for the Host page.
	if err := m.recordDeleted(info); err != nil {
		m.log.Warn("record deleted server's data", "server", info.Name, "err", err)
	}
	if err := m.docker.RemoveContainer(ctx, info.ID, true); err != nil {
		_ = m.ForgetDeleted(info.ID, "", false) // it wasn't deleted after all
		return err
	}
	m.ClearCrashLoop(strings.TrimPrefix(info.Name, "/"))
	return nil
}

// Logs streams a server's logs.
func (m *Manager) Logs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.docker.Logs(ctx, info.ID, opts)
}

// Stats returns a resource usage sample for a running server.
func (m *Manager) Stats(ctx context.Context, id string) (docker.Stats, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return docker.Stats{}, err
	}
	if !info.State.Running {
		return docker.Stats{}, nil
	}
	return m.docker.ContainerStats(ctx, info.ID)
}

// AllStats returns a resource usage sample for every running server and
// connection container, keyed by container ID. Containers whose stats can't
// be read are left out.
func (m *Manager) AllStats(ctx context.Context) (map[string]docker.Stats, error) {
	servers, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	conns, err := m.Connections(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		servers = append(servers, Server{ID: c.ID, State: c.State})
	}
	return m.StatsOf(ctx, servers), nil
}

// StatsOf returns a resource usage sample for each of the given servers that
// is running, keyed by container ID. Containers whose stats can't be read are
// left out.
func (m *Manager) StatsOf(ctx context.Context, servers []Server) map[string]docker.Stats {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string]docker.Stats{}
		// Each stats call takes about a second (the daemon waits for a second
		// CPU sample), so run them in parallel, a few at a time.
		sem = make(chan struct{}, 8)
	)
	for _, srv := range servers {
		if srv.State != "running" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			st, err := m.docker.ContainerStats(sctx, id)
			if err != nil {
				m.log.Debug("container stats", "container", id, "err", err)
				return
			}
			mu.Lock()
			out[id] = st
			mu.Unlock()
		}(srv.ID)
	}
	wg.Wait()
	return out
}

// Config returns the server's template (localized) and the request it was
// created with, so a client can show an edit form.
func (m *Manager) Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return nil, deploy.Request{}, err
	}
	req, _ := deploy.RequestFromLabels(info.Config.Labels)
	tid := info.Config.Labels[deploy.LabelTemplate]
	app, ok := m.AppOn(tid, HostOf(info.ID))
	if !ok {
		return nil, req, fmt.Errorf("template %q is no longer in the catalog", tid)
	}
	return app, req, nil
}

// ImportImage pulls an image and makes a catalog template from the defaults
// built into it (see catalog.FromImage). Importing the same reference again
// refreshes its template.
func (m *Manager) ImportImage(ctx context.Context, ref string, emit func(Event)) (*catalog.App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.ContainsAny(ref, " \t\n") {
		return nil, fmt.Errorf("invalid image reference %q", ref)
	}
	if err := m.pull(ctx, LocalHost, ref, emit); err != nil {
		return nil, err
	}
	img, err := m.docker.InspectImage(ctx, LocalHost, ref)
	if err != nil {
		return nil, fmt.Errorf("read image %s: %w", ref, err)
	}
	info := catalog.ImageInfo{Ref: ref, Env: img.Config.Env, Labels: img.Config.Labels, User: img.Config.User}
	for p := range img.Config.ExposedPorts {
		info.ExposedPorts = append(info.ExposedPorts, p)
	}
	for v := range img.Config.Volumes {
		info.Volumes = append(info.Volumes, v)
	}
	app := catalog.FromImage(info)
	emit(Event{Stage: "create", Message: fmt.Sprintf("Found %d settings in %s", len(app.Config), ref)})
	if err := m.catalog.AddCustom(app); err != nil {
		return nil, err
	}
	m.log.Info("made template from image", "image", ref, "id", app.ID, "settings", len(app.Config))
	return app, nil
}
