package cftunnel

import (
	"context"
	"github.com/danotchuuy/home-lab-manager/internal/cloudflare/cloudflaretest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// A route to hlm itself goes to its port, as cloudflared reaches it.
func TestSelfOrigin(t *testing.T) {
	ctx := context.Background()
	hlm := Route{Hostname: "friends.example.com", Hlm: true}
	container := func(network string, bindings map[string][]docker.PortBinding) *docker.ContainerJSON {
		var c docker.ContainerJSON
		c.HostConfig.NetworkMode = network
		c.HostConfig.PortBindings = bindings
		return &c
	}
	for _, tt := range []struct {
		name string
		self SelfOrigin
		t    Tunnel
		want string // or the error, after "error: "
	}{
		{"not in a container", SelfOrigin{Port: 8080}, Tunnel{}, "http://localhost:8080"},
		{"cloudflared on a LAN network", SelfOrigin{Port: 8080}, Tunnel{LANNetwork: "lan", BridgeGateway: "172.17.0.1"}, "http://172.17.0.1:8080"},
		{"container publishing it elsewhere", SelfOrigin{Port: 8080, Container: container("bridge",
			map[string][]docker.PortBinding{"8080/tcp": {{HostPort: "18080"}}})}, Tunnel{}, "http://localhost:18080"},
		{"container on the host's network", SelfOrigin{Port: 8080, Container: container("host", nil)}, Tunnel{}, "http://localhost:8080"},
		{"container not publishing it", SelfOrigin{Port: 8080, Container: container("bridge", nil)}, Tunnel{}, "doesn't publish its port 8080"},
	} {
		s := &Service{}
		s.UseSelf(func(context.Context) (SelfOrigin, error) { return tt.self, nil })
		got, err := s.resolve(ctx, hlm, tt.t)
		if err != nil {
			got = "error: " + err.Error()
		}
		if !strings.Contains(got, tt.want) {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
	if _, err := (&Service{}).resolve(ctx, hlm, Tunnel{}); err == nil {
		t.Error("resolved hlm without knowing where it is")
	}
}

// A status page site's domain, published through the (fake) Cloudflare
// API: a DNS record and an ingress rule to hlm itself.
func TestPublishToHlm(t *testing.T) {
	s, _, fake := newService(t, fakeServers{})
	s.UseSelf(func(context.Context) (SelfOrigin, error) {
		var c docker.ContainerJSON
		c.HostConfig.NetworkMode = "bridge"
		c.HostConfig.PortBindings = map[string][]docker.PortBinding{"8080/tcp": {{HostPort: "8080"}}}
		return SelfOrigin{Port: 8080, Container: &c}, nil
	})
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{}, nil); err != nil {
		t.Fatal(err)
	}
	// Whatever else the route says, it goes to hlm.
	res, err := s.SetRoutes(ctx, []Route{{Hostname: "Friends.example.com", Hlm: true, Server: "x", Address: "y", Port: 1, Scheme: "https", NoTLSVerify: true}})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Routes[0]
	if r.Hostname != "friends.example.com" || !r.Hlm || r.Origin != "http://localhost:8080" || r.State != "published" ||
		r.Server != "" || r.Address != "" || r.Port != 0 || r.NoTLSVerify {
		t.Errorf("route = %+v", r)
	}
	if recs := fake.Records("z1"); len(recs) != 1 || recs[0].Name != "friends.example.com" {
		t.Errorf("records: %+v", recs)
	}
	found := false
	for _, rule := range fake.Config(fake.Tunnels()[0].ID).Ingress {
		if rule.Hostname == "friends.example.com" {
			found = rule.Service == "http://localhost:8080" && rule.OriginRequest == nil
		}
	}
	if !found {
		t.Errorf("ingress: %+v", fake.Config(fake.Tunnels()[0].ID).Ingress)
	}
	// Still published when listed again.
	if rs, _ := s.Routes(ctx); rs.Routes[0].State != "published" {
		t.Errorf("listed: %+v", rs.Routes[0])
	}
}
