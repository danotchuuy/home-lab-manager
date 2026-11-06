package manager

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// TestDockerRemoteTestCopy runs against the real daemon (HLM_DOCKER_TESTS=1),
// reached as if it were another host ("other"), so hlm can't reach the
// copy's network and a probe helper (a static build of hlm, in the copy's
// image) asks from next to it.
func TestDockerRemoteTestCopy(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run against Docker")
	}
	const name = "hlm-rt-remote"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	exe := filepath.Join(t.TempDir(), "hlm")
	build := exec.Command("go", "build", "-o", exe, "../../cmd/hlm")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hlm: %v\n%s", err, out)
	}
	run("pull", "-q", "busybox:1.37")
	script := `if [ -f /data/world ]; then exec nc -lk -p 7777 -e cat /data/world; else exit 3; fi`
	run("create", "--name", name, "--label", "io.hlm.managed=true", "-v", t.TempDir()+":/data", "busybox:1.37", "sh", "-c", script)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", name).Run() })

	dc, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	m.SetHosts([]RemoteHost{{ID: "other", Name: "Other", Docker: dc}})
	m.UseExecutable(exe)
	ctx := context.Background()
	server := name + "@other"
	t.Cleanup(func() { m.RemoveTestCopies(context.Background(), server) })

	check := func(withWorld bool) (code int, out string) {
		tc, err := m.CreateTestCopy(ctx, server)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := m.RemoveTestCopy(ctx, tc); err != nil {
				t.Errorf("remove: %v", err)
			}
		}()
		if !tc.Remote() || tc.Host != "other" {
			t.Fatalf("copy: %+v", tc)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
		if withWorld {
			tw.WriteHeader(&tar.Header{Name: "data/world", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
			tw.Write([]byte("hello\n"))
		}
		tw.Close()
		if _, err := m.RestoreTestCopy(ctx, tc, &buf, nil); err != nil {
			t.Fatal(err)
		}
		if err := m.StartTestCopy(ctx, tc); err != nil {
			t.Fatal(err)
		}
		if err := m.StartTestProbe(ctx, tc, "probe-game", []string{"--host", tc.Name, "--methods", "tcp:7777", "--timeout", "15s", "--interval", "1s"}); err != nil {
			t.Fatal(err)
		}
		for range 60 {
			out, code, done, err := m.TestProbe(ctx, tc)
			if err != nil {
				t.Fatal(err)
			}
			if done {
				return code, out
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatal("the probe didn't finish")
		return 0, ""
	}
	code, out := check(true)
	var po struct {
		Result *struct{ Protocol string }
		Error  string
	}
	json.Unmarshal([]byte(strings.TrimSpace(out)), &po)
	if code != 0 || po.Result == nil || po.Result.Protocol != "tcp" {
		t.Fatalf("good backup: %d %q", code, out)
	}
	code, out = check(false)
	if code != 1 || !strings.Contains(out, "tcp port 7777") {
		t.Fatalf("backup without the world: %d %q", code, out)
	}
	for _, kind := range []string{"ps -a", "volume ls", "network ls"} {
		args := append(strings.Fields(kind), "-q", "--filter", "label="+LabelRestoreTest)
		if out := run(args...); out != "" {
			t.Errorf("%s left: %s", kind, out)
		}
	}
}
