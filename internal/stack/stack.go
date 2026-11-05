// Package stack runs Docker Compose stacks: each service is a container hlm
// manages like a server, labelled with its stack, on the stack's own
// networks. The Compose file and its .env are kept in the secrets store
// (they often hold passwords), so they're in configuration backups too.
package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/compose"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "stacks"

// Definition is a stack's Compose file and .env.
type Definition struct {
	YAML    string    `json:"yaml"`
	Env     string    `json:"env,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Event is a step of applying a stack, streamed to the client.
type Event = manager.Event

// Manager is what the service needs from the server manager.
type Manager interface {
	PortConflictsOf(ctx context.Context, c docker.CreateRequest, self string) ([]manager.PortConflict, error)
	Remove(ctx context.Context, id string) error
	AppdataRoot() string
}

// Docker is what the service needs from Docker.
type Docker interface {
	ListContainers(ctx context.Context, labels ...string) ([]docker.ContainerSummary, error)
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	ConnectNetwork(ctx context.Context, network, container string, endpoint any) error
	ListNetworks(ctx context.Context) ([]docker.Network, error)
	CreateNetwork(ctx context.Context, req docker.CreateNetworkRequest) (string, error)
	RemoveNetwork(ctx context.Context, id string) error
	PullImage(ctx context.Context, ref string, progress func(docker.PullProgress)) error
	InspectImage(ctx context.Context, ref string) (docker.Image, error)
}

// Service manages stacks.
type Service struct {
	secrets *secrets.Store
	docker  Docker
	mgr     Manager
	log     *slog.Logger

	saveMu sync.Mutex
	mu     sync.Mutex
	busy   map[string]bool // stacks being changed
	// healthWait is how long a service_healthy dependency is waited for.
	healthWait time.Duration
}

// New creates the service.
func New(store *secrets.Store, d Docker, m Manager, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{secrets: store, docker: d, mgr: m, log: log, busy: map[string]bool{}, healthWait: 5 * time.Minute}
}

func (s *Service) all() (map[string]Definition, error) {
	m := map[string]Definition{}
	_, err := s.secrets.Get(secretName, &m)
	return m, err
}

// Definition returns a stack's Compose file and .env.
func (s *Service) Definition(name string) (Definition, error) {
	all, err := s.all()
	if err != nil {
		return Definition{}, err
	}
	d, ok := all[name]
	if !ok {
		return Definition{}, ErrNotFound
	}
	return d, nil
}

func (s *Service) save(name string, d *Definition) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all, err := s.all()
	if err != nil {
		return err
	}
	if d == nil {
		delete(all, name)
	} else {
		all[name] = *d
	}
	return s.secrets.Set(secretName, all)
}

// ErrNotFound is returned for a stack that doesn't exist.
var ErrNotFound = errors.New("no such stack")

// ErrBusy is returned while a stack is being changed.
var ErrBusy = errors.New("this stack is being changed; try again when that's done")

func (s *Service) claim(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[name] {
		return ErrBusy
	}
	s.busy[name] = true
	return nil
}

func (s *Service) release(name string) {
	s.mu.Lock()
	delete(s.busy, name)
	s.mu.Unlock()
}

// --- What's running ---

// Member is a stack's service as it runs.
type Member struct {
	Service   string `json:"service"`
	Container string `json:"container"`
	ID        string `json:"id"`
	State     string `json:"state"`
	Status    string `json:"status"`
	Health    string `json:"health,omitempty"`
	Image     string `json:"image"`
	hash      string
}

// members lists a stack's containers, by service.
func (s *Service) members(ctx context.Context, name string) (map[string]Member, error) {
	list, err := s.docker.ListContainers(ctx, compose.LabelStack+"="+name)
	if err != nil {
		return nil, err
	}
	out := map[string]Member{}
	for _, c := range list {
		m := Member{Service: c.Labels[compose.LabelService], ID: c.ID, State: c.State, Status: c.Status, Image: c.Image,
			hash: c.Labels[compose.LabelHash]}
		if len(c.Names) > 0 {
			m.Container = strings.TrimPrefix(c.Names[0], "/")
		}
		if strings.Contains(c.Status, "(healthy)") {
			m.Health = "healthy"
		} else if strings.Contains(c.Status, "(unhealthy)") {
			m.Health = "unhealthy"
		} else if strings.Contains(c.Status, "health: starting") {
			m.Health = "starting"
		}
		out[m.Service] = m
	}
	return out, nil
}

// Stack is a stack and its services' state.
type Stack struct {
	Name     string    `json:"name"`
	Services []Member  `json:"services"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Busy     bool      `json:"busy,omitempty"`
	Error    string    `json:"error,omitempty"` // the file can't be read any more
}

// List returns the stacks, by name.
func (s *Service) List(ctx context.Context) ([]Stack, error) {
	all, err := s.all()
	if err != nil {
		return nil, err
	}
	var out []Stack
	for _, name := range sortedNames(all) {
		st, err := s.get(ctx, name, all[name])
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Get returns one stack.
func (s *Service) Get(ctx context.Context, name string) (Stack, error) {
	d, err := s.Definition(name)
	if err != nil {
		return Stack{}, err
	}
	return s.get(ctx, name, d)
}

func (s *Service) get(ctx context.Context, name string, d Definition) (Stack, error) {
	st := Stack{Name: name, Created: d.Created, Updated: d.Updated, Services: []Member{}}
	s.mu.Lock()
	st.Busy = s.busy[name]
	s.mu.Unlock()
	running, err := s.members(ctx, name)
	if err != nil {
		return st, err
	}
	p, perr := compose.Parse([]byte(d.YAML), d.Env, name)
	if perr != nil {
		st.Error = perr.Error()
	}
	seen := map[string]bool{}
	if p != nil {
		for _, svc := range p.Services {
			m, ok := running[svc.Name]
			if !ok {
				m = Member{Service: svc.Name, Container: p.ContainerName(svc), State: "missing", Image: svc.Image}
			}
			st.Services = append(st.Services, m)
			seen[svc.Name] = true
		}
	}
	for _, svc := range sortedNames(running) {
		if !seen[svc] {
			st.Services = append(st.Services, running[svc])
		}
	}
	return st, nil
}

func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// --- Previewing and applying a Compose file ---

// Plan is what applying a Compose file would do.
type Plan struct {
	Services []ServicePlan `json:"services"`
	Networks []string      `json:"networks"` // Docker names, created if missing
	Warnings []string      `json:"warnings"`
	project  *compose.Project
	specs    map[string]compose.ContainerSpec
}

// Changes to a service.
const (
	ChangeCreate    = "create"
	ChangeUpdate    = "update"
	ChangeUnchanged = "unchanged"
	ChangeRemove    = "remove"
)

// ServicePlan is a service, and what happens to it.
type ServicePlan struct {
	Service   string   `json:"service"`
	Container string   `json:"container"`
	Image     string   `json:"image"`
	Ports     []string `json:"ports"`
	Mounts    []string `json:"mounts"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Change    string   `json:"change"`
}

// Preview reads a Compose file for a stack (new, or replacing the running
// one) and says what applying it would do.
func (s *Service) Preview(ctx context.Context, name, yaml, env string, existing bool) (Plan, error) {
	p, err := compose.Parse([]byte(yaml), env, name)
	if err != nil {
		return Plan{}, err
	}
	if _, err := s.Definition(name); err == nil && !existing {
		return Plan{}, fmt.Errorf("a stack called %s exists already", name)
	} else if errors.Is(err, ErrNotFound) && existing {
		return Plan{}, ErrNotFound
	}
	running, err := s.members(ctx, name)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{project: p, specs: map[string]compose.ContainerSpec{}, Warnings: slices.Clone(p.Warnings)}
	if plan.Warnings == nil {
		plan.Warnings = []string{}
	}
	appdata := s.mgr.AppdataRoot()
	for _, svc := range p.Services {
		spec, err := p.Spec(svc, appdata)
		if err != nil {
			return Plan{}, err
		}
		plan.specs[svc.Name] = spec
		sp := ServicePlan{Service: svc.Name, Container: spec.Name, Image: svc.Image, Ports: []string{}, Mounts: []string{}, Change: ChangeCreate}
		for _, pt := range svc.Ports {
			hp := pt.Published
			if hp == "" {
				hp = "(not published)"
			} else if pt.HostIP != "" {
				hp = pt.HostIP + ":" + hp
			}
			sp.Ports = append(sp.Ports, fmt.Sprintf("%s → %s/%s", hp, pt.Target, pt.Protocol))
		}
		sp.Mounts = append(sp.Mounts, spec.Create.HostConfig.Binds...)
		for d := range spec.Create.Volumes {
			sp.Mounts = append(sp.Mounts, d+" (anonymous volume)")
		}
		for _, d := range svc.DependsOn {
			sp.DependsOn = append(sp.DependsOn, d.Service)
		}
		if m, ok := running[svc.Name]; ok {
			sp.Change = ChangeUpdate
			if m.hash == spec.Hash {
				sp.Change = ChangeUnchanged
			}
		} else if info, err := s.docker.InspectContainer(ctx, spec.Name); err == nil {
			return Plan{}, fmt.Errorf("service %s: a container called %s already exists and isn't part of this stack", svc.Name, strings.TrimPrefix(info.Name, "/"))
		}
		// Ports other containers (or programs) use.
		if sp.Change != ChangeUnchanged {
			if conflicts, err := s.mgr.PortConflictsOf(ctx, spec.Create, spec.Name); err == nil {
				for _, c := range conflicts {
					if c.UsedBy != "" && running[serviceOf(c.UsedBy, running)].Container == c.UsedBy {
						continue // another of this stack's services, being replaced
					}
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("service %s: %s", svc.Name, c.Message))
				}
			}
		}
		plan.Services = append(plan.Services, sp)
	}
	for _, svc := range sortedNames(running) {
		if p.Service(svc) == nil {
			m := running[svc]
			plan.Services = append(plan.Services, ServicePlan{Service: svc, Container: m.Container, Image: m.Image, Change: ChangeRemove,
				Ports: []string{}, Mounts: []string{}})
		}
	}
	for _, key := range sortedNames(p.Networks) {
		n := p.Networks[key]
		if !n.External {
			plan.Networks = append(plan.Networks, p.NetworkName(key))
		}
	}
	return plan, nil
}

func serviceOf(container string, running map[string]Member) string {
	for svc, m := range running {
		if m.Container == container {
			return svc
		}
	}
	return ""
}

// Apply creates a stack, or changes one to a new Compose file: networks
// first, then each service in dependency order (only those that changed are
// recreated), then services no longer in the file are removed (their data
// is kept).
func (s *Service) Apply(ctx context.Context, name, yaml, env string, existing bool, emit func(Event)) error {
	if err := s.claim(name); err != nil {
		return err
	}
	defer s.release(name)
	plan, err := s.Preview(ctx, name, yaml, env, existing)
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	now := time.Now().UTC()
	def := Definition{YAML: yaml, Env: env, Created: now, Updated: now}
	if old, err := s.Definition(name); err == nil {
		def.Created = old.Created
	}
	// Saved first, so a stack that fails half way can be fixed and applied
	// again.
	if err := s.save(name, &def); err != nil {
		return err
	}
	p := plan.project
	if err := s.ensureNetworks(ctx, p, emit); err != nil {
		return err
	}
	running, err := s.members(ctx, name)
	if err != nil {
		return err
	}
	for _, sp := range plan.Services {
		if sp.Change == ChangeRemove {
			continue
		}
		svc := p.Service(sp.Service)
		if err := s.waitFor(ctx, p, svc, emit); err != nil {
			return err
		}
		spec := plan.specs[sp.Service]
		switch sp.Change {
		case ChangeUnchanged:
			if m := running[sp.Service]; m.State != "running" {
				emit(Event{Stage: "start", Message: "Starting " + spec.Name})
				if err := s.docker.StartContainer(ctx, m.ID); err != nil {
					return fmt.Errorf("start %s: %w", spec.Name, err)
				}
			}
		case ChangeCreate:
			if err := s.create(ctx, spec, false, emit); err != nil {
				return err
			}
		case ChangeUpdate:
			if err := s.replace(ctx, running[sp.Service], spec, emit); err != nil {
				return err
			}
		}
	}
	for _, sp := range plan.Services {
		if sp.Change == ChangeRemove {
			emit(Event{Stage: "stop", Message: fmt.Sprintf("Removing %s (no longer in the file; its data is kept)", sp.Container)})
			if err := s.mgr.Remove(ctx, running[sp.Service].ID); err != nil {
				return fmt.Errorf("remove %s: %w", sp.Container, err)
			}
		}
	}
	emit(Event{Stage: "done", Message: "The stack " + name + " is up"})
	return nil
}

// ensureNetworks creates the stack's networks that don't exist, and checks
// external ones do.
func (s *Service) ensureNetworks(ctx context.Context, p *compose.Project, emit func(Event)) error {
	nets, err := s.docker.ListNetworks(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, n := range nets {
		have[n.Name] = true
	}
	for _, key := range sortedNames(p.Networks) {
		n, name := p.Networks[key], p.NetworkName(key)
		if have[name] {
			continue
		}
		if n.External {
			return fmt.Errorf("the external network %s doesn't exist", name)
		}
		emit(Event{Stage: "create", Message: "Creating network " + name})
		if _, err := s.docker.CreateNetwork(ctx, docker.CreateNetworkRequest{Name: name, Driver: n.Driver, Internal: n.Internal,
			Labels: map[string]string{compose.LabelStack: p.Name}}); err != nil {
			return fmt.Errorf("create network %s: %w", name, err)
		}
	}
	return nil
}

// waitFor waits for a service's dependencies to be ready, as depends_on
// says.
func (s *Service) waitFor(ctx context.Context, p *compose.Project, svc *compose.Service, emit func(Event)) error {
	for _, d := range svc.DependsOn {
		if d.Condition == "service_started" {
			continue
		}
		name := p.ContainerName(p.Service(d.Service))
		what := map[string]string{"service_healthy": "healthy", "service_completed_successfully": "finished"}[d.Condition]
		emit(Event{Stage: "start", Message: fmt.Sprintf("Waiting for %s to be %s", name, what)})
		deadline := time.Now().Add(s.healthWait)
		for {
			info, err := s.docker.InspectContainer(ctx, name)
			if err != nil {
				return err
			}
			switch d.Condition {
			case "service_healthy":
				if info.State.Health == nil {
					return fmt.Errorf("%s has no health check, so it can't be waited for as healthy", name)
				}
				if info.State.Health.Status == "healthy" {
					goto next
				}
				if !info.State.Running {
					return fmt.Errorf("%s stopped before it was healthy (exit code %d)", name, info.State.ExitCode)
				}
			case "service_completed_successfully":
				if !info.State.Running && !info.State.Restarting && info.State.Status != "created" {
					if info.State.ExitCode != 0 {
						return fmt.Errorf("%s failed (exit code %d)", name, info.State.ExitCode)
					}
					goto next
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s wasn't %s after %s", name, what, s.healthWait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	next:
	}
	return nil
}

func (s *Service) pull(ctx context.Context, image string, emit func(Event)) error {
	emit(Event{Stage: "pull", Message: "Pulling " + image})
	if err := s.docker.PullImage(ctx, image, func(p docker.PullProgress) {
		emit(Event{Stage: "pull", Progress: &p})
	}); err != nil {
		// A local image (built on this host) can still be used.
		if _, ierr := s.docker.InspectImage(ctx, image); ierr == nil {
			emit(Event{Stage: "warning", Message: fmt.Sprintf("Couldn't pull %s (%v); using the image already here", image, err)})
			return nil
		}
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// create creates and starts a service's container, pulling its image
// first unless pulled says it was just pulled.
func (s *Service) create(ctx context.Context, spec compose.ContainerSpec, pulled bool, emit func(Event)) error {
	if !pulled {
		if err := s.pull(ctx, spec.Create.Image, emit); err != nil {
			return err
		}
	}
	emit(Event{Stage: "create", Message: "Creating " + spec.Name})
	id, warnings, err := s.docker.CreateContainer(ctx, spec.Name, spec.Create)
	if err != nil {
		return fmt.Errorf("create %s: %w", spec.Name, err)
	}
	for _, w := range warnings {
		emit(Event{Stage: "warning", Message: w})
	}
	for _, a := range spec.Connect {
		if err := s.docker.ConnectNetwork(ctx, a.Network, id, map[string]any{"Aliases": a.Aliases}); err != nil {
			s.docker.RemoveContainer(context.WithoutCancel(ctx), id, true)
			return fmt.Errorf("connect %s to %s: %w", spec.Name, a.Network, err)
		}
	}
	emit(Event{Stage: "start", Message: "Starting " + spec.Name})
	if err := s.docker.StartContainer(ctx, id); err != nil {
		s.docker.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return fmt.Errorf("start %s: %w", spec.Name, err)
	}
	return nil
}

// replace recreates a service's container with new settings. The old one is
// kept, renamed, until the new one runs, and put back if it doesn't.
func (s *Service) replace(ctx context.Context, old Member, spec compose.ContainerSpec, emit func(Event)) error {
	if err := s.pull(ctx, spec.Create.Image, emit); err != nil {
		return err
	}
	wasRunning := old.State == "running"
	emit(Event{Stage: "stop", Message: "Stopping " + old.Container})
	if err := s.docker.StopContainer(ctx, old.ID, -1); err != nil {
		return fmt.Errorf("stop %s: %w", old.Container, err)
	}
	backup := fmt.Sprintf("%s-old-%d", old.Container, time.Now().Unix())
	if err := s.docker.RenameContainer(ctx, old.ID, backup); err != nil {
		return fmt.Errorf("rename %s: %w", old.Container, err)
	}
	restore := func(cause error) error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := s.docker.RenameContainer(rctx, old.ID, old.Container); err != nil {
			s.log.Error("put back the old container", "container", old.Container, "err", err)
		} else if wasRunning {
			s.docker.StartContainer(rctx, old.ID)
		}
		emit(Event{Stage: "warning", Message: "Put the old " + old.Container + " back"})
		return cause
	}
	if err := s.create(ctx, spec, true, emit); err != nil {
		return restore(err)
	}
	if err := s.docker.RemoveContainer(ctx, old.ID, true); err != nil {
		emit(Event{Stage: "warning", Message: "couldn't remove the old container " + backup + ": " + err.Error()})
	}
	return nil
}

// --- Actions on a whole stack ---

// Act starts, stops or restarts a stack's services: in dependency order
// (stopping in reverse).
func (s *Service) Act(ctx context.Context, name, action string, emit func(Event)) error {
	d, err := s.Definition(name)
	if err != nil {
		return err
	}
	if err := s.claim(name); err != nil {
		return err
	}
	defer s.release(name)
	p, err := compose.Parse([]byte(d.YAML), d.Env, name)
	if err != nil {
		return err
	}
	running, err := s.members(ctx, name)
	if err != nil {
		return err
	}
	order := p.Services
	stop := func() error {
		for i := len(order) - 1; i >= 0; i-- {
			m, ok := running[order[i].Name]
			if !ok || m.State != "running" {
				continue
			}
			emit(Event{Stage: "stop", Message: "Stopping " + m.Container})
			if err := s.docker.StopContainer(ctx, m.ID, -1); err != nil {
				return fmt.Errorf("stop %s: %w", m.Container, err)
			}
		}
		return nil
	}
	start := func() error {
		for _, svc := range order {
			m, ok := running[svc.Name]
			if !ok {
				emit(Event{Stage: "warning", Message: p.ContainerName(svc) + " doesn't exist: apply the stack again to create it"})
				continue
			}
			if err := s.waitFor(ctx, p, svc, emit); err != nil {
				return err
			}
			emit(Event{Stage: "start", Message: "Starting " + m.Container})
			if err := s.docker.StartContainer(ctx, m.ID); err != nil {
				return fmt.Errorf("start %s: %w", m.Container, err)
			}
		}
		return nil
	}
	switch action {
	case "start":
		err = start()
	case "stop":
		err = stop()
	case "restart":
		if err = stop(); err == nil {
			running, err = s.members(ctx, name)
			if err == nil {
				err = start()
			}
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err == nil {
		emit(Event{Stage: "done", Message: fmt.Sprintf("The stack %s is %s", name, map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}[action])})
	}
	return err
}

// Update pulls each service's image, and recreates those whose image
// changed.
func (s *Service) Update(ctx context.Context, name string, emit func(Event)) error {
	d, err := s.Definition(name)
	if err != nil {
		return err
	}
	if err := s.claim(name); err != nil {
		return err
	}
	defer s.release(name)
	plan, err := s.Preview(ctx, name, d.YAML, d.Env, true)
	if err != nil {
		return err
	}
	running, err := s.members(ctx, name)
	if err != nil {
		return err
	}
	updated := 0
	for _, sp := range plan.Services {
		if sp.Change == ChangeRemove {
			continue
		}
		m, ok := running[sp.Service]
		if !ok {
			continue
		}
		if err := s.pull(ctx, sp.Image, emit); err != nil {
			return err
		}
		img, err := s.docker.InspectImage(ctx, sp.Image)
		if err != nil {
			return err
		}
		info, err := s.docker.InspectContainer(ctx, m.ID)
		if err != nil {
			return err
		}
		if info.Image == img.ID {
			emit(Event{Stage: "pull", Message: m.Container + " is up to date"})
			continue
		}
		if err := s.waitFor(ctx, plan.project, plan.project.Service(sp.Service), emit); err != nil {
			return err
		}
		if err := s.replace(ctx, m, plan.specs[sp.Service], emit); err != nil {
			return err
		}
		updated++
	}
	emit(Event{Stage: "done", Message: fmt.Sprintf("Updated %d of the stack's services", updated)})
	return nil
}

// Delete removes a stack's containers (their data is kept, and listed on
// the Host page) and its own networks, and forgets it.
func (s *Service) Delete(ctx context.Context, name string) error {
	d, err := s.Definition(name)
	if err != nil {
		return err
	}
	if err := s.claim(name); err != nil {
		return err
	}
	defer s.release(name)
	running, err := s.members(ctx, name)
	if err != nil {
		return err
	}
	// Dependents first.
	order := sortedNames(running)
	if p, err := compose.Parse([]byte(d.YAML), d.Env, name); err == nil {
		order = nil
		for i := len(p.Services) - 1; i >= 0; i-- {
			if _, ok := running[p.Services[i].Name]; ok {
				order = append(order, p.Services[i].Name)
			}
		}
		for _, svc := range sortedNames(running) {
			if p.Service(svc) == nil {
				order = append(order, svc)
			}
		}
	}
	for _, svc := range order {
		if err := s.mgr.Remove(ctx, running[svc].ID); err != nil {
			return fmt.Errorf("remove %s: %w", running[svc].Container, err)
		}
	}
	if nets, err := s.docker.ListNetworks(ctx); err == nil {
		for _, n := range nets {
			if n.Labels[compose.LabelStack] == name {
				if err := s.docker.RemoveNetwork(ctx, n.ID); err != nil {
					s.log.Warn("remove stack network", "network", n.Name, "err", err)
				}
			}
		}
	}
	return s.save(name, nil)
}
