package proxy

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/linode/linodetest"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// A proxy set up before the rename from gsh to hlm keeps its nginx file.
func TestChooseConfScript(t *testing.T) {
	for dump, want := range map[string]string{
		"": ConfigPath,
		"stream {\n  include /etc/nginx/gsh-stream.conf;\n}":                           legacyConfigPath,
		"  include /etc/nginx/gsh-stream.conf;\n  include /etc/nginx/hlm-stream.conf;": ConfigPath,
		"# include /etc/nginx/gsh-stream.conf;":                                        ConfigPath,
	} {
		out, err := exec.Command("sh", "-c", "dump=\"$1\"\n"+chooseConfScript+"\necho \"$conf\"", "sh", dump).CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != want {
			t.Errorf("%q: conf = %q, %v; want %s", dump, got, err, want)
		}
	}
}

// Firewall rules labelled before the rename are hlm's, and are relabelled
// when the firewall is next synced.
func TestLegacyFirewallRules(t *testing.T) {
	fake, srv := linodetest.New()
	defer srv.Close()
	ssh := linode.Rule{Action: "ACCEPT", Protocol: "TCP", Ports: "22", Label: "ssh", Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}}}
	old := linode.Rule{Action: "ACCEPT", Protocol: "UDP", Ports: "2456-2457", Label: "gsh-udp-1", Description: ruleNote, Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}, IPv6: []string{"::/0"}}}
	fake.AddFirewall(7, "vps-fw", linode.Rules{Inbound: []linode.Rule{ssh, old}, InboundPolicy: "DROP", OutboundPolicy: "ACCEPT"}, 100)
	fake.AddInstance(linode.Instance{ID: 100, Label: "relay", IPv4: []string{"198.51.100.5"}})

	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2456", "2457/udp": "2457"})}, slog.New(slog.DiscardHandler))
	s.UseLinodeAPI(srv.URL)
	relay := &fakeRelay{}
	s.run = relay.run
	ctx := context.Background()
	if _, err := s.Save(Settings{Target: remote.Target{Host: "198.51.100.5", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveFirewall(ctx, FirewallConfig{Token: linodetest.Token, FirewallID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.FirewallStatus(ctx); !st.InSync || len(st.Rules) != 1 || st.OtherRules != 1 {
		t.Errorf("status = %+v", st)
	}
	var labels []string
	for _, r := range fake.Rules(7).Inbound {
		labels = append(labels, r.Label+":"+r.Ports)
	}
	if got := strings.Join(labels, " "); got != "ssh:22 hlm-udp-1:2456-2457" {
		t.Errorf("rules = %s", got)
	}
}
