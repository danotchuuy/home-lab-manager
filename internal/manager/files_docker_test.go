package manager

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// TestDockerModFiles runs against the real daemon (HLM_DOCKER_TESTS=1):
// MakeDir makes a folder owned like the one it's in, and RemoveFiles
// deletes files in a stopped server's volume, through helper containers
// (this manager can't see the host's filesystem).
func TestDockerModFiles(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run against Docker")
	}
	const name, vol = "hlm-modfiles-test", "hlm-modfiles-test"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	inVolume := func(script string) string {
		t.Helper()
		return run("run", "--rm", "-v", vol+":/data", "busybox:1.37", "sh", "-c", script)
	}
	run("pull", "-q", "busybox:1.37")
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "-v", name).Run()
		exec.Command("docker", "volume", "rm", vol).Run()
	})
	run("volume", "create", vol)
	inVolume("chown 1234:4321 /data && mkdir /data/plugins && touch /data/plugins/a.jar /data/plugins/b.jar")
	run("create", "--name", name, "--label", "io.hlm.managed=true", "-v", vol+":/data", "busybox:1.37", "true")

	dc, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	ctx := context.Background()
	if err := m.MakeDir(ctx, name, "/data/mods"); err != nil {
		t.Fatal(err)
	}
	if got := inVolume("stat -c %u:%g /data/mods"); got != "1234:4321" {
		t.Errorf("new folder's owner: %s", got)
	}
	if err := m.MakeDir(ctx, name, "/data/mods"); err != nil {
		t.Errorf("again: %v", err)
	}
	if err := m.RemoveFiles(ctx, name, []string{"/data/plugins/a.jar", "/data/plugins/gone.jar"}); err != nil {
		t.Fatal(err)
	}
	if got := inVolume("ls /data/plugins"); got != "b.jar" {
		t.Errorf("left: %q", got)
	}
	if err := m.RemoveFiles(ctx, name, []string{"/etc/passwd"}); err == nil {
		t.Error("removed a file outside the data folders")
	}
	if err := m.MakeDir(ctx, name, "/data/../etc/x"); err == nil {
		t.Error("made a folder outside the data folders")
	}
}
