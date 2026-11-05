package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// When a server is deleted its data folders and volumes stay on disk. hlm
// keeps a record of them, so the Host page can show what they still take up
// and the user can delete them or stop tracking them.

// DeletedServer is a deleted server's leftover data.
type DeletedServer struct {
	ID        string          `json:"id"` // the container's ID
	Name      string          `json:"name"`
	App       string          `json:"app,omitempty"`
	Icon      string          `json:"icon,omitempty"`
	Image     string          `json:"image,omitempty"`
	DeletedAt time.Time       `json:"deletedAt"`
	Folders   []DeletedFolder `json:"folders"`
}

// DeletedFolder is a folder or volume a deleted server used.
type DeletedFolder struct {
	Path   string `json:"path"` // host path, or volume name
	Target string `json:"target"`
	Volume bool   `json:"volume,omitempty"`
	Size   int64  `json:"size"` // when the server was deleted; -1 if unknown
}

// DeletedStorage is a deleted server in the storage report.
type DeletedStorage struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	App       string       `json:"app,omitempty"`
	Icon      string       `json:"icon,omitempty"`
	Image     string       `json:"image,omitempty"`
	DeletedAt time.Time    `json:"deletedAt"`
	Data      []DataFolder `json:"data"`
	DataTotal int64        `json:"dataTotal"`
}

type deletedStore struct {
	mu   sync.Mutex
	path string
}

// UseDeletedStore keeps the record of deleted servers' data in path.
func (m *Manager) UseDeletedStore(path string) {
	m.deleted = &deletedStore{path: path}
}

func (d *deletedStore) load() ([]DeletedServer, error) {
	data, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return []DeletedServer{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []DeletedServer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("read %s: %w", d.path, err)
	}
	return list, nil
}

func (d *deletedStore) save(list []DeletedServer) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

// DeletedServers returns the record of deleted servers' data.
func (m *Manager) DeletedServers() ([]DeletedServer, error) {
	if m.deleted == nil {
		return []DeletedServer{}, nil
	}
	m.deleted.mu.Lock()
	defer m.deleted.mu.Unlock()
	return m.deleted.load()
}

// recordDeleted remembers the data folders of a server about to be deleted,
// with their sizes from the last storage report.
func (m *Manager) recordDeleted(info docker.ContainerJSON) error {
	// The Host page is about the local host: other hosts' folders can't be
	// measured or deleted from here.
	if m.deleted == nil || HostOf(info.ID) != LocalHost {
		return nil
	}
	known := map[string]int64{}
	for _, s := range m.Storage().Servers {
		if s.ID == info.ID {
			for _, f := range s.Data {
				known[f.Target] = f.Size
			}
		}
	}
	rec := DeletedServer{
		ID:        info.ID,
		Name:      strings.TrimPrefix(info.Name, "/"),
		App:       info.Config.Labels[deploy.LabelApp],
		Icon:      info.Config.Labels["net.unraid.docker.icon"],
		Image:     info.Config.Image,
		DeletedAt: time.Now().UTC(),
		Folders:   []DeletedFolder{},
	}
	for _, mnt := range info.Mounts {
		f := DeletedFolder{Target: mnt.Destination, Size: -1}
		if size, ok := known[mnt.Destination]; ok {
			f.Size = size
		}
		switch mnt.Type {
		case "volume":
			f.Path, f.Volume = mnt.Name, true
		case "bind":
			src := path.Clean(mnt.Source)
			if skipDataPath[src] || strings.HasSuffix(src, ".sock") {
				continue
			}
			if m.host != nil && m.host.Available() {
				if st, err := os.Stat(m.host.HostPath(src)); err == nil && !st.IsDir() {
					continue // a socket or single file
				}
			}
			f.Path = src
		default:
			continue
		}
		rec.Folders = append(rec.Folders, f)
	}
	if len(rec.Folders) == 0 {
		return nil
	}
	m.deleted.mu.Lock()
	defer m.deleted.mu.Unlock()
	list, err := m.deleted.load()
	if err != nil {
		return err
	}
	list = append(list, rec)
	return m.deleted.save(list)
}

// ErrNotTracked means the folder isn't part of a deleted server's record.
var ErrNotTracked = errors.New("that folder isn't tracked for a deleted server")

// changeDeleted applies fn to the record and saves it, dropping servers
// with no folders left, then updates the last storage report to match.
func (m *Manager) changeDeleted(fn func([]DeletedServer) ([]DeletedServer, error)) error {
	if m.deleted == nil {
		return ErrNotTracked
	}
	m.deleted.mu.Lock()
	list, err := m.deleted.load()
	if err == nil {
		list, err = fn(list)
	}
	if err == nil {
		kept := []DeletedServer{}
		for _, d := range list {
			if len(d.Folders) > 0 {
				kept = append(kept, d)
			}
		}
		list = kept
		err = m.deleted.save(list)
	}
	m.deleted.mu.Unlock()
	if err != nil {
		return err
	}
	// Keep the report's measured sizes for what's still tracked.
	t := m.storage
	t.mu.Lock()
	tracked := map[string]map[string]bool{}
	for _, d := range list {
		tracked[d.ID] = map[string]bool{}
		for _, f := range d.Folders {
			tracked[d.ID][folderKey(f.Path, f.Volume)] = true
		}
	}
	var report []DeletedStorage
	for _, d := range t.report.Deleted {
		var data []DataFolder
		for _, f := range d.Data {
			if tracked[d.ID][folderKey(f.Path, f.Volume)] {
				data = append(data, f)
			}
		}
		if len(data) > 0 {
			d.Data = data
			d.DataTotal = deletedTotal(d.Data)
			report = append(report, d)
		}
	}
	t.report.Deleted = report
	t.mu.Unlock()
	return nil
}

// deletedTotal adds up a deleted server's folders, leaving out nested
// folders and ones an existing server uses again (they're counted there).
func deletedTotal(data []DataFolder) int64 {
	totalWithoutNested(data) // marks nested folders
	var total int64
	for _, f := range data {
		if f.Within == "" && len(f.InUseBy) == 0 && f.Size > 0 {
			total += f.Size
		}
	}
	return total
}

func folderKey(p string, volume bool) string {
	if volume {
		return "volume:" + p
	}
	return "bind:" + p
}

// ForgetDeleted stops tracking a deleted server's folder or volume, or all
// of its data when p is "". Nothing is deleted from disk.
func (m *Manager) ForgetDeleted(id, p string, volume bool) error {
	return m.changeDeleted(func(list []DeletedServer) ([]DeletedServer, error) {
		for i, d := range list {
			if d.ID != id {
				continue
			}
			if p == "" {
				list[i].Folders = nil
				return list, nil
			}
			for j, f := range d.Folders {
				if f.Path == p && f.Volume == volume {
					list[i].Folders = append(d.Folders[:j:j], d.Folders[j+1:]...)
					return list, nil
				}
			}
		}
		return nil, ErrNotTracked
	})
}

// DeleteDeletedData deletes a deleted server's folder or volume from disk
// and stops tracking it (for every deleted server that used it).
func (m *Manager) DeleteDeletedData(ctx context.Context, id, p string, volume bool) error {
	list, err := m.DeletedServers()
	if err != nil {
		return err
	}
	found := false
	for _, d := range list {
		for _, f := range d.Folders {
			if d.ID == id && f.Path == p && f.Volume == volume {
				found = true
			}
		}
	}
	if !found {
		return ErrNotTracked
	}
	if volume {
		if err := m.docker.RemoveVolume(ctx, LocalHost, p); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("remove volume %s: %w", p, err)
		}
	} else {
		if err := m.checkDeletable(ctx, p); err != nil {
			return err
		}
		if err := m.removeHostDir(ctx, p); err != nil {
			return err
		}
	}
	m.log.Info("deleted a deleted server's data", "server", id, "path", p, "volume", volume)
	return m.changeDeleted(func(list []DeletedServer) ([]DeletedServer, error) {
		for i, d := range list {
			var keep []DeletedFolder
			for _, f := range d.Folders {
				if !(f.Path == p && f.Volume == volume) {
					keep = append(keep, f)
				}
			}
			list[i].Folders = keep
		}
		return list, nil
	})
}

// checkDeletable refuses to delete host folders that aren't plainly a
// server's own data: system paths, top-level folders, the appdata root (or
// anything containing it), and folders any container still uses.
func (m *Manager) checkDeletable(ctx context.Context, p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p {
		return fmt.Errorf("%q isn't a clean absolute path", p)
	}
	if skipDataPath[p] || strings.Count(p, "/") < 2 {
		return fmt.Errorf("refusing to delete %s: it's a system or top-level folder", p)
	}
	if root := path.Clean(m.appdataRoot); m.appdataRoot != "" && (root == p || strings.HasPrefix(root, p+"/")) {
		return fmt.Errorf("refusing to delete %s: it holds every server's data (%s)", p, root)
	}
	list, err := m.docker.ListContainersOn(ctx, LocalHost)
	if err != nil {
		return err
	}
	for _, c := range list {
		for _, mnt := range c.Mounts {
			if mnt.Type != "bind" {
				continue
			}
			src := path.Clean(mnt.Source)
			if src == p || strings.HasPrefix(src, p+"/") {
				return fmt.Errorf("%s is still used by %s (mounted at %s)", p, strings.TrimPrefix(firstOr(c.Names, c.ID), "/"), mnt.Destination)
			}
		}
	}
	return nil
}

// cleanupImage runs the helper that deletes folders. Containers are used
// because hlm may not be able to write to the host's filesystem itself (it
// sees it read-only when it runs in a container, and the files often
// belong to the game server's user).
const cleanupImage = "busybox:1.37"

func (m *Manager) removeHostDir(ctx context.Context, p string) error {
	parent, base := path.Split(p)
	code, out, err := m.runHelper(ctx, LocalHost, strings.TrimRight(parent, "/")+":/hlm-parent", "rm", "-rf", "--", "/hlm-parent/"+base)
	if err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	if code != 0 {
		return fmt.Errorf("deleting %s failed (exit status %d): %s", p, code, out)
	}
	return nil
}

// checkRemoteFresh is checkFreshData's folder check on another host: a
// helper container looks for the folder there.
func (m *Manager) checkRemoteFresh(ctx context.Context, host, base string, inUse error) error {
	parent, name := path.Split(base)
	code, out, err := m.runHelper(ctx, host, strings.TrimRight(parent, "/")+":/hlm-parent:ro", "test", "!", "-e", "/hlm-parent/"+name)
	if err != nil {
		return fmt.Errorf("check %s on %s: %w", base, m.HostName(host), err)
	}
	if code != 0 {
		if out != "" {
			return fmt.Errorf("check %s on %s: %s", base, m.HostName(host), out)
		}
		return inUse
	}
	return nil
}

// runHelper runs a command in a throwaway busybox container on a host, with
// one bind mount, and returns its exit status and the end of its output.
func (m *Manager) runHelper(ctx context.Context, host, bind string, cmd ...string) (int, string, error) {
	if _, err := m.docker.InspectImage(ctx, host, cleanupImage); err != nil {
		if err := m.docker.PullImage(ctx, host, cleanupImage, nil); err != nil {
			return 0, "", fmt.Errorf("pull %s: %w", cleanupImage, err)
		}
	}
	id, _, err := m.docker.CreateContainerOn(ctx, host, "", docker.CreateRequest{
		Image:  cleanupImage,
		Cmd:    cmd,
		Labels: map[string]string{"io.hlm.cleanup": "true"},
		HostConfig: docker.HostConfig{
			Binds:       []string{bind},
			NetworkMode: "none",
		},
	})
	if err != nil {
		return 0, "", fmt.Errorf("create helper container: %w", err)
	}
	defer m.docker.RemoveContainer(context.Background(), id, true)
	if err := m.docker.StartContainer(ctx, id); err != nil {
		return 0, "", fmt.Errorf("start helper container: %w", err)
	}
	code, err := m.docker.WaitContainer(ctx, id)
	if err != nil {
		return 0, "", err
	}
	msg := ""
	if code != 0 {
		if r, err := m.docker.Logs(ctx, id, docker.LogsOptions{Tail: "20"}); err == nil {
			out, _ := io.ReadAll(io.LimitReader(r, 4096))
			r.Close()
			msg = strings.TrimSpace(string(out))
		}
	}
	return code, msg, nil
}

// deletedReport measures the folders of deleted servers for the storage
// report. sizes holds folders already measured for existing servers, and
// users the existing servers using each folder.
func (m *Manager) deletedReport(ctx context.Context, du *docker.DiskUsage, canMeasure bool, sizes map[string]DataFolder, users map[string][]string) []DeletedStorage {
	list, err := m.DeletedServers()
	if err != nil {
		m.log.Warn("read deleted servers", "err", err)
		return nil
	}
	var out []DeletedStorage
	for _, d := range list {
		ds := DeletedStorage{ID: d.ID, Name: d.Name, App: d.App, Icon: d.Icon, Image: d.Image, DeletedAt: d.DeletedAt, Data: []DataFolder{}}
		for _, f := range d.Folders {
			mnt := docker.MountPoint{Type: "bind", Source: f.Path, Destination: f.Target}
			if f.Volume {
				mnt = docker.MountPoint{Type: "volume", Name: f.Path, Destination: f.Target}
			}
			key := mountKey(mnt)
			folder, done := sizes[key]
			if !done {
				folder = m.measure(ctx, mnt, du, canMeasure)
				sizes[key] = folder
			}
			folder.Path, folder.Volume, folder.Target = f.Path, f.Volume, f.Target
			folder.InUseBy = users[key]
			switch {
			case f.Volume && du != nil && folder.Size < 0:
				folder.Missing = true // Docker no longer has it
			case !f.Volume && canMeasure && strings.HasPrefix(folder.Error, "not readable:") && os.IsNotExist(statErr(m, f.Path)):
				folder.Missing, folder.Error = true, ""
			}
			if folder.Missing {
				folder.Size = 0
			}
			ds.Data = append(ds.Data, folder)
		}
		ds.DataTotal = deletedTotal(ds.Data)
		sort.Slice(ds.Data, func(i, j int) bool { return ds.Data[i].Size > ds.Data[j].Size })
		out = append(out, ds)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt.After(out[j].DeletedAt) })
	return out
}

func statErr(m *Manager, p string) error {
	_, err := os.Stat(m.host.HostPath(p))
	return err
}

// checkFreshData makes sure a new confined server's folder under the appdata
// root is new. Something else may have put symlinks in a folder that's already
// there, which Docker would follow on the host when it mounts them.
func (m *Manager) checkFreshData(ctx context.Context, name string) error {
	host, bare := SplitID(name)
	base := path.Join(m.appdataFor(host), bare)
	inUse := fmt.Errorf("%s already has data in it, so a server named %s has to be created by an admin", base, name)
	list, err := m.docker.ListContainersOn(ctx, host)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	inBase := func(p string) bool { return p == base || strings.HasPrefix(path.Clean(p), base+"/") }
	for _, c := range list {
		for _, mount := range c.Mounts {
			if inBase(mount.Source) {
				return inUse
			}
		}
	}
	if host != LocalHost {
		// hlm can't look at another host's folders, so it can't tell an
		// empty folder from a planted one.
		return m.checkRemoteFresh(ctx, host, base, inUse)
	}
	if m.deleted != nil {
		gone, err := m.deleted.load()
		if err != nil {
			return fmt.Errorf("read deleted servers: %w", err)
		}
		for _, d := range gone {
			for _, f := range d.Folders {
				if !f.Volume && inBase(f.Path) {
					return inUse
				}
			}
		}
	}
	if m.host != nil && m.host.Available() {
		if _, err := os.Lstat(m.host.HostPath(base)); !errors.Is(err, os.ErrNotExist) {
			return inUse
		}
	}
	return nil
}
