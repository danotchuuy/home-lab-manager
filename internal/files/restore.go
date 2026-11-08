package files

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Restore extracts a tar stream made by Archive into the container: each
// entry goes back to its container path, replacing the file there. Files
// that aren't in the archive are left alone.
//
// Everything goes through Docker's archive API, which works on stopped
// containers, writes into their mounts and sets each file's owner and mode
// from the archive. (The host's view of data folders may be read-only, and
// hlm may not be allowed to set owners there.) Entries in read-only mounts
// are skipped. progress, if not nil, is called after each file.
func (b *Browser) Restore(ctx context.Context, info docker.ContainerJSON, r io.Reader, progress func(ArchiveStats)) (ArchiveStats, error) {
	var st ArchiveStats
	if progress == nil {
		progress = func(ArchiveStats) {}
	}
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := b.docker.CopyTo(ctx, info.ID, "/", pr)
		// Stop the writer below if Docker gave up reading.
		pr.CloseWithError(errDockerStopped)
		copied <- dockerErr(err)
	}()

	err := rewrite(info, tar.NewReader(r), tar.NewWriter(pw), &st, progress)
	pw.CloseWithError(err) // nil ends the stream normally
	cerr := <-copied
	// A bad archive also fails the copy, which got a broken stream; a failed
	// copy fails the writer only because nothing reads it anymore.
	if err != nil && !errors.Is(err, errDockerStopped) {
		return st, err
	}
	return st, cerr
}

var errDockerStopped = errors.New("docker stopped reading the archive")

// rewrite copies the entries that may be restored from tr to tw.
func rewrite(info docker.ContainerJSON, tr *tar.Reader, tw *tar.Writer, st *ArchiveStats, progress func(ArchiveStats)) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		full := path.Clean("/" + name)
		// Archive never writes these; a hand-made archive could.
		if full == "/" || name == "" || strings.HasPrefix(name, "/") || strings.Contains("/"+name+"/", "/../") {
			st.warn("%s: skipped, not a path inside a folder", hdr.Name)
			continue
		}
		if m := mountFor(info, full); m != nil && !m.RW {
			st.warn("%s: skipped, it's in a read-only mount", full)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		default:
			continue // devices, pipes and the like
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Bytes are counted as they're written, so a big file's progress
		// shows while it's copied, not only once it's done.
		if _, err := io.Copy(&countingWriter{w: tw, add: func(n int) {
			st.Bytes += int64(n)
			progress(*st)
		}}, tr); err != nil {
			return err
		}
		st.Files++
		progress(*st)
	}
}

// countingWriter passes writes on and says how much each wrote.
type countingWriter struct {
	w   io.Writer
	add func(n int)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.add(n)
	}
	return n, err
}

// mountFor returns the innermost mount containing p, if any.
func mountFor(info docker.ContainerJSON, p string) *docker.MountPoint {
	var best *docker.MountPoint
	for i := range info.Mounts {
		m := &info.Mounts[i]
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		dest := path.Clean(m.Destination)
		if within(p, dest) && (best == nil || len(dest) > len(path.Clean(best.Destination))) {
			best = m
		}
	}
	return best
}
