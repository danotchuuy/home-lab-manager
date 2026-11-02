package manager

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
)

func TestCheckLANAddress(t *testing.T) {
	m := &Manager{log: slog.New(slog.DiscardHandler)}
	lan := &deploy.Spec{}
	lan.Create.Labels = map[string]string{deploy.LabelDirectIP: "true"}
	bridge := &deploy.Spec{}
	bridge.Create.Labels = map[string]string{}
	req := deploy.Request{IPAddress: "192.168.1.50"}
	var events []Event
	emit := func(e Event) { events = append(events, e) }

	// No checker (UniFi not wired up): nothing happens.
	if err := m.checkLANAddress(context.Background(), lan, req, nil, emit); err != nil {
		t.Fatal(err)
	}

	var gotIgnore []string
	m.UseIPChecker(func(_ context.Context, ip string, ignore []string) (string, string, error) {
		gotIgnore = ignore
		switch ip {
		case "192.168.1.50":
			return "192.168.1.50 is in use by Living room TV", "", nil
		case "192.168.1.100":
			return "", "inside the DHCP range", nil
		default:
			return "", "", errors.New("controller unreachable")
		}
	})

	err := m.checkLANAddress(context.Background(), lan, req, []string{"02:42:aa"}, emit)
	if err == nil || !strings.Contains(err.Error(), "Living room TV") || !strings.Contains(err.Error(), "UniFi") {
		t.Errorf("conflict: err = %v", err)
	}
	if len(gotIgnore) != 1 || gotIgnore[0] != "02:42:aa" {
		t.Errorf("ignore = %v", gotIgnore)
	}
	// Private bridge addresses aren't on the LAN, so UniFi isn't asked.
	if err := m.checkLANAddress(context.Background(), bridge, req, nil, emit); err != nil {
		t.Errorf("bridge network: %v", err)
	}
	// Warnings and an unreachable controller don't block the install.
	events = nil
	if err := m.checkLANAddress(context.Background(), lan, deploy.Request{IPAddress: "192.168.1.100"}, nil, emit); err != nil {
		t.Error(err)
	}
	if err := m.checkLANAddress(context.Background(), lan, deploy.Request{IPAddress: "192.168.1.7"}, nil, emit); err != nil {
		t.Error(err)
	}
	if len(events) != 2 || !strings.Contains(events[0].Message, "DHCP") || !strings.Contains(events[1].Message, "couldn't check") {
		t.Errorf("warnings = %+v", events)
	}
}
