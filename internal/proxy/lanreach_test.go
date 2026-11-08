package proxy

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
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
	// Stopped with a fixed address, which Docker keeps.
	fixed := onLAN()
	fixed.NetworkSettings.Networks["br0"] = docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "192.168.1.60"}}
	if addr, _, err := wg.lanReach(fixed, "nas"); err != nil || addr.String() != "192.168.1.60" {
		t.Errorf("stopped, fixed: %v %v", addr, err)
	}
	// No end reaches a LAN at all.
	plain := WireGuard{Network: "10.66.0.0/24"}
	if _, _, err := plain.lanReach(onLAN("192.168.1.60"), localHost); err == nil || !strings.Contains(err.Error(), "Reach servers with their own LAN address") {
		t.Errorf("no LAN reach: %v", err)
	}
}

// A server with its own LAN address that's asleep (stopped, its wake
// listener at its address) keeps its relay: by its fixed address, or where
// it was last relayed to when Docker chose its address.
func TestRelayWhileAsleep(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	lan := func(running bool, ep docker.EndpointSettings) docker.ContainerJSON {
		info := server(nil)
		info.State.Running = running
		info.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
		port := 15637
		if ep.IPAMConfig != nil {
			port = 15640
		}
		info.Config.Env = []string{"PORT=" + strconv.Itoa(port)}
		info.HostConfig.NetworkMode = "br0"
		info.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": ep}
		return info
	}
	servers := fakeServers{
		"fixed":  lan(false, docker.EndpointSettings{IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "192.168.1.70"}}),
		"chosen": lan(true, docker.EndpointSettings{IPAddress: "192.168.1.71"}),
	}
	s := New(store, servers, slog.New(slog.DiscardHandler))
	s.UsePorts(func(info docker.ContainerJSON) ([]forwarding.Mapping, error) {
		port, _ := strconv.Atoi(strings.TrimPrefix(info.Config.Env[0], "PORT="))
		return []forwarding.Mapping{{Port: port, Local: port, Proto: "udp", Proxy: true, Firewall: true}}, nil
	})
	wg := testTunnel(t)
	wg.LANNetwork, wg.LANSubnet = "br0", "192.168.1.0/24"
	store.Set(secretWireGuard, wg)
	st := Settings{Target: remote.Target{Host: "vps", User: "root"}, UpstreamSource: "wireguard", Servers: []string{"chosen", "fixed"}}
	ctx := context.Background()
	got := func() map[string]Proxied {
		m := map[string]Proxied{}
		for _, p := range s.relayed(ctx, st) {
			m[p.Server] = p
		}
		return m
	}
	r := got()
	if r["fixed"].Address != "192.168.1.70" || len(r["fixed"].Ports) != 1 || r["fixed"].Error != "" {
		t.Errorf("asleep, fixed address: %+v", r["fixed"])
	}
	if r["chosen"].Address != "192.168.1.71" {
		t.Fatalf("running, chosen address: %+v", r["chosen"])
	}
	s.recordApplied("10.77.77.2", s.relayed(ctx, st))
	// It goes to sleep: Docker forgets the address it chose.
	servers["chosen"] = lan(false, docker.EndpointSettings{})
	if r := got(); r["chosen"].Address != "192.168.1.71" || len(r["chosen"].Ports) != 1 || r["chosen"].Error != "" {
		t.Errorf("asleep, chosen address: %+v", r["chosen"])
	}
	// Never relayed before, and stopped: it says so, as before.
	store.Delete(secretRelayAddresses)
	if r := got(); r["chosen"].Error == "" {
		t.Errorf("unknown address: %+v", r["chosen"])
	}
}
