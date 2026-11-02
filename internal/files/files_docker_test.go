package files

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// These tests run a busybox container. Enable them with HLM_DOCKER_TESTS=1.
func dockerTest(t *testing.T) (*docker.Client, docker.ContainerJSON, string) {
	t.Helper()
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx := context.Background()
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PullImage(ctx, "busybox:1.37", nil); err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(data, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("enshrouded_server.json", `{"name": "My Server"}`)
	write("sub/server.properties", "motd=hello\n")
	write(".env", "A=1\n")
	write("world.bin", "\x00\x01\x02")
	// A link that would expose the host's /etc/passwd if followed on the host.
	if err := os.Symlink("/etc/passwd", filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}

	id, _, err := d.CreateContainer(ctx, "", docker.CreateRequest{
		Image: "busybox:1.37",
		Cmd: []string{"sh", "-c", "mkdir -p /cfg && echo 'port=1' > /cfg/app.ini && chown 1000:1000 /cfg/app.ini " +
			"&& ln -s /cfg/app.ini /cfg/link.ini && echo text > /cfg/game.dll && ln -s game.dll /cfg/plugin.cfg && exec sleep 600"},
		HostConfig: docker.HostConfig{Binds: []string{data + ":/data"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveContainer(context.Background(), id, true) })
	if err := d.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	info, err := d.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d, info, data
}

func names(l Listing) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestDockerFiles(t *testing.T) {
	d, info, data := dockerTest(t)
	ctx := context.Background()
	b := New(d, func(p string) string { return p })

	// Data folder: served from the host.
	l, err := b.List(ctx, info, "/data")
	if err != nil {
		t.Fatal(err)
	}
	if l.Source != "host" || !slices.Equal(names(l), []string{"sub", ".env", "enshrouded_server.json", "escape", "world.bin"}) {
		t.Errorf("/data listing = %s %v", l.Source, names(l))
	}

	// The container's own files: served by Docker.
	l, err = b.List(ctx, info, "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if l.Source != "docker" || !slices.Equal(names(l), []string{"app.ini", "game.dll", "link.ini", "plugin.cfg"}) {
		t.Errorf("/cfg listing = %s %v", l.Source, names(l))
	}

	// The root lists the mount point. On a running container this goes
	// through exec, so it's quick and complete however big the mounts are.
	start := time.Now()
	l, err = b.List(ctx, info, "/")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("listing / took %s", took)
	}
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range l.Entries {
		if e.Name == "data" && e.IsMount {
			found = true
		}
	}
	if !found || l.Truncated {
		t.Errorf("/ listing: data mount found=%v truncated=%v", found, l.Truncated)
	}

	// Edit a data file, with conflict detection.
	f, err := b.Read(ctx, info, "/data/enshrouded_server.json")
	if err != nil || f.Content != `{"name": "My Server"}` || f.Source != "host" {
		t.Fatalf("read = %+v, %v", f, err)
	}
	if _, err := b.Write(ctx, info, f.Path, "stale", f.Mtime.Add(-time.Hour)); !errors.Is(err, ErrConflict) {
		t.Errorf("stale write: err = %v, want conflict", err)
	}
	f2, err := b.Write(ctx, info, f.Path, `{"name": "Renamed"}`, f.Mtime)
	if err != nil || f2.Content != `{"name": "Renamed"}` {
		t.Fatalf("write = %+v, %v", f2, err)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "enshrouded_server.json")); string(got) != `{"name": "Renamed"}` {
		t.Errorf("host file = %q", got)
	}

	// A symlink out of the data folder resolves inside the container, never
	// to the host's file.
	hostPasswd, _ := os.ReadFile("/etc/passwd")
	esc, err := b.Read(ctx, info, "/data/escape")
	if err != nil {
		t.Fatal(err)
	}
	if esc.Source != "docker" || esc.Content == string(hostPasswd) {
		t.Errorf("symlink escape: source=%s, content is the host's passwd: %v", esc.Source, esc.Content == string(hostPasswd))
	}
	if _, err := b.Write(ctx, info, "/data/escape", "pwned", time.Time{}); err == nil {
		if got, _ := os.ReadFile("/etc/passwd"); string(got) != string(hostPasswd) {
			t.Fatal("wrote to the host's /etc/passwd")
		}
	}

	// Binary files open read-only.
	bin, err := b.Read(ctx, info, "/data/world.bin")
	if err != nil || !bin.Binary || bin.Content != "" {
		t.Errorf("binary = %+v, %v", bin, err)
	}

	// Edit a container file through Docker, keeping its owner.
	app, err := b.Read(ctx, info, "/cfg/app.ini")
	if err != nil || app.Content != "port=1\n" || app.Source != "docker" {
		t.Fatalf("read app.ini = %+v, %v", app, err)
	}
	if _, err := b.Write(ctx, info, "/cfg/app.ini", "port=2\n", app.Mtime); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "exec", info.ID, "sh", "-c", "stat -c %u:%g /cfg/app.ini; cat /cfg/app.ini").CombinedOutput()
	if err != nil || string(out) != "1000:1000\nport=2\n" {
		t.Errorf("after write: %q, %v", out, err)
	}

	// DLLs can't be opened or saved through Docker either, even via a link.
	for _, p := range []string{"/cfg/game.dll", "/cfg/plugin.cfg"} {
		if _, err := b.Read(ctx, info, p); !errors.Is(err, ErrNotEditable) {
			t.Errorf("read %s: err = %v, want ErrNotEditable", p, err)
		}
		if _, err := b.Write(ctx, info, p, "pwned", time.Time{}); !errors.Is(err, ErrNotEditable) {
			t.Errorf("write %s: err = %v, want ErrNotEditable", p, err)
		}
	}

	// Writing through a symlink changes the target and keeps the link.
	if _, err := b.Write(ctx, info, "/cfg/link.ini", "port=3\n", time.Time{}); err != nil {
		t.Fatal(err)
	}
	out, _ = exec.Command("docker", "exec", info.ID, "sh", "-c", "readlink /cfg/link.ini; cat /cfg/app.ini").CombinedOutput()
	if string(out) != "/cfg/app.ini\nport=3\n" {
		t.Errorf("after write through link: %q", out)
	}

	// Search the data folder for config files, and the container by name.
	matches, _, err := b.Search(ctx, info, "/data", "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range matches {
		paths = append(paths, m.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/data/.env", "/data/enshrouded_server.json", "/data/sub/server.properties"}) {
		t.Errorf("config search = %v", paths)
	}
	matches, _, err = b.Search(ctx, info, "/cfg", "app")
	if err != nil || len(matches) != 1 || matches[0].Path != "/cfg/app.ini" {
		t.Errorf("docker search = %+v, %v", matches, err)
	}

	// Paths are always absolute and can't climb out.
	if _, err := b.Read(ctx, info, "data/x"); !errors.Is(err, ErrBadPath) {
		t.Errorf("relative path: %v", err)
	}
	if _, err := b.Read(ctx, info, "/data/../../../etc/hostname"); err != nil && strings.Contains(err.Error(), "escape") {
		t.Errorf("cleaned path still escaped: %v", err)
	}

	// Stopped containers can be read and edited too.
	if err := d.StopContainer(ctx, info.ID, 1); err != nil {
		t.Fatal(err)
	}
	app, err = b.Read(ctx, info, "/cfg/app.ini")
	if err != nil || app.Content != "port=3\n" {
		t.Errorf("read while stopped = %+v, %v", app, err)
	}
	if _, err := b.Write(ctx, info, "/cfg/app.ini", "port=4\n", app.Mtime); err != nil {
		t.Errorf("write while stopped: %v", err)
	}

	// Listing a stopped container goes through the archive API, which locks
	// the container, so it is cut off after maxArchiveTime.
	stopped, err := d.InspectContainer(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if _, err := b.List(ctx, stopped, "/"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > maxArchiveTime+2*time.Second {
		t.Errorf("listing / of a stopped container took %s", took)
	}
}

func TestDockerArchive(t *testing.T) {
	d, info, _ := dockerTest(t)
	b := New(d, func(p string) string { return p })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// /cfg comes through Docker's archive API, /data from the host.
	st, err := b.Archive(context.Background(), info, []string{"/cfg", "/data/sub"}, tw, nil)
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	names, contents := readTar(t, buf.Bytes())
	want := []string{"cfg/", "cfg/app.ini", "cfg/game.dll", "cfg/link.ini", "cfg/plugin.cfg", "data/sub/", "data/sub/server.properties"}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %v, want %v", names, want)
	}
	if contents["cfg/app.ini"] != "port=1\n" || contents["data/sub/server.properties"] != "motd=hello\n" || st.Files != 3 {
		t.Errorf("contents = %v, stats = %+v", contents, st)
	}
}

func TestDockerRestore(t *testing.T) {
	d, info, data := dockerTest(t)
	ctx := context.Background()
	b := New(d, func(p string) string { return p })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if _, err := b.Archive(ctx, info, []string{"/cfg", "/data/sub"}, tw, nil); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	// Change files in the container and the data folder, add one, then
	// restore into the stopped container.
	if _, err := d.Exec(ctx, info.ID, "0", []string{"sh", "-c", "echo changed > /cfg/app.ini && echo new > /cfg/new.txt"}, 1024); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "sub/server.properties"), []byte("changed\n"), 0o644)
	if err := d.StopContainer(ctx, info.ID, 1); err != nil {
		t.Fatal(err)
	}
	st, err := b.Restore(ctx, info, &buf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 {
		t.Errorf("restored %d files, want 3 (%v)", st.Files, st.Warnings)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "sub/server.properties")); string(got) != "motd=hello\n" {
		t.Errorf("data folder file = %q", got)
	}
	rc, _, err := d.CopyFrom(ctx, info.ID, "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		seen[hdr.Name] = true
		if hdr.Name == "cfg/app.ini" {
			body := new(bytes.Buffer)
			body.ReadFrom(tr)
			if body.String() != "port=1\n" || hdr.Uid != 1000 || hdr.Gid != 1000 {
				t.Errorf("app.ini = %q owned by %d:%d, want the original, owned by 1000:1000", body, hdr.Uid, hdr.Gid)
			}
		}
	}
	if !seen["cfg/new.txt"] || !seen["cfg/link.ini"] {
		t.Errorf("after restore /cfg has %v; files not in the backup stay, links come back", seen)
	}
}
