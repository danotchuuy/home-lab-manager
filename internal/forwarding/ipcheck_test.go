package forwarding

import (
	"context"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/unifi"
)

func TestCheckIP(t *testing.T) {
	s, fake, _ := setup(t, fakeServers{})
	fake.SetNetworks([]unifi.Network{
		{Name: "Default", Purpose: "corporate", IPSubnet: "192.168.1.1/24", DHCPEnabled: true, DHCPStart: "192.168.1.6", DHCPStop: "192.168.1.199"},
		{Name: "IoT", Purpose: "corporate", IPSubnet: "10.20.0.1/24"},
	})
	fake.SetDevices(
		[]unifi.Device{
			{MAC: "aa:bb:cc:00:00:01", IP: "192.168.1.50", Name: "Living room TV"},
			{MAC: "02:42:c0:a8:01:fa", IP: "192.168.1.250", Hostname: "ark"}, // the server's own container
		},
		[]unifi.Device{
			{MAC: "aa:bb:cc:00:00:02", Name: "NAS", UseFixedIP: true, FixedIP: "192.168.1.240"}, // offline but reserved
			{MAC: "aa:bb:cc:00:00:03", Name: "Old phone", FixedIP: "192.168.1.241"},             // not a reservation
		},
	)
	ctx := context.Background()
	tests := []struct {
		ip, conflict, warning, network string
		ignore                         []string
	}{
		{ip: "192.168.1.50", conflict: "in use by Living room TV (aa:bb:cc:00:00:01)", warning: "DHCP range", network: "Default"},
		{ip: "192.168.1.240", conflict: "reserved as the fixed IP of NAS"},
		{ip: "192.168.1.1", conflict: "gateway's address on Default"},
		{ip: "192.168.1.241", network: "Default"},
		{ip: "192.168.1.250", conflict: "in use by ark"},
		{ip: "192.168.1.250", ignore: []string{"02-42-C0-A8-01-FA"}}, // reconfiguring the server itself
		{ip: "192.168.1.100", warning: "inside Default's DHCP range (192.168.1.6–192.168.1.199)"},
		{ip: "10.20.0.9", network: "IoT"},
	}
	for _, tt := range tests {
		res, err := s.CheckIP(ctx, tt.ip, tt.ignore)
		if err != nil || !res.Checked {
			t.Errorf("%s: %+v, %v", tt.ip, res, err)
			continue
		}
		if (tt.conflict == "") == res.InUse || !strings.Contains(res.Conflict, tt.conflict) {
			t.Errorf("%s (ignore %v): conflict = %q, want %q", tt.ip, tt.ignore, res.Conflict, tt.conflict)
		}
		if !strings.Contains(res.Warning, tt.warning) || (tt.warning == "" && res.Warning != "") {
			t.Errorf("%s: warning = %q, want %q", tt.ip, res.Warning, tt.warning)
		}
		if tt.network != "" && res.Network != tt.network {
			t.Errorf("%s: network = %q", tt.ip, res.Network)
		}
	}
	if _, err := s.CheckIP(ctx, "not-an-ip", nil); err == nil {
		t.Error("invalid IP: want error")
	}

	// Without UniFi there's nothing to check.
	s.Delete()
	if res, err := s.CheckIP(ctx, "192.168.1.50", nil); err != nil || res.Checked || res.InUse {
		t.Errorf("not configured: %+v, %v", res, err)
	}
}
