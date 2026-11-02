package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/linode/linodetest"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestWantedRules(t *testing.T) {
	m := func(port int, proto string) forwarding.Mapping {
		return forwarding.Mapping{Port: port, Local: port, Proto: proto, Proxy: true, Firewall: true}
	}
	servers := []Proxied{
		{Server: "valheim", Ports: []forwarding.Mapping{m(2456, "udp"), m(2457, "udp"), m(2458, "udp")}},
		{Server: "mc", Ports: []forwarding.Mapping{m(25565, "tcp"), m(27015, "udp")}},
		// Relayed, but left out of the firewall.
		{Server: "manual", Ports: []forwarding.Mapping{{Port: 9000, Local: 9000, Proto: "tcp", Proxy: true}}},
	}
	rules := WantedRules(servers)
	var got []string
	for _, r := range rules {
		got = append(got, r.Label+" "+r.Protocol+" "+r.Ports)
	}
	if want := "hlm-tcp-1 TCP 25565 | hlm-udp-1 UDP 2456-2458,27015"; strings.Join(got, " | ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " | "), want)
	}

	// 20 separate ports: split to stay within 15 pieces per rule.
	var many []forwarding.Mapping
	for i := 0; i < 20; i++ {
		many = append(many, m(30000+2*i, "tcp"))
	}
	rules = WantedRules([]Proxied{{Ports: many}})
	if len(rules) != 2 || strings.Count(rules[0].Ports, ",") != 14 || rules[1].Label != "hlm-tcp-2" {
		t.Errorf("split = %+v", rules)
	}
	if len(WantedRules(nil)) != 0 {
		t.Error("rules for nothing")
	}
}

func TestFirewallSync(t *testing.T) {
	fake, srv := linodetest.New()
	defer srv.Close()
	ssh := linode.Rule{Action: "ACCEPT", Protocol: "TCP", Ports: "22", Label: "ssh", Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}}}
	fake.AddFirewall(7, "vps-fw", linode.Rules{Inbound: []linode.Rule{ssh}, InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"}, 100)
	fake.AddFirewall(8, "other-fw", linode.Rules{InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"})
	fake.AddInstance(linode.Instance{ID: 100, Label: "relay", IPv4: []string{"198.51.100.5", "192.168.130.2"}})
	fake.AddInstance(linode.Instance{ID: 101, Label: "web", IPv4: []string{"198.51.100.6"}})

	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{
		"a": server(map[string]string{"2456/udp": "2456", "2457/udp": "2457"}),
		"b": server(map[string]string{"25565/tcp": "25565"}),
	}, slog.New(slog.DiscardHandler))
	s.UseLinodeAPI(srv.URL)
	relay := &fakeRelay{}
	s.run = relay.run
	ctx := context.Background()
	if _, err := s.Save(Settings{Target: remote.Target{Host: "198.51.100.5", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"a"}}); err != nil {
		t.Fatal(err)
	}

	// The token finds the proxy server's firewall by its address.
	if _, err := s.TestFirewall(ctx, "wrong"); err == nil || !strings.Contains(err.Error(), "isn't valid") {
		t.Errorf("bad token: %v", err)
	}
	test, err := s.TestFirewall(ctx, linodetest.Token)
	if err != nil || len(test.Firewalls) != 2 || test.Suggested != 7 || test.Linode != "relay" || test.Firewalls[0].Linodes[0] != "relay" {
		t.Fatalf("test = %+v, %v", test, err)
	}

	// Saving before the proxy is applied opens nothing.
	st, err := s.SaveFirewall(ctx, FirewallConfig{Token: linodetest.Token, FirewallID: 7})
	if err != nil || !st.Configured || st.FirewallLabel != "vps-fw" || len(st.Rules) != 0 || !st.InSync || st.OtherRules != 1 {
		t.Fatalf("save = %+v, %v", st, err)
	}

	// Applying opens the relayed ports, after the user's rules.
	res, err := s.Apply(ctx)
	if err != nil || res.Firewall == nil || !res.Firewall.Changed || res.Firewall.Error != "" {
		t.Fatalf("apply = %+v, %v", res.Firewall, err)
	}
	in := fake.Rules(7).Inbound
	if len(in) != 2 || in[0].Label != "ssh" || in[1].Label != "hlm-udp-1" || in[1].Ports != "2456-2457" || fake.Rules(7).InboundPolicy != "DROP" {
		t.Fatalf("rules = %+v", in)
	}
	// Applying the same again doesn't touch the firewall.
	puts := fake.Puts()
	if res, _ := s.Apply(ctx); res.Firewall.Changed || fake.Puts() != puts {
		t.Error("unchanged ports rewrote the firewall")
	}

	// Another server: its port is added; a rule the user added later stays.
	rules := fake.Rules(7)
	rules.Inbound = append(rules.Inbound, linode.Rule{Action: "ACCEPT", Protocol: "TCP", Ports: "443", Label: "https", Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}}})
	fake.AddFirewall(7, "vps-fw", rules, 100)
	st0, _, _ := s.Settings()
	st0.Servers = []string{"a", "b"}
	s.Save(st0)
	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range fake.Rules(7).Inbound {
		labels = append(labels, r.Label+":"+r.Ports)
	}
	if got := strings.Join(labels, " "); got != "ssh:22 https:443 hlm-tcp-1:25565 hlm-udp-1:2456-2457" {
		t.Errorf("rules = %s", got)
	}
	if st := s.FirewallStatus(ctx); !st.InSync || len(st.Rules) != 2 || st.OtherRules != 2 {
		t.Errorf("status = %+v", st)
	}

	// A token that can't write: nginx is still applied, the error reported.
	fake.SetReadOnly(true)
	st0.Servers = []string{"b"}
	s.Save(st0)
	res, err = s.Apply(ctx)
	if err != nil || res.Firewall == nil || !strings.Contains(res.Firewall.Error, "isn't allowed") {
		t.Errorf("read-only token: %+v, %v", res.Firewall, err)
	}
	if st := s.FirewallStatus(ctx); st.InSync {
		t.Error("status should show the firewall out of sync")
	}
	fake.SetReadOnly(false)
	if _, err := s.SyncFirewall(ctx); err != nil || !s.FirewallStatus(ctx).InSync {
		t.Errorf("sync: %v", err)
	}

	// Removing the relays closes the ports.
	rm, err := s.Remove(ctx)
	if err != nil || rm.Firewall == nil || !rm.Firewall.Changed {
		t.Fatalf("remove = %+v, %v", rm, err)
	}
	for _, r := range fake.Rules(7).Inbound {
		if strings.HasPrefix(r.Label, "hlm-") {
			t.Errorf("rule left open: %+v", r)
		}
	}

	// Moving to another firewall takes the rules off the first one.
	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFirewall(ctx, FirewallConfig{FirewallID: 8}); err != nil { // token kept
		t.Fatal(err)
	}
	if n := len(fake.Rules(7).Inbound); n != 2 {
		t.Errorf("old firewall still has hlm rules: %d rules", n)
	}
	if in := fake.Rules(8).Inbound; len(in) != 1 || in[0].Label != "hlm-tcp-1" {
		t.Errorf("new firewall = %+v", in)
	}
	// Forgetting it closes the ports too.
	if err := s.DeleteFirewall(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.Rules(8).Inbound); n != 0 {
		t.Errorf("ports left open after forgetting: %d", n)
	}
	if st := s.FirewallStatus(ctx); st.Configured {
		t.Error("still configured")
	}
}

func TestFirewallRuleLimit(t *testing.T) {
	fake, srv := linodetest.New()
	defer srv.Close()
	var full []linode.Rule
	for i := 0; i < 25; i++ {
		full = append(full, linode.Rule{Action: "ACCEPT", Protocol: "TCP", Ports: fmt.Sprint(1000 + i), Label: fmt.Sprintf("user-%d", i), Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}}})
	}
	fake.AddFirewall(1, "full", linode.Rules{Inbound: full, InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"})
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2456"})}, slog.New(slog.DiscardHandler))
	s.UseLinodeAPI(srv.URL)
	s.run = (&fakeRelay{}).run
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"a"}})
	s.SaveFirewall(context.Background(), FirewallConfig{Token: linodetest.Token, FirewallID: 1})
	res, err := s.Apply(context.Background())
	if err != nil || res.Firewall == nil || !strings.Contains(res.Firewall.Error, "Linode allows 25") {
		t.Errorf("full firewall: %+v, %v", res.Firewall, err)
	}
}

// The connection can be saved first and the firewall chosen (or changed)
// later.
func TestConnectThenChooseFirewall(t *testing.T) {
	fake, srv := linodetest.New()
	defer srv.Close()
	fake.AddFirewall(3, "relay", linode.Rules{InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"})
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2456"})}, slog.New(slog.DiscardHandler))
	s.UseLinodeAPI(srv.URL)
	s.run = (&fakeRelay{}).run
	ctx := context.Background()
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"a"}})

	if _, err := s.SaveFirewall(ctx, FirewallConfig{Token: "nope"}); err == nil {
		t.Error("a bad token was saved")
	}
	st, err := s.SaveFirewall(ctx, FirewallConfig{Token: linodetest.Token})
	if err != nil || !st.Configured || st.FirewallID != 0 {
		t.Fatalf("connect = %+v, %v", st, err)
	}
	// Without a firewall, applying leaves Linode alone.
	if res, err := s.Apply(ctx); err != nil || res.Firewall != nil || fake.Puts() != 0 {
		t.Errorf("apply without firewall: %+v, %v", res.Firewall, err)
	}
	if _, err := s.SyncFirewall(ctx); err == nil || !strings.Contains(err.Error(), "choose the firewall") {
		t.Errorf("sync without firewall: %v", err)
	}
	// Choosing it with the saved token opens the applied relays' ports.
	st, err = s.SaveFirewall(ctx, FirewallConfig{FirewallID: 3})
	if err != nil || st.FirewallLabel != "relay" || !st.InSync || len(st.Rules) != 1 {
		t.Fatalf("choose = %+v, %v", st, err)
	}
	// Unselecting it closes them again.
	if _, err := s.SaveFirewall(ctx, FirewallConfig{}); err != nil || len(fake.Rules(3).Inbound) != 0 {
		t.Errorf("unselect: %v, %+v", err, fake.Rules(3).Inbound)
	}
}
