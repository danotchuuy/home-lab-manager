package proxy

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestWireGuardKeys(t *testing.T) {
	// RFC 7748, section 6.1.
	priv, _ := hex.DecodeString("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	want, _ := hex.DecodeString("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	pub, err := wireGuardPublic(base64.StdEncoding.EncodeToString(priv))
	if err != nil || pub != base64.StdEncoding.EncodeToString(want) {
		t.Errorf("public key = %s, %v", pub, err)
	}
	k, err := newWireGuardKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(k)
	if len(raw) != 32 || raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 {
		t.Errorf("key isn't clamped: %x", raw)
	}
	if _, err := wireGuardPublic("short"); err == nil {
		t.Error("invalid key accepted")
	}
}

func TestValidateWireGuard(t *testing.T) {
	in := WireGuardInput{}
	if err := validateWireGuard(&in); err != nil || in.Network != defaultTunnelNetwork || in.ListenPort != defaultWireGuardPort {
		t.Errorf("defaults: %+v, %v", in, err)
	}
	in = WireGuardInput{Network: "10.9.8.7/24"}
	if err := validateWireGuard(&in); err != nil || in.Network != "10.9.8.0/24" {
		t.Errorf("unmasked network: %+v, %v", in, err)
	}
	for name, bad := range map[string]WireGuardInput{
		"public network":  {Network: "8.8.8.0/24"},
		"IPv6":            {Network: "fd00::/64"},
		"too small":       {Network: "10.0.0.0/31"},
		"bad port":        {ListenPort: 70000},
		"endpoint port":   {Endpoint: "vps.example.com:51820"},
		"endpoint inject": {Endpoint: "x\nPostUp = rm -rf /"},
	} {
		if err := validateWireGuard(&bad); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func testTunnel(t *testing.T) WireGuard {
	t.Helper()
	wg := WireGuard{Network: "10.77.77.0/24", ListenPort: 51820}
	for _, k := range []*string{&wg.ProxyKey, &wg.HomeKey} {
		var err error
		if *k, err = newWireGuardKey(); err != nil {
			t.Fatal(err)
		}
	}
	return wg
}

func TestRenderWireGuard(t *testing.T) {
	wg := testTunnel(t)
	st := Settings{Target: remote.Target{Host: "2001:db8::1"}}
	proxyConf, homeConf, err := RenderWireGuard(wg, st)
	if err != nil {
		t.Fatal(err)
	}
	homePub, _ := wireGuardPublic(wg.HomeKey)
	proxyPub, _ := wireGuardPublic(wg.ProxyKey)
	for _, want := range []string{"Address = 10.77.77.1/24", "ListenPort = 51820", "PrivateKey = " + wg.ProxyKey, "PublicKey = " + homePub, "AllowedIPs = 10.77.77.2/32"} {
		if !strings.Contains(proxyConf, want) {
			t.Errorf("proxy config is missing %q:\n%s", want, proxyConf)
		}
	}
	for _, want := range []string{"Address = 10.77.77.2/24", "PrivateKey = " + wg.HomeKey, "PublicKey = " + proxyPub, "Endpoint = [2001:db8::1]:51820", "AllowedIPs = 10.77.77.1/32", "PersistentKeepalive = 25"} {
		if !strings.Contains(homeConf, want) {
			t.Errorf("home config is missing %q:\n%s", want, homeConf)
		}
	}
	if strings.Contains(proxyConf, wg.HomeKey) || strings.Contains(homeConf, wg.ProxyKey) {
		t.Error("a private key ended up on the other end")
	}
	wg.Endpoint = "203.0.113.9"
	if _, homeConf, _ := RenderWireGuard(wg, st); !strings.Contains(homeConf, "Endpoint = 203.0.113.9:51820") {
		t.Errorf("endpoint override:\n%s", homeConf)
	}
}

func TestPlanThroughWireGuard(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	direct := server(map[string]string{"27015/udp": "27015"})
	direct.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2466"}), "lan": direct}, slog.New(slog.DiscardHandler))
	st, err := s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "1.2.3.4", UpstreamSource: "wireguard", UpstreamPorts: "internet", Servers: []string{"a", "lan"}})
	if err != nil || st.Upstream != "" || st.UpstreamPorts != "local" {
		t.Fatalf("save: %+v, %v", st, err)
	}
	if _, _, err := s.Plan(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't set up") {
		t.Errorf("plan without a tunnel: %v", err)
	}
	if err := store.Set(secretWireGuard, testTunnel(t)); err != nil {
		t.Fatal(err)
	}
	st, plan, err := s.Plan(context.Background())
	if err != nil || st.Upstream != "10.77.77.2" {
		t.Fatalf("plan: %+v, %v", st, err)
	}
	if c := Render(st, plan); !strings.Contains(c, "listen 2466 udp;\n    proxy_pass 10.77.77.2:2466;") {
		t.Errorf("config:\n%s", c)
	}
	if len(plan) != 2 || plan[1].Server != "lan" || plan[1].Error == "" || len(plan[1].Ports) != 0 {
		t.Errorf("server with its own LAN address: %+v", plan)
	}
}

func TestWantedRulesKeepTheTunnelOpen(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	if rules := s.wantedRules(nil); len(rules) != 0 {
		t.Errorf("without a tunnel: %+v", rules)
	}
	wg := testTunnel(t)
	wg.ListenPort = 51999
	store.Set(secretWireGuard, wg)
	rules := s.wantedRules(nil)
	if len(rules) != 1 || rules[0].Label != "hlm-wireguard" || rules[0].Protocol != "UDP" || rules[0].Ports != "51999" {
		t.Errorf("with a tunnel: %+v", rules)
	}
	if others, ours := splitRules(rules); len(others) != 0 || len(ours) != 1 {
		t.Error("the tunnel's rule isn't recognised as hlm's")
	}
}

// fakeDocker records what's done to the home end's container.
type fakeDocker struct {
	created  *docker.CreateRequest
	files    map[string]string
	started  bool
	removed  int
	volumes  int
	createFn func() error
	networks []docker.Network
	joined   []string
	stopped  int
	// cleanups counts the containers that delete a leftover gsh0.
	cleanups int
	logs     string
	// routes is what the cleanup container prints as the host's routes.
	routes string
}

func (f *fakeDocker) ListNetworks(context.Context) ([]docker.Network, error) { return f.networks, nil }
func (f *fakeDocker) Logs(_ context.Context, id string, _ docker.LogsOptions) (io.ReadCloser, error) {
	if id == "cleanup" {
		return io.NopCloser(strings.NewReader(f.routes)), nil
	}
	if f.logs != "" {
		return io.NopCloser(strings.NewReader(f.logs)), nil
	}
	return io.NopCloser(strings.NewReader("[#] ip link add gsh0 type wireguard\n**** All tunnels are now active ****\nnot read\n")), nil
}
func (f *fakeDocker) StopContainer(_ context.Context, id string, _ int) error {
	if f.created == nil || id == legacyWireGuardContainer {
		return &docker.APIError{Status: 404}
	}
	f.stopped++
	f.started = false
	return nil
}
func (f *fakeDocker) WaitContainer(context.Context, string) (int, error) { return 1, nil }
func (f *fakeDocker) ConnectNetwork(_ context.Context, network, _ string, _ any) error {
	f.joined = append(f.joined, network)
	return nil
}

func (f *fakeDocker) PullImage(context.Context, string, func(docker.PullProgress)) error {
	return errors.New("offline")
}
func (f *fakeDocker) InspectContainer(_ context.Context, id string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	if f.created == nil {
		return info, &docker.APIError{Status: 404, Message: "no such container"}
	}
	info.State.Status, info.State.Running = "running", f.started
	info.Config.Labels = f.created.Labels
	return info, nil
}
func (f *fakeDocker) CreateContainer(_ context.Context, name string, req docker.CreateRequest) (string, []string, error) {
	if name == WireGuardContainer+"-cleanup" {
		if req.HostConfig.NetworkMode != "host" || !strings.Contains(strings.Join(req.Entrypoint, " "), "ip link del gsh0") {
			return "", nil, errors.New("unexpected cleanup container")
		}
		f.cleanups++
		return "cleanup", nil, nil
	}
	if f.createFn != nil {
		if err := f.createFn(); err != nil {
			return "", nil, err
		}
	}
	f.created = &req
	return "id-" + name, nil, nil
}
func (f *fakeDocker) CopyTo(_ context.Context, _, dir string, r io.Reader) error {
	f.files = map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(tr)
		f.files[dir+"/"+h.Name] = string(b)
	}
}
func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	if id != "cleanup" {
		f.started = true
	}
	return nil
}
func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	if strings.HasPrefix(id, "cleanup") || strings.HasSuffix(id, "-cleanup") {
		return nil
	}
	if id == legacyWireGuardContainer {
		return &docker.APIError{Status: 404}
	}
	f.removed++
	f.created, f.started = nil, false
	return &docker.APIError{Status: 404}
}
func (f *fakeDocker) RemoveVolume(_ context.Context, name string) error {
	if name == legacyWireGuardContainer {
		return &docker.APIError{Status: 404}
	}
	f.volumes++
	return nil
}
func (f *fakeDocker) RenameContainer(context.Context, string, string) error { return nil }

func TestSetupAndRemoveWireGuard(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	fd := &fakeDocker{}
	s.UseDocker(fd, "")
	var scripts []string
	var proxyConf string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "wg-quick up") {
			proxyConf = string(stdin)
			return remote.Result{Stdout: "hlm:wgup\n"}, nil
		}
		if strings.Contains(script, "latest-handshakes") {
			wg, _, _ := s.tunnel()
			pub, _ := wireGuardPublic(wg.HomeKey)
			return remote.Result{Stdout: "other=\t1600000000\n" + pub + "\t1700000000\nhlm:ping=yes\n"}, nil
		}
		return remote.Result{}, nil
	}
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("before the proxy is set up: %v", err)
	}
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, Upstream: "203.0.113.7"})

	res, err := s.SetupWireGuard(context.Background(), WireGuardInput{UseForRelays: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proxyConf, "ListenPort = 51820") {
		t.Errorf("proxy config:\n%s", proxyConf)
	}
	home := fd.files["/config/wg_confs/gsh0.conf"]
	if !strings.Contains(home, "Endpoint = vps.example.com:51820") || !fd.started {
		t.Errorf("home end: started=%v config:\n%s", fd.started, home)
	}
	if c := fd.created; c.Image != DefaultWireGuardImage || c.HostConfig.NetworkMode != "host" || c.HostConfig.CapAdd[0] != "NET_ADMIN" || c.Labels[deploy.LabelManaged] != "" {
		t.Errorf("container: %+v", c)
	}
	// Not the image's init, whose kill switch deletes the host's default
	// route when a tunnel fails.
	if c := fd.created; len(c.Entrypoint) != 3 || c.Entrypoint[2] != homeScript || strings.Contains(homeScript, "route del") {
		t.Errorf("entrypoint: %q", c.Entrypoint)
	}
	if !res.Status.Configured || !res.Status.InUse || res.Status.HomeAddress != "10.77.77.2" || !res.Status.Home.Running || res.Apply != nil {
		t.Errorf("status: %+v", res)
	}
	if st, _, _ := s.Settings(); st.UpstreamSource != "wireguard" || st.UpstreamPorts != "local" {
		t.Errorf("settings: %+v", st)
	}

	// Setting up again keeps the keys.
	first, _, _ := s.tunnel()
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{ListenPort: 51821}, nil); err != nil {
		t.Fatal(err)
	}
	second, _, _ := s.tunnel()
	if second.ProxyKey != first.ProxyKey || second.HomeKey != first.HomeKey || second.ListenPort != 51821 {
		t.Errorf("setting up again: %+v", second)
	}

	check, err := s.CheckWireGuard(context.Background())
	if err != nil || check.Handshake.Unix() != 1700000000 || check.Reachable == nil || !*check.Reachable {
		t.Errorf("check: %+v, %v", check, err)
	}

	// Relays applied through the tunnel keep it from being removed.
	s.recordApplied("10.77.77.2", nil)
	if _, err := s.RemoveWireGuard(context.Background()); err == nil {
		t.Error("removed the tunnel the relays use")
	}
	s.recordApplied("", nil)
	res, err = s.RemoveWireGuard(context.Background())
	if err != nil || res.Status.Configured || fd.created != nil {
		t.Fatalf("remove: %+v, %v", res, err)
	}
	if st, _, _ := s.Settings(); st.UpstreamSource != "manual" {
		t.Errorf("home address after removing the tunnel: %+v", st)
	}
	if !strings.Contains(scripts[len(scripts)-1], "rm -f /etc/wireguard/gsh0.conf") {
		t.Errorf("last script: %s", scripts[len(scripts)-1])
	}
}

func TestSetupWireGuardHomeEndFails(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.UseDocker(&fakeDocker{createFn: func() error { return errors.New("no such image") }}, "")
	s.run = func(context.Context, string, []byte, func(string)) (remote.Result, error) {
		return remote.Result{Stdout: "hlm:wgup\n"}, nil
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7"})
	_, err := s.SetupWireGuard(context.Background(), WireGuardInput{UseForRelays: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "pull "+DefaultWireGuardImage) {
		t.Errorf("err = %v", err)
	}
	// The proxy end's keys are kept for the next try; the relays still go
	// to the old address.
	if _, ok, _ := s.tunnel(); !ok {
		t.Error("tunnel forgotten")
	}
	if st, _, _ := s.Settings(); st.UpstreamSource != "manual" {
		t.Errorf("settings: %+v", st)
	}
}

func lanDocker() *fakeDocker {
	net := func(name, driver, subnet, gw string) docker.Network {
		n := docker.Network{Name: name, Driver: driver, Options: map[string]string{"parent": "eth0"}}
		n.IPAM.Config = []docker.NetworkIPAMRange{{Subnet: subnet, Gateway: gw}}
		return n
	}
	return &fakeDocker{networks: []docker.Network{
		net("bridge", "bridge", "172.17.0.0/16", "172.17.0.1"),
		net("br0", "macvlan", "192.168.1.0/24", "192.168.1.1"),
		net("games", "bridge", "172.20.0.0/16", "172.20.0.1"),
	}}
}

func TestWireGuardLANMode(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	onLAN := func(ip string) docker.ContainerJSON {
		info := server(nil)
		info.HostConfig.NetworkMode = "br0"
		info.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
		info.Config.ExposedPorts = map[string]struct{}{"2456/udp": {}}
		info.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: ip}}
		return info
	}
	s := New(store, fakeServers{
		"valheim1": onLAN("192.168.1.61"),
		"valheim2": onLAN("192.168.1.62"),
		"bridged":  server(map[string]string{"27015/udp": "27016"}),
	}, slog.New(slog.DiscardHandler))
	// The second server's internet port is moved, since both use 2456.
	s.UsePorts(func(info docker.ContainerJSON) ([]forwarding.Mapping, error) {
		ports := forwarding.Mappings(info, nil)
		if info.NetworkSettings.Networks["br0"].IPAddress == "192.168.1.62" {
			ports[0].Port = 2466
		}
		return ports, nil
	})
	fd := lanDocker()
	s.UseDocker(fd, "")
	var proxyConf string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		proxyConf = string(stdin)
		return remote.Result{Stdout: "hlm:wgup\n"}, nil
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Servers: []string{"valheim1", "valheim2", "bridged"}})

	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{LANNetwork: "games"}, nil); err == nil {
		t.Error("a bridge network accepted as the LAN")
	}
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{LANNetwork: "br0", Network: "192.168.1.0/28"}, nil); err == nil {
		t.Error("tunnel network overlapping the LAN accepted")
	}
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{LANNetwork: "br0", LANAddress: "10.0.0.5"}, nil); err == nil {
		t.Error("LAN address outside the network accepted")
	}
	res, err := s.SetupWireGuard(context.Background(), WireGuardInput{LANNetwork: "br0", LANAddress: "192.168.1.250", UseForRelays: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proxyConf, "AllowedIPs = 10.77.77.2/32, 192.168.1.0/24") {
		t.Errorf("proxy config:\n%s", proxyConf)
	}
	home := fd.files["/config/wg_confs/gsh0.conf"]
	for _, want := range []string{
		"PostUp = iptables -t nat -A PREROUTING -i %i -d 10.77.77.2 -j DNAT --to-destination 172.17.0.1; iptables -t nat -A POSTROUTING -s 10.77.77.0/24 ! -o %i -j MASQUERADE",
		"PostDown = iptables -t nat -D PREROUTING",
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home config is missing %q:\n%s", want, home)
		}
	}
	c := fd.created
	if c.HostConfig.NetworkMode != "br0" || c.HostConfig.Sysctls["net.ipv4.ip_forward"] != "1" ||
		c.NetworkingConfig.EndpointsConfig["br0"].IPAMConfig.IPv4Address != "192.168.1.250" || len(fd.joined) != 1 || fd.joined[0] != "bridge" {
		t.Errorf("container: %+v, joined %v", c, fd.joined)
	}
	if len(res.Status.LANNetworks) != 1 || res.Status.LANNetworks[0].Name != "br0" || res.Status.LANSubnet != "192.168.1.0/24" {
		t.Errorf("status: %+v", res.Status)
	}

	// Each server keeps its own LAN address and port; only the internet
	// ports differ.
	st, plan, err := s.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conf := Render(st, plan)
	for _, want := range []string{
		"listen 2456 udp;\n    proxy_pass 192.168.1.61:2456;",
		"listen 2466 udp;\n    proxy_pass 192.168.1.62:2456;",
		"listen 27016 udp;\n    proxy_pass 10.77.77.2:27016;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config is missing %q:\n%s", want, conf)
		}
	}

	// Back on the host's network, LAN servers can't be reached.
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{}, nil); err != nil {
		t.Fatal(err)
	}
	if fd.created.HostConfig.NetworkMode != "host" || strings.Contains(fd.files["/config/wg_confs/gsh0.conf"], "PostUp") {
		t.Errorf("host mode: %+v", fd.created)
	}
	_, plan, _ = s.Plan(context.Background())
	for _, p := range plan {
		if strings.HasPrefix(p.Server, "valheim") && (p.Error == "" || p.Address != "") {
			t.Errorf("%s in host mode: %+v", p.Server, p)
		}
	}
}

func TestSetupWireGuardReportsProgress(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.UseDocker(&fakeDocker{}, "")
	s.run = func(_ context.Context, _ string, _ []byte, onLine func(string)) (remote.Result, error) {
		if onLine == nil {
			return remote.Result{}, nil
		}
		for _, l := range []string{"Setting up wireguard-tools (1.0.20210914-1ubuntu4) ...", "hlm:wgup"} {
			onLine(l)
		}
		return remote.Result{Stdout: "hlm:wgup\n"}, nil
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7"})
	var got []string
	if _, err := s.SetupWireGuard(context.Background(), WireGuardInput{}, func(e WireGuardEvent) {
		got = append(got, e.Stage+" "+e.Message)
	}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"step Setting up WireGuard on the proxy server vps",
		"output Setting up wireguard-tools (1.0.20210914-1ubuntu4) ...",
		"step Pulling " + DefaultWireGuardImage,
		"output Pulling failed (offline); trying the image already on the host",
		"step Creating the hlm-wireguard container",
		"step Starting it",
		"output [#] ip link add gsh0 type wireguard",
		"output **** All tunnels are now active ****",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "hlm:wgup") || strings.Contains(all, "not read") {
		t.Errorf("markers or later log lines reported:\n%s", all)
	}
}

func TestRouteFor(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	lan := server(nil)
	lan.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	lan.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: "192.168.1.61"}}
	// The same, on another host (whose end can't reach the LAN too).
	far := server(nil)
	far.ID = "abc@nas"
	far.Config.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	far.NetworkSettings.Networks = map[string]docker.EndpointSettings{"br0": {IPAddress: "192.168.1.62"}}
	s := New(store, fakeServers{"bridged": server(map[string]string{"2456/udp": "2456"}), "lan": lan, "enshrouded@nas": far}, slog.New(slog.DiscardHandler))
	if _, err := s.RouteFor(context.Background(), "bridged"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("before setup: %v", err)
	}
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, Upstream: "203.0.113.7"})
	if r, _ := s.RouteFor(context.Background(), "bridged"); r.Source != "manual" || r.Target != "203.0.113.7" || r.OwnPorts || r.ProxyHost != "vps.example.com" {
		t.Errorf("manual: %+v", r)
	}
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, UpstreamSource: "wireguard"})
	if r, _ := s.RouteFor(context.Background(), "bridged"); r.Error == "" {
		t.Errorf("tunnel not set up: %+v", r)
	}
	wg := testTunnel(t)
	store.Set(secretWireGuard, wg)
	if r, _ := s.RouteFor(context.Background(), "bridged"); r.Target != "10.77.77.2" || !r.OwnPorts || r.Error != "" {
		t.Errorf("tunnel: %+v", r)
	}
	if r, _ := s.RouteFor(context.Background(), "lan"); r.Error == "" {
		t.Errorf("LAN server without LAN mode: %+v", r)
	}
	wg.LANNetwork, wg.LANSubnet = "br0", "192.168.1.0/24"
	store.Set(secretWireGuard, wg)
	if r, _ := s.RouteFor(context.Background(), "lan"); r.Target != "192.168.1.61" || !r.OwnPorts || !r.ServerAddress || r.Error != "" {
		t.Errorf("LAN mode: %+v", r)
	}
	// A server on another host's br0, whose end doesn't reach the LAN
	// (only one end may), is reached through this host's end, which does.
	wg.Hosts = map[string]WireGuardEnd{"nas": {Address: "10.77.77.3"}}
	store.Set(secretWireGuard, wg)
	if r, _ := s.RouteFor(context.Background(), "enshrouded@nas"); r.Target != "192.168.1.62" || !r.ServerAddress || r.Error != "" {
		t.Errorf("LAN server on another host: %+v", r)
	}
}

func TestRecreateWireGuardHome(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	fd := &fakeDocker{networks: []docker.Network{
		{Name: "bridge", Driver: "bridge", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "172.17.0.0/16", Gateway: "172.17.0.1"}}}},
	}}
	s.UseDocker(fd, "")
	ctx := context.Background()
	noop := func(string) {}
	if err := s.RecreateWireGuardHome(ctx, noop, noop); err == nil || !strings.Contains(err.Error(), "isn't set up") {
		t.Errorf("without a tunnel: %v", err)
	}
	if _, err := s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	wg := testTunnel(t)
	store.Set(secretWireGuard, wg)

	// The first time there's no old container to stop.
	var steps []string
	if err := s.RecreateWireGuardHome(ctx, func(m string) { steps = append(steps, m) }, noop); err != nil {
		t.Fatal(err)
	}
	if fd.stopped != 0 || fd.cleanups != 1 || !fd.started || fd.created.HostConfig.NetworkMode != "host" {
		t.Errorf("first: stopped %d, cleanups %d, started %v", fd.stopped, fd.cleanups, fd.started)
	}
	// Then the running one is stopped gracefully before it's removed, and a
	// leftover gsh0 is cleared before the new one starts.
	if err := s.RecreateWireGuardHome(ctx, noop, noop); err != nil {
		t.Fatal(err)
	}
	if fd.stopped != 1 || fd.cleanups != 2 || !fd.started {
		t.Errorf("again: stopped %d, cleanups %d, started %v", fd.stopped, fd.cleanups, fd.started)
	}
	if !strings.Contains(strings.Join(steps, "\n"), "Stopping the old hlm-wireguard container") {
		t.Errorf("steps: %v", steps)
	}

	// A tunnel that fails to come up fails the recreate.
	fd.logs = "wg-quick: `gsh0' already exists\n**** Tunnel /config/wg_confs/gsh0.conf failed, will stop all others! ****\n"
	err := s.RecreateWireGuardHome(ctx, noop, noop)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("failed tunnel: %v", err)
	}

	// In LAN mode gsh0 lives in the container's own network namespace, so
	// there's nothing to clear on the host.
	fd.logs = ""
	fd.networks = append(fd.networks, docker.Network{Name: "lan", Driver: "macvlan", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "192.168.1.0/24"}}}})
	wg.LANNetwork, wg.LANSubnet, wg.BridgeGateway = "lan", "192.168.1.0/24", "172.17.0.1"
	store.Set(secretWireGuard, wg)
	cleanups := fd.cleanups
	if err := s.RecreateWireGuardHome(ctx, noop, noop); err != nil {
		t.Fatal(err)
	}
	if fd.cleanups != cleanups || fd.created.HostConfig.NetworkMode != "lan" {
		t.Errorf("LAN mode: cleanups %d → %d, network %s", cleanups, fd.cleanups, fd.created.HostConfig.NetworkMode)
	}
}

func TestRouteConflict(t *testing.T) {
	routes := `default via 10.0.0.1 dev ens18 proto dhcp src 10.0.14.42 metric 100
1.0.0.1 via 10.0.0.1 dev ens18 proto dhcp src 10.0.14.42 metric 100
10.0.0.0/20 dev ens18 proto kernel scope link src 10.0.14.42 metric 100
10.199.199.0/24 dev gsh0 proto kernel scope link src 10.199.199.2
unreachable 192.168.0.0/16
172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1
`
	for n, want := range map[string]string{
		"10.77.77.0/24":   "",
		"10.199.199.0/24": "", // the tunnel's own
		"192.168.1.0/24":  "",
		"10.0.0.0/24":     "default gateway 10.0.0.1",
		"10.0.14.0/24":    "overlaps the network 10.0.0.0/20 on ens18",
		"10.0.0.0/16":     "default gateway",
		"172.17.5.0/24":   "docker0",
	} {
		err := routeConflict(routes, []netip.Prefix{netip.MustParsePrefix(n)})
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, want %q", n, err, want)
		}
	}
}

func TestWireGuardKeepsHostNetworking(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	fd := &fakeDocker{routes: "default via 10.77.77.1 dev eth0\n10.77.77.0/24 dev eth0 scope link\n"}
	s.UseDocker(fd, "")
	proxyRoutes := ""
	s.run = func(_ context.Context, script string, _ []byte, _ func(string)) (remote.Result, error) {
		if script == routesScript {
			return remote.Result{Stdout: proxyRoutes}, nil
		}
		return remote.Result{Stdout: "hlm:wgup\n"}, nil
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7"})
	ctx := context.Background()

	// The host's LAN is the default tunnel network: the home end isn't
	// started.
	_, err := s.SetupWireGuard(ctx, WireGuardInput{}, nil)
	if err == nil || !strings.Contains(err.Error(), "on this host, the default gateway 10.77.77.1") || fd.started || fd.created != nil {
		t.Errorf("overlapping the host: %v, started %v", err, fd.started)
	}
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{Network: "10.88.0.0/24"}, nil); err != nil || !fd.started {
		t.Errorf("another network: %v", err)
	}

	// Nor is the proxy end, if it overlaps the proxy server's networks.
	proxyRoutes = "10.88.0.0/16 dev eth1 scope link\n"
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{Network: "10.88.0.0/24"}, nil); err == nil || !strings.Contains(err.Error(), "on the proxy server") {
		t.Errorf("overlapping the proxy server: %v", err)
	}
	proxyRoutes = ""
	if _, err := s.SetupWireGuard(ctx, WireGuardInput{Network: "10.88.0.0/24", Endpoint: "10.88.0.9"}, nil); err == nil || !strings.Contains(err.Error(), "is in the tunnel network") {
		t.Errorf("endpoint in the tunnel: %v", err)
	}
}

func TestUpgradeWireGuardHome(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	fd := &fakeDocker{}
	s.UseDocker(fd, "")
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "1.2.3.4"})
	ctx := context.Background()
	s.UpgradeWireGuardHome(ctx) // no tunnel: nothing to do
	if fd.created != nil {
		t.Fatal("created a home end without a tunnel")
	}
	store.Set(secretWireGuard, testTunnel(t))
	// One made before homeScript, running the image's init.
	fd.created = &docker.CreateRequest{Labels: map[string]string{LabelWireGuard: "home"}}
	s.UpgradeWireGuardHome(ctx)
	if fd.stopped != 1 || fd.created == nil || fd.created.Labels[labelHomeVersion] != homeVersion || !fd.started {
		t.Fatalf("not upgraded: stopped %d, %+v", fd.stopped, fd.created)
	}
	s.UpgradeWireGuardHome(ctx)
	if fd.stopped != 1 {
		t.Error("an up-to-date home end recreated")
	}
}
