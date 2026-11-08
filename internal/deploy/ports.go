package deploy

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// largeRange is the size above which a published port range gets a warning:
// Docker sets up forwarding for every port separately, which is slow and
// resource-hungry for big ranges.
const largeRange = 1000

// portRange is an inclusive range of port numbers. A single port has
// start == end.
type portRange struct {
	start, end int
}

func (r portRange) size() int { return r.end - r.start + 1 }

func (r portRange) String() string {
	if r.start == r.end {
		return strconv.Itoa(r.start)
	}
	return fmt.Sprintf("%d-%d", r.start, r.end)
}

// parsePortRange parses "27015", "27015-27020" or, as some templates write
// it, "27015:27020".
func parsePortRange(s string) (portRange, error) {
	s = strings.TrimSpace(s)
	lo, hi, isRange := strings.Cut(s, "-")
	if !isRange {
		lo, hi, isRange = strings.Cut(s, ":")
	}
	start, err := parsePort(lo)
	if err != nil {
		return portRange{}, err
	}
	if !isRange {
		return portRange{start, start}, nil
	}
	end, err := parsePort(hi)
	if err != nil {
		return portRange{}, err
	}
	if end < start {
		return portRange{}, fmt.Errorf("range %q ends before it starts", s)
	}
	return portRange{start, end}, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	return n, nil
}

// parseHostPorts parses the host side of a port mapping: a port or range,
// optionally preceded by the host address to bind, as in "0.0.0.0:27015" or
// "[::1]:27015-27020".
func parseHostPorts(s string) (hostIP string, r portRange, err error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		// "27015:27020" is a range, not an address, so only split when the
		// part before the colon really is an IP.
		if ip, perr := netip.ParseAddr(strings.Trim(s[:i], "[]")); perr == nil {
			hostIP, s = ip.String(), s[i+1:]
		}
	}
	r, err = parsePortRange(s)
	return hostIP, r, err
}

// publish adds the bindings for one template port entry: container ports
// target (a port or range) published on the host ports in value. A single
// host port with a container range is taken as the start of an equally long
// host range.
func publish(c *docker.CreateRequest, target, proto, value string, hostPorts map[string]int, warnings *[]string) error {
	container, err := parsePortRange(target)
	if err != nil {
		return fmt.Errorf("invalid container port %q: %w", target, err)
	}
	hostIP, host, err := parseHostPorts(value)
	if err != nil {
		return fmt.Errorf("invalid host port %q: use a port such as 27015, a range such as 27015-27020, or ip:port", value)
	}
	if host.size() == 1 && container.size() > 1 {
		host.end = host.start + container.size() - 1
		if host.end > 65535 {
			return fmt.Errorf("host range starting at %d would end past port 65535", host.start)
		}
	}
	if host.size() != container.size() {
		return fmt.Errorf("host ports %s (%d ports) don't match container ports %s (%d ports)",
			host, host.size(), container, container.size())
	}
	if container.size() > largeRange {
		*warnings = append(*warnings, fmt.Sprintf(
			"publishing %d ports (%s/%s) is slow and uses a lot of resources; consider host networking", container.size(), container, proto))
	}

	if c.ExposedPorts == nil {
		c.ExposedPorts = map[string]struct{}{}
	}
	if c.HostConfig.PortBindings == nil {
		c.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	}
	for i := 0; i < container.size(); i++ {
		cp, hp := container.start+i, host.start+i
		key := strconv.Itoa(cp) + "/" + proto
		c.ExposedPorts[key] = struct{}{}
		c.HostConfig.PortBindings[key] = append(c.HostConfig.PortBindings[key],
			docker.PortBinding{HostIP: hostIP, HostPort: strconv.Itoa(hp)})
		if hostPorts != nil {
			hostPorts[strconv.Itoa(cp)] = hp
		}
	}
	return nil
}

// unraidPortVar matches the variables Unraid passes ports as to containers
// on host, macvlan and ipvlan networks, where it doesn't publish them:
// TCP_PORT_<container port>=<port> and UDP_PORT_<…> (dockerMan's
// xmlToCommand).
var unraidPortVar = regexp.MustCompile(`^(TCP|UDP)_PORT_([0-9]{1,5})=`)

// UnraidPorts are the ports a container's environment passes the way
// Unraid does for containers on host, macvlan and ipvlan networks, keyed
// like Docker's exposed ports ("15637/udp").
func UnraidPorts(env []string) []string {
	var out []string
	for _, kv := range env {
		m := unraidPortVar.FindStringSubmatch(kv)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[2]); err == nil && n > 0 && n < 65536 {
			out = append(out, strconv.Itoa(n)+"/"+strings.ToLower(m[1]))
		}
	}
	return out
}

// DeclaredPorts are the ports a container says it listens on: those it
// exposes, and those Unraid passes it as variables. Containers on the
// host's network or their own LAN address are reached on these.
func DeclaredPorts(exposed map[string]struct{}, env []string) map[string]struct{} {
	out := make(map[string]struct{}, len(exposed))
	for k := range exposed {
		out[k] = struct{}{}
	}
	for _, k := range UnraidPorts(env) {
		out[k] = struct{}{}
	}
	return out
}
