package manager

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
)

// browser returns a file browser for the host a container is on, and the
// container as that host knows it. It reads data folders from the host
// when hlm can see the Docker host's filesystem (the local one only).
func (m *Manager) browser(info docker.ContainerJSON) (*files.Browser, docker.ContainerJSON, error) {
	host, bare := SplitID(info.ID)
	c, err := m.docker.client(host)
	if err != nil {
		return nil, info, err
	}
	var hostPath func(string) string
	if host == LocalHost && m.host != nil && m.host.Available() {
		hostPath = m.host.HostPath
	}
	info.ID = bare
	_, info.Name = SplitID(info.Name)
	return files.New(c, hostPath), info, nil
}

// FileListing is a directory listing plus the server's data folders.
type FileListing struct {
	files.Listing
	Mounts []files.Mount `json:"mounts"`
}

// ListFiles lists a directory in a server's container. An empty path lists
// the first data folder (or / if there is none).
func (m *Manager) ListFiles(ctx context.Context, id, path string) (FileListing, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return FileListing{}, err
	}
	if path == "" {
		// Start in the first data folder: that's where settings live, and the
		// container's root can be slow to list.
		path = "/"
		if mounts := files.Mounts(info); len(mounts) > 0 {
			path = mounts[0].Path
		}
	}
	b, local, err := m.browser(info)
	if err != nil {
		return FileListing{}, err
	}
	l, err := b.List(ctx, local, path)
	if err != nil {
		return FileListing{}, err
	}
	mounts := files.Mounts(info)
	if mounts == nil {
		mounts = []files.Mount{}
	}
	return FileListing{Listing: l, Mounts: mounts}, nil
}

// ReadFile opens a file in a server's container for editing.
func (m *Manager) ReadFile(ctx context.Context, id, path string) (files.File, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return files.File{}, err
	}
	b, local, err := m.browser(info)
	if err != nil {
		return files.File{}, err
	}
	return b.Read(ctx, local, path)
}

// WriteFile saves a file in a server's container. expected is the
// modification time the file had when it was opened; see files.Browser.Write.
func (m *Manager) WriteFile(ctx context.Context, id, path, content string, expected time.Time) (files.File, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return files.File{}, err
	}
	m.log.Info("editing file", "server", info.Name, "path", path)
	b, local, err := m.browser(info)
	if err != nil {
		return files.File{}, err
	}
	return b.Write(ctx, local, path, content, expected)
}

// SearchFiles finds files under dir in a server's container.
func (m *Manager) SearchFiles(ctx context.Context, id, dir, query string) ([]files.Match, bool, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, local, err := m.browser(info)
	if err != nil {
		return nil, false, err
	}
	return b.Search(ctx, local, dir, query)
}

// ArchiveFolders writes folders in a server's container, with everything in
// them, to tw; see files.Browser.Archive.
func (m *Manager) ArchiveFolders(ctx context.Context, id string, folders, exclude []string, tw *tar.Writer, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	b, local, err := m.browser(info)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	return b.ArchiveExcluding(ctx, local, folders, exclude, tw, progress)
}

// RestoreArchive extracts a tar stream made by ArchiveFolders back into a
// server's container; see files.Browser.Restore.
func (m *Manager) RestoreArchive(ctx context.Context, id string, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	m.log.Info("restoring files", "server", info.Name)
	b, local, err := m.browser(info)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	return b.Restore(ctx, local, r, progress)
}

// CopyData copies folders (container paths, with everything in them) from
// one server's container to the same paths in another's, as a backup and a
// restore would, without storing anything in between.
func (m *Manager) CopyData(ctx context.Context, fromID, toID string, folders []string, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	from, err := m.inspect(ctx, fromID)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	to, err := m.inspect(ctx, toID)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	m.log.Info("copying data", "from", from.Name, "to", to.Name, "folders", folders)
	fromB, from, err := m.browser(from)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	toB, to, err := m.browser(to)
	if err != nil {
		return files.ArchiveStats{}, err
	}
	pr, pw := io.Pipe()
	archived := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		_, err := fromB.Archive(ctx, from, folders, tw, nil)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
		archived <- err
	}()
	st, err := toB.Restore(ctx, to, pr, progress)
	pr.CloseWithError(err) // stop the reader if the restore gave up
	if aerr := <-archived; aerr != nil && err == nil {
		err = fmt.Errorf("read %s: %w", from.Name, aerr)
	}
	return st, err
}
