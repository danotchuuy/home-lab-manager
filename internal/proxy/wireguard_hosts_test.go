package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// The tunnel's ends on other Docker hosts: each a peer on the proxy, with
// its own address and key, its container on its host, and nginx relaying
// that host's servers to it.
func TestWireGuardEndsOnOtherHosts(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	here := server(map[string]string{"27015/udp": "27015"})
	here.ID = "aaa"
	mc := server(map[string]string{"25565/tcp": "25565"})
	mc.ID = "bbb@nas"
	pal := server(map[string]string{"8211/udp": "8211"})
	pal.ID = "ccc@pi"
	s := New(store, fakeServers{"valheim": here, "mc@nas": mc, "pal@pi": pal}, slog.New(slog.DiscardHandler))
	local, nas := &fakeDocker{}, &fakeDocker{}
	s.UseDocker(local, "")
	s.UseDockerHosts(func(host string) (Containers, error) {
		if host == "nas" {
			return nas, nil
		}
		return nil, errors.New("there's no Docker host " + host)
	})
	var proxyConf, handshakes string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		switch {
		case strings.Contains(script, "wg-quick up"):
			proxyConf = string(stdin)
			return remote.Result{Stdout: "hlm:wgup\n"}, nil
		case strings.Contains(script, "latest-handshakes"):
			return remote.Result{Stdout: handshakes}, nil
		}
		return remote.Result{}, nil
	}
	ctx := context.Background()
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"valheim", "mc@nas", "pal@pi"}})

	if _, err := s.SetupHostWireGuard(ctx, "nas", HostWireGuardInput{}, nil); err == nil || !strings.Contains(err.Error(), "first") {
		t.Errorf("before the tunnel: %v", err)
	}
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{UseForRelays: true}, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"local", "nope"} {
		if _, err := s.SetupHostWireGuard(ctx, bad, HostWireGuardInput{}, nil); err == nil {
			t.Errorf("%s: set up", bad)
		}
	}
	res, err := s.SetupHostWireGuard(ctx, "nas", HostWireGuardInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wg, _, _ := s.tunnel()
	e := wg.Hosts["nas"]
	if e.Address != "10.77.77.3" || e.HomeKey == "" || e.HomeKey == wg.HomeKey {
		t.Fatalf("end: %+v", e)
	}
	nasPub, _ := wireGuardPublic(e.HomeKey)
	if strings.Count(proxyConf, "[Peer]") != 2 || !strings.Contains(proxyConf, "# Docker host nas\nPublicKey = "+nasPub+"\nAllowedIPs = 10.77.77.3/32") {
		t.Errorf("proxy config:\n%s", proxyConf)
	}
	home := nas.files["/config/wg_confs/gsh0.conf"]
	if !nas.started || !strings.Contains(home, "Address = 10.77.77.3/24") || !strings.Contains(home, "PrivateKey = "+e.HomeKey) ||
		!strings.Contains(home, "Endpoint = vps.example.com:51820") {
		t.Errorf("nas's end: started=%v\n%s", nas.started, home)
	}
	if strings.Contains(local.files["/config/wg_confs/gsh0.conf"], e.HomeKey) {
		t.Error("nas's key on this host")
	}
	if len(res.Status.Hosts) != 1 || res.Status.Hosts[0].Host != "nas" || res.Status.Hosts[0].HomeAddress != "10.77.77.3" || res.Status.Hosts[0].HomePublicKey != nasPub {
		t.Errorf("status hosts: %+v", res.Status.Hosts)
	}

	// nginx relays each server to its host's end.
	st, plan, err := s.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Proxied{}
	for _, p := range plan {
		by[p.Server] = p
	}
	if by["valheim"].Upstream != "" || by["mc@nas"].Upstream != "10.77.77.3" || !strings.Contains(by["pal@pi"].Error, "no end of the WireGuard tunnel") {
		t.Errorf("plan: %+v", plan)
	}
	conf := Render(st, plan)
	if !strings.Contains(conf, "proxy_pass 10.77.77.2:27015;") || !strings.Contains(conf, "proxy_pass 10.77.77.3:25565;") {
		t.Errorf("nginx:\n%s", conf)
	}
	if r, _ := s.RouteFor(ctx, "mc@nas"); r.Target != "10.77.77.3" {
		t.Errorf("route: %+v", r)
	}

	// The check tells the ends' handshakes apart.
	localPub, _ := wireGuardPublic(wg.HomeKey)
	handshakes = localPub + "\t1700000000\n" + nasPub + "\t1700000100\n"
	if c, err := s.CheckWireGuard(ctx); err != nil || c.Handshake.Unix() != 1700000000 || c.Hosts["nas"].Unix() != 1700000100 {
		t.Errorf("check: %+v %v", c, err)
	}
	// The tunnel's network can't change under the other ends.
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{Network: "10.88.0.0/24"}, nil); err == nil || !strings.Contains(err.Error(), "remove them") {
		t.Errorf("network change: %v", err)
	}
	// Setting nas's end up again keeps its address and key.
	s.SetupHostWireGuard(ctx, "nas", HostWireGuardInput{}, nil)
	if again, _, _ := s.tunnel(); again.Hosts["nas"].Address != e.Address || again.Hosts["nas"].HomeKey != e.HomeKey {
		t.Errorf("again: %+v", again.Hosts["nas"])
	}

	// Removing nas's end: its container and its peer go.
	removedBefore := nas.removed
	if _, err := s.RemoveHostWireGuard(ctx, "nas"); err != nil {
		t.Fatal(err)
	}
	if nas.removed == removedBefore || strings.Contains(proxyConf, "Docker host nas") || strings.Count(proxyConf, "[Peer]") != 1 {
		t.Errorf("removed: %d\n%s", nas.removed, proxyConf)
	}
	if _, plan, _ := s.Plan(ctx); !strings.Contains(planFor(plan, "mc@nas").Error, "no end") {
		t.Errorf("after removing: %+v", plan)
	}
	if _, err := s.RemoveHostWireGuard(ctx, "nas"); err == nil {
		t.Error("removed twice")
	}
}

func planFor(plan []Proxied, server string) Proxied {
	for _, p := range plan {
		if p.Server == server {
			return p
		}
	}
	return Proxied{}
}

// Another host's end in LAN mode: its networks are listed from its Docker,
// the proxy routes its LAN subnet to it, and a LAN only one end can reach.
func TestWireGuardHostEndLANMode(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	local, nas := lanDocker(), lanDocker()
	s.UseDocker(local, "")
	s.UseDockerHosts(func(host string) (Containers, error) {
		if host == "nas" {
			return nas, nil
		}
		return nil, errors.New("there's no Docker host " + host)
	})
	var proxyConf string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		if strings.Contains(script, "wg-quick up") {
			proxyConf = string(stdin)
			return remote.Result{Stdout: "hlm:wgup\n"}, nil
		}
		return remote.Result{}, nil
	}
	ctx := context.Background()
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, Upstream: "203.0.113.7"})
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{}, nil); err != nil {
		t.Fatal(err)
	}
	nets, err := s.HostLANNetworks(ctx, "nas")
	if err != nil || len(nets) != 1 || nets[0].Name != "br0" || nets[0].Subnet != "192.168.1.0/24" {
		t.Fatalf("nas's networks: %+v %v", nets, err)
	}
	if _, err := s.HostLANNetworks(ctx, "nope"); err == nil {
		t.Error("networks of an unknown host")
	}
	if _, err := s.SetupHostWireGuard(ctx, "nas", HostWireGuardInput{LANNetwork: "missing"}, nil); err == nil {
		t.Error("a network nas doesn't have")
	}
	if _, err := s.SetupHostWireGuard(ctx, "nas", HostWireGuardInput{LANNetwork: "br0", LANAddress: "192.168.1.250"}, nil); err != nil {
		t.Fatal(err)
	}
	wg, _, _ := s.tunnel()
	e := wg.Hosts["nas"]
	if e.LANNetwork != "br0" || e.LANSubnet != "192.168.1.0/24" || e.LANAddress != "192.168.1.250" || e.BridgeGateway != "172.17.0.1" {
		t.Errorf("end: %+v", e)
	}
	if !strings.Contains(proxyConf, "AllowedIPs = 10.77.77.3/32, 192.168.1.0/24") {
		t.Errorf("proxy config:\n%s", proxyConf)
	}
	home := nas.files["/config/wg_confs/gsh0.conf"]
	if !strings.Contains(home, "-d 10.77.77.3 -j DNAT --to-destination 172.17.0.1") {
		t.Errorf("nas's end:\n%s", home)
	}
	if st, _ := s.WireGuardStatus(ctx); len(st.Hosts) != 1 || st.Hosts[0].LANNetwork != "br0" || st.Hosts[0].LANSubnet != "192.168.1.0/24" {
		t.Errorf("status: %+v", st.Hosts)
	}
	// This host's end can't take the same LAN.
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{LANNetwork: "br0"}, nil); err == nil || !strings.Contains(err.Error(), "already reaches") {
		t.Errorf("same LAN here: %v", err)
	}
}
