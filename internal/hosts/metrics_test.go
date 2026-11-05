package hosts

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"golang.org/x/crypto/ssh"
)

// An SSH host's metrics: the script runs over its SSH connection (here, a
// test SSH server running commands on this machine), and the polling loop
// passes the samples on.
func TestSSHHostMetrics(t *testing.T) {
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no procfs")
	}
	kp, _ := remote.GenerateKey("hlm@test")
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	target, hostKey, _ := sshServer(t, pub, sshOptions{shell: true})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	r, store, _ := registry(t, kp)
	h := Host{ID: "nas", Name: "NAS", Kind: KindSSH, SSH: &target, Socket: DefaultSocket, AppdataRoot: "/srv", Address: target.Host}
	if err := store.Set(secretHosts, []Host{h}); err != nil {
		t.Fatal(err)
	}
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, err := r.HostMetrics(ctx, "nas")
	if err != nil {
		t.Fatal(err)
	}
	// The first sample has no rates yet, but is there.
	if snap.Reason != "" && snap.Reason != notSampled {
		t.Fatalf("first sample: %+v", snap)
	}
	got := make(chan hostmetrics.Snapshot, 10)
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	r.mu.Lock()
	r.collectors["nas"].every = 50 * time.Millisecond
	r.mu.Unlock()
	go r.RunHostMetrics(pctx, 20*time.Millisecond, func(id string, s hostmetrics.Snapshot) {
		if id == "nas" && s.Available {
			select {
			case got <- s:
			default:
			}
		}
	})
	select {
	case s := <-got:
		local := hostmetrics.New(hostmetrics.Config{ProcPath: "/proc"}, "")
		local.SampleNow(ctx)
		want := local.Snapshot()
		if s.CPU.Cores != want.CPU.Cores || s.Memory.Total != want.Memory.Total || len(s.Disks) == 0 {
			t.Errorf("over SSH %+v, here %+v", s, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no sample over SSH")
	}
	// Removing the host drops its sampler.
	if err := r.Delete("nas"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.HostMetrics(ctx, "nas"); err == nil {
		t.Error("metrics of a removed host")
	}
	r.mu.Lock()
	n := len(r.collectors)
	r.mu.Unlock()
	if n != 0 {
		t.Error("sampler kept")
	}
}

// TestDockerHelperMetrics reads this machine's metrics through a helper
// container, as for a TLS host (opt-in: HLM_DOCKER_TESTS=1).
func TestDockerHelperMetrics(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1")
	}
	dc, err := docker.NewClient(docker.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := hostmetrics.NewRemote(time.Second, 10, hostmetrics.FetchWith(helperFetcher(dc), "/host/proc", "/host/rootfs"))
	c.SampleNow(ctx)
	c.SampleNow(ctx)
	if s := c.Snapshot(); !s.Available || s.CPU.Cores == 0 || len(s.Disks) == 0 {
		t.Fatalf("%+v", s)
	}
	left, _ := dc.ListContainers(ctx, LabelHelper+"=metrics")
	if len(left) != 0 {
		t.Errorf("helpers left behind: %d", len(left))
	}
}
