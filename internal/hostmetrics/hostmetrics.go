// Package hostmetrics samples the Docker host's CPU, memory, network, disk
// and filesystem usage from Linux procfs and statfs.
//
// When hlm runs in a container, mount the host's /proc and root filesystem
// read-only and point ProcPath and RootPath at them; when it runs directly on
// the host, the defaults read the host itself.
package hostmetrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config says where to find the host's files.
type Config struct {
	// ProcPath is the host's procfs, normally /proc.
	ProcPath string
	// RootPath is where the host's root filesystem is visible, used as a
	// prefix for mount points and data folders. Empty means "/".
	RootPath string
	// Interval between samples, and how many to keep for the history.
	Interval time.Duration
	History  int
}

// Point is one history sample.
type Point struct {
	T       time.Time `json:"t"`
	CPU     float64   `json:"cpu"`     // percent of all cores
	MemUsed uint64    `json:"memUsed"` // bytes
	Rx      float64   `json:"rx"`      // bytes per second, all host interfaces
	Tx      float64   `json:"tx"`
}

// Snapshot is the latest view of the host.
type Snapshot struct {
	Available bool      `json:"available"`
	Reason    string    `json:"reason,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
	Kernel    string    `json:"kernel,omitempty"`
	Uptime    float64   `json:"uptime"` // seconds
	SampledAt time.Time `json:"sampledAt"`
	Interval  float64   `json:"interval"` // seconds between history points

	CPU     CPU          `json:"cpu"`
	Memory  Memory       `json:"memory"`
	Network []Interface  `json:"network"`
	Disks   []Filesystem `json:"filesystems"`
	DiskIO  []DiskIO     `json:"diskIO"`
	History []Point      `json:"history"`
}

type CPU struct {
	Cores   int     `json:"cores"`
	Percent float64 `json:"percent"`
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	Load15  float64 `json:"load15"`
}

type Memory struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
	Cached    uint64 `json:"cached"`
	SwapTotal uint64 `json:"swapTotal"`
	SwapUsed  uint64 `json:"swapUsed"`
}

type Interface struct {
	Name    string  `json:"name"`
	RxRate  float64 `json:"rxRate"`
	TxRate  float64 `json:"txRate"`
	RxTotal uint64  `json:"rxTotal"`
	TxTotal uint64  `json:"txTotal"`
}

type Filesystem struct {
	Device string `json:"device"`
	Mount  string `json:"mount"`
	FSType string `json:"fsType"`
	Total  uint64 `json:"total"`
	Used   uint64 `json:"used"`
	Free   uint64 `json:"free"` // available to unprivileged users
}

type DiskIO struct {
	Device    string  `json:"device"`
	ReadRate  float64 `json:"readRate"`
	WriteRate float64 `json:"writeRate"`
}

// Collector samples the host periodically.
type Collector struct {
	cfg Config

	mu       sync.RWMutex
	snap     Snapshot
	history  []Point
	prevCPU  cpuTimes
	prevNet  map[string][2]uint64
	prevDisk map[string][2]uint64
	prevAt   time.Time
	// observers are called with each sample.
	observers []func(Snapshot)
}

// Observe calls fn with each new sample (without the history), after it's
// taken. Call it before Run.
func (c *Collector) Observe(fn func(Snapshot)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observers = append(c.observers, fn)
}

// New creates a collector. If unavailable is non-empty, the host can't be
// observed (e.g. the Docker host is remote) and every snapshot says why.
func New(cfg Config, unavailable string) *Collector {
	if cfg.ProcPath == "" {
		cfg.ProcPath = "/proc"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.History <= 0 {
		cfg.History = 360
	}
	c := &Collector{cfg: cfg}
	c.snap.Interval = cfg.Interval.Seconds()
	if unavailable != "" {
		c.snap.Reason = unavailable
		return c
	}
	if _, err := os.Stat(filepath.Join(cfg.ProcPath, "stat")); err != nil {
		c.snap.Reason = fmt.Sprintf("host metrics need a Linux procfs at %s", cfg.ProcPath)
	}
	return c
}

// Run samples every interval until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	if c.snap.Reason != "" {
		return
	}
	c.sampleAndNotify()
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sampleAndNotify()
		}
	}
}

func (c *Collector) sampleAndNotify() {
	c.sample()
	c.mu.RLock()
	s, obs, ok := c.snap, c.observers, len(c.history) > 0 && c.snap.Available
	c.mu.RUnlock()
	// The first sample has no rates yet.
	if !ok {
		return
	}
	for _, fn := range obs {
		fn(s)
	}
}

// Snapshot returns the latest sample and the history.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snap
	s.History = append([]Point(nil), c.history...)
	return s
}

// HostPath maps a path on the host to where hlm can read it.
func (c *Collector) HostPath(p string) string {
	if c.cfg.RootPath == "" || c.cfg.RootPath == "/" {
		return p
	}
	return filepath.Join(c.cfg.RootPath, p)
}

// Available reports whether host files can be read.
func (c *Collector) Available() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap.Reason == ""
}

func (c *Collector) proc(parts ...string) string {
	return filepath.Join(append([]string{c.cfg.ProcPath}, parts...)...)
}

func (c *Collector) sample() {
	now := time.Now()
	s := Snapshot{Available: true, SampledAt: now, Interval: c.cfg.Interval.Seconds()}
	// procfs reports the hostname of the reader's namespace, which is the
	// container's when hlm runs in one; the host's root has the real name.
	if c.cfg.RootPath != "" && c.cfg.RootPath != "/" {
		s.Hostname = readTrim(c.HostPath("/etc/hostname"))
	}
	if s.Hostname == "" {
		s.Hostname = readTrim(c.proc("sys", "kernel", "hostname"))
	}
	s.Kernel = readTrim(c.proc("sys", "kernel", "osrelease"))
	if f := strings.Fields(readTrim(c.proc("uptime"))); len(f) > 0 {
		s.Uptime, _ = strconv.ParseFloat(f[0], 64)
	}

	cpu, cores, err := readCPU(c.proc("stat"))
	if err != nil {
		c.fail(err)
		return
	}
	s.CPU.Cores = cores
	if f := strings.Fields(readTrim(c.proc("loadavg"))); len(f) >= 3 {
		s.CPU.Load1, _ = strconv.ParseFloat(f[0], 64)
		s.CPU.Load5, _ = strconv.ParseFloat(f[1], 64)
		s.CPU.Load15, _ = strconv.ParseFloat(f[2], 64)
	}
	s.Memory, _ = readMemory(c.proc("meminfo"))
	// PID 1's view is the host's network namespace and mounts, even when hlm
	// itself runs in a container with the host's /proc mounted.
	net, _ := readNetDev(c.proc("1", "net", "dev"))
	disks, _ := readDiskStats(c.proc("diskstats"))
	s.Disks = c.filesystems()

	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := now.Sub(c.prevAt).Seconds()
	first := c.prevAt.IsZero()
	if !first {
		s.CPU.Percent = cpu.percentSince(c.prevCPU)
	}
	var rx, tx float64
	for _, name := range sortedKeys(net) {
		v := net[name]
		iface := Interface{Name: name, RxTotal: v[0], TxTotal: v[1]}
		if p, ok := c.prevNet[name]; ok && !first && elapsed > 0 {
			iface.RxRate = rate(v[0], p[0], elapsed)
			iface.TxRate = rate(v[1], p[1], elapsed)
		}
		rx += iface.RxRate
		tx += iface.TxRate
		s.Network = append(s.Network, iface)
	}
	for _, name := range sortedKeys(disks) {
		v := disks[name]
		io := DiskIO{Device: name}
		if p, ok := c.prevDisk[name]; ok && !first && elapsed > 0 {
			io.ReadRate = rate(v[0], p[0], elapsed)
			io.WriteRate = rate(v[1], p[1], elapsed)
		}
		s.DiskIO = append(s.DiskIO, io)
	}
	c.prevCPU, c.prevNet, c.prevDisk, c.prevAt = cpu, net, disks, now

	if !first {
		c.history = append(c.history, Point{T: now, CPU: s.CPU.Percent, MemUsed: s.Memory.Used, Rx: rx, Tx: tx})
		if over := len(c.history) - c.cfg.History; over > 0 {
			c.history = append([]Point(nil), c.history[over:]...)
		}
	}
	c.snap = s
}

func (c *Collector) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = Snapshot{Reason: "reading host metrics failed: " + err.Error(), Interval: c.cfg.Interval.Seconds()}
}

func rate(cur, prev uint64, secs float64) float64 {
	if cur < prev {
		return 0 // counter reset, e.g. an interface was recreated
	}
	return float64(cur-prev) / secs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// --- CPU ---

type cpuTimes struct {
	idle, total uint64
}

func (c cpuTimes) percentSince(prev cpuTimes) float64 {
	dt := float64(c.total - prev.total)
	if c.total <= prev.total || dt == 0 {
		return 0
	}
	return (1 - float64(c.idle-prev.idle)/dt) * 100
}

func readCPU(path string) (cpuTimes, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuTimes{}, 0, err
	}
	defer f.Close()
	var t cpuTimes
	cores := 0
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			cores++
			continue
		}
		found = true
		// user nice system idle iowait irq softirq steal (guest time is
		// already included in user and nice).
		for i, v := range fields[1:] {
			if i >= 8 {
				break
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			t.total += n
			if i == 3 || i == 4 {
				t.idle += n
			}
		}
	}
	if !found {
		return t, 0, errors.New("no cpu line in " + path)
	}
	return t, cores, sc.Err()
}

// --- memory ---

func readMemory(path string) (Memory, error) {
	f, err := os.Open(path)
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(fields[0], 10, 64)
		vals[key] = n * 1024 // values are in KiB
	}
	m := Memory{
		Total:     vals["MemTotal"],
		Available: vals["MemAvailable"],
		Cached:    vals["Cached"],
		SwapTotal: vals["SwapTotal"],
	}
	if m.Total > m.Available {
		m.Used = m.Total - m.Available
	}
	if m.SwapTotal > vals["SwapFree"] {
		m.SwapUsed = m.SwapTotal - vals["SwapFree"]
	}
	return m, sc.Err()
}

// --- network ---

// virtualIface matches interfaces Docker and friends create; traffic through
// them is already counted on the host's real interfaces.
var virtualIface = regexp.MustCompile(`^(lo|veth.*|docker\d+|br-.*|virbr.*|cni.*|flannel.*|vxlan.*|dm-.*|tap.*|tailscale.*)$`)

func readNetDev(path string) (map[string][2]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][2]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // header lines
		}
		name = strings.TrimSpace(name)
		if virtualIface.MatchString(name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		out[name] = [2]uint64{rx, tx}
	}
	return out, sc.Err()
}

// --- disks ---

// wholeDisk matches block devices that are disks rather than partitions.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+|md\d+)$`)

// readDiskStats returns bytes read and written per whole disk.
func readDiskStats(path string) (map[string][2]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][2]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || !wholeDisk.MatchString(fields[2]) {
			continue
		}
		read, _ := strconv.ParseUint(fields[5], 10, 64)
		written, _ := strconv.ParseUint(fields[9], 10, 64)
		out[fields[2]] = [2]uint64{read * 512, written * 512} // sectors are 512 bytes
	}
	return out, sc.Err()
}

// realFS lists filesystem types that hold user data; pseudo filesystems like
// tmpfs, overlay and squashfs are left out.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "f2fs": true, "bcachefs": true,
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true,
	"fuse.mergerfs": true, "fuse.shfs": true, "fuseblk": true,
}

func (c *Collector) filesystems() []Filesystem {
	f, err := os.Open(c.proc("1", "mounts"))
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	out := []Filesystem{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !realFS[fields[2]] {
			continue
		}
		dev, mount := fields[0], unescapeMount(fields[1])
		if seen[dev] || strings.HasPrefix(mount, "/var/lib/docker/") || strings.HasPrefix(mount, "/snap/") {
			continue // btrfs subvolumes and bind mounts repeat the same device
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(c.HostPath(mount), &st); err != nil || st.Blocks == 0 {
			continue
		}
		seen[dev] = true
		bs := uint64(st.Bsize)
		out = append(out, Filesystem{
			Device: dev,
			Mount:  mount,
			FSType: fields[2],
			Total:  st.Blocks * bs,
			Used:   (st.Blocks - st.Bfree) * bs,
			Free:   st.Bavail * bs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// unescapeMount decodes the octal escapes (\040 for a space) in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
