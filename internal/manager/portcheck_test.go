package manager

import (
	"net/netip"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestHostPorts(t *testing.T) {
	exposed := map[string]struct{}{"2456/udp": {}, "80": {}}
	host := hostPorts("host", exposed, nil)
	if len(host) != 2 {
		t.Fatalf("host network: %+v", host)
	}
	bridged := hostPorts("bridge", exposed, map[string][]docker.PortBinding{
		"2456/udp": {{HostPort: "2466"}},
		"80/tcp":   {{HostIP: "127.0.0.1", HostPort: "8081"}},
		"9000/tcp": {{HostPort: ""}}, // Docker picks one
	})
	if len(bridged) != 2 {
		t.Fatalf("bridge: %+v", bridged)
	}

	at := func(addr string, port int, proto string) hostPort {
		p := hostPort{port: port, proto: proto}
		if addr != "" {
			p.addr = mustAddr(addr)
		}
		return p
	}
	for _, c := range []struct {
		a, b hostPort
		want bool
	}{
		{at("", 2456, "udp"), at("", 2456, "udp"), true},
		{at("", 2456, "udp"), at("", 2456, "tcp"), false},
		{at("", 2456, "udp"), at("", 2457, "udp"), false},
		{at("0.0.0.0", 80, "tcp"), at("127.0.0.1", 80, "tcp"), true},
		{at("192.168.1.5", 80, "tcp"), at("127.0.0.1", 80, "tcp"), false},
		{at("::", 80, "tcp"), at("192.168.1.5", 80, "tcp"), true},
	} {
		if got := c.a.overlaps(c.b); got != c.want {
			t.Errorf("%+v overlaps %+v = %v", c.a, c.b, got)
		}
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
