package manager

import (
	"context"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// StorageReport is disk usage per server plus Docker's own totals. Measuring
// data folders can take a while, so it is computed in the background and
// served from the last result.
type StorageReport struct {
	Docker      *docker.DiskUsage `json:"docker,omitempty"`
	DockerError string            `json:"dockerError,omitempty"`
	Servers     []ServerStorage   `json:"servers"`
	// Deleted are deleted servers whose data is still tracked.
	Deleted      []DeletedStorage `json:"deleted"`
	CalculatedAt *time.Time       `json:"calculatedAt,omitempty"`
	Calculating  bool             `json:"calculating"`
	// DataReason explains why data folder sizes are missing, if they are.
	DataReason string `json:"dataReason,omitempty"`
}

// ServerStorage is one server's disk usage.
type ServerStorage struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	App   string `json:"app,omitempty"`
	Icon  string `json:"icon,omitempty"`
	Image string `json:"image"`
	// ImageSize is the size of the server's image; ImageShared is set when
	// other servers use the same image, so the space is shared.
	ImageSize   int64        `json:"imageSize"`
	ImageShared bool         `json:"imageShared"`
	Writable    int64        `json:"writable"` // the container's own layer
	Data        []DataFolder `json:"data"`
	DataTotal   int64        `json:"dataTotal"`
}

// DataFolder is a bind mount or volume holding a server's data.
type DataFolder struct {
	Path   string `json:"path"`   // host path, or volume name
	Target string `json:"target"` // path in the container
	Volume bool   `json:"volume,omitempty"`
	Size   int64  `json:"size"` // -1 if unknown
	Error  string `json:"error,omitempty"`
	// Shared is set when several servers mount the same folder (e.g. a
	// shared SteamCMD directory); it is counted in each of them.
	Shared bool `json:"shared,omitempty"`
	// Within names another of the server's folders that contains this one.
	// Its size is already part of that folder's, so it isn't added again.
	Within string `json:"within,omitempty"`
	// For deleted servers: Missing means the folder or volume is gone, and
	// InUseBy names existing servers that use it now (e.g. a reinstall).
	Missing bool     `json:"missing,omitempty"`
	InUseBy []string `json:"inUseBy,omitempty"`
}

// skipDataPath lists host paths that are system locations rather than game
// data, and would be huge or meaningless to measure.
var skipDataPath = map[string]bool{
	"/": true, "/proc": true, "/sys": true, "/dev": true, "/run": true, "/var/run": true,
	"/etc": true, "/usr": true, "/bin": true, "/sbin": true, "/lib": true, "/lib64": true,
	"/boot": true, "/var/lib/docker": true, "/tmp": true,
}

type storageTracker struct {
	mu          sync.Mutex
	reports     map[string]StorageReport // by host
	calculating bool
	run         sync.Mutex
}

// UseHostMetrics lets the manager measure data folders through the host
// metrics collector's view of the host filesystem.
func (m *Manager) UseHostMetrics(c *hostmetrics.Collector) {
	m.host = c
}

// Storage returns the last storage report of a host ("" for this one).
func (m *Manager) Storage(host string) StorageReport {
	if host == "" {
		host = LocalHost
	}
	t := m.storage
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.reports[host]
	r.Calculating = t.calculating
	if r.Servers == nil {
		r.Servers = []ServerStorage{}
	}
	if r.Deleted == nil {
		r.Deleted = []DeletedStorage{}
	}
	return r
}

// RefreshStorage recalculates the storage report in the background, unless a
// calculation is already running.
func (m *Manager) RefreshStorage() {
	t := m.storage
	t.mu.Lock()
	if t.calculating {
		t.mu.Unlock()
		return
	}
	t.calculating = true
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		m.calculateStorage(ctx)
	}()
}

// RunStorageRefresher recalculates storage every interval until ctx is done.
func (m *Manager) RunStorageRefresher(ctx context.Context, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
		m.RefreshStorage()
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.RefreshStorage()
		}
	}
}

func (m *Manager) calculateStorage(ctx context.Context) {
	t := m.storage
	t.run.Lock()
	defer t.run.Unlock()
	defer func() {
		t.mu.Lock()
		t.calculating = false
		t.mu.Unlock()
	}()

	hosts := m.docker.hosts()
	for _, host := range hosts {
		m.calculateHostStorage(ctx, host)
	}
	// Hosts that were removed.
	t.mu.Lock()
	for host := range t.reports {
		if !slices.Contains(hosts, host) {
			delete(t.reports, host)
		}
	}
	t.mu.Unlock()
}

// calculateHostStorage makes one host's storage report.
func (m *Manager) calculateHostStorage(ctx context.Context, host string) {
	var r StorageReport
	du, err := m.docker.SystemDiskUsage(ctx, host)
	if err != nil {
		r.DockerError = err.Error()
	} else {
		r.Docker = &du
	}

	list, err := m.docker.ListContainersWithSizeOn(ctx, host, deploy.LabelManaged+"=true")
	if err != nil {
		r.DockerError = err.Error()
		m.setStorage(host, r)
		return
	}

	// Data folders are measured here through the host's filesystem, or on
	// another host by a helper container.
	var meas measurer
	if host == LocalHost {
		local := localMeasurer{m: m, can: m.host != nil && m.host.Available()}
		if !local.can {
			r.DataReason = "data folder sizes need access to the Docker host's filesystem"
		}
		meas = local
	} else {
		var paths []string
		for _, c := range list {
			for _, mnt := range c.Mounts {
				if mnt.Type == "bind" {
					paths = append(paths, mnt.Source)
				}
			}
		}
		for _, d := range m.deletedOn(host) {
			for _, f := range d.Folders {
				if !f.Volume {
					paths = append(paths, f.Path)
				}
			}
		}
		rm := m.measureRemote(ctx, host, paths)
		if rm.err != nil {
			r.DataReason = "data folder sizes couldn't be measured on " + m.HostName(host) + ": " + rm.err.Error()
		}
		meas = rm
	}

	// Count how many servers use each image and each folder.
	imageUsers := map[string]int{}
	folderUsers := map[string]int{}
	folderNames := map[string][]string{}
	for _, c := range list {
		imageUsers[c.ImageID]++
		for _, mnt := range c.Mounts {
			folderUsers[mountKey(mnt)]++
			folderNames[mountKey(mnt)] = append(folderNames[mountKey(mnt)], strings.TrimPrefix(firstOr(c.Names, ""), "/"))
		}
	}

	sizes := map[string]DataFolder{} // measured once per folder
	for _, c := range list {
		srv := ServerStorage{
			ID:          c.ID,
			Name:        strings.TrimPrefix(firstOr(c.Names, ""), "/"),
			App:         c.Labels[deploy.LabelApp],
			Icon:        c.Labels["net.unraid.docker.icon"],
			Image:       c.Image,
			Writable:    c.SizeRw,
			ImageSize:   c.SizeRootFs - c.SizeRw,
			ImageShared: imageUsers[c.ImageID] > 1,
			Data:        []DataFolder{},
		}
		for _, mnt := range c.Mounts {
			key := mountKey(mnt)
			folder, done := sizes[key]
			if !done {
				folder = m.measure(ctx, mnt, r.Docker, meas)
				sizes[key] = folder
			}
			if folder.Path == "" {
				continue // not a data folder (tmpfs, socket, system path)
			}
			if folder.Missing {
				// Docker mounted it, so it's there: the helper couldn't see it
				// (through a symlink, say).
				folder.Missing, folder.Size, folder.Error = false, -1, "couldn't be found to measure"
			}
			folder.Target = mnt.Destination
			folder.Shared = folderUsers[key] > 1
			srv.Data = append(srv.Data, folder)
		}
		srv.DataTotal = totalWithoutNested(srv.Data)
		sort.Slice(srv.Data, func(i, j int) bool { return srv.Data[i].Size > srv.Data[j].Size })
		r.Servers = append(r.Servers, srv)
	}
	sort.Slice(r.Servers, func(i, j int) bool {
		a, b := r.Servers[i], r.Servers[j]
		return a.DataTotal+a.Writable+a.ImageSize > b.DataTotal+b.Writable+b.ImageSize
	})
	r.Deleted = m.deletedReport(ctx, host, r.Docker, meas, sizes, folderNames)
	m.setStorage(host, r)
}

func (m *Manager) setStorage(host string, r StorageReport) {
	now := time.Now()
	r.CalculatedAt = &now
	m.storage.mu.Lock()
	if m.storage.reports == nil {
		m.storage.reports = map[string]StorageReport{}
	}
	m.storage.reports[host] = r
	m.storage.mu.Unlock()
}

// totalWithoutNested adds up folder sizes, marking folders that sit inside
// another of the same server's folders and not counting them twice.
func totalWithoutNested(folders []DataFolder) int64 {
	var total int64
	for i := range folders {
		f := &folders[i]
		for _, outer := range folders {
			if !f.Volume && !outer.Volume && outer.Path != f.Path && strings.HasPrefix(f.Path, strings.TrimRight(outer.Path, "/")+"/") {
				f.Within = outer.Target
				break
			}
		}
		if f.Within == "" && f.Size > 0 {
			total += f.Size
		}
	}
	return total
}

func mountKey(mnt docker.MountPoint) string {
	if mnt.Type == "volume" {
		return "volume:" + mnt.Name
	}
	return mnt.Type + ":" + mnt.Source
}

// measurer measures a host's data folders.
type measurer interface {
	// bind measures a folder on the host: a zero DataFolder if it isn't one
	// (a socket or a file), Missing if it isn't there.
	bind(ctx context.Context, p string) DataFolder
}

// measure returns the size of one mount. A zero DataFolder means the mount
// isn't a data folder.
func (m *Manager) measure(ctx context.Context, mnt docker.MountPoint, du *docker.DiskUsage, meas measurer) DataFolder {
	switch mnt.Type {
	case "volume":
		f := DataFolder{Path: mnt.Name, Volume: true, Size: -1}
		if du != nil {
			if size, ok := du.VolumeSizes[mnt.Name]; ok {
				f.Size = size
			}
		}
		return f
	case "bind":
	default:
		return DataFolder{}
	}
	if skipDataPath[strings.TrimRight(mnt.Source, "/")] || skipDataPath[mnt.Source] || strings.HasSuffix(mnt.Source, ".sock") {
		return DataFolder{}
	}
	return meas.bind(ctx, mnt.Source)
}

// localMeasurer measures folders on this host, through the host metrics
// collector's view of its filesystem.
type localMeasurer struct {
	m   *Manager
	can bool
}

func (l localMeasurer) bind(ctx context.Context, p string) DataFolder {
	m := l.m
	mnt := docker.MountPoint{Source: p}
	f := DataFolder{Path: mnt.Source, Size: -1}
	if !l.can {
		return f
	}
	if ctx.Err() != nil {
		f.Error = ctx.Err().Error()
		return f
	}
	local := m.host.HostPath(mnt.Source)
	st, err := os.Stat(local)
	if err != nil {
		f.Error = "not readable: " + err.Error()
		return f
	}
	if !st.IsDir() {
		return DataFolder{} // a socket or single file, e.g. docker.sock
	}
	size, err := hostmetrics.DirSize(local)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.Size = int64(size)
	return f
}
