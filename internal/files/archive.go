package files

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// ArchiveStats describes what Archive wrote.
type ArchiveStats struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"` // file contents, before compression
	// Excluded counts the files and folders exclude patterns left out.
	Excluded int `json:"excluded,omitempty"`
	// Warnings are files that were skipped or changed while being read.
	Warnings []string `json:"warnings,omitempty"`
}

const maxWarnings = 20

func (s *ArchiveStats) warn(format string, a ...any) {
	if len(s.Warnings) < maxWarnings {
		s.Warnings = append(s.Warnings, fmt.Sprintf(format, a...))
	} else if len(s.Warnings) == maxWarnings {
		s.Warnings = append(s.Warnings, "more files had problems; only the first are listed")
	}
}

// ErrRootFolder is returned for backing up the container's whole filesystem.
var ErrRootFolder = errors.New("choose folders inside the container, not / itself")

// NormalizeFolders cleans container paths, drops duplicates and folders
// inside another chosen folder, and sorts them.
func NormalizeFolders(folders []string) ([]string, error) {
	var clean []string
	for _, f := range folders {
		p, err := cleanPath(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("%q: %w", f, err)
		}
		if p == "/" {
			return nil, ErrRootFolder
		}
		clean = append(clean, p)
	}
	sort.Strings(clean)
	var out []string
	for _, p := range clean {
		if len(out) > 0 && within(p, out[len(out)-1]) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Archive writes the given folders (container paths), with everything in
// them, to tw. Entries are named by their container path without the
// leading slash, so extracting the archive at / puts every file back.
//
// Folders in a bind mount hlm can see are read from the host. Others are
// streamed through Docker's archive API, which locks the container for as
// long as the copy runs. Data folders mounted below a chosen folder are
// included too, each read the fastest way available. progress, if not nil,
// is called after each file.
func (b *Browser) Archive(ctx context.Context, info docker.ContainerJSON, folders []string, tw *tar.Writer, progress func(ArchiveStats)) (ArchiveStats, error) {
	return b.ArchiveExcluding(ctx, info, folders, nil, tw, progress)
}

// ArchiveExcluding is Archive, leaving out what the exclude patterns match
// (see Excluded).
func (b *Browser) ArchiveExcluding(ctx context.Context, info docker.ContainerJSON, folders, exclude []string, tw *tar.Writer, progress func(ArchiveStats)) (ArchiveStats, error) {
	var st ArchiveStats
	folders, err := NormalizeFolders(folders)
	if err != nil {
		return st, err
	}
	if len(folders) == 0 {
		return st, errors.New("no folders chosen")
	}
	// Each chosen folder, plus every mount inside one: a mount's files are
	// best read from its own source.
	var sources []string
	for _, f := range folders {
		sources = append(sources, f)
		for _, m := range info.Mounts {
			if m.Type != "bind" && m.Type != "volume" {
				continue
			}
			if dest := path.Clean(m.Destination); dest != f && within(dest, f) {
				sources = append(sources, dest)
			}
		}
	}
	sort.Strings(sources)
	if progress == nil {
		progress = func(ArchiveStats) {}
	}
	a := &archiver{tw: tw, stats: &st, progress: progress, exclude: exclude}
	for i, src := range sources {
		if i > 0 && src == sources[i-1] {
			continue
		}
		var skip []string
		for _, other := range sources {
			if other != src && within(other, src) {
				skip = append(skip, other)
			}
		}
		if err := b.archiveSource(ctx, info, src, skip, a); err != nil {
			return st, fmt.Errorf("%s: %w", src, err)
		}
	}
	return st, nil
}

type archiver struct {
	tw       *tar.Writer
	stats    *ArchiveStats
	progress func(ArchiveStats)
	exclude  []string
}

// excluded reports whether a pattern leaves a path out, and counts it.
func (a *archiver) excluded(full string, dir bool) bool {
	if len(a.exclude) == 0 || !Excluded(a.exclude, full, dir) {
		return false
	}
	a.stats.Excluded++
	return true
}

func skipped(p string, skip []string) bool {
	for _, s := range skip {
		if within(p, s) {
			return true
		}
	}
	return false
}

func (b *Browser) archiveSource(ctx context.Context, info docker.ContainerJSON, src string, skip []string, a *archiver) error {
	loc, err := b.locate(info, src)
	if err != nil {
		return err
	}
	defer loc.close()
	if loc.root != nil {
		_, err := loc.root.Stat(loc.rel)
		if err == nil {
			return archiveHost(ctx, loc, skip, a)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		// e.g. a symlink out of the data folder; Docker follows it the way
		// the container does.
	}
	return b.archiveDocker(ctx, info.ID, src, skip, a)
}

func archiveHost(ctx context.Context, loc *location, skip []string, a *archiver) error {
	return fs.WalkDir(loc.root.FS(), loc.rel, func(rel string, d fs.DirEntry, err error) error {
		full := loc.path
		if rel != loc.rel {
			sub := rel
			if loc.rel != "." {
				sub = strings.TrimPrefix(rel, loc.rel+"/")
			}
			full = path.Join(loc.path, sub)
		}
		if err != nil {
			if rel == loc.rel {
				return hostErr(err)
			}
			a.stats.warn("%s: %v", full, err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if full != loc.path && (skipped(full, skip) || a.excluded(full, d.IsDir())) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			a.stats.warn("%s: %v", full, err)
			return nil
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = loc.root.Readlink(rel); err != nil {
				a.stats.warn("%s: %v", full, err)
				return nil
			}
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() && link == "" {
			return nil // sockets, pipes and devices aren't data
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			a.stats.warn("%s: %v", full, err)
			return nil
		}
		hdr.Name = tarName(full, fi.IsDir())
		if !fi.Mode().IsRegular() {
			return a.tw.WriteHeader(hdr)
		}
		f, err := loc.root.Open(rel)
		if err != nil {
			a.stats.warn("%s: %v", full, err)
			return nil
		}
		defer f.Close()
		return a.file(hdr, f, full)
	})
}

// tarName is an entry's name for a container path.
func tarName(p string, dir bool) string {
	name := strings.TrimPrefix(p, "/")
	if dir {
		name += "/"
	}
	return name
}

// file writes a regular file's header and contents. A file that shrank
// while being read is padded with zeros, which keeps the archive readable,
// and reported; one that grew is cut at the size it had.
func (a *archiver) file(hdr *tar.Header, r io.Reader, full string) error {
	if err := a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.CopyN(a.tw, r, hdr.Size)
	if err != nil && err != io.EOF {
		return err
	}
	if n < hdr.Size {
		a.stats.warn("%s changed while it was being backed up", full)
		if _, err := io.CopyN(a.tw, zeros{}, hdr.Size-n); err != nil {
			return err
		}
	}
	a.stats.Files++
	a.stats.Bytes += hdr.Size
	a.progress(*a.stats)
	return nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// archiveDocker copies src out of the container with Docker's archive API.
func (b *Browser) archiveDocker(ctx context.Context, id, src string, skip []string, a *archiver) error {
	// Docker sends a symlink itself rather than what it points to.
	for i := 0; ; i++ {
		st, err := b.docker.StatPath(ctx, id, src)
		if err != nil {
			return dockerErr(err)
		}
		if st.Mode&os.ModeSymlink == 0 {
			if !st.Mode.IsDir() {
				return ErrNotDir
			}
			break
		}
		if i == 16 {
			return errors.New("too many levels of symbolic links")
		}
		target := st.LinkTarget
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(src), target)
		}
		src = path.Clean(target)
	}
	rc, _, err := b.docker.CopyFrom(ctx, id, src)
	if err != nil {
		return dockerErr(err)
	}
	defer rc.Close()
	parent := path.Dir(src)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		full := path.Join(parent, hdr.Name)
		if full != src && skipped(full, skip) {
			continue
		}
		if full != src && a.excluded(full, hdr.Typeflag == tar.TypeDir) {
			if hdr.Typeflag == tar.TypeDir {
				skip = append(skip, full) // and everything in it
			}
			continue
		}
		dir := hdr.Typeflag == tar.TypeDir
		hdr.Name = tarName(full, dir)
		if hdr.Typeflag == tar.TypeLink {
			// Hard links name their target the same way as entries.
			hdr.Linkname = tarName(path.Join(parent, hdr.Linkname), false)
		}
		if hdr.Typeflag != tar.TypeReg {
			if err := a.tw.WriteHeader(hdr); err != nil {
				return err
			}
			continue
		}
		if err := a.file(hdr, tr, full); err != nil {
			return err
		}
	}
}
