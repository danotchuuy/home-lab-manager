package deploy

import (
	"cmp"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// A confined server is one that can't reach the host: it gets no extra
// privileges, devices or host namespaces, and only folders of its own. That
// lets someone create servers without being able to become root on the host.
//
// Confine checks the finished container rather than the request, because a
// template brings its own settings, and ExtraParams can set almost anything.
// It allows what's known to be safe, so whatever it doesn't know is refused.

// LabelConfined marks a server that has to stay confined when it's recreated.
const LabelConfined = "io.hlm.confined"

// lowestHostPort is the first port a confined server may publish. Below it
// are the host's own services, like SSH and web servers.
const lowestHostPort = 1024

// confinedConfig are the fields of docker.CreateRequest a confined server may
// set freely, because they only affect what's inside the container.
var confinedConfig = map[string]bool{
	"Image": true, "Hostname": true, "User": true, "Env": true, "Cmd": true, "Entrypoint": true,
	"ExposedPorts": true, "Tty": true, "OpenStdin": true, "StopTimeout": true,
	"Labels": true, "HostConfig": true, // both checked below
}

// confinedHost is the same for docker.HostConfig.
var confinedHost = map[string]bool{
	"RestartPolicy": true, "CapDrop": true, "Tmpfs": true, "ExtraHosts": true, "DNS": true, "GroupAdd": true,
	"Ulimits": true, "Memory": true, "NanoCPUs": true, "ShmSize": true, "PidsLimit": true, "Init": true,
	"ReadonlyRootfs": true,
	"Binds":          true, "PortBindings": true, "NetworkMode": true, "SecurityOpt": true, "LogConfig": true, // checked below
}

// describe names the fields people are most likely to run into.
var describe = map[string]string{
	"Privileged": "privileged mode",
	"Devices":    "devices",
	"CapAdd":     "added capabilities",
	"Sysctls":    "kernel settings",
}

// hlmLabels are the labels of this program a confined server may carry: Build
// sets them. Others can make hlm treat a container as part of itself.
var hlmLabels = []string{LabelManaged, LabelTemplate, LabelApp, LabelRequest, LabelWebUI, LabelConfined}

var notFolderChar = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// folderFor names the folder for a container path: /config/app is config-app.
func folderFor(target string) string {
	name := notFolderChar.ReplaceAllString(strings.TrimPrefix(path.Clean("/"+target), "/"), "-")
	return cmp.Or(strings.Trim(name, "-."), "root")
}

// ConfinePaths returns req with each of the template's data folders in a
// folder of its own under root/<name>, wherever the template or request put
// it. They're side by side, never one inside another: see confinedBinds.
func ConfinePaths(app *catalog.App, req Request, root string) Request {
	values := maps.Clone(req.Values)
	if values == nil {
		values = map[string]string{}
	}
	for _, cfg := range app.Config {
		if cfg.Type == catalog.TypePath && cfg.Target != "" {
			values[cfg.ID] = path.Join(root, req.Name, folderFor(cfg.Target))
		}
	}
	req.Values = values
	return req
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// Confine lists what stops spec from being a confined server, whose folders
// have to be under root/<name>. If there's nothing, it marks spec as confined.
func Confine(spec *Spec, root string) []string {
	var out []string
	c := &spec.Create
	refuse := func(v reflect.Value, free map[string]bool) {
		for _, f := range reflect.VisibleFields(v.Type()) {
			if !free[f.Name] && !v.FieldByIndex(f.Index).IsZero() {
				out = append(out, cmp.Or(describe[f.Name], "the Docker setting "+f.Name))
			}
		}
	}
	refuse(reflect.ValueOf(*c), confinedConfig)
	refuse(reflect.ValueOf(c.HostConfig), confinedHost)

	out = append(out, confinedBinds(c.HostConfig.Binds, spec.Name, root)...)
	if mode := c.HostConfig.NetworkMode; mode != "bridge" && mode != "none" {
		out = append(out, "the "+mode+" network, only bridge is allowed")
	}
	for port, bindings := range c.HostConfig.PortBindings {
		for _, b := range bindings {
			if n, err := strconv.Atoi(b.HostPort); err != nil || n < lowestHostPort {
				out = append(out, "host port "+cmp.Or(b.HostPort, "for "+port)+", it must be "+strconv.Itoa(lowestHostPort)+" or above")
			}
		}
	}
	for _, opt := range c.HostConfig.SecurityOpt {
		if opt != "no-new-privileges" && opt != "no-new-privileges:true" {
			out = append(out, "security option "+opt)
		}
	}
	if lc := c.HostConfig.LogConfig; lc != nil && lc.Type != "" && lc.Type != "json-file" && lc.Type != "local" {
		out = append(out, "log driver "+lc.Type)
	}
	for k := range c.Labels {
		if (strings.HasPrefix(k, "io.hlm.") || strings.HasPrefix(k, "io.gsh.")) && !slices.Contains(hlmLabels, k) {
			out = append(out, "label "+k)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		c.Labels[LabelConfined] = "true"
	}
	return out
}

// confinedBinds checks a server's folders. Each has to be inside root/<name>,
// and none inside another: the contents of a mounted folder are the
// container's to change, so it could swap a nested mount's folder for a
// symlink to /, and Docker would follow that on the host when it next
// recreates the container. A folder that's only mounted itself is out of its
// reach, since it can't replace the folder it's in.
func confinedBinds(binds []string, name, root string) []string {
	var out []string
	base := path.Join(root, name)
	var sources []string
	for _, b := range binds {
		parts := strings.Split(b, ":")
		if !path.IsAbs(root) || len(parts) != 3 || parts[0] != path.Clean(parts[0]) || !within(parts[0], base) {
			out = append(out, "the folder "+parts[0]+", only "+base+" is allowed")
			continue
		}
		switch parts[2] {
		case "ro", "rw", "rw,slave", "rw,rslave":
		default:
			out = append(out, "mount option "+parts[2])
		}
		sources = append(sources, parts[0])
	}
	for _, a := range sources {
		for _, b := range sources {
			if a != b && within(b, a) {
				out = append(out, "the folder "+b+", which is inside "+a)
			}
		}
	}
	return out
}
