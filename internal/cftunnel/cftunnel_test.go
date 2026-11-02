package cftunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
	"github.com/danotchuuy/home-lab-manager/internal/cloudflare/cloudflaretest"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeServers map[string]docker.ContainerJSON

func (f fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	if info, ok := f[id]; ok {
		info.Name = "/" + id
		return info, nil
	}
	return docker.ContainerJSON{}, manager.ErrNotFound
}

func (f fakeServers) List(context.Context) ([]manager.Server, error) {
	var out []manager.Server
	for name, info := range f {
		out = append(out, manager.Server{Name: name, State: "running", WebUI: info.Config.Labels[deploy.LabelWebUI]})
	}
	return out, nil
}

// publishing is a server on a bridge network publishing container ports
// ("80/tcp") on host ports ("8080", or "127.0.0.1:8080").
func publishing(bindings map[string]string) docker.ContainerJSON {
	var info docker.ContainerJSON
	info.HostConfig.NetworkMode = "bridge"
	info.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	for c, h := range bindings {
		b := docker.PortBinding{HostPort: h}
		if ip, port, ok := strings.Cut(h, ":"); ok {
			b = docker.PortBinding{HostIP: ip, HostPort: port}
		}
		info.HostConfig.PortBindings[c] = []docker.PortBinding{b}
	}
	return info
}

func lanServer(ip string, ports ...string) docker.ContainerJSON {
	var info docker.ContainerJSON
	info.HostConfig.NetworkMode = "lan"
	info.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	info.Config.ExposedPorts = map[string]struct{}{}
	for _, p := range ports {
		info.Config.ExposedPorts[p] = struct{}{}
	}
	info.NetworkSettings.Networks = map[string]docker.EndpointSettings{"lan": {IPAddress: ip}}
	return info
}

type fakeDocker struct {
	created *docker.CreateRequest
	running bool
	removed int
	joined  []string
	actions []string
}

func (f *fakeDocker) PullImage(context.Context, string, func(docker.PullProgress)) error {
	return errors.New("offline")
}
func (f *fakeDocker) InspectContainer(context.Context, string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	if f.created == nil {
		return info, &docker.APIError{Status: 404, Message: "no such container"}
	}
	info.State.Status, info.State.Running = "running", f.running
	if !f.running {
		info.State.Status = "exited"
	}
	return info, nil
}
func (f *fakeDocker) CreateContainer(_ context.Context, name string, req docker.CreateRequest) (string, []string, error) {
	f.created = &req
	return "id-" + name, nil, nil
}
func (f *fakeDocker) StartContainer(context.Context, string) error {
	f.running = true
	f.actions = append(f.actions, "start")
	return nil
}
func (f *fakeDocker) StopContainer(_ context.Context, id string, _ int) error {
	if f.created == nil || id == legacyContainer {
		return &docker.APIError{Status: 404, Message: "no such container"}
	}
	f.running = false
	f.actions = append(f.actions, "stop")
	return nil
}
func (f *fakeDocker) RestartContainer(context.Context, string, int) error {
	f.actions = append(f.actions, "restart")
	return nil
}
func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	if id == legacyContainer {
		return &docker.APIError{Status: 404, Message: "no such container"}
	}
	f.removed++
	f.created, f.running = nil, false
	return &docker.APIError{Status: 404}
}
func (f *fakeDocker) RenameContainer(context.Context, string, string) error { return nil }
func (f *fakeDocker) ListNetworks(context.Context) ([]docker.Network, error) {
	return []docker.Network{
		{Name: "bridge", Driver: "bridge", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "172.17.0.0/16", Gateway: "172.17.0.1"}}}},
		{Name: "lan", Driver: "macvlan", Options: map[string]string{"parent": "eth0"}, IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "192.168.1.0/24", Gateway: "192.168.1.1"}}}},
	}, nil
}
func (f *fakeDocker) ConnectNetwork(_ context.Context, network, _ string, _ any) error {
	f.joined = append(f.joined, network)
	return nil
}
func (f *fakeDocker) Logs(context.Context, string, docker.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("INF Starting tunnel tunnelID=x\nINF Registered tunnel connection connIndex=0 location=ams01\nnot read\n")), nil
}

func newService(t *testing.T, servers fakeServers) (*Service, *fakeDocker, *cloudflaretest.Fake) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	fake, srv := cloudflaretest.New()
	t.Cleanup(srv.Close)
	fake.AddZone("z1", "example.com")
	fake.AddZone("z2", "games.example.org")
	fd := &fakeDocker{}
	s := New(store, servers, fd, "", slog.New(slog.DiscardHandler))
	s.UseAPI(srv.URL)
	return s, fd, fake
}

func TestAccount(t *testing.T) {
	s, _, _ := newService(t, fakeServers{})
	ctx := context.Background()
	if _, err := s.Test(ctx, "wrong"); err == nil || !strings.Contains(err.Error(), "isn't valid") {
		t.Errorf("bad token: %v", err)
	}
	res, err := s.Test(ctx, cloudflaretest.Token)
	if err != nil || len(res.Accounts) != 1 || len(res.Zones) != 2 {
		t.Fatalf("test: %+v, %v", res, err)
	}
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || !st.Configured || st.AccountName != "Home" || len(st.Zones) != 2 || st.Tunnel != nil {
		t.Errorf("status: %+v, %v", st, err)
	}
	if len(st.LANNetworks) != 1 || st.LANNetworks[0].Name != "lan" {
		t.Errorf("LAN networks: %+v", st.LANNetworks)
	}
	// An empty token keeps the saved one.
	if err := s.SaveAccount(ctx, AccountInput{AccountID: "acc1"}); err != nil {
		t.Error(err)
	}
	if err := s.SaveAccount(ctx, AccountInput{AccountID: "other"}); err == nil {
		t.Error("an account the token can't use was accepted")
	}
	if err := s.DeleteAccount(); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(ctx); st.Configured {
		t.Error("still configured after delete")
	}
}

// A token with just the permissions the tunnel needs: Cloudflare doesn't
// list its account, so it's found through the token's domains.
func TestAccountWithoutAccountSettings(t *testing.T) {
	s, _, fake := newService(t, fakeServers{})
	ctx := context.Background()
	fake.AccountsHidden = true
	res, err := s.Test(ctx, cloudflaretest.Token)
	if err != nil || len(res.Accounts) != 1 || res.Accounts[0].ID != "acc1" || res.Accounts[0].Name != "Home" {
		t.Fatalf("test: %+v, %v", res, err)
	}
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(ctx); st.AccountName != "Home" {
		t.Errorf("status: %+v", st)
	}

	// Without tunnel permissions, the error says so, and only that.
	fake.NoTunnels = true
	_, err = s.Test(ctx, cloudflaretest.Token)
	if err == nil || !strings.Contains(err.Error(), "can't manage Cloudflare Tunnels in Home") || strings.Contains(err.Error(), "DNS") {
		t.Errorf("without tunnel permissions: %v", err)
	}
}

func TestOrigin(t *testing.T) {
	host := Tunnel{}
	lan := Tunnel{LANNetwork: "lan", LANSubnet: "192.168.1.0/24", BridgeGateway: "172.17.0.1"}
	route := func(port int) Route { return Route{Server: "s", Port: port, Scheme: "http"} }
	for _, c := range []struct {
		name   string
		info   docker.ContainerJSON
		t      Tunnel
		port   int
		want   string
		errHas string
	}{
		{"published", publishing(map[string]string{"80/tcp": "8080"}), host, 8080, "http://localhost:8080", ""},
		{"published, LAN mode", publishing(map[string]string{"80/tcp": "8080"}), lan, 8080, "http://172.17.0.1:8080", ""},
		{"loopback only", publishing(map[string]string{"80/tcp": "127.0.0.1:8080"}), host, 8080, "http://127.0.0.1:8080", ""},
		{"loopback only, LAN mode", publishing(map[string]string{"80/tcp": "127.0.0.1:8080"}), lan, 8080, "", "only published on 127.0.0.1"},
		{"container port isn't the host's", publishing(map[string]string{"80/tcp": "8080"}), host, 80, "", "doesn't use TCP port 80"},
		{"UDP only", publishing(map[string]string{"2456/udp": "2456"}), host, 2456, "", "doesn't use TCP port"},
		{"own LAN address", lanServer("192.168.1.61", "8080/tcp"), lan, 8080, "http://192.168.1.61:8080", ""},
		{"own LAN address without LAN mode", lanServer("192.168.1.61", "8080/tcp"), host, 8080, "", "own LAN address"},
		{"own LAN address, stopped", lanServer("", "8080/tcp"), lan, 8080, "", "is it running"},
	} {
		got, err := origin(c.info, route(c.port), c.t)
		if c.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("%s: got %q, %v; want error with %q", c.name, got, err, c.errHas)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	r := Route{Server: "s", Port: 8443, Scheme: "https"}
	if got, _ := origin(publishing(map[string]string{"443/tcp": "8443"}), r, host); got != "https://localhost:8443" {
		t.Errorf("https: %s", got)
	}
}

func TestHostnames(t *testing.T) {
	for in, want := range map[string]string{
		"Panel.Example.com":                 "panel.example.com",
		"https://panel.example.com/admin?x": "panel.example.com",
		"panel.example.com.":                "panel.example.com",
	} {
		if got := normalizeHostname(in); got != want {
			t.Errorf("normalize %q = %q", in, got)
		}
	}
	for h, ok := range map[string]bool{"panel.example.com": true, "a-b.example.com": true, "example": false, "-a.example.com": false, "a_b.example.com": false, "*.example.com": false} {
		if validHostname(h) != ok {
			t.Errorf("validHostname(%q) = %v", h, !ok)
		}
	}
	zones := []cloudflare.Zone{{ID: "1", Name: "example.org"}, {ID: "2", Name: "games.example.org"}}
	if z, ok := zoneOf("mc.games.example.org", zones); !ok || z.ID != "2" {
		t.Errorf("longest zone: %+v", z)
	}
	if _, ok := zoneOf("notexample.org", zones); ok {
		t.Error("a zone matched a name it's only a suffix of")
	}
	if p, scheme := webUI("http://[IP]:8080/admin"); p != 8080 || scheme != "http" {
		t.Errorf("webUI: %d %s", p, scheme)
	}
	if p, scheme := webUI("https://[IP]/"); p != 443 || scheme != "https" {
		t.Errorf("webUI without port: %d %s", p, scheme)
	}
}

func TestSetupRoutesAndRemove(t *testing.T) {
	servers := fakeServers{
		"panel": publishing(map[string]string{"80/tcp": "8080", "27015/udp": "27015"}),
		"web":   publishing(map[string]string{"443/tcp": "8443"}),
	}
	s, fd, fake := newService(t, servers)
	ctx := context.Background()
	if _, _, err := s.Setup(ctx, SetupInput{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("setup without a token: %v", err)
	}
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	// Routes can be saved before the tunnel exists; they're published when
	// it's set up.
	res, err := s.SetRoutes(ctx, []Route{{Hostname: "Panel.example.com", Server: "panel", Port: 8080}})
	if err != nil || len(res.Routes) != 1 || res.Routes[0].State != "pending" || res.Routes[0].Zone != "example.com" || res.Routes[0].Scheme != "http" {
		t.Fatalf("routes before setup: %+v, %v", res, err)
	}
	for name, bad := range map[string][]Route{
		"not a zone":   {{Hostname: "panel.other.net", Server: "panel", Port: 8080}},
		"UDP port":     {{Hostname: "x.example.com", Server: "panel", Port: 27015}},
		"no server":    {{Hostname: "x.example.com", Server: "gone", Port: 80}},
		"twice":        {{Hostname: "x.example.com", Server: "panel", Port: 8080}, {Hostname: "X.example.com", Server: "panel", Port: 8080}},
		"bad protocol": {{Hostname: "x.example.com", Server: "panel", Port: 8080, Scheme: "ftp"}},
	} {
		if _, err := s.SetRoutes(ctx, bad); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	var log []string
	st, ap, err := s.Setup(ctx, SetupInput{Name: "home"}, func(e Event) { log = append(log, e.Stage+": "+e.Message) })
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, strings.Join(log, "\n"))
	}
	tunnels := fake.Tunnels()
	if len(tunnels) != 1 || tunnels[0].Name != "home" || tunnels[0].ConfigSrc != "cloudflare" {
		t.Fatalf("tunnels: %+v", tunnels)
	}
	id := tunnels[0].ID
	if st.Tunnel == nil || st.Tunnel.ID != id || !st.Tunnel.Connector.Running {
		t.Errorf("status: %+v", st.Tunnel)
	}
	req := fd.created
	if req == nil || req.HostConfig.NetworkMode != "host" || req.Env[0] != "TUNNEL_TOKEN=token-for-"+id || strings.Join(req.Cmd, " ") != "tunnel --no-autoupdate run" || req.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("container: %+v", req)
	}
	joined := strings.Join(log, "\n")
	if !strings.Contains(joined, "Registered tunnel connection") || strings.Contains(joined, "not read") {
		t.Errorf("console:\n%s", joined)
	}
	if ap == nil || ap.Created != 1 {
		t.Errorf("apply during setup: %+v", ap)
	}
	cfg := fake.Config(id)
	if cfg == nil || len(cfg.Ingress) != 2 || cfg.Ingress[0].Hostname != "panel.example.com" || cfg.Ingress[0].Service != "http://localhost:8080" || cfg.Ingress[1].Service != "http_status:404" {
		t.Errorf("ingress: %+v", cfg)
	}
	recs := fake.Records("z1")
	if len(recs) != 1 || recs[0].Name != "panel.example.com" || recs[0].Type != "CNAME" || recs[0].Content != id+".cfargotunnel.com" || !recs[0].Proxied || recs[0].Comment != RecordComment {
		t.Errorf("records: %+v", recs)
	}
	routes, err := s.Routes(ctx)
	if err != nil || len(routes.Routes) != 1 || routes.Routes[0].State != "published" || routes.Pending || !routes.Tunnel {
		t.Errorf("routes: %+v, %v", routes, err)
	}
	if len(routes.Servers) != 2 {
		t.Errorf("servers: %+v", routes.Servers)
	}

	// Someone else's record is never replaced.
	fake.AddRecord("z1", cloudflare.DNSRecord{Type: "A", Name: "shop.example.com", Content: "203.0.113.5"})
	res, err = s.SetRoutes(ctx, []Route{
		{Hostname: "web.games.example.org", Server: "web", Port: 8443, Scheme: "https", NoTLSVerify: true},
		{Hostname: "shop.example.com", Server: "panel", Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]RouteState{}
	for _, r := range res.Routes {
		states[r.Hostname] = r
	}
	if states["shop.example.com"].State != "conflict" || !strings.Contains(states["shop.example.com"].Error, "A 203.0.113.5") {
		t.Errorf("conflict: %+v", states["shop.example.com"])
	}
	if states["web.games.example.org"].State != "published" || res.Created != 1 || res.Removed != 1 {
		t.Errorf("apply: %+v", res)
	}
	if recs := fake.Records("z1"); len(recs) != 1 || recs[0].Name != "shop.example.com" || recs[0].Content != "203.0.113.5" {
		t.Errorf("zone 1 after: %+v", recs)
	}
	cfg = fake.Config(id)
	if len(cfg.Ingress) != 2 || cfg.Ingress[0].Service != "https://localhost:8443" || cfg.Ingress[0].OriginRequest == nil || !cfg.Ingress[0].OriginRequest.NoTLSVerify {
		t.Errorf("ingress after: %+v", cfg.Ingress)
	}

	// The container can be controlled.
	if st, err := s.Control(ctx, "stop"); err != nil || st.Tunnel.Connector.Running {
		t.Errorf("stop: %+v, %v", st.Tunnel, err)
	}
	if _, err := s.Control(ctx, "restart"); err != nil || strings.Join(fd.actions, ",") != "start,stop,restart" {
		t.Errorf("restart: %v, %v", fd.actions, err)
	}
	if _, err := s.Control(ctx, "explode"); err == nil {
		t.Error("unknown action accepted")
	}

	// Setting up again keeps the tunnel.
	fake.Connect(id)
	if _, _, err := s.Setup(ctx, SetupInput{Name: "ignored"}, nil); err != nil {
		t.Fatal(err)
	}
	if ts := fake.Tunnels(); len(ts) != 1 || ts[0].ID != id {
		t.Errorf("tunnels after setting up again: %+v", ts)
	}
	if st, _ := s.Status(ctx); st.Tunnel.Health != "healthy" || len(st.Tunnel.Connections) != 1 {
		t.Errorf("health: %+v", st.Tunnel)
	}
	if err := s.DeleteAccount(); err == nil {
		t.Error("forgot the token while the tunnel exists")
	}

	// Deleting a server unpublishes its hostname.
	if err := s.ForgetServer(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if recs := fake.Records("z2"); len(recs) != 0 {
		t.Errorf("records of a deleted server: %+v", recs)
	}

	st, err = s.Remove(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tunnel != nil || len(fake.Tunnels()) != 0 || fd.created != nil {
		t.Errorf("after remove: %+v, %+v", st.Tunnel, fake.Tunnels())
	}
	if recs := fake.Records("z1"); len(recs) != 1 || recs[0].Name != "shop.example.com" {
		t.Errorf("records after remove: %+v", recs)
	}
	// The routes stay for the next tunnel.
	if r, _ := s.Routes(ctx); len(r.Routes) != 1 || r.Tunnel {
		t.Errorf("routes after remove: %+v", r)
	}
}

func TestSetupRefusesSomeoneElsesTunnel(t *testing.T) {
	s, _, fake := newService(t, fakeServers{})
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	c, _ := cloudflare.New(cloudflaretest.Token, s.apiBase)
	if _, err := c.CreateTunnel(ctx, "acc1", "hlm"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{}, nil); err == nil || !strings.Contains(err.Error(), "already has a tunnel") {
		t.Errorf("setup: %v", err)
	}
	if len(fake.Tunnels()) != 1 {
		t.Errorf("tunnels: %+v", fake.Tunnels())
	}
}

func TestLANMode(t *testing.T) {
	servers := fakeServers{
		"lan":   lanServer("192.168.1.61", "8080/tcp"),
		"panel": publishing(map[string]string{"80/tcp": "8080"}),
	}
	s, fd, fake := newService(t, servers)
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{LANNetwork: "lan", LANAddress: "10.0.0.5"}, nil); err == nil || !strings.Contains(err.Error(), "subnet") {
		t.Errorf("address outside the subnet: %v", err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{LANNetwork: "bridge"}, nil); err == nil {
		t.Error("bridge accepted as a LAN network")
	}
	st, _, err := s.Setup(ctx, SetupInput{LANNetwork: "lan", LANAddress: "192.168.1.250"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := fd.created
	if req.HostConfig.NetworkMode != "lan" || req.NetworkingConfig.EndpointsConfig["lan"].IPAMConfig.IPv4Address != "192.168.1.250" || strings.Join(fd.joined, ",") != "bridge" {
		t.Errorf("container: %+v, joined %v", req, fd.joined)
	}
	if st.Tunnel.LANSubnet != "192.168.1.0/24" {
		t.Errorf("status: %+v", st.Tunnel)
	}
	if _, err := s.SetRoutes(ctx, []Route{
		{Hostname: "lan.example.com", Server: "lan", Port: 8080},
		{Hostname: "panel.example.com", Server: "panel", Port: 8080},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := fake.Config(fake.Tunnels()[0].ID)
	got := map[string]string{}
	for _, r := range cfg.Ingress {
		got[r.Hostname] = r.Service
	}
	if got["lan.example.com"] != "http://192.168.1.61:8080" || got["panel.example.com"] != "http://172.17.0.1:8080" {
		t.Errorf("ingress: %+v", got)
	}

	// While the LAN server is stopped it has no address; its hostname keeps
	// what was published.
	servers["lan"] = lanServer("", "8080/tcp")
	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if recs := fake.Records("z1"); len(recs) != 2 {
		t.Errorf("records while stopped: %+v", recs)
	}
	cfg = fake.Config(fake.Tunnels()[0].ID)
	if len(cfg.Ingress) != 3 || cfg.Ingress[0].Service != "http://192.168.1.61:8080" {
		t.Errorf("ingress while stopped: %+v", cfg.Ingress)
	}
	r, _ := s.Routes(ctx)
	if r.Pending {
		t.Errorf("a stopped server made the routes pending: %+v", r.Routes)
	}
}

func TestAddresses(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.20":          "192.168.1.20",
		" NAS.lan. ":            "nas.lan",
		"http://nas.lan:5000/x": "nas.lan",
		"[fd00::1]":             "fd00::1",
		"[fd00::1]:8080":        "fd00::1",
		"fd00::1":               "fd00::1",
		"homeassistant":         "homeassistant",
	} {
		if got := normalizeAddress(in); got != want {
			t.Errorf("normalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
	for a, ok := range map[string]bool{"192.168.1.20": true, "nas": true, "nas.lan": true, "fd00::1": true, "": false, "a b": false, "-nas": false, "nas_1": false} {
		if validAddress(a) != ok {
			t.Errorf("validAddress(%q) = %v", a, !ok)
		}
	}
}

func TestRoutesToOtherServers(t *testing.T) {
	servers := fakeServers{"panel": publishing(map[string]string{"80/tcp": "8080"})}
	s, _, fake := newService(t, servers)
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{}, nil); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]Route{
		"no server or address": {{Hostname: "x.example.com", Port: 80}},
		"bad address":          {{Hostname: "x.example.com", Address: "nas lan", Port: 80}},
		"no port":              {{Hostname: "x.example.com", Address: "nas.lan"}},
		"port too big":         {{Hostname: "x.example.com", Address: "nas.lan", Port: 70000}},
	} {
		if _, err := s.SetRoutes(ctx, bad); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	res, err := s.SetRoutes(ctx, []Route{
		{Hostname: "nas.example.com", Address: "https://NAS.lan:5001/", Port: 5001, Scheme: "https", NoTLSVerify: true},
		{Hostname: "example.com", Address: "192.168.1.30", Port: 80},
		// An address wins over a server given with it.
		{Hostname: "ha.games.example.org", Server: "panel", Address: "fd00::8", Port: 8123},
		{Hostname: "panel.example.com", Server: "panel", Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RouteState{}
	for _, r := range res.Routes {
		got[r.Hostname] = r
	}
	for host, want := range map[string]string{
		"nas.example.com":      "https://nas.lan:5001",
		"example.com":          "http://192.168.1.30:80",
		"ha.games.example.org": "http://[fd00::8]:8123",
		"panel.example.com":    "http://localhost:8080",
	} {
		if r := got[host]; r.Origin != want || r.State != "published" {
			t.Errorf("%s: %+v, want origin %s", host, r, want)
		}
	}
	if r := got["ha.games.example.org"]; r.Server != "" || r.Address != "fd00::8" {
		t.Errorf("address with a server: %+v", r)
	}
	if recs := fake.Records("z1"); len(recs) != 3 {
		t.Errorf("records: %+v", recs)
	}
	cfg := fake.Config(fake.Tunnels()[0].ID)
	for _, rule := range cfg.Ingress {
		if rule.Hostname == "nas.example.com" && (rule.OriginRequest == nil || !rule.OriginRequest.NoTLSVerify) {
			t.Errorf("self-signed: %+v", rule)
		}
	}

	// Deleting a game server leaves the other servers' hostnames alone.
	if err := s.ForgetServer(ctx, "panel"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Routes(ctx)
	if len(r.Routes) != 3 {
		t.Errorf("routes after forgetting panel: %+v", r.Routes)
	}
}

// DNS records made before the project was renamed, marked with the old
// comment, are still hlm's: they're taken over and get the new comment.
func TestLegacyRecordComment(t *testing.T) {
	servers := fakeServers{"panel": publishing(map[string]string{"80/tcp": "8080"})}
	s, _, fake := newService(t, servers)
	ctx := context.Background()
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Setup(ctx, SetupInput{Name: "home"}, nil); err != nil {
		t.Fatal(err)
	}
	id := fake.Tunnels()[0].ID
	// Pointing at an earlier tunnel, so only the comment says it's hlm's.
	fake.AddRecord("z1", cloudflare.DNSRecord{Type: "CNAME", Name: "old.example.com", Content: "earlier-tunnel.cfargotunnel.com", Proxied: true, Comment: legacyRecordComment})
	fake.AddRecord("z1", cloudflare.DNSRecord{Type: "CNAME", Name: "other.example.com", Content: "someone.cfargotunnel.com", Proxied: true, Comment: "mine"})
	res, err := s.SetRoutes(ctx, []Route{
		{Hostname: "old.example.com", Server: "panel", Port: 8080},
		{Hostname: "other.example.com", Server: "panel", Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]RouteState{}
	for _, r := range res.Routes {
		states[r.Hostname] = r
	}
	if states["old.example.com"].State != "published" || states["other.example.com"].State != "conflict" {
		t.Errorf("states: %+v", states)
	}
	for _, rec := range fake.Records("z1") {
		if rec.Name == "old.example.com" && (rec.Content != id+".cfargotunnel.com" || rec.Comment != RecordComment) {
			t.Errorf("legacy record not taken over: %+v", rec)
		}
		if rec.Name == "other.example.com" && rec.Content != "someone.cfargotunnel.com" {
			t.Errorf("someone else's record changed: %+v", rec)
		}
	}
}
