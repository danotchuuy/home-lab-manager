package transfer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

func TestMigrate(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	ctx := context.Background()

	if _, err := s.PreviewMigrate(ctx, "valheim", manager.LocalHost); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("to its own host: %v", err)
	}
	if _, err := s.PreviewMigrate(ctx, "valheim", "nowhere"); err == nil {
		t.Error("to a host that doesn't exist")
	}
	p, err := s.PreviewMigrate(ctx, "valheim", "nas")
	if err != nil {
		t.Fatal(err)
	}
	v := p.Request.Values
	// The same name and ports there; its folders under that host's appdata.
	if p.Name != "valheim@nas" || p.Request.Name != "valheim@nas" || !p.Running || p.Network != nil || v["port"] != "" ||
		v["worlds"] != "/nas/appdata/valheim/server" || p.Request.Extra[0].Value != "/nas/appdata/valheim/mods" {
		t.Errorf("preview %+v", p)
	}
	if len(p.Copies) != 3 || len(p.Warnings) != 0 {
		t.Errorf("copies %+v, warnings %q", p.Copies, p.Warnings)
	}

	var switched []string
	srv, err := s.Migrate(ctx, "valheim", MigrateOptions{Host: "nas"}, func(_ context.Context, from, to string, _ func(manager.Event)) {
		switched = append(switched, from+" → "+to)
	}, func(manager.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create valheim@nas", "stop id-valheim", "copy id-valheim id-valheim@nas /config,/opt/valheim,/var/lib/cache",
		"start id-valheim@nas", "remove id-valheim"}
	if srv.Name != "valheim@nas" || !slices.Equal(m.calls, want) || !slices.Equal(switched, []string{"valheim → valheim@nas"}) {
		t.Errorf("server %+v, calls %q, switched %q", srv, m.calls, switched)
	}
	// Its settings move with the switchover; they aren't copied.
	if _, ok := f.consoles["valheim@nas"]; ok {
		t.Error("settings copied")
	}
	if _, ok := m.servers["valheim"]; ok {
		t.Error("the original is still there")
	}
}

// A failed copy leaves the original as it was, running.
func TestMigrateFails(t *testing.T) {
	s, m, f := setup(t, "/srv/appdata")
	addValheim(m, f, "/srv/appdata")
	m.copyFail = errors.New("disk full")
	switched := false
	_, err := s.Migrate(context.Background(), "valheim", MigrateOptions{Host: "nas"}, func(context.Context, string, string, func(manager.Event)) {
		switched = true
	}, func(manager.Event) {})
	if err == nil || !strings.Contains(err.Error(), "disk full") || switched {
		t.Fatalf("err %v, switched %v", err, switched)
	}
	if !slices.Contains(m.calls, "remove id-valheim@nas") || !slices.Contains(m.calls, "start id-valheim") || slices.Contains(m.calls, "remove id-valheim") {
		t.Errorf("calls %q", m.calls)
	}
	if _, ok := m.servers["valheim"]; !ok {
		t.Error("the original is gone")
	}
}

// The network and address are chosen again when they don't carry over.
func TestMigrateNetworkChoice(t *testing.T) {
	lan := func(subnet string, used ...manager.NetworkIP) manager.NetworkInfo {
		return manager.NetworkInfo{Name: "br0", Driver: "macvlan", Subnets: []string{subnet}, StaticIP: true, DirectIP: true, UsedIPs: used}
	}
	here := lan("192.168.1.0/24")
	for _, tt := range []struct {
		name  string
		ip    string
		there []manager.NetworkInfo
		want  string // in the reason; "" for no choice
	}{
		{"no network there", "192.168.1.50", nil, "has no network called br0"},
		{"another subnet", "192.168.1.50", []manager.NetworkInfo{lan("10.0.0.0/24")}, "doesn't fit there"},
		{"address taken there", "192.168.1.50", []manager.NetworkInfo{lan("192.168.1.0/24", manager.NetworkIP{IP: "192.168.1.50", Container: "plex"})}, "used by plex"},
		{"fits", "192.168.1.50", []manager.NetworkInfo{lan("192.168.1.0/24")}, ""},
		{"automatic, same subnet", "", []manager.NetworkInfo{lan("192.168.1.0/24")}, ""},
		{"automatic, another subnet", "", []manager.NetworkInfo{lan("10.0.0.0/24")}, "address on your LAN changes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, m, f := setup(t, "/srv/appdata")
			addValheim(m, f, "/srv/appdata")
			r := m.reqs["valheim"]
			r.Network, r.IPAddress = "br0", tt.ip
			m.reqs["valheim"] = r
			m.netsOn = map[string][]manager.NetworkInfo{manager.LocalHost: {here}, "nas": tt.there}
			p, err := s.PreviewMigrate(context.Background(), "valheim", "nas")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == "" && p.Network != nil:
				t.Errorf("choice %+v", p.Network)
			case tt.want == "" && p.Request.IPAddress != tt.ip:
				t.Errorf("address %q", p.Request.IPAddress)
			case tt.want != "" && (p.Network == nil || !strings.Contains(p.Network.Reason, tt.want)):
				t.Errorf("choice %+v", p.Network)
			case tt.want != "" && (p.Request.IPAddress != "" || p.Network.IPAddress != tt.ip || p.Network.Subnets[0] != "192.168.1.0/24"):
				t.Errorf("request %+v, choice %+v", p.Request, p.Network)
			}
			if tt.want == "" {
				return
			}
			// Moving needs the choice made.
			noop := func(context.Context, string, string, func(manager.Event)) {}
			if _, err := s.Migrate(context.Background(), "valheim", MigrateOptions{Host: "nas"}, noop, func(manager.Event) {}); err == nil || !strings.Contains(err.Error(), "Choose its network") {
				t.Errorf("moved without choosing: %v", err)
			}
			req := p.Request
			req.Network, req.IPAddress = "br0", "10.0.0.5"
			if _, err := s.Migrate(context.Background(), "valheim", MigrateOptions{Host: "nas", Request: &req}, noop, func(manager.Event) {}); err != nil {
				t.Fatal(err)
			}
			if got := m.reqs["valheim@nas"]; got.IPAddress != "10.0.0.5" || got.Name != "valheim@nas" || got.TemplateID != "valheim" {
				t.Errorf("made %+v", got)
			}
		})
	}
}
