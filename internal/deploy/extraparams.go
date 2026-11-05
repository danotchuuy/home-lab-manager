package deploy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// applyExtraParams applies a template's ExtraParams, which are raw `docker
// run` flags, to c. Flags that are supported are applied; unsupported flags
// are skipped with a warning.
func applyExtraParams(c *docker.CreateRequest, params string, warnings *[]string) error {
	args, err := splitArgs(params)
	if err != nil {
		return err
	}
	hc := &c.HostConfig

	for i := 0; i < len(args); i++ {
		flag, value, hasValue := strings.Cut(args[i], "=")
		if !strings.HasPrefix(flag, "-") {
			*warnings = append(*warnings, fmt.Sprintf("ignored ExtraParams argument %q", args[i]))
			continue
		}
		// next returns the flag's value, from "--flag=value" or "--flag value".
		next := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", flag)
			}
			i++
			return args[i], nil
		}
		boolFlag := func() bool {
			return !hasValue || value == "true" || value == "1"
		}

		var v string
		switch flag {
		// Flags without a value.
		case "--init":
			b := boolFlag()
			hc.Init = &b
			continue
		case "--read-only":
			hc.ReadonlyRootfs = boolFlag()
			continue
		case "-t", "--tty":
			c.Tty = boolFlag()
			continue
		case "-i", "--interactive":
			c.OpenStdin = boolFlag()
			continue
		case "-it", "-ti":
			c.Tty, c.OpenStdin = true, true
			continue
		case "--privileged":
			hc.Privileged = boolFlag()
			continue
		case "--no-healthcheck":
			if boolFlag() {
				health(c).Test = []string{"NONE"}
			}
			continue
		case "--rm", "-d", "--detach":
			continue
		}

		if v, err = next(); err != nil {
			return err
		}
		switch flag {
		case "--health-cmd":
			health(c).Test = []string{"CMD-SHELL", v}
		case "--health-interval", "--health-timeout", "--health-start-period":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return fmt.Errorf("%s: invalid duration %q", flag, v)
			}
			h := health(c)
			switch flag {
			case "--health-interval":
				h.Interval = int64(d)
			case "--health-timeout":
				h.Timeout = int64(d)
			default:
				h.StartPeriod = int64(d)
			}
		case "--health-retries":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("--health-retries: invalid number %q", v)
			}
			health(c).Retries = n
		case "--restart":
			hc.RestartPolicy.Name = strings.SplitN(v, ":", 2)[0]
		case "--stop-timeout":
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("--stop-timeout: %w", err)
			}
			c.StopTimeout = &n
		case "-m", "--memory":
			n, err := parseBytes(v)
			if err != nil {
				return fmt.Errorf("--memory: %w", err)
			}
			hc.Memory = n
		case "--shm-size":
			n, err := parseBytes(v)
			if err != nil {
				return fmt.Errorf("--shm-size: %w", err)
			}
			hc.ShmSize = n
		case "--cpus":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("--cpus: %w", err)
			}
			hc.NanoCPUs = int64(f * 1e9)
		case "--pids-limit":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("--pids-limit: %w", err)
			}
			hc.PidsLimit = &n
		case "--ulimit":
			u, err := parseUlimit(v)
			if err != nil {
				return err
			}
			hc.Ulimits = append(hc.Ulimits, u)
		case "--cap-add":
			hc.CapAdd = append(hc.CapAdd, v)
		case "--cap-drop":
			hc.CapDrop = append(hc.CapDrop, v)
		case "--security-opt":
			hc.SecurityOpt = append(hc.SecurityOpt, v)
		case "--sysctl":
			k, val, _ := strings.Cut(v, "=")
			if hc.Sysctls == nil {
				hc.Sysctls = map[string]string{}
			}
			hc.Sysctls[k] = val
		case "--tmpfs":
			path, opts, _ := strings.Cut(v, ":")
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs[path] = opts
		case "--add-host":
			hc.ExtraHosts = append(hc.ExtraHosts, v)
		case "--dns":
			hc.DNS = append(hc.DNS, v)
		case "--group-add":
			hc.GroupAdd = append(hc.GroupAdd, v)
		case "-u", "--user":
			c.User = v
		case "-h", "--hostname":
			c.Hostname = v
		case "-e", "--env":
			c.Env = append(c.Env, v)
		case "-l", "--label":
			k, val, _ := strings.Cut(v, "=")
			c.Labels[k] = val
		case "-v", "--volume":
			hc.Binds = append(hc.Binds, v)
		case "--device":
			parts := strings.Split(v, ":")
			d := docker.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
			if len(parts) > 1 {
				d.PathInContainer = parts[1]
			}
			if len(parts) > 2 {
				d.CgroupPermissions = parts[2]
			}
			hc.Devices = append(hc.Devices, d)
		case "--gpus":
			hc.DeviceRequests = append(hc.DeviceRequests, gpuRequest(v))
		case "--runtime":
			hc.Runtime = v
		case "--entrypoint":
			c.Entrypoint = []string{v}
		case "--log-driver":
			if hc.LogConfig == nil {
				hc.LogConfig = &docker.LogConfig{}
			}
			hc.LogConfig.Type = v
		case "--log-opt":
			if hc.LogConfig == nil {
				hc.LogConfig = &docker.LogConfig{}
			}
			if hc.LogConfig.Config == nil {
				hc.LogConfig.Config = map[string]string{}
			}
			k, val, _ := strings.Cut(v, "=")
			hc.LogConfig.Config[k] = val
		case "-p", "--publish":
			if err := addPublish(c, v, warnings); err != nil {
				return err
			}
		case "--net", "--network":
			// Resolved later by applyNetwork, which falls back to bridge if
			// the network doesn't exist.
			hc.NetworkMode = v
		case "--ip", "--ip6":
			*warnings = append(*warnings, fmt.Sprintf("ignored ExtraParams %s %s; set the server's IP address instead", flag, v))
		default:
			*warnings = append(*warnings, fmt.Sprintf("ignored unsupported ExtraParams flag %s", flag))
		}
	}
	return nil
}

// health returns c's health check, adding one.
func health(c *docker.CreateRequest) *docker.Healthcheck {
	if c.Healthcheck == nil {
		c.Healthcheck = &docker.Healthcheck{}
	}
	return c.Healthcheck
}

// addPublish handles -p [hostIP:]hostPort:containerPort[/proto], where the
// ports may be ranges.
func addPublish(c *docker.CreateRequest, v string, warnings *[]string) error {
	proto := "tcp"
	if p, pr, ok := strings.Cut(v, "/"); ok {
		v, proto = p, pr
	}
	i := strings.LastIndex(v, ":")
	if i < 0 {
		// Only a container port: Docker would pick a random host port, which
		// isn't useful for a game server. Just expose it.
		r, err := parsePortRange(v)
		if err != nil {
			return fmt.Errorf("invalid -p value %q", v)
		}
		if c.ExposedPorts == nil {
			c.ExposedPorts = map[string]struct{}{}
		}
		for p := r.start; p <= r.end; p++ {
			c.ExposedPorts[strconv.Itoa(p)+"/"+proto] = struct{}{}
		}
		return nil
	}
	if err := publish(c, v[i+1:], proto, v[:i], nil, warnings); err != nil {
		return fmt.Errorf("-p %s: %w", v, err)
	}
	return nil
}

func gpuRequest(v string) docker.DeviceRequest {
	r := docker.DeviceRequest{Capabilities: [][]string{{"gpu"}}}
	v = strings.Trim(v, `"'`)
	switch {
	case v == "all":
		r.Count = -1
	case strings.HasPrefix(v, "device="):
		r.DeviceIDs = strings.Split(strings.TrimPrefix(v, "device="), ",")
	default:
		if n, err := strconv.Atoi(v); err == nil {
			r.Count = n
		} else {
			r.Count = -1
		}
	}
	return r
}

func parseUlimit(v string) (docker.Ulimit, error) {
	name, limits, ok := strings.Cut(v, "=")
	if !ok {
		return docker.Ulimit{}, fmt.Errorf("invalid --ulimit %q", v)
	}
	softS, hardS, hasHard := strings.Cut(limits, ":")
	soft, err := strconv.ParseInt(softS, 10, 64)
	if err != nil {
		return docker.Ulimit{}, fmt.Errorf("invalid --ulimit %q", v)
	}
	hard := soft
	if hasHard {
		if hard, err = strconv.ParseInt(hardS, 10, 64); err != nil {
			return docker.Ulimit{}, fmt.Errorf("invalid --ulimit %q", v)
		}
	}
	return docker.Ulimit{Name: name, Soft: soft, Hard: hard}, nil
}

// parseBytes parses sizes like "512m" or "8g".
func parseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "b")
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
	if err != nil {
		return 0, err
	}
	return int64(f * float64(mult)), nil
}

// splitArgs splits a command line into words the way a POSIX shell would,
// handling single quotes, double quotes and backslash escapes.
func splitArgs(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
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
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}
