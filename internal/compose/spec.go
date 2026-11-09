package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Labels on a stack's containers and networks.
const (
	LabelStack   = "io.hlm.stack"
	LabelService = "io.hlm.stack.service"
	// LabelHash identifies a service's settings, so an edit only recreates
	// the services it changes.
	LabelHash = "io.hlm.stack.hash"
	// Labels shared with single servers (deploy.LabelManaged, LabelApp).
	labelManaged = "io.hlm.managed"
	labelApp     = "io.hlm.app"
)

// ContainerSpec is what creating a service's container takes.
type ContainerSpec struct {
	Name   string
	Create docker.CreateRequest
	// Connect are the networks to join after creating it (with their
	// aliases); the first network is in Create already.
	Connect []Attach
	Hash    string
}

// Attach is a network a container joins.
type Attach struct {
	Network string
	Aliases []string
}

// Spec works out a service's container. Relative bind mounts go under
// appdata/<project>.
func (p *Project) Spec(s *Service, appdata string) (ContainerSpec, error) {
	c := docker.CreateRequest{
		Image: s.Image, Cmd: s.Command, Entrypoint: s.Entrypoint, User: s.User, Hostname: s.Hostname,
		WorkingDir: s.WorkingDir, Tty: s.Tty, OpenStdin: s.StdinOpen, StopTimeout: s.StopGrace, StopSignal: s.StopSignal,
		Labels: map[string]string{},
	}
	for k, v := range s.Labels {
		if strings.HasPrefix(k, "io.hlm.") {
			return ContainerSpec{}, fmt.Errorf("service %s: io.hlm.* labels are hlm's own", s.Name)
		}
		c.Labels[k] = v
	}
	c.Labels[labelManaged] = "true"
	c.Labels[LabelStack] = p.Name
	c.Labels[LabelService] = s.Name
	c.Labels[labelApp] = s.Name
	for _, k := range sortedKeys(s.Environment) {
		c.Env = append(c.Env, k+"="+s.Environment[k])
	}
	hc := &c.HostConfig
	hc.Privileged, hc.Init, hc.ReadonlyRootfs = s.Privileged, s.Init, s.ReadOnly
	hc.CapAdd, hc.CapDrop, hc.ExtraHosts, hc.DNS, hc.SecurityOpt = s.CapAdd, s.CapDrop, s.ExtraHosts, s.DNS, s.SecurityOpt
	hc.Memory, hc.NanoCPUs, hc.ShmSize, hc.PidsLimit = s.MemLimit, int64(s.CPUs*1e9), s.ShmSize, s.PidsLimit
	if len(s.Sysctls) > 0 {
		hc.Sysctls = s.Sysctls
	}
	for _, u := range s.Ulimits {
		hc.Ulimits = append(hc.Ulimits, docker.Ulimit{Name: u.Name, Soft: u.Soft, Hard: u.Hard})
	}
	for _, d := range s.Devices {
		parts := strings.Split(d, ":")
		m := docker.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
		if len(parts) > 1 {
			m.PathInContainer = parts[1]
		}
		if len(parts) > 2 {
			m.CgroupPermissions = parts[2]
		}
		hc.Devices = append(hc.Devices, m)
	}
	restart := s.Restart
	if restart == "" {
		restart = "no"
	}
	name, retries, _ := strings.Cut(restart, ":")
	switch name {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return ContainerSpec{}, fmt.Errorf("service %s: restart %q isn't known", s.Name, restart)
	}
	hc.RestartPolicy.Name = name
	if retries != "" {
		n, err := strconv.Atoi(retries)
		if err != nil {
			return ContainerSpec{}, fmt.Errorf("service %s: restart %q", s.Name, restart)
		}
		hc.RestartPolicy.MaximumRetryCount = n
	}
	if h := s.Healthcheck; h != nil {
		if h.Disable {
			c.Healthcheck = &docker.Healthcheck{Test: []string{"NONE"}}
		} else {
			c.Healthcheck = &docker.Healthcheck{Test: h.Test, Interval: int64(h.Interval), Timeout: int64(h.Timeout),
				StartPeriod: int64(h.StartPeriod), Retries: h.Retries}
		}
	}

	// Ports.
	for _, pt := range s.Ports {
		if err := publish(&c, pt); err != nil {
			return ContainerSpec{}, fmt.Errorf("service %s: port %s: %w", s.Name, pt.Target, err)
		}
	}

	// Mounts.
	for _, m := range s.Mounts {
		opt := ""
		if m.ReadOnly {
			opt = ":ro"
		}
		switch m.Type {
		case "tmpfs":
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs[m.Target] = ""
		case "bind":
			src, err := p.hostPath(m.Source, appdata)
			if err != nil {
				return ContainerSpec{}, fmt.Errorf("service %s: %w", s.Name, err)
			}
			hc.Binds = append(hc.Binds, src+":"+m.Target+opt)
		case "volume":
			if m.Source == "" {
				if c.Volumes == nil {
					c.Volumes = map[string]struct{}{}
				}
				c.Volumes[m.Target] = struct{}{}
				continue
			}
			hc.Binds = append(hc.Binds, p.VolumeName(m.Source)+":"+m.Target+opt)
		}
	}
	for _, t := range s.Tmpfs {
		dir, opts, _ := strings.Cut(t, ":")
		if hc.Tmpfs == nil {
			hc.Tmpfs = map[string]string{}
		}
		hc.Tmpfs[dir] = opts
	}

	// Networks.
	spec := ContainerSpec{Name: p.ContainerName(s)}
	switch {
	case strings.HasPrefix(s.NetworkMode, "service:"):
		other := p.Service(strings.TrimPrefix(s.NetworkMode, "service:"))
		hc.NetworkMode = "container:" + p.ContainerName(other)
		c.ExposedPorts, hc.PortBindings = nil, nil
	case s.NetworkMode != "":
		hc.NetworkMode = s.NetworkMode
	default:
		for i, n := range s.Networks {
			a := Attach{Network: p.NetworkName(n), Aliases: append([]string{s.Name}, s.Aliases[n]...)}
			if i == 0 {
				hc.NetworkMode = a.Network
				c.NetworkingConfig = &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{
					a.Network: {Aliases: a.Aliases},
				}}
			} else {
				spec.Connect = append(spec.Connect, a)
			}
		}
	}

	b, err := json.Marshal(struct {
		C docker.CreateRequest
		N []Attach
	}{c, spec.Connect})
	if err != nil {
		return ContainerSpec{}, err
	}
	sum := sha256.Sum256(b)
	spec.Hash = hex.EncodeToString(sum[:8])
	c.Labels[LabelHash] = spec.Hash
	spec.Create = c
	return spec, nil
}

// hostPath resolves a bind mount's source: relative paths are inside the
// stack's folder under appdata, or relative to Dir.
func (p *Project) hostPath(src, appdata string) (string, error) {
	if strings.HasPrefix(src, "~") {
		return "", fmt.Errorf("%s: use a path relative to the stack, or an absolute one, not ~", src)
	}
	if strings.HasPrefix(src, "/") {
		return path.Clean(src), nil
	}
	if p.Dir != "" {
		// As Compose does: relative to the file, wherever that leads.
		return path.Join(p.Dir, src), nil
	}
	if appdata == "" {
		return "", errors.New("relative paths need HLM_APPDATA_ROOT")
	}
	base := path.Join(appdata, p.Name)
	full := path.Join(base, src)
	if full != base && !strings.HasPrefix(full, base+"/") {
		return "", fmt.Errorf("%s leaves the stack's folder", src)
	}
	return full, nil
}

// publish adds a port's bindings: equal-length ranges, one to one.
func publish(c *docker.CreateRequest, pt Port) error {
	t0, t1, err := portRange(pt.Target)
	if err != nil {
		return err
	}
	if c.ExposedPorts == nil {
		c.ExposedPorts = map[string]struct{}{}
	}
	var h0, h1 int
	if pt.Published != "" {
		if h0, h1, err = portRange(pt.Published); err != nil {
			return err
		}
		if h1 == h0 && t1 > t0 {
			h1 = h0 + (t1 - t0)
		}
		if h1-h0 != t1-t0 {
			return errors.New("the host and container ranges differ in length")
		}
	}
	for i := 0; i <= t1-t0; i++ {
		key := strconv.Itoa(t0+i) + "/" + pt.Protocol
		c.ExposedPorts[key] = struct{}{}
		if pt.Published == "" {
			continue
		}
		if c.HostConfig.PortBindings == nil {
			c.HostConfig.PortBindings = map[string][]docker.PortBinding{}
		}
		c.HostConfig.PortBindings[key] = append(c.HostConfig.PortBindings[key],
			docker.PortBinding{HostIP: pt.HostIP, HostPort: strconv.Itoa(h0 + i)})
	}
	return nil
}

func portRange(s string) (int, int, error) {
	a, b, isRange := strings.Cut(s, "-")
	lo, err := strconv.Atoi(a)
	hi := lo
	if err == nil && isRange {
		hi, err = strconv.Atoi(b)
	}
	if err != nil || lo < 1 || hi > 65535 || hi < lo {
		return 0, 0, fmt.Errorf("%q isn't a port or range", s)
	}
	return lo, hi, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
