package manager

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// What a new container would lose or change is spelled out.
func TestCompareContainers(t *testing.T) {
	var img docker.Image
	img.Config.Env = []string{"PATH=/bin", "LANG=C"}
	img.Config.Cmd = []string{"run"}
	cur := docker.CreateRequest{
		Image: "lscr.io/linuxserver/plex", Env: []string{"PATH=/bin", "LANG=C", "TZ=Europe/London", "SECRET=x"},
		Cmd: []string{"run"}, Labels: map[string]string{"owner": "me", "net.unraid.docker.icon": "i"},
		HostConfig: docker.HostConfig{
			NetworkMode:  "br0",
			PortBindings: map[string][]docker.PortBinding{"32400/tcp": {{HostPort: "32400"}}},
			Privileged:   true, CapAdd: []string{"NET_ADMIN"},
			Devices: []docker.DeviceMapping{{PathOnHost: "/dev/dri", PathInContainer: "/dev/dri"}},
		},
	}
	var info docker.ContainerJSON
	info.ID = "abcdef0123456789"
	info.Mounts = []docker.MountPoint{
		{Type: "bind", Source: "/mnt/user/appdata/plex", Destination: "/config", RW: true},
		{Type: "volume", Name: strings.Repeat("a", 64), Destination: "/transcode", RW: true},
	}
	info.NetworkSettings.Networks = map[string]docker.EndpointSettings{
		"br0":   {IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "192.168.1.50"}, IPAddress: "192.168.1.50", MacAddress: "02:42:c0:a8:01:32"},
		"other": {},
	}
	var extras containerExtras
	extras.HostConfig.VolumesFrom = []string{"data"}

	same := cur
	same.HostConfig.Binds = []string{"/mnt/user/appdata/plex:/config", strings.Repeat("a", 64) + ":/transcode"}
	same.NetworkingConfig = &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{
		"br0": {IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: "192.168.1.50"}, MacAddress: "02:42:c0:a8:01:32"}}}
	if got := compareContainers(cur, same, img, info, extras, false); len(got) != 2 {
		// The same container still can't keep its second network or
		// --volumes-from.
		t.Errorf("same container:\n%s", strings.Join(got, "\n"))
	}

	want := docker.CreateRequest{
		Image: "lscr.io/linuxserver/plex:latest", Env: []string{"TZ=UTC"}, Cmd: []string{"run"},
		HostConfig: docker.HostConfig{
			NetworkMode:  "br0",
			PortBindings: map[string][]docker.PortBinding{"32400/tcp": {{HostPort: "32401"}}},
			Binds:        []string{"/mnt/user/appdata/plex:/config:ro"},
		},
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{"br0": {MacAddress: "02:00:00:00:00:01"}}},
	}
	got := strings.Join(compareContainers(cur, want, img, info, extras, false), "\n")
	for _, w := range []string{
		"stop publishing port 32400/tcp on 32400", "publish port 32400/tcp on 32401",
		"/config would be /mnt/user/appdata/plex (read-only), not /mnt/user/appdata/plex",
		"/transcode would get a new, empty volume",
		"TZ would have another value", "SECRET would no longer be set",
		"lose the label owner", "no longer be on the network other",
		"fixed address 192.168.1.50 would no longer be fixed", "MAC address on br0",
		"no longer be privileged", "no longer have the device /dev/dri:/dev/dri", "added capability NET_ADMIN",
		"--volumes-from data",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in:\n%s", w, got)
		}
	}
	// The image is the same one, however it's written; Unraid's labels
	// don't count; and secrets' values aren't shown.
	if strings.Contains(got, "image") || strings.Contains(got, "net.unraid") || strings.Contains(got, "=x") {
		t.Errorf("too much:\n%s", got)
	}
}

func TestFromContainerArgs(t *testing.T) {
	for _, tt := range []struct{ in []string }{
		{[]string{"sh", "-c", "echo it's \"quoted\" && sleep 1"}},
		{[]string{"a", ""}},
		{[]string{"--flag=a b", `back\slash`}},
	} {
		joined := joinArgs(tt.in)
		// deploy reads it back as a server's arguments.
		spec, err := deploy.Build(&catalog.App{ID: "x", Repository: "busybox"}, deploy.Request{Name: "x", PostArgs: &joined}, deploy.Env{})
		if err != nil || strings.Join(spec.Create.Cmd, "\x00") != strings.Join(tt.in, "\x00") {
			t.Errorf("%q -> %s -> %q %v", tt.in, joined, spec.Create.Cmd, err)
		}
	}
}

// On br0, an Unraid container's port variables (and ports it exposes
// beyond its image's) become its ports, so they're declared again.
func TestFromContainerUnraidPorts(t *testing.T) {
	var img docker.Image
	img.Config.ExposedPorts = map[string]struct{}{"27015/udp": {}}
	cur := docker.CreateRequest{
		Image: "nodiaque/steamcmd:enshrouded", Env: []string{"UDP_PORT_15637=15637", "TCP_PORT_8080=8081"},
		ExposedPorts: map[string]struct{}{"27015/udp": {}, "9000/tcp": {}},
		HostConfig:   docker.HostConfig{NetworkMode: "br0"},
	}
	var info docker.ContainerJSON
	info.ID = "abc"
	app, req := fromContainer(LocalHost, "enshrouded", info, cur, img, true)
	ports := map[string]string{}
	for _, c := range app.Config {
		if c.Type == catalog.TypePort {
			ports[c.Target+"/"+c.Mode] = req.Values[c.ID]
		}
	}
	// The image's own port isn't repeated; Unraid's value is kept.
	if len(ports) != 3 || ports["15637/udp"] != "15637" || ports["8080/tcp"] != "8081" || ports["9000/tcp"] != "9000" {
		t.Errorf("ports %v", ports)
	}
	// The variables stay too.
	vars := 0
	for _, c := range app.Config {
		if c.Type == catalog.TypeVariable && strings.Contains(c.Target, "_PORT_") {
			vars++
		}
	}
	if vars != 2 {
		t.Errorf("port variables kept: %d", vars)
	}
	// On a bridge network they'd be published, so they aren't made ports.
	cur.HostConfig.NetworkMode = "bridge"
	app, _ = fromContainer(LocalHost, "enshrouded", info, cur, img, false)
	for _, c := range app.Config {
		if c.Type == catalog.TypePort {
			t.Errorf("bridge: port %+v", c)
		}
	}

	// A new container that wouldn't declare them is reported.
	got := strings.Join(compareContainers(cur, docker.CreateRequest{Image: cur.Image}, img, info, containerExtras{}, true), "\n")
	if !strings.Contains(got, "no longer declare port 15637/udp") || !strings.Contains(got, "no longer declare port 9000/tcp") || strings.Contains(got, "27015") {
		t.Errorf("changes:\n%s", got)
	}
}
