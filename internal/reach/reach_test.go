package reach

import (
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
