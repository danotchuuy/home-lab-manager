package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestValidate(t *testing.T) {
	ok := Settings{Target: remote.Target{Host: "vps.example.com", User: "deploy"}, Upstream: "203.0.113.7"}
	if err := Validate(&ok); err != nil || ok.Port != 22 || ok.LogMode != "noip" {
		t.Errorf("valid settings: %+v, %v", ok, err)
	}
	bad := map[string]Settings{
		"host with spaces":   {Target: remote.Target{Host: "vps example", User: "u"}},
		"user injection":     {Target: remote.Target{Host: "vps", User: "u; rm -rf /"}},
		"upstream with port": {Target: remote.Target{Host: "vps", User: "u"}, Upstream: "home.example.com:2456"},
		"upstream injection": {Target: remote.Target{Host: "vps", User: "u"}, Upstream: "x; } server { listen 22"},
		"bad port":           {Target: remote.Target{Host: "vps", User: "u", Port: 70000}},
		"bad log mode":       {Target: remote.Target{Host: "vps", User: "u"}, LogMode: "verbose"},
	}
	for name, st := range bad {
		if err := Validate(&st); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestRender(t *testing.T) {
	st := Settings{Upstream: "home.example.net", LogMode: "noip"}
	servers := []Proxied{
		{Server: "valheim", Ports: []forwarding.Mapping{{Port: 2456, Local: 2456, Proto: "udp"}, {Port: 2457, Local: 2457, Proto: "udp"}}},
		{Server: "minecraft", Ports: []forwarding.Mapping{{Port: 25565, Local: 25565, Proto: "tcp"}}},
		{Server: "stopped-without-ports", Ports: []forwarding.Mapping{}},
	}
	conf := Render(st, servers)
	for _, want := range []string{
		"listen 2456 udp;\n    proxy_pass home.example.net:2456;",
		"listen 25565;\n    proxy_pass home.example.net:25565;",
		"log_format hlm_noip",
		"access_log /var/log/nginx/hlm-stream.log hlm_noip;",
		"error_log /var/log/nginx/hlm-stream-error.log crit;",
		"# valheim", "# minecraft",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config is missing %q:\n%s", want, conf)
		}
	}
	// The whole point: nothing records who connected.
	for _, leak := range []string{"$remote_addr", "$binary_remote_addr", "$realip", "$proxy_protocol_addr"} {
		if strings.Contains(conf, leak) {
			t.Errorf("config logs client addresses (%s)", leak)
		}
	}
	if strings.Contains(conf, "stopped-without-ports") {
		t.Error("server without ports got a section")
	}

	st.LogMode = "off"
	off := Render(st, servers)
	if strings.Contains(off, "log_format") || strings.Count(off, "access_log off;") != 3 {
		t.Errorf("log mode off:\n%s", off)
	}
	// A second server of the same game, on other internet ports.
	moved := []Proxied{{Server: "valheim2", Ports: []forwarding.Mapping{{Port: 2466, Local: 2456, Proto: "udp"}}}}
	if c := Render(Settings{Upstream: "203.0.113.7", LogMode: "off", UpstreamPorts: "internet"}, moved); !strings.Contains(c, "listen 2466 udp;\n    proxy_pass 203.0.113.7:2466;") {
		t.Errorf("internet ports:\n%s", c)
	}
	if c := Render(Settings{Upstream: "10.8.0.2", LogMode: "off", UpstreamPorts: "local"}, moved); !strings.Contains(c, "listen 2466 udp;\n    proxy_pass 10.8.0.2:2456;") {
		t.Errorf("local ports:\n%s", c)
	}
	if v6 := Render(Settings{Upstream: "2001:db8::7", LogMode: "off"}, servers[:1]); !strings.Contains(v6, "proxy_pass [2001:db8::7]:2456;") {
		t.Errorf("IPv6 upstream:\n%s", v6)
	}
}

type fakeServers map[string]docker.ContainerJSON

func (f fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	if info, ok := f[id]; ok {
		return info, nil
	}
	return docker.ContainerJSON{}, errors.New("server not found")
}

func server(bindings map[string]string) docker.ContainerJSON {
	var info docker.ContainerJSON
	info.HostConfig.NetworkMode = "bridge"
	info.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	for c, h := range bindings {
		info.HostConfig.PortBindings[c] = []docker.PortBinding{{HostPort: h}}
	}
	return info
}

func TestPlan(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{
		"a": server(map[string]string{"27015/udp": "27015", "27015/tcp": "27015"}),
		"b": server(map[string]string{"27015/udp": "27015", "7777/udp": "7777"}),
	}, slog.New(slog.DiscardHandler))
	if _, _, err := s.Plan(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("plan before setup: %v", err)
	}
	if _, err := s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "1.2.3.4", Servers: []string{"b", "a", "gone"}}); err != nil {
		t.Fatal(err)
	}
	_, plan, err := s.Plan(context.Background())
	if err != nil || len(plan) != 3 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	// Sorted by name; b's 27015/udp clashes with a's and is skipped.
	if plan[0].Server != "a" || len(plan[0].Ports) != 2 {
		t.Errorf("a = %+v", plan[0])
	}
	if plan[1].Server != "b" || len(plan[1].Ports) != 1 || plan[1].Ports[0].Port != 7777 || len(plan[1].Skipped) != 1 {
		t.Errorf("b = %+v", plan[1])
	}
	if plan[2].Server != "gone" || plan[2].Error == "" {
		t.Errorf("missing server = %+v", plan[2])
	}
}

func TestKeyIsGeneratedOnceAndStoredEncrypted(t *testing.T) {
	dir := t.TempDir()
	store, _ := secrets.Open(dir, "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	if k, _ := s.Key(); k != nil {
		t.Fatal("key before generating")
	}
	k1, err := s.EnsureKey(false)
	if err != nil || !strings.HasPrefix(k1.Public, "ssh-ed25519 ") {
		t.Fatalf("generate: %+v, %v", k1, err)
	}
	k2, _ := s.EnsureKey(false)
	if k2.Fingerprint != k1.Fingerprint {
		t.Error("EnsureKey replaced an existing key")
	}
	k3, _ := s.EnsureKey(true)
	if k3.Fingerprint == k1.Fingerprint {
		t.Error("replace didn't make a new key")
	}
}

func TestPortsLeftOutAndRelayToggle(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2456", "2457/udp": "2457"})}, slog.New(slog.DiscardHandler))
	// 2457 isn't relayed.
	s.UsePorts(func(info docker.ContainerJSON) ([]forwarding.Mapping, error) {
		return []forwarding.Mapping{
			{Port: 2456, Local: 2456, Proto: "udp", Proxy: true, Firewall: true},
			{Port: 2457, Local: 2457, Proto: "udp", Firewall: true},
		}, nil
	})
	if err := s.SetRelayed("a", true); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("before setup: %v", err)
	}
	s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "203.0.113.7", Servers: []string{"z"}})
	if err := s.SetRelayed("a", true); err != nil || !s.Relayed("a") || !s.Relayed("z") {
		t.Fatalf("relay a: %v", err)
	}
	_, plan, _ := s.Plan(context.Background())
	if len(plan) != 2 || plan[0].Server != "a" || len(plan[0].Ports) != 1 || plan[0].Ports[0].Port != 2456 {
		t.Fatalf("plan = %+v", plan)
	}
	if rules := WantedRules(plan); len(rules) != 1 || rules[0].Ports != "2456" {
		t.Errorf("firewall rules = %+v", rules)
	}
	if err := s.SetRelayed("a", false); err != nil || s.Relayed("a") || !s.Relayed("z") {
		t.Errorf("unrelay a: %v", err)
	}
}
