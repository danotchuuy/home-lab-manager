package manager

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestSplitID(t *testing.T) {
	for _, tt := range []struct{ id, host, bare string }{
		{"valheim", LocalHost, "valheim"},
		{"valheim@nas", "nas", "valheim"},
		{"0123abcd@gpu-box", "gpu-box", "0123abcd"},
		{"", LocalHost, ""},
	} {
		host, bare := SplitID(tt.id)
		if host != tt.host || bare != tt.bare {
			t.Errorf("SplitID(%q) = %q, %q", tt.id, host, bare)
		}
		if tt.id != "" && JoinID(host, bare) != tt.id {
			t.Errorf("JoinID(%q, %q) = %q", host, bare, JoinID(host, bare))
		}
	}
}

func fakeClient(t *testing.T, fd *labelDocker) *docker.Client {
	t.Helper()
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	return dc
}

// Servers on another host are named name@host, are listed beside the local
// ones and are controlled on their own host; a host that can't be reached
// leaves its servers out without failing the list.
func TestManagerHosts(t *testing.T) {
	local := &labelDocker{containers: []docker.ContainerJSON{gameServer("l1", "valheim")}}
	nasGame := gameServer("n1", "valheim")
	nasGame.State.Status = "running"
	var wg docker.ContainerJSON
	wg.ID, wg.Name = "w1", "/hlm-wireguard"
	wg.Config.Labels = map[string]string{"io.hlm.wireguard": "home"}
	nas := &labelDocker{containers: []docker.ContainerJSON{nasGame, wg}}

	m := New(fakeClient(t, local), nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	var recreatedOn []string
	m.UseConnection("io.hlm.wireguard", ConnectionKind{Kind: "wireguard",
		Recreate: func(_ context.Context, host string, _, _ func(string)) error {
			recreatedOn = append(recreatedOn, host)
			return nil
		}})
	gone := httptest.NewServer(nil)
	gone.Close()
	goneClient, _ := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(gone.URL, "http://")})
	m.SetHosts([]RemoteHost{
		{ID: "nas", Name: "NAS", Docker: fakeClient(t, nas), AppdataRoot: "/mnt/user/appdata", Address: "192.168.1.20"},
		{ID: "gone", Docker: goneClient},
	})
	ctx := context.Background()

	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name+" "+s.ID+" "+s.Host)
	}
	if !slices.Equal(names, []string{"valheim l1 local", "valheim@nas n1@nas nas"}) {
		t.Errorf("servers = %q", names)
	}
	st := m.HostStatuses()
	if !st["nas"].OK || st["gone"].OK || st["gone"].Error == "" {
		t.Errorf("statuses = %+v", st)
	}

	srv, err := m.Get(ctx, "valheim@nas")
	if err != nil || srv.ID != "n1@nas" || srv.State != "running" {
		t.Errorf("get = %+v, %v", srv, err)
	}
	if err := m.Restart(ctx, "valheim@nas"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, "n1@nas"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(nas.actions, []string{"restart n1", "stop n1"}) || len(local.actions) != 0 {
		t.Errorf("nas did %q, local did %q", nas.actions, local.actions)
	}
	if _, err := m.Get(ctx, "valheim@nowhere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
	if m.HostAddress("valheim@nas") != "192.168.1.20" || m.HostAddress("valheim") != "" || m.AppdataRootOf("nas") != "/mnt/user/appdata" {
		t.Error("host details are wrong")
	}

	// hlm's connections on other hosts (a tunnel's end there) are listed
	// too, named for their host.
	if conns, err := m.Connections(ctx); err != nil || len(conns) != 1 || conns[0].Name != "hlm-wireguard@nas" || conns[0].ID != "w1@nas" {
		t.Errorf("connections = %+v, %v", conns, err)
	}
	if c, err := m.Connection(ctx, "hlm-wireguard@nas"); err != nil || c.Kind != "wireguard" {
		t.Errorf("connection on another host: %+v %v", c, err)
	}
	// Updating it recreates it there, through the service that runs it.
	if err := m.UpdateConnection(ctx, "hlm-wireguard@nas", func(Event) {}); err != nil || len(recreatedOn) != 1 || recreatedOn[0] != "nas" {
		t.Errorf("update on nas: %v, recreated on %v", err, recreatedOn)
	}

	// A removed host's servers are gone; its status is forgotten.
	m.SetHosts(nil)
	if list, _ := m.List(ctx); len(list) != 1 {
		t.Errorf("after removing the host: %+v", list)
	}
	if _, ok := m.HostStatuses()["gone"]; ok {
		t.Error("removed host's status kept")
	}
}
