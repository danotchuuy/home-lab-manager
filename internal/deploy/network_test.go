package deploy

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func testEnv() Env {
	return Env{Networks: map[string]docker.Network{
		"bridge": {Name: "bridge", Driver: "bridge", IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "172.17.0.0/16", Gateway: "172.17.0.1"}}}},
		"games": {
			Name: "games", Driver: "bridge",
			IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "172.30.0.0/24", Gateway: "172.30.0.1"}}},
			Containers: map[string]docker.NetworkContainer{
				"abc": {Name: "valheim", IPv4Address: "172.30.0.10/24"},
				"def": {Name: "my-server", IPv4Address: "172.30.0.20/24"},
			},
		},
		"br0": {
			Name: "br0", Driver: "macvlan",
			IPAM: docker.NetworkIPAM{Config: []docker.NetworkIPAMRange{{Subnet: "192.168.1.0/24", Gateway: "192.168.1.1"}}},
		},
		"nosubnet": {Name: "nosubnet", Driver: "bridge"},
	}}
}

func TestStaticIPOnUserBridge(t *testing.T) {
	spec, err := Build(testApp(), Request{Name: "s", Network: "games", IPAddress: "172.30.0.50"}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if c.HostConfig.NetworkMode != "games" {
		t.Errorf("network = %q", c.HostConfig.NetworkMode)
	}
	ep := c.NetworkingConfig.EndpointsConfig["games"]
	if ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address != "172.30.0.50" {
		t.Errorf("endpoint = %+v", ep)
	}
	// User-defined bridge networks still publish ports on the host.
	if len(c.HostConfig.PortBindings) == 0 {
		t.Error("ports were not published on a bridge network")
	}
	if c.Labels[LabelDirectIP] != "" {
		t.Error("bridge network marked as direct IP")
	}
}

func TestMacvlanFromTemplate(t *testing.T) {
	app := testApp()
	app.Network = "br0" // Unraid's macvlan network
	spec, err := Build(app, Request{Name: "s", IPAddress: "192.168.1.60"}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Create
	if c.HostConfig.NetworkMode != "br0" || c.NetworkingConfig.EndpointsConfig["br0"].IPAMConfig.IPv4Address != "192.168.1.60" {
		t.Errorf("network = %q, config = %+v", c.HostConfig.NetworkMode, c.NetworkingConfig)
	}
	if len(c.HostConfig.PortBindings) != 0 {
		t.Errorf("ports published on macvlan: %v", c.HostConfig.PortBindings)
	}
	if _, ok := c.ExposedPorts["15637/udp"]; !ok {
		t.Error("ports should still be exposed")
	}
	if c.Labels[LabelDirectIP] != "true" {
		t.Error("macvlan network not marked as direct IP")
	}
	// The Web UI is reached on the container port, not the host port.
	if c.Labels[LabelWebUI] != "http://[IP]:8080/" {
		t.Errorf("webui = %q", c.Labels[LabelWebUI])
	}
}

func TestMissingTemplateNetworkFallsBack(t *testing.T) {
	app := testApp()
	app.Network = "br0"
	spec, err := Build(app, Request{Name: "s"}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.HostConfig.NetworkMode != "bridge" || len(spec.Warnings) == 0 {
		t.Errorf("network = %q, warnings = %v", spec.Create.HostConfig.NetworkMode, spec.Warnings)
	}
}

func TestNetworkSelectionErrors(t *testing.T) {
	tests := map[string]struct {
		req  Request
		want string
	}{
		"unknown network": {Request{Network: "nope"}, "does not exist"},
		"ip on default":   {Request{IPAddress: "172.17.0.5"}, "user-defined network"},
		"ip on host":      {Request{Network: "host", IPAddress: "10.0.0.5"}, "user-defined network"},
		"bad ip":          {Request{Network: "games", IPAddress: "300.1.1.1"}, "invalid IP"},
		"outside subnet":  {Request{Network: "games", IPAddress: "10.0.0.5"}, "not in network"},
		"gateway":         {Request{Network: "games", IPAddress: "172.30.0.1"}, "gateway"},
		"network address": {Request{Network: "games", IPAddress: "172.30.0.0"}, "network address"},
		"taken":           {Request{Network: "games", IPAddress: "172.30.0.10"}, `used by container "valheim"`},
		"no subnet":       {Request{Network: "nosubnet", IPAddress: "172.31.0.5"}, "no configured subnet"},
	}
	for name, tt := range tests {
		tt.req.Name = "s"
		_, err := Build(testApp(), tt.req, testEnv())
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		}
	}

	// Reconfiguring a server may keep the address it already has.
	if _, err := Build(testApp(), Request{Name: "my-server", Network: "games", IPAddress: "172.30.0.20"}, testEnv()); err != nil {
		t.Errorf("keeping own IP: %v", err)
	}
}

func TestHostNetworkSelection(t *testing.T) {
	spec, err := Build(testApp(), Request{Name: "s", Network: "host"}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Create.HostConfig.NetworkMode != "host" || spec.Create.HostConfig.PortBindings != nil {
		t.Errorf("host: mode=%q bindings=%v", spec.Create.HostConfig.NetworkMode, spec.Create.HostConfig.PortBindings)
	}
}

func TestStableMAC(t *testing.T) {
	a, b := StableMAC("valheim"), StableMAC("valheim")
	// Servers keep their MACs (and DHCP leases) across releases.
	if want := "02:63:50:39:99:e8"; a != want {
		t.Errorf("StableMAC(valheim) = %s, want %s", a, want)
	}
	if a != b || a == StableMAC("enshrouded") {
		t.Errorf("StableMAC not stable/unique: %s %s %s", a, b, StableMAC("enshrouded"))
	}
	if !strings.HasPrefix(a, "02:") || len(a) != 17 {
		t.Errorf("StableMAC = %s, want a locally administered address", a)
	}
	// LAN servers get it whether or not they have a fixed IP.
	for _, ip := range []string{"", "192.168.1.60"} {
		spec, err := Build(testApp(), Request{Name: "valheim", Network: "br0", IPAddress: ip}, testEnv())
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.Create.NetworkingConfig.EndpointsConfig["br0"].MacAddress; got != a {
			t.Errorf("ip %q: MAC = %q, want %s", ip, got, a)
		}
	}
	// Bridge networks don't need one.
	spec, _ := Build(testApp(), Request{Name: "valheim", Network: "games", IPAddress: "172.30.0.50"}, testEnv())
	if got := spec.Create.NetworkingConfig.EndpointsConfig["games"].MacAddress; got != "" {
		t.Errorf("bridge network got MAC %s", got)
	}
}
