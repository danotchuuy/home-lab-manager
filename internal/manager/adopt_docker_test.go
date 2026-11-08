package manager

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func adoptTestSetup(t *testing.T, feed string) (context.Context, *docker.Client, *Manager) {
	t.Helper()
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PullImage(ctx, "busybox:1.37", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	cat := catalog.New("http://127.0.0.1:1/none", dir, log)
	if feed != "" {
		if err := os.WriteFile(filepath.Join(dir, "applicationFeed.json"), []byte(feed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cat.LoadCache(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.UseCustomStore(filepath.Join(dir, "custom.json")); err != nil {
		t.Fatal(err)
	}
	return ctx, d, New(d, cat, t.TempDir(), log)
}

func removeAfter(t *testing.T, d *docker.Client, name string, volumes ...string) {
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := d.ListContainers(ctx)
		for _, c := range list {
			for _, n := range c.Names {
				if n == "/"+name || strings.HasPrefix(n, "/"+name+"-old-") {
					d.RemoveContainer(ctx, c.ID, true)
				}
			}
		}
		for _, v := range volumes {
			d.RemoveVolume(ctx, v)
		}
	})
}

func execOut(t *testing.T, ctx context.Context, d *docker.Client, id string, cmd ...string) string {
	t.Helper()
	res, err := d.Exec(ctx, id, "", cmd, 4096)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exec %v: %v %s %s", cmd, err, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(string(res.Stdout))
}

// A container hlm didn't make is listed, and adopting it keeps everything:
// its port, folder, volumes (named and anonymous, with their data),
// variables, labels, flags and command.
func TestDockerAdoptContainer(t *testing.T) {
	ctx, d, m := adoptTestSetup(t, "")
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	name, vol := "hlm-test-adopt-"+suffix, "hlm-test-adopt-vol-"+suffix
	removeAfter(t, d, name, vol)
	data := t.TempDir()
	port := freePort(t)
	init := true
	id, _, err := d.CreateContainer(ctx, name, docker.CreateRequest{
		Image:   "busybox:1.37",
		Cmd:     []string{"sh", "-c", "echo up; exec sleep 3600"},
		Env:     []string{"FOO=bar", "QUOTE=it's \"here\""},
		Labels:  map[string]string{"com.example.owner": "me"},
		Volumes: map[string]struct{}{"/anon": {}},
		HostConfig: docker.HostConfig{
			Binds:         []string{data + ":/data", vol + ":/vol"},
			PortBindings:  map[string][]docker.PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}}},
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			CapAdd:        []string{"NET_ADMIN"},
			Init:          &init,
			Memory:        64 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	execOut(t, ctx, d, id, "sh", "-c", "echo named > /vol/f && echo anon > /anon/f")

	// Another, on its network, can't be adopted.
	side := name + "-side"
	removeAfter(t, d, side)
	sideID, _, err := d.CreateContainer(ctx, side, docker.CreateRequest{Image: "busybox:1.37", Cmd: []string{"sleep", "3600"},
		HostConfig: docker.HostConfig{NetworkMode: "container:" + name}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveContainer(context.Background(), sideID, true) })

	list, err := m.Unmanaged(ctx, LocalHost)
	if err != nil {
		t.Fatal(err)
	}
	var got, gotSide *Unmanaged
	for i := range list {
		switch list[i].Name {
		case name:
			got = &list[i]
		case side:
			gotSide = &list[i]
		}
	}
	if got == nil || !got.Adoptable || got.State != "running" || len(got.Mounts) != 3 {
		t.Fatalf("listed %+v", got)
	}
	if gotSide == nil || gotSide.Adoptable || !strings.Contains(gotSide.Reason, "another container's network") {
		t.Errorf("side container %+v", gotSide)
	}

	plan, err := m.AdoptPlan(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != AdoptContainer || !plan.Restart || len(plan.Changes) != 0 {
		t.Fatalf("plan: source %s, restart %v, changes:\n%s\nnotes %v", plan.Source, plan.Restart, strings.Join(plan.Changes, "\n"), plan.Notes)
	}
	srv, err := m.Adopt(ctx, name, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Name != name || srv.State != "running" || srv.Request == nil || !strings.HasPrefix(srv.TemplateID, "adopted-container-") {
		t.Fatalf("server %+v", srv)
	}
	info, err := d.InspectContainer(ctx, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == id || info.Config.Labels[deploy.LabelManaged] != "true" || info.Config.Labels["com.example.owner"] != "me" {
		t.Errorf("labels %v", info.Config.Labels)
	}
	if b := info.HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != strconv.Itoa(port) {
		t.Errorf("ports %+v", info.HostConfig.PortBindings)
	}
	if !slices.Contains(info.Config.Env, "FOO=bar") || !slices.Contains(info.Config.Env, `QUOTE=it's "here"`) {
		t.Errorf("env %v", info.Config.Env)
	}
	if v := execOut(t, ctx, d, srv.ID, "cat", "/vol/f", "/anon/f"); v != "named\nanon" {
		t.Errorf("volume data = %q", v)
	}
	if out, _ := m.Logs(ctx, srv.ID, docker.LogsOptions{Tail: "5"}); out != nil {
		out.Close()
	}
	// Nothing would change adopting it again: it's a server now.
	if _, err := m.AdoptPlan(ctx, name); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("plan for a server: %v", err)
	}
	// And it's updated and reconfigured from its new template.
	app, req, err := m.Config(ctx, srv.ID)
	if err != nil || app.Name != name || *req.ExtraParams == "" || !strings.Contains(*req.ExtraParams, "--cap-add=NET_ADMIN") {
		t.Fatalf("config %+v %+v %v", app, req, err)
	}
	if _, err := m.Reconfigure(ctx, srv.ID, req, func(Event) {}); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if v := execOut(t, ctx, d, name, "cat", "/vol/f", "/anon/f"); v != "named\nanon" {
		t.Errorf("volume data after reconfiguring = %q", v)
	}
}

const unraidTestFeed = `{"last_updated": "2026-11-01 00:00", "applist": [{
  "Name": "BusyTest", "Repository": "busybox:1.37", "Repo": "Test's Repository",
  "TemplateURL": "https://example.com/busytest.xml", "TemplatePath": "/tmp/busytest.xml",
  "Network": "bridge", "Privileged": "false", "PostArgs": "sleep 60",
  "CategoryList": ["Tools"],
  "Config": [
    {"@attributes": {"Name": "Web", "Target": "80", "Default": "8080", "Mode": "tcp", "Type": "Port", "Display": "always", "Required": "true"}, "value": "8080"},
    {"@attributes": {"Name": "Data", "Target": "/data", "Default": "/mnt/user/appdata/busytest", "Mode": "rw", "Type": "Path", "Display": "always", "Required": "true"}, "value": "/mnt/user/appdata/busytest"},
    {"@attributes": {"Name": "Mode", "Target": "MODE", "Default": "a", "Type": "Variable", "Display": "always", "Required": "false"}, "value": "a"},
    {"@attributes": {"Name": "Optional", "Target": "OPTIONAL", "Default": "yes", "Type": "Variable", "Display": "advanced", "Required": "false"}, "value": "yes"}
  ]}]}`

// A container Unraid made is adopted with the Community Applications
// template it came from, and the user's values from Unraid's copy of it.
func TestDockerAdoptUnraid(t *testing.T) {
	ctx, d, m := adoptTestSetup(t, unraidTestFeed)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "hlm-test-unraid-" + suffix
	removeAfter(t, d, name)
	data := t.TempDir()
	port := freePort(t)

	flash := t.TempDir()
	unraidFlash = flash
	t.Cleanup(func() { unraidFlash = "/boot" })
	dir := filepath.Join(flash, strings.TrimPrefix(catalog.UnraidTemplatesDir, "/boot/"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	xml := fmt.Sprintf(`<?xml version="1.0"?>
<Container version="2">
  <Name>%s</Name>
  <Repository>busybox:1.37</Repository>
  <Network>bridge</Network>
  <MyIP/>
  <Privileged>false</Privileged>
  <TemplateURL>https://example.com/busytest.xml</TemplateURL>
  <ExtraParams>--memory=128m</ExtraParams>
  <PostArgs>sleep 3600</PostArgs>
  <CPUset/>
  <Config Name="Web" Target="80" Default="8080" Mode="tcp" Description="" Type="Port" Display="always" Required="true" Mask="false">%d</Config>
  <Config Name="Data" Target="/data" Default="/mnt/user/appdata/busytest" Mode="rw" Description="" Type="Path" Display="always" Required="true" Mask="false">%s</Config>
  <Config Name="Mode" Target="MODE" Default="a" Mode="" Description="" Type="Variable" Display="always" Required="false" Mask="false">b &amp; c</Config>
  <Config Name="Mine" Target="MINE" Default="" Mode="" Description="" Type="Variable" Display="always" Required="false" Mask="false">x</Config>
</Container>`, name, port, data)
	if err := os.WriteFile(filepath.Join(dir, "my-"+name+".xml"), []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _, err := d.CreateContainer(ctx, name, docker.CreateRequest{
		Image: "busybox:1.37", Cmd: []string{"sleep", "3600"},
		Env:    []string{"MODE=b & c", "MINE=x", "HOST_OS=Unraid", "HOST_CONTAINERNAME=" + name},
		Labels: map[string]string{catalog.UnraidManagedLabel: "dockerman", "net.unraid.docker.icon": "https://example.com/i.png"},
		HostConfig: docker.HostConfig{
			Binds:         []string{data + ":/data:rw"},
			PortBindings:  map[string][]docker.PortBinding{"80/tcp": {{HostPort: strconv.Itoa(port)}}},
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Memory:        128 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	plan, err := m.AdoptPlan(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != AdoptCommunityApps || plan.Template != "BusyTest" || len(plan.Changes) != 0 {
		t.Fatalf("plan: source %s %q, changes:\n%s\nnotes %v", plan.Source, plan.Template, strings.Join(plan.Changes, "\n"), plan.Notes)
	}
	r := plan.Request
	if r.Values["variable:MODE"] != "b & c" || !slices.Contains(r.Disabled, "variable:OPTIONAL") || len(r.Extra) != 3 {
		t.Errorf("request %+v", r)
	}
	srv, err := m.Adopt(ctx, name, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if srv.TemplateID != plan.TemplateID || srv.App != "BusyTest" {
		t.Errorf("server %+v", srv)
	}

	// Without the catalog's template, Unraid's copy becomes a custom one.
	m.catalog = catalog.New("http://127.0.0.1:1/none", t.TempDir(), slog.New(slog.DiscardHandler))
	m.catalog.UseCustomStore(filepath.Join(t.TempDir(), "custom.json"))
	other := name + "-b"
	removeAfter(t, d, other)
	os.WriteFile(filepath.Join(dir, "my-"+other+".xml"), []byte(strings.ReplaceAll(xml, "<Name>"+name, "<Name>"+other)), 0o644)
	port2 := freePort(t)
	os.WriteFile(filepath.Join(dir, "my-"+other+".xml"), []byte(strings.ReplaceAll(strings.ReplaceAll(xml, "<Name>"+name, "<Name>"+other), ">"+strconv.Itoa(port)+"<", ">"+strconv.Itoa(port2)+"<")), 0o644)
	if _, _, err := d.CreateContainer(ctx, other, docker.CreateRequest{
		Image: "busybox:1.37", Cmd: []string{"sleep", "3600"}, Env: []string{"MODE=b & c", "MINE=x"},
		Labels: map[string]string{catalog.UnraidManagedLabel: "dockerman"},
		HostConfig: docker.HostConfig{Binds: []string{data + ":/data:rw"}, Memory: 128 << 20,
			PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostPort: strconv.Itoa(port2)}}}},
	}); err != nil {
		t.Fatal(err)
	}
	plan, err = m.AdoptPlan(ctx, other)
	if err != nil || plan.Source != AdoptUnraid || len(plan.Changes) != 0 {
		t.Fatalf("plan without the catalog: %+v %v", plan, err)
	}

	// Unraid's template missing: the container's own settings.
	os.Remove(filepath.Join(dir, "my-"+other+".xml"))
	plan, err = m.AdoptPlan(ctx, other)
	if err != nil || plan.Source != AdoptContainer || len(plan.Notes) == 0 || !strings.Contains(plan.Notes[0], "couldn't be read") {
		t.Fatalf("plan without Unraid's template: %+v %v", plan, err)
	}
}
