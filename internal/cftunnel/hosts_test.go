package cftunnel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare/cloudflaretest"
)

// A tunnel on another Docker host carries that host's servers' routes,
// reaching them as a local connector would; the rest go through this
// host's tunnel. Removing it moves its routes back.
func TestTunnelsOnOtherHosts(t *testing.T) {
	here := publishing(map[string]string{"80/tcp": "8080"})
	here.ID = "aaa"
	nasWeb := publishing(map[string]string{"80/tcp": "8090"})
	nasWeb.ID = "bbb@nas"
	piWeb := publishing(map[string]string{"80/tcp": "8100"})
	piWeb.ID = "ccc@pi"
	s, local, fake := newService(t, fakeServers{"panel": here, "web@nas": nasWeb, "map@pi": piWeb})
	nas := &fakeDocker{}
	s.UseDockerHosts(func(host string) (Containers, error) {
		if host == "nas" {
			return nas, nil
		}
		return nil, errors.New("there's no Docker host " + host)
	})
	s.UseHostAddress(func(id string) string {
		switch {
		case strings.HasSuffix(id, "@nas"):
			return "192.168.1.20"
		case strings.HasSuffix(id, "@pi"):
			return "192.168.1.30"
		}
		return ""
	})
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRoutes(ctx, []Route{
		{Hostname: "panel.example.com", Server: "panel", Port: 8080},
		{Hostname: "web.example.com", Server: "web@nas", Port: 8090},
		{Hostname: "map.example.com", Server: "map@pi", Port: 8100},
	}); err != nil {
		t.Fatal(err)
	}
	ingressOf := func(id string) map[string]string {
		out := map[string]string{}
		for _, r := range fake.Config(id).Ingress {
			out[r.Hostname] = r.Service
		}
		return out
	}
	localID := fake.Tunnels()[0].ID
	// Before nas has a tunnel, its server goes through this host's, over the LAN.
	if in := ingressOf(localID); in["web.example.com"] != "http://192.168.1.20:8090" || in["panel.example.com"] != "http://localhost:8080" {
		t.Fatalf("ingress before: %v", in)
	}

	if _, _, err := s.Setup(ctx, SetupInput{Host: "nope"}, nil); err == nil {
		t.Error("set up on an unknown host")
	}
	st, ap, err := s.Setup(ctx, SetupInput{Host: "nas"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nas.created == nil || !nas.running || local.removed > 1 {
		t.Errorf("nas's connector: %+v, local removed %d", nas.created, local.removed)
	}
	var nasID string
	for _, tu := range fake.Tunnels() {
		if tu.Name == "hlm-nas" {
			nasID = tu.ID
		}
	}
	if nasID == "" || len(st.Hosts) != 1 || st.Hosts[0].Host != "nas" || st.Hosts[0].ID != nasID || !st.Hosts[0].Connector.Running {
		t.Fatalf("tunnels %+v, status hosts %+v", fake.Tunnels(), st.Hosts)
	}
	if !strings.Contains(nas.created.Env[0], nasID) {
		t.Errorf("nas's connector token: %v", nas.created.Env)
	}
	// nas's server now goes through nas's tunnel, on localhost there; pi's
	// (no tunnel) still through this host's.
	if in := ingressOf(nasID); in["web.example.com"] != "http://localhost:8090" || in["panel.example.com"] != "" {
		t.Errorf("nas ingress: %v", in)
	}
	if in := ingressOf(localID); in["web.example.com"] != "" || in["map.example.com"] != "http://192.168.1.30:8100" {
		t.Errorf("local ingress: %v", in)
	}
	records := map[string]string{}
	for _, r := range fake.Records("z1") {
		records[r.Name] = r.Content
	}
	if records["web.example.com"] != nasID+".cfargotunnel.com" || records["panel.example.com"] != localID+".cfargotunnel.com" || len(records) != 3 {
		t.Errorf("records: %v", records)
	}
	if ap == nil {
		t.Error("nothing applied at setup")
	}
	r, _ := s.Routes(ctx)
	for _, rs := range r.Routes {
		want := map[string]string{"panel.example.com": "local", "web.example.com": "nas", "map.example.com": "local"}[rs.Hostname]
		if rs.Via != want || rs.State != "published" {
			t.Errorf("%s: via %s, %s (%s)", rs.Hostname, rs.Via, rs.State, rs.Error)
		}
	}
	if _, err := s.ControlOn(ctx, "nas", "restart"); err != nil || nas.actions[len(nas.actions)-1] != "restart" {
		t.Errorf("restart on nas: %v %v", err, nas.actions)
	}

	// Removing nas's tunnel: its route goes back through this host's.
	if _, err := s.RemoveOn(ctx, "nas"); err != nil {
		t.Fatal(err)
	}
	if nas.created != nil {
		t.Error("nas's connector left")
	}
	for _, tu := range fake.Tunnels() {
		if tu.Name == "hlm-nas" && tu.DeletedAt == nil {
			t.Error("nas's tunnel left in Cloudflare")
		}
	}
	if in := ingressOf(localID); in["web.example.com"] != "http://192.168.1.20:8090" {
		t.Errorf("after removing: %v", in)
	}
	records = map[string]string{}
	for _, r := range fake.Records("z1") {
		records[r.Name] = r.Content
	}
	if records["web.example.com"] != localID+".cfargotunnel.com" {
		t.Errorf("records after removing: %v", records)
	}
	if st, _ := s.Status(ctx); len(st.Hosts) != 0 {
		t.Errorf("status hosts after: %+v", st.Hosts)
	}
}
