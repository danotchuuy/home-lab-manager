package hostmetrics

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// runSh runs the dump script with this machine's sh.
func runSh(ctx context.Context, script string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...).Output()
}

// A dump of this machine, read like another host's, gives what reading it
// directly does.
func TestDumpOfThisMachine(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no procfs")
	}
	ctx := context.Background()
	remote := NewRemote(time.Second, 10, FetchWith(runSh, "/proc", ""))
	if remote.Snapshot().Available || remote.Available() {
		t.Fatal("available before sampling")
	}
	remote.SampleNow(ctx)
	time.Sleep(200 * time.Millisecond)
	remote.SampleNow(ctx)
	got := remote.Snapshot()
	local := New(Config{ProcPath: "/proc"}, "")
	local.sample(ctx)
	want := local.Snapshot()
	if !got.Available || got.CPU.Cores != want.CPU.Cores || got.Memory.Total != want.Memory.Total ||
		got.Kernel != want.Kernel || got.Hostname != want.Hostname || len(got.History) != 1 {
		t.Fatalf("remote %+v\nlocal %+v", got, want)
	}
	if len(got.Disks) != len(want.Disks) {
		t.Errorf("disks: remote %+v, local %+v", got.Disks, want.Disks)
	}
	for i := range got.Disks {
		g, w := got.Disks[i], want.Disks[i]
		// df rounds to KiB.
		if g.Mount != w.Mount || g.Total/1024 != w.Total/1024 {
			t.Errorf("disk %d: %+v vs %+v", i, g, w)
		}
	}
	if len(got.Network) == 0 && len(want.Network) > 0 {
		t.Errorf("no network: %+v", got)
	}
	// The collector isn't "available" for measuring folders through it.
	if remote.Available() {
		t.Error("a remote collector reads local folders")
	}
}

func TestParseDump(t *testing.T) {
	out := "==hlm:proc stat\ncpu  1 2 3 4 5 6 7 8\ncpu0 1 2 3 4 5 6 7 8\n\n==hlm:proc sys/kernel/hostname\nnas\n\n" +
		"==hlm:root /etc/hostname\n\n==hlm:df\n/ 1000 400 500\n/mnt/disk 2000 x 1\n==hlm:end\n"
	d, err := ParseDump([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := d.proc("stat"); !strings.HasPrefix(string(b), "cpu  1") {
		t.Errorf("stat %q", b)
	}
	if _, err := d.root("/etc/hostname"); err == nil {
		t.Error("empty root hostname read")
	}
	if total, used, free, err := d.statfs("/"); err != nil || total != 1000*1024 || used != 400*1024 || free != 500*1024 {
		t.Errorf("df / %d %d %d %v", total, used, free, err)
	}
	if _, _, _, err := d.statfs("/mnt/disk"); err == nil {
		t.Error("bad df line read")
	}
	for _, bad := range []string{"", "==hlm:proc stat\ncpu 1\n", "==hlm:proc meminfo\nx\n==hlm:end\n"} {
		if _, err := ParseDump([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestDockerDump runs the dump in busybox with the host's procfs and root
// mounted read-only, as hlm does on hosts it reaches over TLS (opt-in:
// HLM_DOCKER_TESTS=1).
func TestDockerDump(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1")
	}
	run := func(ctx context.Context, script string, args []string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"run", "--rm", "--network", "none",
			"-v", "/proc:/host/proc:ro", "-v", "/:/host/rootfs:ro", "busybox:1.37", "sh", "-c", script, "sh"}, args...)...).Output()
	}
	c := NewRemote(time.Second, 10, FetchWith(run, "/host/proc", "/host/rootfs"))
	c.SampleNow(context.Background())
	c.SampleNow(context.Background())
	s := c.Snapshot()
	host, _ := os.ReadFile("/etc/hostname")
	if !s.Available || s.CPU.Cores == 0 || len(s.Disks) == 0 || s.Hostname != strings.TrimSpace(string(host)) || len(s.Network) == 0 {
		t.Fatalf("%+v", s)
	}
	t.Logf("cores %d, mem %d, disks %+v, net %d", s.CPU.Cores, s.Memory.Total, s.Disks, len(s.Network))
}
