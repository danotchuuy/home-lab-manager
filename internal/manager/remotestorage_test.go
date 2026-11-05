package manager

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestParseMeasure(t *testing.T) {
	got, err := parseMeasure("dir 12 /srv/a\ndir -1 /srv/b c\nfile 0 /var/run/docker.sock\nmissing 0 /srv/gone\njunk\ndir x /srv/x\nhlm:done\n")
	if err != nil {
		t.Fatal(err)
	}
	if f := got["/srv/a"]; f.Size != 12*1024 || f.Path != "/srv/a" {
		t.Errorf("a = %+v", f)
	}
	if f := got["/srv/b c"]; f.Size != -1 || f.Error == "" {
		t.Errorf("b c = %+v", f)
	}
	if f, ok := got["/var/run/docker.sock"]; !ok || f.Path != "" {
		t.Errorf("a file counted as a folder: %+v", f)
	}
	if f := got["/srv/gone"]; !f.Missing || f.Size != 0 {
		t.Errorf("gone = %+v", f)
	}
	if _, ok := got["/srv/x"]; ok {
		t.Error("bad line read")
	}
	if _, err := parseMeasure("hlm:done\n"); err == nil {
		t.Error("nothing measured, no error")
	}
	// Without the helper's answer, folders are unknown, with the reason.
	rm := remoteMeasurer{err: os.ErrPermission}
	if f := rm.bind(context.Background(), "/srv/a"); f.Size != -1 || f.Error == "" {
		t.Errorf("unmeasured = %+v", f)
	}
}

// TestDockerMeasureRemote measures folders with the helper container, as on
// another host (opt-in: HLM_DOCKER_TESTS=1; it runs on this host).
func TestDockerMeasureRemote(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run against Docker")
	}
	dc, err := docker.NewClient(docker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "world.db"), make([]byte, 300<<10), 0o644)
	os.Mkdir(filepath.Join(dir, "with space"), 0o755)
	file := filepath.Join(dir, "world.db")
	rm := m.measureRemote(context.Background(), LocalHost, []string{dir, dir + "/", file, dir + "/gone", dir + "/with space", "/proc"})
	if rm.err != nil {
		t.Fatal(rm.err)
	}
	if f := rm.bind(context.Background(), dir); f.Size < 300<<10 || f.Missing {
		t.Errorf("dir = %+v", f)
	}
	if f := rm.bind(context.Background(), file); f.Path != "" {
		t.Errorf("file = %+v", f)
	}
	if f := rm.bind(context.Background(), dir+"/gone"); !f.Missing {
		t.Errorf("gone = %+v", f)
	}
	if f := rm.bind(context.Background(), dir+"/with space"); f.Size < 0 || f.Missing {
		t.Errorf("with space = %+v", f)
	}
	left, _ := dc.ListContainers(context.Background(), "io.hlm.cleanup=true")
	if len(left) != 0 {
		t.Errorf("helpers left: %d", len(left))
	}
}
