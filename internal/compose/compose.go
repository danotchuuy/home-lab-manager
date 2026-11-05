// Package compose reads a Docker Compose file (the subset hlm supports)
// into a normalized model, ordered so dependencies come first. It doesn't
// run anything; see internal/stack.
package compose

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Project is a parsed Compose file.
type Project struct {
	Name     string
	Services []*Service // dependencies first
	Networks map[string]Network
	Volumes  map[string]Volume
	Warnings []string
	env      map[string]string // the .env's variables
}

// Service is one service.
type Service struct {
	Name          string
	Image         string
	ContainerName string
	Command       []string // nil keeps the image's
	Entrypoint    []string // nil keeps the image's
	Environment   map[string]string
	Ports         []Port
	Mounts        []Mount
	Networks      []string            // keys of Project.Networks
	Aliases       map[string][]string // by network
	NetworkMode   string              // "", host, none, bridge, or service:<name>
	DependsOn     []Dependency
	Restart       string
	Healthcheck   *Healthcheck
	Labels        map[string]string
	Devices       []string
	CapAdd        []string
	CapDrop       []string
	User          string
	WorkingDir    string
	Hostname      string
	Privileged    bool
	Init          *bool
	Tty           bool
	StdinOpen     bool
	ReadOnly      bool
	MemLimit      int64
	CPUs          float64
	PidsLimit     *int64
	ShmSize       int64
	Ulimits       []Ulimit
	ExtraHosts    []string
	DNS           []string
	SecurityOpt   []string
	Sysctls       map[string]string
	Tmpfs         []string
	StopGrace     *int // seconds
	StopSignal    string
}

// Port is a published (or only exposed) port.
type Port struct {
	HostIP    string
	Published string // host port or range; "" only exposes it
	Target    string // container port or range
	Protocol  string // tcp or udp
}

// Mount is a volume: a bind mount (Source is a host path, relative ones
// resolved later), a named volume (Source is a key of Project.Volumes), an
// anonymous volume (no Source) or a tmpfs.
type Mount struct {
	Type     string // bind, volume or tmpfs
	Source   string
	Target   string
	ReadOnly bool
}

// Dependency is a depends_on entry.
type Dependency struct {
	Service   string
	Condition string // service_started, service_healthy or service_completed_successfully
}

// Healthcheck is a service's health check; Disable turns the image's off.
type Healthcheck struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	StartPeriod time.Duration
	Retries     int
	Disable     bool
}

// Ulimit is a resource limit.
type Ulimit struct {
	Name       string
	Soft, Hard int64
}

// Network is a top-level network.
type Network struct {
	Name     string // Docker's name; "" means <project>_<key>
	External bool
	Driver   string
	Internal bool
}

// Volume is a top-level volume.
type Volume struct {
	Name     string // Docker's name; "" means <project>_<key>
	External bool
}

// ValidName matches project and service names hlm accepts.
var ValidName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Parse reads a Compose file for the project name, with variables from env
// (a .env file's contents). Only env's variables are used, never hlm's own.
func Parse(data []byte, env, name string) (*Project, error) {
	if !ValidName.MatchString(name) {
		return nil, errors.New("the stack's name is lower-case letters, digits, dashes and underscores, starting with a letter or digit")
	}
	vars, err := ParseEnv(env)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("a Compose file is a map with services: at the top")
	}
	p := &Project{Name: name, Networks: map[string]Network{}, Volumes: map[string]Volume{}, env: vars}
	in := &interpolator{vars: vars, missing: map[string]bool{}}
	v, err := in.walk(top)
	if err != nil {
		return nil, err
	}
	top = v.(map[string]any)
	for k := range in.missing {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s isn't set in the .env, so it's empty", k))
	}
	sort.Strings(p.Warnings)

	for k := range top {
		switch {
		case k == "services", k == "networks", k == "volumes", k == "version", k == "name", strings.HasPrefix(k, "x-"):
		case k == "configs", k == "secrets":
			return nil, fmt.Errorf("top-level %s aren't supported: mount the files as volumes instead", k)
		case k == "include":
			return nil, errors.New("include isn't supported: paste the included services into this file")
		default:
			return nil, fmt.Errorf("unknown top-level key %q", k)
		}
	}
	if err := p.parseNetworks(top["networks"]); err != nil {
		return nil, err
	}
	if err := p.parseVolumes(top["volumes"]); err != nil {
		return nil, err
	}
	svcs, ok := top["services"].(map[string]any)
	if !ok || len(svcs) == 0 {
		return nil, errors.New("there are no services")
	}
	for _, sname := range slices.Sorted(maps.Keys(svcs)) {
		s, err := p.parseService(sname, svcs[sname])
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", sname, err)
		}
		p.Services = append(p.Services, s)
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	if err := p.order(); err != nil {
		return nil, err
	}
	return p, nil
}

// --- .env and interpolation ---

// ParseEnv reads KEY=VALUE lines; # starts a comment, and values may be
// quoted.
func ParseEnv(env string) (map[string]string, error) {
	vars := map[string]string{}
	for i, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envName.MatchString(k) {
			return nil, fmt.Errorf(".env line %d isn't KEY=VALUE", i+1)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			end := strings.IndexByte(v[1:], v[0])
			if end < 0 {
				return nil, fmt.Errorf(".env line %d has an unclosed quote", i+1)
			}
			v = v[1 : end+1]
		} else if j := strings.Index(v, " #"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
		vars[k] = v
	}
	return vars, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type interpolator struct {
	vars    map[string]string
	missing map[string]bool
}

var varRE = regexp.MustCompile(`\$\$|\$\{([^}]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func (in *interpolator) walk(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return in.expand(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			e, err := in.walk(val)
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			e, err := in.walk(val)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	return v, nil
}

func (in *interpolator) expand(s string) (string, error) {
	var err error
	out := varRE.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		expr := strings.TrimPrefix(m, "$")
		expr = strings.TrimSuffix(strings.TrimPrefix(expr, "{"), "}")
		for _, op := range []string{":-", "-", ":?", "?", ":+", "+"} {
			name, arg, ok := strings.Cut(expr, op)
			if !ok || !envName.MatchString(name) {
				continue
			}
			val, set := in.vars[name]
			empty := !set || (strings.HasPrefix(op, ":") && val == "")
			switch strings.TrimPrefix(op, ":") {
			case "-":
				if empty {
					return arg
				}
				return val
			case "?":
				if empty {
					err = fmt.Errorf("%s is required: %s", name, cmpOr(arg, "set it in the .env"))
				}
				return val
			case "+":
				if empty {
					return ""
				}
				return arg
			}
		}
		if !envName.MatchString(expr) {
			err = fmt.Errorf("can't read the variable %q", m)
			return m
		}
		val, set := in.vars[expr]
		if !set {
			in.missing[expr] = true
		}
		return val
	})
	return out, err
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// --- top level ---

func (p *Project) parseNetworks(v any) error {
	m, ok := asMap(v)
	if !ok && v != nil {
		return errors.New("networks: expected a map")
	}
	for k, def := range m {
		n := Network{Driver: "bridge"}
		d, _ := asMap(def)
		for dk, dv := range d {
			switch dk {
			case "name":
				n.Name = str(dv)
			case "external":
				n.External = truthy(dv)
			case "driver":
				n.Driver = str(dv)
			case "internal":
				n.Internal = truthy(dv)
			case "labels", "driver_opts", "attachable", "enable_ipv6", "ipam":
				p.Warnings = append(p.Warnings, fmt.Sprintf("network %s: %s is ignored", k, dk))
			default:
				return fmt.Errorf("network %s: unknown key %q", k, dk)
			}
		}
		if n.External && n.Name == "" {
			n.Name = k
		}
		p.Networks[k] = n
	}
	return nil
}

func (p *Project) parseVolumes(v any) error {
	m, ok := asMap(v)
	if !ok && v != nil {
		return errors.New("volumes: expected a map")
	}
	for k, def := range m {
		vol := Volume{}
		d, _ := asMap(def)
		for dk, dv := range d {
			switch dk {
			case "name":
				vol.Name = str(dv)
			case "external":
				vol.External = truthy(dv)
			case "labels", "driver", "driver_opts":
				p.Warnings = append(p.Warnings, fmt.Sprintf("volume %s: %s is ignored", k, dk))
			default:
				return fmt.Errorf("volume %s: unknown key %q", k, dk)
			}
		}
		if vol.External && vol.Name == "" {
			vol.Name = k
		}
		p.Volumes[k] = vol
	}
	return nil
}

// NetworkName is a network's name in Docker.
func (p *Project) NetworkName(key string) string {
	if n := p.Networks[key]; n.Name != "" {
		return n.Name
	}
	return p.Name + "_" + key
}

// VolumeName is a named volume's name in Docker.
func (p *Project) VolumeName(key string) string {
	if v := p.Volumes[key]; v.Name != "" {
		return v.Name
	}
	return p.Name + "_" + key
}

// ContainerName is a service's container name.
func (p *Project) ContainerName(s *Service) string {
	if s.ContainerName != "" {
		return s.ContainerName
	}
	return p.Name + "-" + s.Name
}

// Service returns the service called name.
func (p *Project) Service(name string) *Service {
	for _, s := range p.Services {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// check validates references between services, networks and volumes.
func (p *Project) check() error {
	names := map[string]bool{}
	for _, s := range p.Services {
		if !ValidName.MatchString(s.Name) {
			return fmt.Errorf("service name %q: use lower-case letters, digits, dashes and underscores", s.Name)
		}
		cn := p.ContainerName(s)
		if names[cn] {
			return fmt.Errorf("two services would both be called %s", cn)
		}
		names[cn] = true
		for _, d := range s.DependsOn {
			if p.Service(d.Service) == nil {
				return fmt.Errorf("service %s depends on %s, which isn't in the file", s.Name, d.Service)
			}
		}
		if other, ok := strings.CutPrefix(s.NetworkMode, "service:"); ok && p.Service(other) == nil {
			return fmt.Errorf("service %s uses the network of %s, which isn't in the file", s.Name, other)
		}
		for _, n := range s.Networks {
			if _, ok := p.Networks[n]; !ok && n != "default" {
				return fmt.Errorf("service %s is on network %s, which isn't under networks:", s.Name, n)
			}
		}
		for _, m := range s.Mounts {
			if m.Type == "volume" && m.Source != "" {
				if _, ok := p.Volumes[m.Source]; !ok {
					return fmt.Errorf("service %s mounts the volume %s, which isn't under volumes:", s.Name, m.Source)
				}
			}
		}
	}
	return nil
}

// order sorts services so each comes after those it depends on.
func (p *Project) order() error {
	deps := map[string][]string{}
	for _, s := range p.Services {
		for _, d := range s.DependsOn {
			deps[s.Name] = append(deps[s.Name], d.Service)
		}
		if other, ok := strings.CutPrefix(s.NetworkMode, "service:"); ok {
			deps[s.Name] = append(deps[s.Name], other)
		}
	}
	var out []*Service
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("services depend on each other in a loop: %s", strings.Join(append(path, name), " → "))
		case 2:
			return nil
		}
		state[name] = 1
		for _, d := range slices.Sorted(slices.Values(deps[name])) {
			if err := visit(d, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, p.Service(name))
		return nil
	}
	for _, s := range p.Services {
		if err := visit(s.Name, nil); err != nil {
			return err
		}
	}
	p.Services = out
	return nil
}

// --- helpers for the loosely typed YAML ---

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "yes"
	}
	return false
}

// strList reads a string or a list of strings.
func strList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if _, ok := e.(map[string]any); ok {
				return nil, errors.New("expected a list of strings")
			}
			out = append(out, str(e))
		}
		return out, nil
	}
	return nil, errors.New("expected a list")
}

// mapOrList reads KEY=VALUE lists or maps (environment, labels, sysctls).
func mapOrList(v any) (map[string]string, error) {
	out := map[string]string{}
	switch x := v.(type) {
	case nil:
	case map[string]any:
		for k, val := range x {
			if val == nil {
				out[k] = ""
			} else {
				out[k] = str(val)
			}
		}
	case []any:
		for _, e := range x {
			k, val, _ := strings.Cut(str(e), "=")
			out[k] = val
		}
	default:
		return nil, errors.New("expected a map or a list of KEY=VALUE")
	}
	return out, nil
}
