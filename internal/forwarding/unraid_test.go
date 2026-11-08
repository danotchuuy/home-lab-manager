package forwarding

import (
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// A container Unraid made on br0 declares no ports; Unraid passes them as
// variables, which are its ports to forward.
func TestUnraidPortVariables(t *testing.T) {
	var info docker.ContainerJSON
	info.HostConfig.NetworkMode = "br0"
	info.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	info.Config.Env = []string{"UDP_PORT_15637=15637", "TZ=UTC"}
	got := localPorts(info)
	if len(got) != 1 || got[0] != (Port{15637, "udp"}) {
		t.Errorf("ports = %v", got)
	}
	// On a bridge network, only published ports count.
	info.HostConfig.NetworkMode = "bridge"
	info.Config.Labels = nil
	if got := localPorts(info); len(got) != 0 {
		t.Errorf("bridge ports = %v", got)
	}
}
