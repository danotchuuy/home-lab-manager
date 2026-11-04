package reach

import (
	"context"
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestAddresses(t *testing.T) {
	var c docker.ContainerJSON
	c.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: "192.168.11.14"}, "bridge": {IPAddress: "172.17.0.3"}}
	c.HostConfig.PortBindings = map[string][]docker.PortBinding{
		"2457/udp":  {{HostPort: "12457"}},
		"2457/tcp":  {{HostPort: "22457"}},
		"25575/tcp": {{HostIP: "127.0.0.1", HostPort: "25580"}},
	}
	hosts := []string{"127.0.0.1", "192.168.11.101"}
	got := Addresses(c, 2457, "udp", hosts)
	want := []string{"192.168.11.14:2457", "172.17.0.3:2457", "127.0.0.1:12457", "192.168.11.101:12457"}
	if !slices.Equal(got, want) {
		t.Errorf("udp = %q, want %q", got, want)
	}
	if got := Addresses(c, 25575, "tcp", hosts); got[len(got)-1] != "127.0.0.1:25580" || len(got) != 3 {
		t.Errorf("published on one address = %q", got)
	}
	var h docker.ContainerJSON
	h.HostConfig.NetworkMode = "host"
	if got := Addresses(h, 27015, "udp", hosts); !slices.Equal(got, []string{"127.0.0.1:27015", "192.168.11.101:27015"}) {
		t.Errorf("host networking = %q", got)
	}
	if got := Addresses(docker.ContainerJSON{}, 1, "tcp", hosts); len(got) != 0 {
		t.Errorf("no networks, nothing published = %q", got)
	}
}

type fakeDocker struct {
	info     docker.ContainerJSON
	networks map[string]docker.Network
}

func (f fakeDocker) InspectContainer(context.Context, string) (docker.ContainerJSON, error) {
	return f.info, nil
}
func (f fakeDocker) InspectNetwork(_ context.Context, id string) (docker.Network, error) {
	return f.networks[id], nil
}

func TestHostGateways(t *testing.T) {
	var self docker.ContainerJSON
	self.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: "192.168.11.5"}, "game-server-host_default": {IPAddress: "172.18.0.2"}}
	d := fakeDocker{info: self, networks: map[string]docker.Network{
		"br0": {Driver: "macvlan", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "192.168.11.0/24", Gateway: "192.168.11.1"}}}},
		"game-server-host_default": {Driver: "bridge", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{
			{Subnet: "172.18.0.0/16", Gateway: "172.18.0.1"}, {Subnet: "fd00::/64", Gateway: "fd00::1"},
		}}},
	}}
	got, err := HostGateways(context.Background(), d, "self")
	if err != nil || !slices.Equal(got, []string{"172.18.0.1"}) {
		t.Errorf("gateways = %q, %v (want only the bridge's IPv4 gateway)", got, err)
	}
}
