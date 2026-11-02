package forwarding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/unifi"
	"github.com/danotchuuy/home-lab-manager/internal/unifi/unifitest"
)

type fakeServers map[string]docker.ContainerJSON

func (f fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	info, ok := f[id]
	if !ok {
		return info, errors.New("no such server")
	}
	return info, nil
}

func bridgeServer(name string, bindings map[string]string) docker.ContainerJSON {
	var info docker.ContainerJSON
	info.Name = "/" + name
	info.HostConfig.NetworkMode = "bridge"
	info.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	for container, host := range bindings {
		info.HostConfig.PortBindings[container] = []docker.PortBinding{{HostPort: host}, {HostIP: "::", HostPort: host}}
	}
	return info
}

func TestGroupPorts(t *testing.T) {
	m := func(port, local int, proto string) Mapping {
		return Mapping{Port: port, Local: local, Proto: proto, Forward: true}
	}
	ports := []Mapping{
		m(2456, 2456, "udp"), m(2457, 2457, "udp"), m(2458, 2458, "udp"),
		m(2456, 2456, "tcp"), m(2457, 2457, "tcp"), m(2458, 2458, "tcp"),
		m(27015, 27015, "udp"), m(27015, 27015, "udp"), // duplicate
		m(8080, 80, "tcp"),
	}
	got := groupPorts(ports, "10.0.0.5")
	var keys []string
	for _, w := range got {
		keys = append(keys, w.Key+" "+w.DstPort+">"+w.FwdPort+" "+w.Proto)
	}
	want := "2456-2458/tcp+udp 2456-2458>2456-2458 tcp_udp | 8080/tcp 8080>80 tcp | 27015/udp 27015>27015 udp"
	if strings.Join(keys, " | ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(keys, " | "), want)
	}
}

// setup returns a service connected to a fake controller, with UniFi saved.
func setup(t *testing.T, servers fakeServers) (*Service, *unifitest.Fake, *httptest.Server) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	fake, srv := unifitest.New(t, true)
	s := New(store, servers, func() string { return "192.168.1.20" }, slog.New(slog.DiscardHandler))

	cfg := Config{Settings: unifi.Settings{URL: srv.URL, AuthType: "password", Username: "admin", Password: "pw"}}
	_, err = s.Test(context.Background(), cfg)
	var certErr *unifi.CertError
	if !errors.As(err, &certErr) {
		t.Fatalf("first test should ask to trust the certificate: %v", err)
	}
	cfg.Fingerprint = certErr.Fingerprint
	pub, err := s.Save(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Configured || !pub.HasPassword || pub.Interface != "wan" || pub.DetectedHostIP != "192.168.1.20" {
		t.Errorf("public config = %+v", pub)
	}
	return s, fake, srv
}

func TestSyncAndRemove(t *testing.T) {
	servers := fakeServers{
		"valheim": bridgeServer("valheim", map[string]string{"2456/udp": "2456", "2457/udp": "2457", "2458/udp": "2458"}),
		"cs":      bridgeServer("cs", map[string]string{"27015/tcp": "27015", "27015/udp": "27015"}),
	}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()

	st, err := s.Status(ctx, "valheim")
	if err != nil || st.Target != "192.168.1.20" || len(st.Rules) != 1 || st.Rules[0].State != "missing" {
		t.Fatalf("status before = %+v, %v", st, err)
	}

	st, err = s.Sync(ctx, "valheim")
	if err != nil || st.Rules[0].State != "forwarded" {
		t.Fatalf("after sync = %+v, %v", st, err)
	}
	rules := fake.Rules()
	if len(rules) != 1 || rules[0].Name != "hlm:valheim 2456-2458/udp" || rules[0].Fwd != "192.168.1.20" ||
		rules[0].DstPort != "2456-2458" || rules[0].Proto != "udp" || !rules[0].Enabled || rules[0].Interface != "wan" {
		t.Errorf("rules = %+v", rules)
	}

	// A rule made by hand on the same WAN port is reported, not overwritten.
	fake.Add(unifi.PortForward{Name: "my old cs server", Enabled: true, DstPort: "27015", Fwd: "192.168.1.99", FwdPort: "27015", Proto: "tcp_udp"})
	st, _ = s.Status(ctx, "cs")
	if len(st.Rules) != 1 || len(st.Rules[0].Conflicts) != 1 {
		t.Fatalf("conflict status = %+v", st)
	}
	if _, err := s.Sync(ctx, "cs"); err == nil || !strings.Contains(err.Error(), "my old cs server") {
		t.Errorf("sync with conflict: err = %v", err)
	}

	// The server's ports change: the old rule is replaced.
	servers["valheim"] = bridgeServer("valheim", map[string]string{"2456/udp": "3456", "2457/udp": "3457", "2458/udp": "3458"})
	st, _ = s.Status(ctx, "valheim")
	if len(st.Stale) != 1 || st.Rules[0].State != "missing" {
		t.Errorf("after port change = %+v", st)
	}
	if st, err = s.Sync(ctx, "valheim"); err != nil || len(st.Stale) != 0 || st.Rules[0].State != "forwarded" {
		t.Errorf("resync = %+v, %v", st, err)
	}

	// Remove deletes every rule the server owns, including copies (e.g. a
	// rule duplicated in the UniFi UI), and nothing else.
	for _, r := range fake.Rules() {
		if strings.HasPrefix(r.Name, "hlm:valheim") {
			fake.Add(r)
		}
	}
	n, err := s.Remove(ctx, "valheim")
	if err != nil || n != 2 {
		t.Errorf("remove = %d, %v", n, err)
	}
	for _, r := range fake.Rules() {
		if strings.HasPrefix(r.Name, "hlm:valheim") {
			t.Errorf("rule left behind: %+v", r)
		}
	}
	if len(fake.Rules()) != 1 {
		t.Errorf("the hand-made rule should remain: %+v", fake.Rules())
	}
}

func TestOutdatedRuleIsUpdated(t *testing.T) {
	servers := fakeServers{"mc": bridgeServer("mc", map[string]string{"25565/tcp": "25565"})}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()
	fake.Add(unifi.PortForward{Name: "hlm:mc 25565/tcp", Enabled: false, Src: "203.0.113.0/24", DstPort: "25565", Fwd: "192.168.1.5", FwdPort: "25565", Proto: "tcp"})
	st, _ := s.Status(ctx, "mc")
	if st.Rules[0].State != "outdated" {
		t.Fatalf("state = %s", st.Rules[0].State)
	}
	if _, err := s.Sync(ctx, "mc"); err != nil {
		t.Fatal(err)
	}
	r := fake.Rules()[0]
	if !r.Enabled || r.Fwd != "192.168.1.20" || r.Src != "203.0.113.0/24" {
		t.Errorf("updated rule = %+v (source restriction should be kept)", r)
	}
}

func TestLegacyRuleIsRenamed(t *testing.T) {
	servers := fakeServers{"mc": bridgeServer("mc", map[string]string{"25565/tcp": "25565"})}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()
	fake.Add(unifi.PortForward{Name: "gsh:mc 25565/tcp", Enabled: true, Src: "203.0.113.0/24", DstPort: "25565", Fwd: "192.168.1.20", FwdPort: "25565", Proto: "tcp"})
	st, _ := s.Status(ctx, "mc")
	if st.Rules[0].State != "forwarded" || len(st.Stale) != 0 {
		t.Fatalf("status = %+v", st)
	}
	if _, err := s.Sync(ctx, "mc"); err != nil {
		t.Fatal(err)
	}
	if r := fake.Rules(); len(r) != 1 || r[0].Name != "hlm:mc 25565/tcp" || r[0].Src != "203.0.113.0/24" {
		t.Errorf("rules = %+v", r)
	}
	fake.Add(unifi.PortForward{Name: "gsh:mc 1/tcp", DstPort: "1", Fwd: "192.168.1.20", FwdPort: "1", Proto: "tcp"})
	if n, err := s.Remove(ctx, "mc"); n != 2 || err != nil {
		t.Errorf("remove = %d, %v", n, err)
	}
}

func TestLANNetworkServer(t *testing.T) {
	var info docker.ContainerJSON
	info.Name = "/ark"
	info.HostConfig.NetworkMode = "lan"
	info.Config.ExposedPorts = map[string]struct{}{"7777/udp": {}, "7778/udp": {}, "27015/udp": {}}
	info.Config.Labels = map[string]string{
		deploy.LabelDirectIP: "true",
		deploy.LabelRequest:  `{"name":"ark","ipAddress":"192.168.1.60"}`,
	}
	s := &Service{hostIP: func() string { return "192.168.1.20" }}
	wants, target, note := s.Wants(info, Config{}, Mappings(info, nil))
	if target != "192.168.1.60" || note != "" || len(wants) != 2 || wants[0].Key != "7777-7778/udp" {
		t.Errorf("wants = %+v target=%s note=%q", wants, target, note)
	}

	delete(info.Config.Labels, deploy.LabelRequest) // no fixed IP, not running
	if wants, target, note := s.Wants(info, Config{}, Mappings(info, nil)); len(wants) != 0 || target != "" || !strings.Contains(note, "fixed IP") {
		t.Errorf("without fixed IP: %+v %q %q", wants, target, note)
	}
}

func TestCredentialsAreKeptOnlyForTheSameController(t *testing.T) {
	s, _, srv := setup(t, fakeServers{})
	pub := s.PublicConfig()

	// Re-saving without re-entering the password keeps it.
	cfg := Config{Settings: unifi.Settings{URL: srv.URL, AuthType: "password", Username: "admin", Fingerprint: pub.Fingerprint}}
	merged, err := s.merge(cfg)
	if err != nil || merged.Password != "pw" {
		t.Errorf("same controller: password kept = %q, %v", merged.Password, err)
	}
	// A different URL must not receive the saved password.
	cfg.URL = "https://198.51.100.7"
	if merged, err := s.merge(cfg); err == nil || merged.Password != "" {
		t.Errorf("other controller got the saved password (err=%v)", err)
	}
	if err := s.Delete(); err != nil || s.Configured() {
		t.Errorf("delete: %v, configured=%v", err, s.Configured())
	}
	if st, err := s.Status(context.Background(), "x"); err != nil || st.Configured {
		t.Errorf("status when not configured = %+v, %v", st, err)
	}
}

// UniFi OS rate-limits sign-ins; a whole page load + "Forward ports" click
// must not sign in more than once.
func TestOneSignInPerSession(t *testing.T) {
	servers := fakeServers{"valheim": bridgeServer("valheim", map[string]string{"2456/udp": "2456", "2457/udp": "2457"})}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()
	before := fake.Logins()
	fake.LimitLogins(before + 1)

	if _, err := s.Status(ctx, "valheim"); err != nil { // the tab loading
		t.Fatal(err)
	}
	st, err := s.Sync(ctx, "valheim") // "Forward ports"
	if err != nil || st.Rules[0].State != "forwarded" {
		t.Fatalf("sync = %+v, %v", st, err)
	}
	if _, err := s.Status(ctx, "valheim"); err != nil {
		t.Fatal(err)
	}
	if n := fake.Logins() - before; n != 1 {
		t.Errorf("signed in %d times, want 1", n)
	}
}

func TestWANAddress(t *testing.T) {
	s, fake, _ := setup(t, fakeServers{})
	ctx := context.Background()
	if _, err := s.WANAddress(ctx, ""); err == nil || !strings.Contains(err.Error(), "doesn't report") {
		t.Errorf("no WAN reported: %v", err)
	}
	fake.SetWAN("wan", "203.0.113.7")
	fake.SetWAN("wan2", "198.51.100.20")
	if ip, err := s.WANAddress(ctx, ""); err != nil || ip != "203.0.113.7" {
		t.Errorf("wan = %q, %v", ip, err)
	}
	if ip, err := s.WANAddress(ctx, "wan2"); err != nil || ip != "198.51.100.20" {
		t.Errorf("wan2 = %q, %v", ip, err)
	}
	for _, bad := range []string{"192.168.0.10", "100.72.1.2", "10.0.0.1"} {
		fake.SetWAN("wan", bad)
		if _, err := s.WANAddress(ctx, "wan"); err == nil || !strings.Contains(err.Error(), "isn't reachable") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

// Rules forward to the port published on the host, not the container's.
func TestRemappedHostPort(t *testing.T) {
	servers := fakeServers{"web": bridgeServer("web", map[string]string{"80/tcp": "8080"})}
	s, fake, _ := setup(t, servers)
	if _, err := s.Sync(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if r := fake.Rules()[0]; r.DstPort != "8080" || r.FwdPort != "8080" {
		t.Errorf("rule = %s>%s, want 8080>8080", r.DstPort, r.FwdPort)
	}
}

// Two servers of the same game on their own LAN IPs use the same ports; one
// gets other internet ports.
func TestInternetPorts(t *testing.T) {
	lan := func(name, ip string) docker.ContainerJSON {
		var info docker.ContainerJSON
		info.Name = "/" + name
		info.HostConfig.NetworkMode = "lan"
		info.Config.ExposedPorts = map[string]struct{}{"2456/udp": {}, "2457/udp": {}, "2458/udp": {}}
		info.Config.Labels = map[string]string{deploy.LabelDirectIP: "true", deploy.LabelRequest: `{"ipAddress":"` + ip + `"}`}
		return info
	}
	servers := fakeServers{"valheim1": lan("valheim1", "192.168.1.61"), "valheim2": lan("valheim2", "192.168.1.62")}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()
	if _, err := s.Sync(ctx, "valheim1"); err != nil {
		t.Fatal(err)
	}
	// Same internet ports: a conflict.
	if _, err := s.Sync(ctx, "valheim2"); err == nil {
		t.Fatal("want a conflict")
	}

	// Every port included, on the given internet port.
	all := func(ports ...int) map[string]PortSetting {
		out := map[string]PortSetting{}
		for i, n := range ports {
			out[fmt.Sprintf("%d/udp", 2456+i)] = PortSetting{Port: n, Forward: true, Proxy: true, Firewall: true}
		}
		return out
	}
	bad := map[string]map[string]PortSetting{
		"unknown port":  {"9999/udp": {Port: 9999}},
		"out of range":  all(70000, 2457, 2458),
		"shared by two": all(2466, 2466, 2458),
		"clashes kept":  all(2457, 2457, 2458),
	}
	for name, in := range bad {
		if _, err := s.SetOptions(ctx, "valheim2", in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	ports, err := s.SetOptions(ctx, "valheim2", all(2466, 2467, 2468))
	if err != nil || len(ports) != 3 || ports[0] != (Mapping{2466, 2456, "udp", true, true, true}) {
		t.Fatalf("set = %+v, %v", ports, err)
	}
	st, err := s.Sync(ctx, "valheim2")
	if err != nil || len(st.Rules) != 1 || st.Rules[0].Key != "2466-2468/udp" {
		t.Fatalf("sync = %+v, %v", st, err)
	}
	var r unifi.PortForward
	for _, x := range fake.Rules() {
		if strings.HasPrefix(x.Name, "hlm:valheim2 ") {
			r = x
		}
	}
	if r.DstPort != "2466-2468" || r.FwdPort != "2456-2458" || r.Fwd != "192.168.1.62" {
		t.Errorf("rule = %+v", r)
	}

	// Leaving a port out of UniFi: the rule shrinks to the others.
	in := all(2466, 2467, 2468)
	in["2458/udp"] = PortSetting{Port: 2468, Proxy: true, Firewall: true}
	if _, err := s.SetOptions(ctx, "valheim2", in); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(ctx, "valheim2"); len(st.Rules) != 1 || st.Rules[0].Key != "2466-2467/udp" || len(st.Stale) != 1 {
		t.Errorf("one port left out: %+v", st)
	}
	// None forwarded: a note says so.
	none := map[string]PortSetting{}
	for k, v := range in {
		v.Forward = false
		none[k] = v
	}
	s.SetOptions(ctx, "valheim2", none)
	if st, _ := s.Status(ctx, "valheim2"); len(st.Rules) != 0 || !strings.Contains(st.Note, "None of this server's ports") {
		t.Errorf("none forwarded: %+v", st)
	}

	// Setting them back to the defaults forgets the options.
	if _, err := s.SetOptions(ctx, "valheim2", all(2456, 2457, 2458)); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Options("valheim2"); len(o) != 0 {
		t.Errorf("options = %v", o)
	}
	s.SetOptions(ctx, "valheim2", all(3000, 2457, 2458))
	if err := s.ForgetServer("valheim2"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Options("valheim2"); len(o) != 0 {
		t.Errorf("after forget = %v", o)
	}
}

// Internet ports saved before the other options existed are kept.
func TestLegacyInternetPorts(t *testing.T) {
	servers := fakeServers{"web": bridgeServer("web", map[string]string{"80/tcp": "8080"})}
	s, _, _ := setup(t, servers)
	if err := s.secrets.Set(secretPorts, map[string]map[string]int{"web": {"8080/tcp": 9090}}); err != nil {
		t.Fatal(err)
	}
	ports, err := s.ServerPorts(servers["web"])
	if err != nil || len(ports) != 1 || ports[0] != (Mapping{9090, 8080, "tcp", true, true, true}) {
		t.Fatalf("ports = %+v, %v", ports, err)
	}
	// Saving moves them to the new store.
	if _, err := s.SetOptions(context.Background(), "web", map[string]PortSetting{"8080/tcp": {Port: 9091, Forward: true, Proxy: true}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.secrets.Get(secretPorts, &map[string]any{}); ok {
		t.Error("old store not removed")
	}
	if o, _ := s.Options("web"); o["8080/tcp"] != (PortOptions{Internet: 9091, NoFirewall: true}) {
		t.Errorf("options = %+v", o)
	}
}

func TestPortStates(t *testing.T) {
	servers := fakeServers{"cs": bridgeServer("cs", map[string]string{"27015/udp": "27015", "27016/udp": "27016", "27020/tcp": "27020"})}
	s, fake, _ := setup(t, servers)
	ctx := context.Background()
	fake.Add(unifi.PortForward{Name: "someone else's", Enabled: true, DstPort: "27020", Fwd: "192.168.1.99", FwdPort: "27020", Proto: "tcp"})

	st, err := s.PortStates(ctx, "cs")
	if err != nil || st["27015/udp"].State != SyncPending || st["27020/tcp"].State != SyncConflict {
		t.Fatalf("before = %+v, %v", st, err)
	}
	s.Sync(ctx, "cs")
	if st, _ = s.PortStates(ctx, "cs"); st["27015/udp"].State != SyncDone || st["27016/udp"].State != SyncDone {
		t.Errorf("after sync = %+v", st)
	}
	// 27016 left out: its rule (shared with 27015 as a range) is replaced.
	s.SetOptions(ctx, "cs", map[string]PortSetting{
		"27015/udp": {Forward: true}, "27016/udp": {}, "27020/tcp": {Forward: true},
	})
	st, _ = s.PortStates(ctx, "cs")
	if st["27016/udp"].State != SyncRemoving || st["27015/udp"].State != SyncPending {
		t.Errorf("left out = %+v", st)
	}
	s.Sync(ctx, "cs")
	if st, _ = s.PortStates(ctx, "cs"); st["27016/udp"].State != SyncOff || st["27015/udp"].State != SyncDone {
		t.Errorf("after resync = %+v", st)
	}
	// Without UniFi there's nothing to report.
	s.Delete()
	if st, err := s.PortStates(ctx, "cs"); st != nil || err != nil {
		t.Errorf("not configured = %+v, %v", st, err)
	}
}
