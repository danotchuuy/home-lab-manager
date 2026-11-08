package manager

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Adopting containers hlm didn't make. Docker can't add labels to a
// container, so hlm makes a template and settings that describe it and
// recreates it with them, the way an update does: the old container is
// kept, renamed, until the new one runs. For a container Unraid made, the
// settings come from Unraid's own copy of its template (and the Community
// Applications template it was made from, when the catalog has it);
// otherwise from the container's own configuration. Either way, what the
// new container would do differently is worked out first, for the user to
// see before anything changes.

// composeProjectLabel marks containers Docker Compose made.
const composeProjectLabel = "com.docker.compose.project"

// Unmanaged is a container on a Docker host that hlm doesn't manage.
type Unmanaged struct {
	// ID is the container's name, with @host for another host: the name
	// it'll have as a server.
	ID      string               `json:"id"`
	Host    string               `json:"host"`
	Name    string               `json:"name"`
	Image   string               `json:"image"`
	Icon    string               `json:"icon,omitempty"`
	State   string               `json:"state"`
	Status  string               `json:"status"`
	Created time.Time            `json:"created"`
	Ports   []docker.PortSummary `json:"ports"`
	Network string               `json:"network,omitempty"`
	// Mounts are its folders and volumes, "source → destination".
	Mounts []string `json:"mounts,omitempty"`
	// Unraid is set for a container Unraid made; Compose names the Compose
	// project of one Docker Compose made.
	Unraid  bool   `json:"unraid,omitempty"`
	Compose string `json:"compose,omitempty"`
	// Adoptable says whether hlm can adopt it; Reason says why not.
	Adoptable bool   `json:"adoptable"`
	Reason    string `json:"reason,omitempty"`
}

// Unmanaged lists the containers on a host that hlm doesn't manage,
// leaving out hlm's own (itself, its agents, helpers and connections).
func (m *Manager) Unmanaged(ctx context.Context, host string) ([]Unmanaged, error) {
	if !m.KnownHost(host) {
		return nil, errNoHost(host)
	}
	list, err := m.docker.ListContainersOn(ctx, host)
	if err != nil {
		return nil, err
	}
	// Containers on another's network name it by ID; say its name.
	names := map[string]string{}
	for _, c := range list {
		_, id := SplitID(c.ID)
		_, n := SplitID(strings.TrimPrefix(firstOr(c.Names, ""), "/"))
		names[id] = n
	}
	out := []Unmanaged{}
	for _, c := range list {
		if m.hlmOwn(c.ID, c.Image, c.Labels) {
			continue
		}
		u := Unmanaged{
			ID: strings.TrimPrefix(firstOr(c.Names, c.ID), "/"), Host: host, Image: c.Image, State: c.State, Status: c.Status,
			Created: time.Unix(c.Created, 0), Ports: dedupePorts(c.Ports), Icon: c.Labels["net.unraid.docker.icon"], Network: c.HostConfig.NetworkMode,
			Unraid: c.Labels[catalog.UnraidManagedLabel] != "", Compose: c.Labels[composeProjectLabel],
		}
		_, u.Name = SplitID(u.ID)
		if other, ok := strings.CutPrefix(u.Network, "container:"); ok && names[other] != "" {
			u.Network = "container:" + names[other]
		}
		for _, mt := range c.Mounts {
			src := mt.Source
			if mt.Type == "volume" {
				src = "volume " + shortVolume(mt.Name)
			}
			u.Mounts = append(u.Mounts, src+" → "+mt.Destination)
		}
		u.Reason = adoptBlocker(u)
		u.Adoptable = u.Reason == ""
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hlmOwn says whether a container is hlm's own: a server, hlm itself, its
// agent, a helper or a connection.
func (m *Manager) hlmOwn(id, image string, labels map[string]string) bool {
	if labels[deploy.LabelManaged] == "true" {
		return true
	}
	for k := range labels {
		if strings.HasPrefix(k, "io.hlm.") || strings.HasPrefix(k, "io.gsh.") {
			return true
		}
	}
	if m.selfID != "" && id == m.selfID {
		return true
	}
	// hlm and its agent run from hlm's image.
	name := strings.ToLower(image)
	return strings.Contains(name, "home-lab-manager") || strings.Contains(name, "game-server-hoster")
}

func adoptBlocker(u Unmanaged) string {
	switch {
	case u.Compose != "":
		return "Docker Compose made it (project " + u.Compose + "): adopting one of its containers would split it from the rest. Bring the project into hlm as a Compose stack instead."
	case strings.HasPrefix(u.Network, "container:"):
		return "It uses another container's network (" + u.Network + "), which hlm's servers can't."
	case u.State == "paused":
		return "It's paused: unpause it first."
	case u.State == "dead" || u.State == "removing":
		return "Docker can't recreate it while it's " + u.State + "."
	case !validServerName.MatchString(u.Name):
		return "hlm can't use its name, " + u.Name + "."
	}
	return ""
}

var validServerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func shortVolume(name string) string {
	if len(name) == 64 && isHex(name) {
		return name[:12] + " (anonymous)"
	}
	return name
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// Where an adopted server's settings come from.
const (
	// AdoptCommunityApps: Unraid's copy of the Community Applications
	// template, matched to that template in the catalog.
	AdoptCommunityApps = "community-apps"
	// AdoptUnraid: Unraid's copy of the template, the catalog not having
	// the original.
	AdoptUnraid = "unraid"
	// AdoptContainer: the container's own configuration.
	AdoptContainer = "container"
)

// AdoptPlan is how hlm would adopt a container.
type AdoptPlan struct {
	Container Unmanaged `json:"container"`
	// Source is where the settings come from (AdoptCommunityApps,
	// AdoptUnraid or AdoptContainer); Template names the template, and
	// TemplateID is the catalog's for AdoptCommunityApps.
	Source     string         `json:"source"`
	Template   string         `json:"template"`
	TemplateID string         `json:"templateId,omitempty"`
	Request    deploy.Request `json:"request"`
	// Changes are what the new container would do differently.
	Changes []string `json:"changes"`
	// Notes say why a better source of settings wasn't used, and the
	// like.
	Notes []string `json:"notes,omitempty"`
	// Restart is set when the container is running: adopting restarts it.
	Restart bool `json:"restart"`

	app  *catalog.App // the template, when it's to be saved as a custom one
	info docker.ContainerJSON
}

// AdoptPlan works out how hlm would adopt a container (by name, with @host
// for another host), without changing anything.
func (m *Manager) AdoptPlan(ctx context.Context, id string) (AdoptPlan, error) {
	host, bare := SplitID(id)
	if !m.KnownHost(host) {
		return AdoptPlan{}, errNoHost(host)
	}
	info, err := m.docker.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return AdoptPlan{}, ErrNotFound
	}
	if err != nil {
		return AdoptPlan{}, err
	}
	labels := info.Config.Labels
	if m.hlmOwn(info.ID, info.Config.Image, labels) {
		return AdoptPlan{}, fmt.Errorf("%s is already hlm's", id)
	}
	_, bare = SplitID(strings.TrimPrefix(info.Name, "/"))
	plan := AdoptPlan{info: info, Restart: info.State.Running}
	plan.Container = Unmanaged{
		ID: JoinID(host, bare), Host: host, Name: bare, Image: info.Config.Image, State: info.State.Status, Network: info.HostConfig.NetworkMode,
		Icon: labels["net.unraid.docker.icon"], Unraid: labels[catalog.UnraidManagedLabel] != "", Compose: labels[composeProjectLabel],
	}
	if plan.Container.Reason = adoptBlocker(plan.Container); plan.Container.Reason != "" {
		return plan, errors.New(plan.Container.Reason)
	}
	plan.Container.Adoptable = true

	c, err := m.docker.client(host)
	if err != nil {
		return AdoptPlan{}, err
	}
	raw, err := c.InspectContainerRaw(ctx, bare)
	if err != nil {
		return AdoptPlan{}, err
	}
	cur, extras, err := currentContainer(raw)
	if err != nil {
		return AdoptPlan{}, err
	}
	img, err := m.docker.InspectImage(ctx, host, info.Image)
	if err != nil {
		return AdoptPlan{}, fmt.Errorf("read its image: %w", err)
	}
	networks, err := m.inspectNetworks(ctx, host)
	if err != nil {
		return AdoptPlan{}, fmt.Errorf("list networks: %w", err)
	}
	// A container with its own address on the LAN keeps it.
	keepIP := ""
	if n, ok := networks[cur.HostConfig.NetworkMode]; ok {
		if ep, ok := info.NetworkSettings.Networks[n.Name]; ok {
			if ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address != "" {
				keepIP = ep.IPAMConfig.IPv4Address
			} else if n.Driver == "macvlan" || n.Driver == "ipvlan" {
				keepIP = ep.IPAddress
			}
		}
	}

	type candidate struct {
		source string
		app    *catalog.App
		req    deploy.Request
		save   bool
	}
	var tries []candidate
	if plan.Container.Unraid {
		ut, err := m.unraidTemplate(ctx, host, bare)
		switch {
		case err != nil:
			plan.Notes = append(plan.Notes, "Unraid's template for it couldn't be read ("+err.Error()+"), so hlm used the container's own settings.")
		default:
			if app, tag, ok := m.catalog.FindTemplate(ut.App.TemplateURL, ut.App.Name, info.Config.Image); ok {
				tries = append(tries, candidate{AdoptCommunityApps, app, requestFromUnraid(app, ut, tag, bare), false})
			} else {
				plan.Notes = append(plan.Notes, "The catalog doesn't have the Community Applications template it came from, so hlm keeps Unraid's copy as a custom template.")
			}
			app := *ut.App
			app.ID = adoptedID("unraid", host, bare)
			app.Categories = []string{catalog.CustomCategory}
			tries = append(tries, candidate{AdoptUnraid, &app, requestFromUnraid(&app, ut, "", bare), true})
		}
	}
	app, req := fromContainer(host, bare, info, cur, img)
	tries = append(tries, candidate{AdoptContainer, app, req, true})

	for i, t := range tries {
		if keepIP != "" && t.req.IPAddress == "" {
			t.req.IPAddress = keepIP
		}
		if t.req.Network == "" {
			t.req.Network = cur.HostConfig.NetworkMode
		}
		if p := info.HostConfig.RestartPolicy.Name; p != "" {
			t.req.RestartPolicy = p
		}
		local := t.app.Localize(m.appdataFor(host))
		spec, err := m.buildApp(ctx, host, local, t.req)
		if err == nil && t.source != AdoptContainer {
			// Unraid sets some variables itself (HOST_OS, TZ, …), and the
			// container may have others its template doesn't: keep them.
			if missing := missingEnv(cur.Env, img.Config.Env, spec.Create.Env); len(missing) > 0 {
				t.req.Extra = append(t.req.Extra, missing...)
				spec, err = m.buildApp(ctx, host, local, t.req)
			}
		}
		if err != nil {
			if i < len(tries)-1 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("Its %s settings didn't work (%v), so hlm used the next best.", sourceName(t.source), err))
				continue
			}
			return plan, fmt.Errorf("hlm can't describe this container: %w", err)
		}
		plan.Source, plan.Template, plan.Request = t.source, t.app.Name, t.req
		if t.save {
			plan.app = t.app
			plan.Request.TemplateID = t.app.ID
		} else {
			plan.TemplateID = t.app.ID
		}
		plan.Changes = compareContainers(cur, spec.Create, img, info, extras)
		for _, w := range spec.Warnings {
			plan.Notes = append(plan.Notes, w)
		}
		return plan, nil
	}
	return plan, errors.New("hlm can't describe this container")
}

func sourceName(s string) string {
	switch s {
	case AdoptCommunityApps:
		return "Community Applications template's"
	case AdoptUnraid:
		return "Unraid template's"
	}
	return "own"
}

// adoptedID is the ID of the custom template made for an adopted
// container; adopting it again replaces the template.
func adoptedID(kind, host, name string) string {
	sum := sha1.Sum([]byte(host + "\x00" + name))
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	return "adopted-" + kind + "-" + slug + "-" + hex.EncodeToString(sum[:])[:8]
}

// missingEnv are the variables a container has, that its image doesn't set,
// and that a new container with env wouldn't have, as extra items.
func missingEnv(cur, image, env []string) []deploy.ExtraConfig {
	has := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		has[k] = true
	}
	var out []deploy.ExtraConfig
	for _, kv := range cur {
		k, v, _ := strings.Cut(kv, "=")
		if !has[k] && !slices.Contains(image, kv) {
			out = append(out, deploy.ExtraConfig{Type: catalog.TypeVariable, Name: k, Target: k, Value: v})
		}
	}
	return out
}

// unraidFlash is where Unraid's flash drive is on its host.
var unraidFlash = "/boot"

// unraidTemplate reads Unraid's template for a container, through a
// helper container (so it works on any host hlm reaches Docker on). Only
// called for containers Unraid made, so /boot is Unraid's flash drive.
func (m *Manager) unraidTemplate(ctx context.Context, host, name string) (catalog.UnraidTemplate, error) {
	file := catalog.UnraidTemplatesDir + "/my-" + name + ".xml"
	code, out, err := m.runHelperOutput(ctx, host, unraidFlash+":/boot:ro", "all", "cat", file)
	if err != nil {
		return catalog.UnraidTemplate{}, err
	}
	if code != 0 {
		return catalog.UnraidTemplate{}, fmt.Errorf("%s: %s", file, strings.TrimSpace(out))
	}
	return catalog.ParseUnraidTemplate([]byte(out))
}

// requestFromUnraid is the request that makes a container as Unraid's
// template ut describes it, from template app: the user's values for the
// template's items, the items the user added as extra ones, and the
// template's items the user removed left out.
func requestFromUnraid(app *catalog.App, ut catalog.UnraidTemplate, tag, name string) deploy.Request {
	ep, pa, priv := ut.App.ExtraParams, ut.App.PostArgs, ut.App.Privileged
	if ut.CPUSet != "" {
		ep = strings.TrimSpace(ep + " --cpuset-cpus=" + ut.CPUSet)
	}
	req := deploy.Request{
		TemplateID: app.ID, Name: name, Tag: tag, Values: map[string]string{},
		Network: ut.App.Network, IPAddress: ut.MyIP, ExtraParams: &ep, PostArgs: &pa, Privileged: &priv,
	}
	ids := map[string]bool{}
	for _, c := range app.Config {
		ids[c.ID] = true
	}
	mine := map[string]bool{}
	for _, c := range ut.App.Config {
		mine[c.ID] = true
		if ids[c.ID] {
			req.Values[c.ID] = c.Value
			continue
		}
		req.Extra = append(req.Extra, deploy.ExtraConfig{Type: c.Type, Name: c.Name, Target: c.Target, Value: c.Value, Mode: c.Mode})
	}
	for _, c := range app.Config {
		if !mine[c.ID] && !c.Required {
			req.Disabled = append(req.Disabled, c.ID)
		}
	}
	return req
}

// Adopt makes a container hlm didn't make one of its servers, as
// AdoptPlan describes, recreating it (and so restarting it, if it runs).
func (m *Manager) Adopt(ctx context.Context, id string, emit func(Event)) (srv Server, err error) {
	plan, err := m.AdoptPlan(ctx, id)
	if err != nil {
		return Server{}, err
	}
	job := Job{Kind: "adopt", Server: plan.Container.ID, App: plan.Template, Icon: plan.Container.Icon, Image: plan.Container.Image}
	emit, finish, err := m.jobs.start(job, emit)
	if err != nil {
		return Server{}, err
	}
	defer func() { finish(err) }()
	if plan.app != nil {
		emit(Event{Stage: "create", Message: "Saving the template " + plan.Template})
		if err := m.catalog.AddCustom(plan.app); err != nil {
			return Server{}, fmt.Errorf("save the template: %w", err)
		}
	}
	req := plan.Request
	m.log.Info("adopting a container", "container", plan.Container.ID, "source", plan.Source, "template", req.TemplateID)
	return m.recreateFrom(ctx, plan.info, &req, false, false, emit)
}

// containerExtras are the parts of a container's configuration hlm can't
// give a new one; currentContainer reads them to say they'd be lost.
type containerExtras struct {
	HostConfig struct {
		PidMode           string   `json:"PidMode"`
		IpcMode           string   `json:"IpcMode"`
		UTSMode           string   `json:"UTSMode"`
		Links             []string `json:"Links"`
		VolumesFrom       []string `json:"VolumesFrom"`
		CgroupParent      string   `json:"CgroupParent"`
		MemoryReservation int64    `json:"MemoryReservation"`
		CPUShares         int64    `json:"CpuShares"`
		BlkioWeight       int64    `json:"BlkioWeight"`
		PublishAllPorts   bool     `json:"PublishAllPorts"`
		OomKillDisable    *bool    `json:"OomKillDisable"`
	} `json:"HostConfig"`
}

// currentContainer reads a container's inspect output as a create request
// (Docker's inspect and create share their field names).
func currentContainer(raw map[string]any) (docker.CreateRequest, containerExtras, error) {
	var cur docker.CreateRequest
	var extras containerExtras
	data, err := json.Marshal(raw)
	if err != nil {
		return cur, extras, err
	}
	var parts struct {
		Config     json.RawMessage `json:"Config"`
		HostConfig json.RawMessage `json:"HostConfig"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return cur, extras, err
	}
	if err := json.Unmarshal(parts.Config, &cur); err != nil {
		return cur, extras, fmt.Errorf("read the container's configuration: %w", err)
	}
	if err := json.Unmarshal(parts.HostConfig, &cur.HostConfig); err != nil {
		return cur, extras, fmt.Errorf("read the container's host configuration: %w", err)
	}
	_ = json.Unmarshal(data, &extras)
	return cur, extras, nil
}
