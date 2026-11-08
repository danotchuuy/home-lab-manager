package hostmetrics

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeProc writes a procfs with counters scaled by n, so two calls give a
// known delta.
func fakeProc(t *testing.T, dir string, n uint64) {
	t.Helper()
	u := func(v uint64) string { return strconv.FormatUint(v*n, 10) }
	writeFile(t, filepath.Join(dir, "stat"),
		"cpu  "+u(300)+" 0 "+u(100)+" "+u(600)+" 0 0 0 0 0 0\ncpu0 1 2 3 4\ncpu1 1 2 3 4\nintr 1\n")
	writeFile(t, filepath.Join(dir, "meminfo"),
		"MemTotal:       8000000 kB\nMemFree:  1000000 kB\nMemAvailable:   6000000 kB\nCached: 2000000 kB\nSwapTotal: 1000 kB\nSwapFree: 400 kB\n")
	writeFile(t, filepath.Join(dir, "loadavg"), "0.50 0.25 0.10 1/200 1234\n")
	writeFile(t, filepath.Join(dir, "uptime"), "3600.5 7000.0\n")
	writeFile(t, filepath.Join(dir, "sys", "kernel", "hostname"), "gamebox\n")
	writeFile(t, filepath.Join(dir, "1", "net", "dev"),
		"Inter-|   Receive |  Transmit\n face |bytes packets|bytes\n"+
			"    lo: 999999 1 0 0 0 0 0 0 999999 1 0 0 0 0 0 0\n"+
			"  eth0: "+u(5000)+" 10 0 0 0 0 0 0 "+u(1000)+" 5 0 0 0 0 0 0\n"+
			"vethabc: 777 1 0 0 0 0 0 0 777 1 0 0 0 0 0 0\n")
	writeFile(t, filepath.Join(dir, "diskstats"),
		"   8       0 sda 100 0 "+u(20)+" 0 50 0 "+u(40)+" 0 0 0 0\n"+
			"   8       1 sda1 100 0 999 0 50 0 999 0 0 0 0\n"+
			"   7       0 loop0 1 0 999 0 0 0 0 0 0 0 0\n")
	writeFile(t, filepath.Join(dir, "1", "mounts"),
		"/dev/sda1 / ext4 rw 0 0\nproc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n"+
			"/dev/sda1 /var/lib/docker/overlay ext4 rw 0 0\n")
}

func TestSample(t *testing.T) {
	dir := t.TempDir()
	c := New(Config{ProcPath: dir, RootPath: "/", Interval: time.Second}, "")
	fakeProc(t, dir, 1)
	c.sample(context.Background())
	s := c.Snapshot()
	if !s.Available || s.Hostname != "gamebox" || s.CPU.Cores != 2 || s.Uptime != 3600.5 || s.CPU.Load1 != 0.5 {
		t.Fatalf("first sample = %+v", s)
	}
	if len(s.History) != 0 {
		t.Errorf("first sample has no rates, so no history point; got %d", len(s.History))
	}

	// Double every counter after a second (plus however long sampling
	// takes, which rates are divided by too: slow CI runners take tens of
	// milliseconds).
	c.mu.Lock()
	c.prevAt = c.prevAt.Add(-time.Second)
	before := c.prevAt
	c.mu.Unlock()
	fakeProc(t, dir, 2)
	c.sample(context.Background())
	s = c.Snapshot()
	c.mu.Lock()
	elapsed := c.prevAt.Sub(before).Seconds()
	c.mu.Unlock()

	// Busy 400 of 1000 jiffies in the interval.
	if math.Abs(s.CPU.Percent-40) > 0.01 {
		t.Errorf("cpu = %.2f, want 40", s.CPU.Percent)
	}
	if s.Memory.Total != 8000000*1024 || s.Memory.Used != 2000000*1024 || s.Memory.SwapUsed != 600*1024 {
		t.Errorf("memory = %+v", s.Memory)
	}
	if len(s.Network) != 1 || s.Network[0].Name != "eth0" {
		t.Fatalf("network = %+v (lo and veth should be skipped)", s.Network)
	}
	if r, want := s.Network[0].RxRate, 5000/elapsed; math.Abs(r-want) > 1 {
		t.Errorf("rx rate = %.0f, want %.0f", r, want)
	}
	if len(s.DiskIO) != 1 || s.DiskIO[0].Device != "sda" || math.Abs(s.DiskIO[0].ReadRate-20*512/elapsed) > 1 {
		t.Errorf("disk io = %+v (partitions and loop devices should be skipped)", s.DiskIO)
	}
	if len(s.Disks) != 1 || s.Disks[0].Mount != "/" || s.Disks[0].Total == 0 {
		t.Errorf("filesystems = %+v", s.Disks)
	}
	if len(s.History) != 1 || s.History[0].Rx == 0 {
		t.Errorf("history = %+v", s.History)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	dir := t.TempDir()
	fakeProc(t, dir, 1)
	c := New(Config{ProcPath: dir, Interval: time.Second, History: 3}, "")
	for i := 0; i < 6; i++ {
		c.sample(context.Background())
	}
	if n := len(c.Snapshot().History); n != 3 {
		t.Errorf("history length = %d, want 3", n)
	}
}

func TestUnavailable(t *testing.T) {
	c := New(Config{}, "the Docker host is remote")
	if s := c.Snapshot(); s.Available || s.Reason == "" {
		t.Errorf("snapshot = %+v", s)
	}
	if c := New(Config{ProcPath: t.TempDir()}, ""); c.Available() {
		t.Error("collector without a procfs reports available")
	}
}

func TestRealProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux procfs")
	}
	c := New(Config{Interval: 100 * time.Millisecond}, "")
	c.sample(context.Background())
	time.Sleep(200 * time.Millisecond)
	c.sample(context.Background())
	s := c.Snapshot()
	if !s.Available || s.CPU.Cores == 0 || s.Memory.Total == 0 || len(s.Disks) == 0 {
		t.Errorf("real host snapshot looks empty: %+v", s)
	}
	t.Logf("cores=%d cpu=%.1f%% mem=%d/%d MiB filesystems=%d ifaces=%d disks=%d",
		s.CPU.Cores, s.CPU.Percent, s.Memory.Used>>20, s.Memory.Total>>20, len(s.Disks), len(s.Network), len(s.DiskIO))
	for _, f := range s.Disks {
		t.Logf("  %s on %s (%s): %d/%d GiB", f.Device, f.Mount, f.FSType, f.Used>>30, f.Total>>30)
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "big"), string(make([]byte, 100_000)))
	writeFile(t, filepath.Join(dir, "small"), "x")
	if err := os.Link(filepath.Join(dir, "a", "big"), filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	size, err := DirSize(dir)
	if err != nil {
		t.Fatal(err)
	}
	// ~100 KB counted once despite the hard link; the symlink isn't followed.
	if size < 100_000 || size > 200_000 {
		t.Errorf("DirSize = %d", size)
	}
	if _, err := DirSize(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory: want error")
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/my\040disk`); got != "/mnt/my disk" {
		t.Errorf("got %q", got)
	}
}

func TestInterfaces(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "1", "net", "route"), ""+
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"ens18\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"+
		"ens18\t01000001\t0100000A\t0007\t0\t0\t100\tFFFFFFFF\t0\t0\t0\n"+
		"ens18\t0000000A\t00000000\t0001\t0\t0\t100\t00F0FFFF\t0\t0\t0\n"+
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+
		"eth1\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n")
	c := New(Config{ProcPath: dir}, "")
	got, err := c.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []HostInterface{
		{Name: "ens18", Subnets: []string{"10.0.0.0/20"}, Gateway: "10.0.0.1", Default: true},
		{Name: "eth1", Subnets: []string{"192.168.1.0/24"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("interfaces = %+v\nwant %+v", got, want)
	}
}

func TestLANAddress(t *testing.T) {
	trie := `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     +-- 10.0.0.0/20 2 0 2
        |-- 10.0.0.0
           /20 link UNICAST
        |-- 10.0.14.42
           /32 host LOCAL
     |-- 127.0.0.1
        /32 host LOCAL
     |-- 172.17.0.1
        /32 host LOCAL
`
	sub := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/20")}
	if got := lanAddress(trie, sub); got != "10.0.14.42" {
		t.Errorf("lanAddress = %q", got)
	}
	if runtime.GOOS == "linux" {
		c := New(Config{}, "")
		t.Logf("this host's LAN address: %q", c.LANAddress())
	}
}
