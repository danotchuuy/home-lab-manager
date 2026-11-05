package compose

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseService reads one service.
func (p *Project) parseService(name string, v any) (*Service, error) {
	m, ok := asMap(v)
	if !ok {
		return nil, errors.New("expected a map")
	}
	s := &Service{Name: name, Environment: map[string]string{}, Labels: map[string]string{}, Aliases: map[string][]string{}}
	var err error
	for k, val := range m {
		if strings.HasPrefix(k, "x-") {
			continue
		}
		switch k {
		case "image":
			s.Image = str(val)
		case "container_name":
			s.ContainerName = str(val)
		case "command":
			s.Command, err = command(val)
		case "entrypoint":
			s.Entrypoint, err = command(val)
		case "environment":
			var env map[string]string
			if env, err = mapOrList(val); err == nil {
				for ek, ev := range env {
					s.Environment[ek] = ev
				}
			}
		case "env_file":
			// The uploaded .env is the only file there is; its variables
			// are what ${...} reads, and env_file asks for them in the
			// container too.
			s.Environment, err = p.envFile(val, s.Environment)
		case "ports":
			s.Ports, err = ports(val)
		case "expose":
			var list []string
			if list, err = strList(val); err == nil {
				for _, e := range list {
					target, proto, _ := strings.Cut(e, "/")
					s.Ports = append(s.Ports, Port{Target: target, Protocol: cmpOr(proto, "tcp")})
				}
			}
		case "volumes":
			s.Mounts, err = mounts(val)
		case "tmpfs":
			s.Tmpfs, err = strList(val)
		case "networks":
			err = s.networks(val)
		case "network_mode":
			s.NetworkMode = str(val)
		case "depends_on":
			s.DependsOn, err = dependsOn(val)
		case "restart":
			s.Restart = str(val)
		case "healthcheck":
			s.Healthcheck, err = healthcheck(val)
		case "labels":
			var l map[string]string
			if l, err = mapOrList(val); err == nil {
				s.Labels = l
			}
		case "devices":
			s.Devices, err = strList(val)
		case "cap_add":
			s.CapAdd, err = strList(val)
		case "cap_drop":
			s.CapDrop, err = strList(val)
		case "user":
			s.User = str(val)
		case "working_dir":
			s.WorkingDir = str(val)
		case "hostname":
			s.Hostname = str(val)
		case "privileged":
			s.Privileged = truthy(val)
		case "init":
			b := truthy(val)
			s.Init = &b
		case "tty":
			s.Tty = truthy(val)
		case "stdin_open":
			s.StdinOpen = truthy(val)
		case "read_only":
			s.ReadOnly = truthy(val)
		case "mem_limit":
			s.MemLimit, err = bytes(val)
		case "cpus":
			s.CPUs, err = strconv.ParseFloat(str(val), 64)
		case "pids_limit":
			var n int64
			if n, err = strconv.ParseInt(str(val), 10, 64); err == nil {
				s.PidsLimit = &n
			}
		case "shm_size":
			s.ShmSize, err = bytes(val)
		case "ulimits":
			s.Ulimits, err = ulimits(val)
		case "extra_hosts":
			if mm, ok := asMap(val); ok {
				for h, ip := range mm {
					s.ExtraHosts = append(s.ExtraHosts, h+":"+str(ip))
				}
			} else {
				s.ExtraHosts, err = strList(val)
			}
		case "dns":
			s.DNS, err = strList(val)
		case "security_opt":
			s.SecurityOpt, err = strList(val)
		case "sysctls":
			s.Sysctls, err = mapOrList(val)
		case "stop_grace_period":
			var d time.Duration
			if d, err = duration(val); err == nil {
				n := int(d / time.Second)
				s.StopGrace = &n
			}
		case "stop_signal":
			s.StopSignal = str(val)
		case "deploy":
			err = s.deploy(val, &p.Warnings)
		case "logging", "container_name_prefix", "platform", "pull_policy", "domainname", "mac_address", "stop_timeout":
			p.Warnings = append(p.Warnings, fmt.Sprintf("service %s: %s is ignored", name, k))
		case "build":
			return nil, errors.New("build isn't supported: hlm runs images, so build it elsewhere, push it to a registry, and give its image:")
		case "extends":
			return nil, errors.New("extends isn't supported: copy the settings into the service")
		case "profiles":
			return nil, errors.New("profiles aren't supported: leave out the services you don't want")
		case "configs", "secrets":
			return nil, fmt.Errorf("%s aren't supported: mount the files as volumes instead", k)
		default:
			return nil, fmt.Errorf("%s isn't supported", k)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if s.Image == "" {
		return nil, errors.New("it has no image:")
	}
	if len(s.Networks) == 0 && s.NetworkMode == "" {
		s.Networks = []string{"default"}
	}
	if s.NetworkMode != "" && len(s.Networks) > 0 && s.Networks[0] != "default" {
		return nil, errors.New("network_mode and networks can't both be set")
	}
	if containsDefault(s.Networks) {
		if _, ok := p.Networks["default"]; !ok {
			p.Networks["default"] = Network{Driver: "bridge"}
		}
	}
	return s, nil
}

func containsDefault(ns []string) bool {
	for _, n := range ns {
		if n == "default" {
			return true
		}
	}
	return false
}

func (p *Project) envFile(v any, env map[string]string) (map[string]string, error) {
	files, err := strList(v)
	if err != nil {
		// The long form: a list of {path, required}.
		list, ok := v.([]any)
		if !ok {
			return nil, err
		}
		files = nil
		for _, e := range list {
			m, _ := asMap(e)
			files = append(files, str(m["path"]))
		}
	}
	for _, f := range files {
		if f != ".env" && f != "./.env" {
			return nil, fmt.Errorf("only the .env given with the stack can be used, not %s", f)
		}
	}
	for k, val := range p.env {
		if _, set := env[k]; !set {
			env[k] = val
		}
	}
	return env, nil
}

// command reads a command or entrypoint: a list, or a string split as a
// shell would.
func command(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return splitShell(x)
	case []any:
		return strList(x)
	}
	return nil, errors.New("expected a string or a list")
}

// splitShell splits a command line into words, with quotes and backslashes.
func splitShell(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

func ports(v any) ([]Port, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("expected a list")
	}
	var out []Port
	for _, e := range list {
		if m, ok := asMap(e); ok {
			p := Port{Target: str(m["target"]), Published: str(m["published"]), HostIP: str(m["host_ip"]), Protocol: cmpOr(str(m["protocol"]), "tcp")}
			if p.Target == "" {
				return nil, errors.New("a port needs its target")
			}
			out = append(out, p)
			continue
		}
		s := str(e)
		p := Port{Protocol: "tcp"}
		if body, proto, ok := strings.Cut(s, "/"); ok {
			s, p.Protocol = body, proto
		}
		// [ip:][published:]target, where ip may be [v6].
		if strings.HasPrefix(s, "[") {
			end := strings.Index(s, "]:")
			if end < 0 {
				return nil, fmt.Errorf("can't read the port %q", str(e))
			}
			p.HostIP, s = s[1:end], s[end+2:]
		}
		parts := strings.Split(s, ":")
		switch len(parts) {
		case 1:
			p.Target = parts[0]
		case 2:
			p.Published, p.Target = parts[0], parts[1]
		case 3:
			p.HostIP, p.Published, p.Target = parts[0], parts[1], parts[2]
		default:
			return nil, fmt.Errorf("can't read the port %q", str(e))
		}
		if p.Target == "" {
			return nil, fmt.Errorf("can't read the port %q", str(e))
		}
		out = append(out, p)
	}
	for _, p := range out {
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return nil, fmt.Errorf("protocol %q: tcp or udp", p.Protocol)
		}
	}
	return out, nil
}

func mounts(v any) ([]Mount, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("expected a list")
	}
	var out []Mount
	for _, e := range list {
		if m, ok := asMap(e); ok {
			mt := Mount{Type: cmpOr(str(m["type"]), "volume"), Source: str(m["source"]), Target: str(m["target"]), ReadOnly: truthy(m["read_only"])}
			if mt.Type == "bind" && mt.Source == "" {
				return nil, errors.New("a bind mount needs its source")
			}
			if mt.Type != "bind" && mt.Type != "volume" && mt.Type != "tmpfs" {
				return nil, fmt.Errorf("volume type %q isn't supported", mt.Type)
			}
			out = append(out, mt)
			continue
		}
		parts := strings.Split(str(e), ":")
		mt := Mount{Type: "volume"}
		switch len(parts) {
		case 1:
			mt.Target = parts[0]
		case 2, 3:
			mt.Source, mt.Target = parts[0], parts[1]
			if len(parts) == 3 {
				for _, o := range strings.Split(parts[2], ",") {
					switch o {
					case "ro":
						mt.ReadOnly = true
					case "rw", "z", "Z", "cached", "delegated", "consistent":
					default:
						return nil, fmt.Errorf("volume option %q isn't supported", o)
					}
				}
			}
			if isPath(mt.Source) {
				mt.Type = "bind"
			}
		default:
			return nil, fmt.Errorf("can't read the volume %q", str(e))
		}
		out = append(out, mt)
	}
	for _, m := range out {
		if !strings.HasPrefix(m.Target, "/") {
			return nil, fmt.Errorf("the container path %q isn't absolute", m.Target)
		}
	}
	return out, nil
}

// isPath tells a bind mount's host path from a named volume.
func isPath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~")
}

func (s *Service) networks(v any) error {
	switch x := v.(type) {
	case []any:
		list, err := strList(x)
		if err != nil {
			return err
		}
		s.Networks = list
	case map[string]any:
		for n, def := range x {
			s.Networks = append(s.Networks, n)
			if d, ok := asMap(def); ok {
				if a, err := strList(d["aliases"]); err == nil {
					s.Aliases[n] = a
				}
			}
		}
	default:
		return errors.New("expected a list or a map")
	}
	return nil
}

func dependsOn(v any) ([]Dependency, error) {
	switch x := v.(type) {
	case []any:
		list, err := strList(x)
		if err != nil {
			return nil, err
		}
		var out []Dependency
		for _, n := range list {
			out = append(out, Dependency{Service: n, Condition: "service_started"})
		}
		return out, nil
	case map[string]any:
		var out []Dependency
		for n, def := range x {
			d := Dependency{Service: n, Condition: "service_started"}
			if m, ok := asMap(def); ok && m["condition"] != nil {
				d.Condition = str(m["condition"])
			}
			switch d.Condition {
			case "service_started", "service_healthy", "service_completed_successfully":
			default:
				return nil, fmt.Errorf("condition %q isn't known", d.Condition)
			}
			out = append(out, d)
		}
		return out, nil
	}
	return nil, errors.New("expected a list or a map")
}

func healthcheck(v any) (*Healthcheck, error) {
	m, ok := asMap(v)
	if !ok {
		return nil, errors.New("expected a map")
	}
	h := &Healthcheck{}
	for k, val := range m {
		var err error
		switch k {
		case "test":
			switch x := val.(type) {
			case string:
				h.Test = []string{"CMD-SHELL", x}
			default:
				h.Test, err = strList(x)
			}
		case "interval":
			h.Interval, err = duration(val)
		case "timeout":
			h.Timeout, err = duration(val)
		case "start_period":
			h.StartPeriod, err = duration(val)
		case "start_interval":
		case "retries":
			h.Retries, err = strconv.Atoi(str(val))
		case "disable":
			h.Disable = truthy(val)
		default:
			return nil, fmt.Errorf("unknown key %q", k)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	return h, nil
}

func (s *Service) deploy(v any, warnings *[]string) error {
	m, ok := asMap(v)
	if !ok {
		return errors.New("expected a map")
	}
	for k, val := range m {
		switch k {
		case "resources":
			r, _ := asMap(val)
			for rk, rv := range r {
				switch rk {
				case "limits":
					l, _ := asMap(rv)
					if l["memory"] != nil {
						n, err := bytes(l["memory"])
						if err != nil {
							return fmt.Errorf("memory: %w", err)
						}
						s.MemLimit = n
					}
					if l["cpus"] != nil {
						f, err := strconv.ParseFloat(str(l["cpus"]), 64)
						if err != nil {
							return fmt.Errorf("cpus: %w", err)
						}
						s.CPUs = f
					}
				case "reservations":
					*warnings = append(*warnings, fmt.Sprintf("service %s: deploy.resources.reservations is ignored", s.Name))
				default:
					return fmt.Errorf("resources.%s isn't supported", rk)
				}
			}
		case "restart_policy":
			r, _ := asMap(val)
			switch str(r["condition"]) {
			case "any":
				s.Restart = "always"
			case "on-failure":
				s.Restart = "on-failure"
			case "none":
				s.Restart = "no"
			}
		case "replicas":
			if str(val) != "1" {
				return errors.New("replicas other than 1 aren't supported")
			}
		default:
			return fmt.Errorf("deploy.%s isn't supported (only resource limits are)", k)
		}
	}
	return nil
}

func ulimits(v any) ([]Ulimit, error) {
	m, ok := asMap(v)
	if !ok {
		return nil, errors.New("expected a map")
	}
	var out []Ulimit
	for name, val := range m {
		u := Ulimit{Name: name}
		if lm, ok := asMap(val); ok {
			soft, err1 := strconv.ParseInt(str(lm["soft"]), 10, 64)
			hard, err2 := strconv.ParseInt(str(lm["hard"]), 10, 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("%s: soft and hard are numbers", name)
			}
			u.Soft, u.Hard = soft, hard
		} else {
			n, err := strconv.ParseInt(str(val), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: not a number", name)
			}
			u.Soft, u.Hard = n, n
		}
		out = append(out, u)
	}
	return out, nil
}

// bytes reads sizes such as 512m, 1g or 1073741824.
func bytes(v any) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(str(v)))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "b"), "i")
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
		if mult > 1 {
			s = s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q isn't a size", str(v))
	}
	return int64(f * float64(mult)), nil
}

// duration reads Compose durations: 1m30s, 10s, or a number of seconds.
func duration(v any) (time.Duration, error) {
	s := strings.TrimSpace(str(v))
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q isn't a duration", s)
	}
	return d, nil
}
