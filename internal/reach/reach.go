// Package reach works out where hlm can reach a container's port: straight
// at the container on its networks, or through a port published on the
// Docker host.
package reach

import (
	"net"
	"slices"
	"strconv"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Addresses lists host:port addresses for a container's port (proto is tcp
// or udp), best first. hostAddrs are where the Docker host's published
// ports can be reached from hlm.
func Addresses(info docker.ContainerJSON, port int, proto string, hostAddrs []string) []string {
	p := strconv.Itoa(port)
	var out []string
	add := func(host, port string) {
		if host == "" || port == "" {
			return
		}
		if a := net.JoinHostPort(host, port); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if info.HostConfig.NetworkMode == "host" {
		for _, h := range hostAddrs {
			add(h, p)
		}
		return out
	}
	// Straight to the container, on each of its networks.
	nets := make([]string, 0, len(info.NetworkSettings.Networks))
	for n := range info.NetworkSettings.Networks {
		nets = append(nets, n)
	}
	slices.Sort(nets)
	for _, n := range nets {
		add(info.NetworkSettings.Networks[n].IPAddress, p)
	}
	// Or through the port published on the host.
	for _, b := range info.HostConfig.PortBindings[p+"/"+proto] {
		if b.HostIP != "" && b.HostIP != "0.0.0.0" && b.HostIP != "::" {
			add(b.HostIP, b.HostPort) // published on one address only
			continue
		}
		for _, h := range hostAddrs {
			add(h, b.HostPort)
		}
	}
	return out
}
