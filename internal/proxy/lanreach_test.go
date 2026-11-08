package proxy

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func onLAN(ips ...string) docker.ContainerJSON {
	var info docker.ContainerJSON
	info.NetworkSettings.Networks = map[string]docker.EndpointSettings{}
	for i, ip := range ips {
		info.NetworkSettings.Networks["net"+string(rune('a'+i))] = docker.EndpointSettings{IPAddress: ip}
	}
	return info
}

// A server with its own LAN address is reached by whichever end is on its
// LAN, not only its own host's: with two hosts on one LAN, only one end can
// reach it.
func TestLANReach(t *testing.T) {
	wg := WireGuard{Network: "10.66.0.0/24", LANNetwork: "br0", LANSubnet: "192.168.1.0/24",
		Hosts: map[string]WireGuardEnd{"nas": {Address: "10.66.0.3"}, "pi": {Address: "10.66.0.4", LANNetwork: "br0", LANSubnet: "10.0.5.0/24"}}}

	// On the NAS, whose end can't reach its LAN: this host's end does.
	addr, via, err := wg.lanReach(onLAN("192.168.1.60"), "nas")
	if err != nil || addr.String() != "192.168.1.60" || via != localHost {
		t.Errorf("nas server: %v %q %v", addr, via, err)
	}
	// Its own host's end first.
	addr, via, err = wg.lanReach(onLAN("10.0.5.20"), "pi")
	if err != nil || addr.String() != "10.0.5.20" || via != "pi" {
		t.Errorf("pi server: %v %q %v", addr, via, err)
	}
	// Any end on its LAN will do, whichever host runs it.
	if _, via, err := wg.lanReach(onLAN("10.0.5.21"), "nas"); err != nil || via != "pi" {
		t.Errorf("nas server on the pi's LAN: %q %v", via, err)
	}
	// On no end's LAN: the error says what each end reaches.
	_, _, err = wg.lanReach(onLAN("172.20.0.9"), "nas")
	if err == nil || !strings.Contains(err.Error(), "172.20.0.9") || !strings.Contains(err.Error(), "this host's end reaches br0 (192.168.1.0/24)") ||
		!strings.Contains(err.Error(), "the end on pi reaches br0 (10.0.5.0/24)") {
		t.Errorf("unreached: %v", err)
	}
	// Not running: no address.
	if _, _, err := wg.lanReach(onLAN(), "nas"); err == nil || !strings.Contains(err.Error(), "is it running") {
		t.Errorf("stopped: %v", err)
	}
	// No end reaches a LAN at all.
	plain := WireGuard{Network: "10.66.0.0/24"}
	if _, _, err := plain.lanReach(onLAN("192.168.1.60"), localHost); err == nil || !strings.Contains(err.Error(), "Reach servers with their own LAN address") {
		t.Errorf("no LAN reach: %v", err)
	}
}
