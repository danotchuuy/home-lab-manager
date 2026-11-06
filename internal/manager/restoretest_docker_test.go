package manager

import (
	"archive/tar"
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// TestDockerTestCopy runs against the real daemon (HLM_DOCKER_TESTS=1): a
// "server" that serves a TCP port only when its data has a world file.
// Its copy gets the backup's world, answers on its internal network
// without touching the server's data or publishing ports, can't reach out,
// and leaves nothing behind.
func TestDockerTestCopy(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run against Docker")
	}
	const name = "hlm-rt-server"
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("pull", "-q", "busybox:1.37")
	data := t.TempDir()
	os.WriteFile(data+"/real-save", []byte("the server's own"), 0o644)
	script := `if [ -f /data/world ]; then exec nc -lk -p 7777 -e cat /data/world; else echo "no world" >&2; exit 3; fi`
	run("create", "--name", name, "--label", "io.hlm.managed=true", "-p", "127.0.0.1::7777", "-v", data+":/data",
		"--memory", "64m", "busybox:1.37", "sh", "-c", script)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", name).Run() })

	dc, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	ctx := context.Background()
	t.Cleanup(func() { m.RemoveTestCopies(context.Background(), name) })

	backup := func(withWorld bool) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
		if withWorld {
			tw.WriteHeader(&tar.Header{Name: "data/world", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
			tw.Write([]byte("hello\n"))
		}
		tw.Close()
		return buf.Bytes()
	}
	check := func(withWorld bool) (answered bool, exited bool) {
		tc, err := m.CreateTestCopy(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := m.RemoveTestCopy(ctx, tc); err != nil {
				t.Errorf("remove: %v", err)
			}
		}()
		if _, err := m.RestoreTestCopy(ctx, tc, bytes.NewReader(backup(withWorld)), nil); err != nil {
			t.Fatal(err)
		}
		if err := m.StartTestCopy(ctx, tc); err != nil {
			t.Fatal(err)
		}
		for range 50 {
			info, err := m.InspectTestCopy(ctx, tc)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.HostConfig.PortBindings) != 0 {
				t.Errorf("ports published: %v", info.HostConfig.PortBindings)
			}
			if !info.State.Running {
				if !strings.Contains(m.TestCopyLogs(ctx, tc, 50), "no world") {
					t.Errorf("logs: %q", m.TestCopyLogs(ctx, tc, 50))
				}
				return false, true
			}
			ip := info.NetworkSettings.Networks["hlm-restore-test-"+name].IPAddress
			if c, err := net.DialTimeout("tcp", ip+":7777", time.Second); err == nil {
				buf := make([]byte, 5)
				c.Read(buf)
				c.Close()
				if string(buf) != "hello" {
					t.Errorf("answer: %q", buf)
				}
				// The copy can't reach out.
				res, _ := dc.Exec(ctx, tc.ID, "", []string{"sh", "-c", "wget -q -T 2 -O /dev/null http://1.1.1.1 && echo out || echo blocked"}, 1024)
				if !strings.Contains(string(res.Stdout), "blocked") {
					t.Errorf("outbound: %q", res.Stdout)
				}
				return true, false
			}
			time.Sleep(200 * time.Millisecond)
		}
		return false, false
	}
	if answered, _ := check(true); !answered {
		t.Fatal("a good backup's copy didn't answer")
	}
	if _, exited := check(false); !exited {
		t.Fatal("a backup without the world: the copy didn't exit")
	}
	if b, _ := os.ReadFile(data + "/real-save"); string(b) != "the server's own" {
		t.Error("the server's data changed")
	}
	if _, err := os.Stat(data + "/world"); err == nil {
		t.Error("the backup was restored into the server's own data")
	}
	if out := run("ps", "-a", "--filter", "label="+LabelRestoreTest, "-q"); out != "" {
		t.Errorf("copies left: %s", out)
	}
	if out := run("volume", "ls", "-q", "--filter", "label="+LabelRestoreTest); out != "" {
		t.Errorf("volumes left: %s", out)
	}
	if out := run("network", "ls", "-q", "--filter", "label="+LabelRestoreTest); out != "" {
		t.Errorf("networks left: %s", out)
	}
}
