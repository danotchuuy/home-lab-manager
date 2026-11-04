// Package deploy turns a Community Applications template and the values a
// user filled in into a Docker container definition.
package deploy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Labels set on every container this program manages.
const (
	LabelManaged  = "io.hlm.managed"
	LabelTemplate = "io.hlm.template"
	LabelApp      = "io.hlm.app"
	LabelRequest  = "io.hlm.request"
	LabelWebUI    = "io.hlm.webui"
	// LabelDirectIP marks containers on a macvlan or ipvlan network. They have
	// their own address on the LAN, which their Web UI link should use.
	LabelDirectIP = "io.hlm.directip"

	// Unraid's own labels, so the containers also look right in Unraid's UI.
	labelUnraidIcon  = "net.unraid.docker.icon"
	labelUnraidWebUI = "net.unraid.docker.webui"
)

// Request describes a game server to create from a template.
type Request struct {
	TemplateID string `json:"templateId"`
	// Name is the container name.
	Name string `json:"name"`
	// Tag selects one of the template's branches. Empty means the default image.
	Tag string `json:"tag,omitempty"`
	// Values holds the value for each template config item, keyed by
	// catalog.Config.ID. Items without a value use the template's value.
	Values map[string]string `json:"values,omitempty"`
	// Disabled lists config items (by ID) to leave out entirely, for example
	// an optional port the user doesn't want to publish.
	Disabled []string `json:"disabled,omitempty"`
	// Network is the Docker network to attach to: "bridge", "host", "none"
	// or the name of a user-defined network. Empty uses the template's.
	Network string `json:"network,omitempty"`
	// IPAddress is a static IPv4 or IPv6 address on Network. Docker only
	// supports this on user-defined networks with a configured subnet.
	IPAddress string `json:"ipAddress,omitempty"`
	// Extra holds config items the user added on top of the template, like
	// Unraid's "Add another Path, Port, Variable, Label or Device".
	Extra []ExtraConfig `json:"extra,omitempty"`
	// ExtraParams, PostArgs and Privileged override the template's values
	// when set.
	ExtraParams *string `json:"extraParams,omitempty"`
	PostArgs    *string `json:"postArgs,omitempty"`
	Privileged  *bool   `json:"privileged,omitempty"`
	// RestartPolicy overrides the template's restart policy: "no", "always",
	// "unless-stopped" or "on-failure". "always" and "unless-stopped" start
	// the server when Docker starts, i.e. on boot.
	RestartPolicy string `json:"restartPolicy,omitempty"`
}

// Env describes the Docker host a spec is built for.
type Env struct {
	// Networks holds the host's networks by name, with their attached
	// containers filled in (see docker.Client.InspectNetwork).
	Networks map[string]docker.Network
}

// Spec is everything needed to create the container.
type Spec struct {
	Name     string
	Image    string
	Create   docker.CreateRequest
	Warnings []string
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Build creates the container spec for req. The app should already be
// localized (see catalog.App.Localize) so that its default paths make sense
// on this host.
func Build(app *catalog.App, req Request, env Env) (*Spec, error) {
	if !validName.MatchString(req.Name) {
		return nil, fmt.Errorf("invalid server name %q: use letters, digits, '_', '.' and '-', starting with a letter or digit", req.Name)
	}

	spec := &Spec{Name: req.Name}
	image, err := imageFor(app, req.Tag)
	if err != nil {
		return nil, err
	}
	spec.Image = image

	c := docker.CreateRequest{
		Image:  image,
		Labels: map[string]string{},
		HostConfig: docker.HostConfig{
			NetworkMode:   strings.TrimSpace(app.Network),
			Privileged:    orDefault(req.Privileged, app.Privileged),
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		},
	}

	disabled := map[string]bool{}
	for _, id := range req.Disabled {
		disabled[id] = true
	}

	hostPorts := map[string]int{} // container port -> host port, for the WebUI link
	extra, err := extraConfigs(req.Extra)
	if err != nil {
		return nil, err
	}
	for _, cfg := range append(app.Config[:len(app.Config):len(app.Config)], extra...) {
		value, ok := req.Values[cfg.ID]
		if !ok {
			value = cfg.Value
		}
		value = strings.TrimSpace(value)
		if disabled[cfg.ID] {
			if cfg.Required {
				return nil, fmt.Errorf("%s is required", cfg.Name)
			}
			continue
		}
		if value == "" && cfg.Type != catalog.TypeVariable && cfg.Type != catalog.TypeLabel {
			if cfg.Required {
				return nil, fmt.Errorf("%s is required", cfg.Name)
			}
			continue
		}

		switch cfg.Type {
		case catalog.TypePort:
			if _, err := parsePortRange(cfg.Target); err != nil {
				spec.Warnings = append(spec.Warnings, fmt.Sprintf("skipped port %q: invalid container port %q", cfg.Name, cfg.Target))
				continue
			}
			if err := publish(&c, cfg.Target, cfg.Mode, value, hostPorts, &spec.Warnings); err != nil {
				return nil, fmt.Errorf("%s: %w", cfg.Name, err)
			}

		case catalog.TypePath:
			if cfg.Target == "" {
				continue
			}
			if !strings.HasPrefix(value, "/") {
				return nil, fmt.Errorf("%s: host path must be absolute, got %q", cfg.Name, value)
			}
			c.HostConfig.Binds = append(c.HostConfig.Binds, value+":"+cfg.Target+":"+pathMode(cfg.Mode))

		case catalog.TypeVariable:
			if cfg.Target == "" {
				continue
			}
			if value == "" && cfg.Required {
				return nil, fmt.Errorf("%s is required", cfg.Name)
			}
			if app.FromImage != "" && value == cfg.Default {
				// The image sets this itself; leaving it out lets a newer
				// image change its own default.
				continue
			}
			c.Env = append(c.Env, cfg.Target+"="+value)

		case catalog.TypeDevice:
			target := cfg.Target
			if target == "" || !strings.HasPrefix(target, "/") {
				target = value
			}
			c.HostConfig.Devices = append(c.HostConfig.Devices, docker.DeviceMapping{
				PathOnHost: value, PathInContainer: target, CgroupPermissions: "rwm",
			})

		case catalog.TypeLabel:
			if cfg.Target != "" {
				c.Labels[cfg.Target] = value
			}
		}
	}

	if err := applyExtraParams(&c, orDefault(req.ExtraParams, app.ExtraParams), &spec.Warnings); err != nil {
		return nil, fmt.Errorf("extra parameters: %w", err)
	}
	// The template's network (possibly overridden by --network in its
	// ExtraParams) is only a default; the user's choice wins.
	netKind, err := applyNetwork(&c, req, env, &spec.Warnings)
	if err != nil {
		return nil, err
	}
	ports := hostPorts
	if netKind == netHost || netKind == netDirect {
		// The container is reached on its own ports, not published ones.
		ports = map[string]int{}
		for p := range hostPorts {
			ports[p] = mustAtoi(p)
		}
	}
	if netKind == netDirect {
		c.Labels[LabelDirectIP] = "true"
	}
	switch req.RestartPolicy {
	case "":
	case "no", "always", "unless-stopped", "on-failure":
		c.HostConfig.RestartPolicy.Name = req.RestartPolicy
	default:
		return nil, fmt.Errorf("invalid restart policy %q", req.RestartPolicy)
	}
	if c.HostConfig.NetworkMode == "host" && c.Hostname != "" {
		// Docker rejects a hostname in host network mode.
		spec.Warnings = append(spec.Warnings, "ignored hostname "+c.Hostname+": not allowed with host networking")
		c.Hostname = ""
	}
	if postArgs := orDefault(req.PostArgs, app.PostArgs); strings.TrimSpace(postArgs) != "" {
		args, err := splitArgs(postArgs)
		if err != nil {
			return nil, fmt.Errorf("post arguments: %w", err)
		}
		c.Cmd = args
	}

	// Many game server images read commands from stdin (or run under screen),
	// so keep stdin open like Unraid does for interactive templates.
	c.OpenStdin = true

	reqJSON, _ := json.Marshal(req)
	c.Labels[LabelManaged] = "true"
	c.Labels[LabelTemplate] = app.ID
	c.Labels[LabelApp] = app.Name
	c.Labels[LabelRequest] = string(reqJSON)
	if app.Icon != "" {
		c.Labels[labelUnraidIcon] = app.Icon
	}
	if app.WebUI != "" {
		c.Labels[labelUnraidWebUI] = app.WebUI
		if link := RenderWebUI(app.WebUI, ports); link != "" {
			c.Labels[LabelWebUI] = link
		}
	}

	spec.Create = c
	return spec, nil
}

// imageFor returns the image reference for the chosen branch tag.
func imageFor(app *catalog.App, tag string) (string, error) {
	if tag == "" {
		return app.Repository, nil
	}
	for _, b := range app.Branches {
		if b.Tag != tag {
			continue
		}
		if b.Repository != "" {
			return b.Repository, nil
		}
		name, _ := docker.SplitImageRef(app.Repository)
		return name + ":" + b.Tag, nil
	}
	return "", fmt.Errorf("template has no branch %q", tag)
}

func pathMode(m string) string {
	switch m {
	case "ro", "r", "ro,rslave", "ro,slave":
		return "ro"
	case "rw,slave", "rw,shared", "rw,rslave", "rw,rshared":
		return m
	default:
		return "rw"
	}
}

var webUIPort = regexp.MustCompile(`\[PORT:(\d+)\]`)

// RenderWebUI fills in an Unraid WebUI template such as
// "http://[IP]:[PORT:8080]/". [IP] is left for the client to fill in, since
// only it knows which address it reached the host on. ports maps container
// ports to host ports.
func RenderWebUI(tmpl string, ports map[string]int) string {
	return webUIPort.ReplaceAllStringFunc(tmpl, func(m string) string {
		p := webUIPort.FindStringSubmatch(m)[1]
		if host, ok := ports[p]; ok {
			return strconv.Itoa(host)
		}
		return p
	})
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// RequestFromLabels recovers the request a container was created from.
func RequestFromLabels(labels map[string]string) (Request, bool) {
	var r Request
	raw, ok := labels[LabelRequest]
	if !ok || json.Unmarshal([]byte(raw), &r) != nil {
		return Request{}, false
	}
	return r, true
}

// ChangedSettings names what differs between a server's settings and new
// ones, for the audit log: template items by ID, and the other settings by
// name. It never includes values.
func ChangedSettings(old, new Request) []string {
	var out []string
	keys := map[string]bool{}
	for k := range old.Values {
		keys[k] = true
	}
	for k := range new.Values {
		keys[k] = true
	}
	var items []string
	for k := range keys {
		ov, oldOK := old.Values[k]
		nv, newOK := new.Values[k]
		if ov != nv || oldOK != newOK {
			items = append(items, k)
		}
	}
	slices.Sort(items)
	out = append(out, items...)
	same := func(a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	for _, f := range []struct {
		name string
		a, b any
	}{
		{"tag", old.Tag, new.Tag}, {"disabled", old.Disabled, new.Disabled}, {"network", old.Network, new.Network},
		{"ipAddress", old.IPAddress, new.IPAddress}, {"extra", old.Extra, new.Extra},
		{"extraParams", old.ExtraParams, new.ExtraParams}, {"postArgs", old.PostArgs, new.PostArgs},
		{"privileged", old.Privileged, new.Privileged}, {"restartPolicy", old.RestartPolicy, new.RestartPolicy},
	} {
		if !same(f.a, f.b) {
			out = append(out, f.name)
		}
	}
	return out
}
