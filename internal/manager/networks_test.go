package manager

import (
	"strings"
	"testing"
)

func TestValidateNetwork(t *testing.T) {
	ok := []NetworkRequest{
		{Name: "games", Driver: "bridge", Subnet: "172.30.0.0/24"},
		{Name: "lan", Driver: "macvlan", Parent: "ens18", Subnet: "10.0.0.0/20", Gateway: "10.0.0.1", IPRange: "10.0.15.0/26"},
	}
	for _, r := range ok {
		if err := validateNetwork(&r); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := map[string]NetworkRequest{
		"reserved":          {Name: "bridge", Driver: "bridge", Subnet: "172.30.0.0/24"},
		"bad name":          {Name: "my net", Driver: "bridge", Subnet: "172.30.0.0/24"},
		"no parent":         {Name: "lan", Driver: "macvlan", Subnet: "10.0.0.0/20", Gateway: "10.0.0.1"},
		"no gateway on LAN": {Name: "lan", Driver: "macvlan", Parent: "ens18", Subnet: "10.0.0.0/20"},
		"host bits":         {Name: "games", Driver: "bridge", Subnet: "172.30.0.5/24"},
		"gateway outside":   {Name: "games", Driver: "bridge", Subnet: "172.30.0.0/24", Gateway: "10.0.0.1"},
		"range outside":     {Name: "lan", Driver: "macvlan", Parent: "e", Subnet: "10.0.0.0/20", Gateway: "10.0.0.1", IPRange: "192.168.0.0/26"},
		"unknown driver":    {Name: "x", Driver: "overlay", Subnet: "10.9.0.0/24"},
	}
	for name, r := range bad {
		if err := validateNetwork(&r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	r := NetworkRequest{Name: "games", Driver: "bridge", Subnet: "172.30.0.5/24"}
	if err := validateNetwork(&r); err == nil || !strings.Contains(err.Error(), "172.30.0.0/24") {
		t.Errorf("host bits hint: %v", err)
	}
}
