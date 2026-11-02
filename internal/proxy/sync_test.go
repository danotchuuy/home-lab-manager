package proxy

import (
	"context"
	"log/slog"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/linode/linodetest"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestPortStates(t *testing.T) {
	fake, srv := linodetest.New()
	defer srv.Close()
	fake.AddFirewall(1, "fw", linode.Rules{InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"})
	named := func(name string, b map[string]string) docker.ContainerJSON {
		info := server(b)
		info.Name = "/" + name
		return info
	}
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{
		"a": named("a", map[string]string{"2456/udp": "2456", "2457/udp": "2457"}),
		"b": named("b", map[string]string{"2456/udp": "2456"}),
	}, slog.New(slog.DiscardHandler))
	s.UseLinodeAPI(srv.URL)
	s.run = (&fakeRelay{}).run
	// a's options: 2457 can be left out of the proxy.
	relay2457 := true
	s.UsePorts(func(info docker.ContainerJSON) ([]forwarding.Mapping, error) {
		ms := forwarding.Mappings(info, nil)
		for i := range ms {
			if info.Name == "/a" && ms[i].Local == 2457 {
				ms[i].Proxy = relay2457
			}
		}
		return ms, nil
	})
	ctx := context.Background()
	state := func(server string) PortSync {
		t.Helper()
		ps, err := s.PortStates(ctx, server)
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}

	if ps := state("a"); ps.Proxy != nil {
		t.Errorf("no proxy set up: %+v", ps)
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"a", "b"}})
	s.SaveFirewall(ctx, FirewallConfig{Token: linodetest.Token, FirewallID: 1})

	ps := state("a")
	if ps.Proxy["2456/udp"].State != forwarding.SyncPending || ps.Firewall["2457/udp"].State != forwarding.SyncPending {
		t.Errorf("before applying: %+v", ps)
	}
	// b's 2456 clashes with a's.
	if ps := state("b"); ps.Proxy["2456/udp"].State != forwarding.SyncConflict {
		t.Errorf("b = %+v", ps.Proxy)
	}

	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	ps = state("a")
	for _, k := range []string{"2456/udp", "2457/udp"} {
		if ps.Proxy[k].State != forwarding.SyncDone || ps.Firewall[k].State != forwarding.SyncDone {
			t.Errorf("after applying, %s: proxy %+v firewall %+v", k, ps.Proxy[k], ps.Firewall[k])
		}
	}

	// 2457 is left out: still relayed and open until the next apply.
	relay2457 = false
	ps = state("a")
	if ps.Proxy["2457/udp"].State != forwarding.SyncRemoving || ps.Firewall["2457/udp"].State != forwarding.SyncRemoving {
		t.Errorf("left out: proxy %+v firewall %+v", ps.Proxy["2457/udp"], ps.Firewall["2457/udp"])
	}
	s.Apply(ctx)
	ps = state("a")
	if ps.Proxy["2457/udp"].State != forwarding.SyncOff || ps.Firewall["2457/udp"].State != forwarding.SyncOff || ps.Proxy["2456/udp"].State != forwarding.SyncDone {
		t.Errorf("after re-applying: %+v", ps)
	}
	// The firewall can't be read: reported, the proxy state still shown.
	fake.AddFirewall(1, "fw", linode.Rules{InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"})
	s.SaveFirewall(ctx, FirewallConfig{FirewallID: 1})
	srv.Close()
	if ps := state("a"); ps.FirewallError == "" || ps.Firewall != nil || ps.Proxy["2456/udp"].State != forwarding.SyncDone {
		t.Errorf("firewall unreachable: %+v", ps)
	}
}
