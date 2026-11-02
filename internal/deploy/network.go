package deploy

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

type netKind int

const (
	netBridge netKind = iota // a bridge network: ports are published on the host
	netHost                  // the host's network stack
	netNone                  // no networking
	netDirect                // macvlan/ipvlan: the container has its own LAN address
)

// applyNetwork picks the network for the container and sets a static IP if
// one was requested. c.HostConfig.NetworkMode holds the template's network on
// entry. Unraid templates often ask for "br0", Unraid's macvlan network; it is
// used if a network with that name exists here.
func applyNetwork(c *docker.CreateRequest, req Request, env Env, warnings *[]string) (netKind, error) {
	name, fromTemplate := strings.TrimSpace(req.Network), false
	if name == "" {
		name, fromTemplate = c.HostConfig.NetworkMode, true
	}

	var kind netKind
	var nw *docker.Network
	switch strings.ToLower(name) {
	case "", "bridge", "default":
		name, kind = "bridge", netBridge
	case "host":
		name, kind = "host", netHost
	case "none":
		name, kind = "none", netNone
	default:
		found, ok := env.Networks[name]
		switch {
		case ok:
			nw = &found
			kind = netBridge
			if found.Driver == "macvlan" || found.Driver == "ipvlan" {
				kind = netDirect
			}
		case fromTemplate:
			*warnings = append(*warnings, fmt.Sprintf("template network %q does not exist on this Docker host; using bridge", name))
			name, kind = "bridge", netBridge
		default:
			return 0, fmt.Errorf("network %q does not exist on this Docker host", name)
		}
	}
	c.HostConfig.NetworkMode = name

	switch kind {
	case netHost, netNone:
		c.HostConfig.PortBindings = nil
	case netDirect:
		if len(c.HostConfig.PortBindings) > 0 {
			*warnings = append(*warnings, fmt.Sprintf("ports are not published on %s network %q; the server is reached at its own IP address on the container ports", nw.Driver, name))
		}
		c.HostConfig.PortBindings = nil
		// On the LAN the container is a device of its own. Give it the same
		// MAC every time it's recreated, so routers see one device (keeping
		// its name, history and any DHCP reservation) rather than a new one
		// after each update.
		c.NetworkingConfig = &docker.NetworkingConfig{
			EndpointsConfig: map[string]docker.EndpointSettings{name: {MacAddress: StableMAC(req.Name)}},
		}
	}

	ipText := strings.TrimSpace(req.IPAddress)
	if ipText == "" {
		return kind, nil
	}
	if nw == nil {
		return 0, fmt.Errorf("a static IP address needs a user-defined network; Docker does not allow one on the %q network", name)
	}
	ip, err := netip.ParseAddr(ipText)
	if err != nil {
		return 0, fmt.Errorf("invalid IP address %q", ipText)
	}
	ip = ip.Unmap()
	if err := checkIP(ip, *nw, req.Name); err != nil {
		return 0, err
	}

	ipam := &docker.EndpointIPAMConfig{}
	if ip.Is4() {
		ipam.IPv4Address = ip.String()
	} else {
		ipam.IPv6Address = ip.String()
	}
	ep := docker.EndpointSettings{IPAMConfig: ipam}
	if kind == netDirect {
		ep.MacAddress = StableMAC(req.Name)
	}
	c.NetworkingConfig = &docker.NetworkingConfig{
		EndpointsConfig: map[string]docker.EndpointSettings{name: ep},
	}
	return kind, nil
}

// StableMAC returns the MAC address a server gets on LAN networks: derived
// from its name, so it stays the same across recreates. The first byte marks
// it as a locally administered unicast address, so it can't clash with a
// real device's vendor-assigned MAC. The seed keeps the old short name, gsh,
// so servers keep their MACs (and DHCP leases) across the rename.
func StableMAC(server string) string {
	sum := sha256.Sum256([]byte("gsh-mac:" + server))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// checkIP makes sure ip can be given to container on nw: it must be inside one
// of the network's subnets and not already taken.
func checkIP(ip netip.Addr, nw docker.Network, container string) error {
	var subnets []string
	inSubnet := false
	for _, cfg := range nw.IPAM.Config {
		prefix, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil {
			continue
		}
		subnets = append(subnets, prefix.String())
		if !prefix.Contains(ip) {
			continue
		}
		inSubnet = true
		if ip == prefix.Masked().Addr() {
			return fmt.Errorf("%s is the network address of %s", ip, prefix)
		}
		if gw, err := netip.ParseAddr(cfg.Gateway); err == nil && gw == ip {
			return fmt.Errorf("%s is the gateway of network %q", ip, nw.Name)
		}
	}
	if len(subnets) == 0 {
		return fmt.Errorf("network %q has no configured subnet, so Docker can't assign it a static IP; recreate it with --subnet", nw.Name)
	}
	if !inSubnet {
		return fmt.Errorf("%s is not in network %q (subnets: %s)", ip, nw.Name, strings.Join(subnets, ", "))
	}

	for _, ep := range nw.Containers {
		if ep.Name == container {
			continue // the server being reconfigured keeps its own address
		}
		for _, addr := range []string{ep.IPv4Address, ep.IPv6Address} {
			if p, err := netip.ParsePrefix(addr); err == nil && p.Addr() == ip {
				return fmt.Errorf("%s is already used by container %q", ip, ep.Name)
			}
		}
	}
	return nil
}
