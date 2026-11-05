// Package reach works out where hlm can reach a container's port: straight
// at the container on its networks, or through a port published on the
// Docker host.
package reach

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// labelDirectIP marks containers with their own address on the LAN
// (deploy.LabelDirectIP).
const labelDirectIP = "io.hlm.directip"

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
	// Straight to the container, on each of its networks. A container on
	// another Docker host (its ID ends in @host) is only reachable that way
	// with its own address on the LAN.
	otherHost := strings.Contains(info.ID, "@")
	nets := make([]string, 0, len(info.NetworkSettings.Networks))
	for n := range info.NetworkSettings.Networks {
		nets = append(nets, n)
	}
	slices.Sort(nets)
	for _, n := range nets {
		if otherHost && info.Config.Labels[labelDirectIP] != "true" {
			break
		}
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

// Docker is what HostGateways needs from the Docker client.
type Docker interface {
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	InspectNetwork(ctx context.Context, id string) (docker.Network, error)
}

// HostGateways returns the gateways of a container's bridge networks: the
// addresses at which it reaches the Docker host, and so the ports the host
// publishes. For hlm in a container, these work where the host's LAN
// address may not (from a macvlan network, the host can't be reached). The
// gateways of other networks, such as a macvlan network's router, are left
// out.
func HostGateways(ctx context.Context, d Docker, id string) ([]string, error) {
	info, err := d.InspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(info.NetworkSettings.Networks))
	for n := range info.NetworkSettings.Networks {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []string
	for _, n := range names {
		nw, err := d.InspectNetwork(ctx, n)
		if err != nil {
			return nil, err
		}
		if nw.Driver != "bridge" {
			continue
		}
		for _, c := range nw.IPAM.Config {
			if ip := net.ParseIP(c.Gateway); ip != nil && ip.To4() != nil && !slices.Contains(out, c.Gateway) {
				out = append(out, c.Gateway)
			}
		}
	}
	return out, nil
}
