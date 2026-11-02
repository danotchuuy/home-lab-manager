// Package files browses, reads and edits files inside game server
// containers, running or stopped.
//
// Paths are always container paths. Paths inside a bind mount or volume are
// served from the host filesystem when hlm can see it, which is fast even for
// large game directories. Otherwise, directories are listed and searched by
// running stat/find in the container (when it is running), and files are
// read and written through Docker's archive API. Host access goes through
// os.Root, so a symlink in a data folder can't reach files outside it.
//
// Docker locks a container for as long as an archive download runs, which
// stalls every other request about that container, so whole directories are
// only streamed through the archive API as a last resort (a stopped
// container, or an image without a shell), with tight limits.
package files

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// MaxEditSize is the largest file, in bytes, that can be opened or saved in
// the editor (1 MiB). Settings files are far smaller; anything bigger is a
// log, save or game asset that a browser text box shouldn't load.
const MaxEditSize = 1 << 20

// Limits for walking directories through Docker's archive API, which
// streams every file's contents along with the listing and locks the
// container meanwhile.
const (
	maxArchiveBytes   = 32 << 20
	maxArchiveEntries = 5_000
	// Docker produced archives at only ~4 MB/s on a test host, so time is
	// the limit that matters; it bounds how long the container stays locked.
	maxArchiveTime   = 3 * time.Second
	maxSearchResults = 300
	maxSearchVisited = 200_000
	maxExecOutput    = 8 << 20
	execTimeout      = 20 * time.Second
)

var (
	ErrNotFound    = errors.New("no such file or directory")
	ErrIsDir       = errors.New("is a directory")
	ErrNotDir      = errors.New("not a directory")
	ErrReadOnly    = errors.New("this path is read-only")
	ErrConflict    = errors.New("the file changed since it was opened")
	ErrTooLarge    = fmt.Errorf("files larger than %d MiB can't be opened or saved here", MaxEditSize>>20)
	ErrBadPath     = errors.New("paths must be absolute")
	ErrNotRegular  = errors.New("only regular files can be edited")
	ErrBinaryWrite = errors.New("binary files can't be edited")
	ErrNotEditable = errors.New("this type of file can't be opened or edited here")
)

// blockedExts are file types the editor refuses outright, even though some
// may pass as text: they're program code, and editing them breaks the game.
var blockedExts = map[string]bool{".dll": true}

// Editable reports whether a file's type may be opened in the editor.
func Editable(name string) bool {
	return !blockedExts[strings.ToLower(path.Ext(name))]
}

func checkEditable(p string) error {
	if Editable(p) {
		return nil
	}
	ext := strings.ToUpper(strings.TrimPrefix(path.Ext(p), "."))
	return fmt.Errorf("%w: %s files are program code, not settings", ErrNotEditable, ext)
}

// checkHostLinks applies checkEditable to every symlink target on the way
// to a host file, so a link with an innocent name can't open a blocked file.
func checkHostLinks(loc *location) error {
	p := loc.rel
	for i := 0; i < 16; i++ {
		fi, err := loc.root.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return nil // missing files fail later with a clearer error
		}
		target, err := loc.root.Readlink(p)
		if err != nil {
			return nil
		}
		if err := checkEditable(target); err != nil {
			return err
		}
		if path.IsAbs(target) {
			return nil // resolved by Docker, which checks each step itself
		}
		p = path.Join(path.Dir(p), target)
	}
	return nil
}

// Entry is one item in a directory listing.
type Entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // dir, file, symlink, other
	Size    int64     `json:"size"`
	Mtime   time.Time `json:"mtime"`
	Mode    string    `json:"mode"` // e.g. -rw-r--r--
	Link    string    `json:"link,omitempty"`
	IsMount bool      `json:"isMount,omitempty"`
}

// Mount is a data folder mounted into the container.
type Mount struct {
	Path     string `json:"path"`   // in the container
	Source   string `json:"source"` // host path or volume name
	Type     string `json:"type"`   // bind or volume
	ReadOnly bool   `json:"readOnly"`
}

// Listing is a directory's contents.
type Listing struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Source    string  `json:"source"` // host or docker
	ReadOnly  bool    `json:"readOnly"`
	Truncated bool    `json:"truncated"`
	Mount     *Mount  `json:"mount,omitempty"`
}

// File is a file opened for editing.
type File struct {
	Path     string    `json:"path"`
	Content  string    `json:"content"`
	Size     int64     `json:"size"`
	Mtime    time.Time `json:"mtime"`
	Mode     string    `json:"mode"`
	Binary   bool      `json:"binary"`
	ReadOnly bool      `json:"readOnly"`
	Source   string    `json:"source"`
}

// Match is a search result.
type Match struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

// Browser reads and writes container files.
type Browser struct {
	docker *docker.Client
	// hostPath maps a host path to where hlm can read it, or is nil when the
	// Docker host's filesystem isn't available (e.g. a remote Docker host).
	hostPath func(string) string
}

// New creates a browser. hostPath may be nil.
func New(d *docker.Client, hostPath func(string) string) *Browser {
	return &Browser{docker: d, hostPath: hostPath}
}

// Mounts returns the container's data folders, sorted by path.
func Mounts(info docker.ContainerJSON) []Mount {
	var out []Mount
	for _, m := range info.Mounts {
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		src := m.Source
		if m.Type == "volume" && m.Name != "" {
			src = m.Name
		}
		out = append(out, Mount{Path: m.Destination, Source: src, Type: m.Type, ReadOnly: !m.RW})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func cleanPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	return path.Clean(p), nil
}

// location says where a container path lives.
type location struct {
	path  string
	mount *docker.MountPoint // the innermost mount containing path, if any
	rel   string             // path relative to the mount, "." for its root
	root  *os.Root           // set when the path can be read on the host
}

func (l *location) close() {
	if l.root != nil {
		l.root.Close()
	}
}

// dropHost switches to Docker for this path, e.g. when it is a symlink the
// host view can't follow.
func (l *location) dropHost() {
	l.close()
	l.root = nil
}

func (l *location) readOnly() bool {
	return l.mount != nil && !l.mount.RW
}

func (b *Browser) locate(info docker.ContainerJSON, p string) (*location, error) {
	p, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	loc := &location{path: p}
	for i := range info.Mounts {
		m := &info.Mounts[i]
		if m.Type != "bind" && m.Type != "volume" {
			continue
		}
		dest := path.Clean(m.Destination)
		if (p == dest || strings.HasPrefix(p, dest+"/")) && (loc.mount == nil || len(dest) > len(loc.mount.Destination)) {
			loc.mount = m
		}
	}
	if loc.mount == nil {
		return loc, nil
	}
	loc.rel = strings.TrimPrefix(strings.TrimPrefix(p, path.Clean(loc.mount.Destination)), "/")
	if loc.rel == "" {
		loc.rel = "."
	}
	if b.hostPath != nil {
		// Fall back to Docker if the folder isn't visible from here.
		if root, err := os.OpenRoot(b.hostPath(loc.mount.Source)); err == nil {
			loc.root = root
		}
	}
	return loc, nil
}

func mountInfo(m *docker.MountPoint) *Mount {
	if m == nil {
		return nil
	}
	src := m.Source
	if m.Type == "volume" && m.Name != "" {
		src = m.Name
	}
	return &Mount{Path: m.Destination, Source: src, Type: m.Type, ReadOnly: !m.RW}
}

func entryType(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// List returns the contents of a directory.
func (b *Browser) List(ctx context.Context, info docker.ContainerJSON, p string) (Listing, error) {
	loc, err := b.locate(info, p)
	if err != nil {
		return Listing{}, err
	}
	defer loc.close()
	l := Listing{Path: loc.path, ReadOnly: loc.readOnly(), Mount: mountInfo(loc.mount), Entries: []Entry{}}
	if loc.root != nil {
		l.Source = "host"
		err = listHost(loc, &l)
		if errors.Is(err, os.ErrPermission) || isEscape(err) {
			// e.g. the directory is a symlink pointing outside the folder;
			// Docker resolves it the way the container does.
			loc.dropHost()
			l.Entries = []Entry{}
			err = b.listDocker(ctx, info, loc, &l)
		}
	} else {
		err = b.listDocker(ctx, info, loc, &l)
	}
	if err != nil {
		return Listing{}, err
	}

	// Mount points below this directory show up even when the listing comes
	// from somewhere they're not visible (e.g. the image's filesystem).
	seen := map[string]bool{}
	for i, e := range l.Entries {
		seen[e.Name] = true
		if mountAt(info, path.Join(loc.path, e.Name)) {
			l.Entries[i].IsMount = true
		}
	}
	for _, m := range info.Mounts {
		dest := path.Clean(m.Destination)
		if path.Dir(dest) == loc.path && dest != loc.path && !seen[path.Base(dest)] {
			l.Entries = append(l.Entries, Entry{Name: path.Base(dest), Type: "dir", Mode: "drwxr-xr-x", IsMount: true})
		}
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return l, nil
}

func mountAt(info docker.ContainerJSON, p string) bool {
	for _, m := range info.Mounts {
		if (m.Type == "bind" || m.Type == "volume") && path.Clean(m.Destination) == p {
			return true
		}
	}
	return false
}

func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

func listHost(loc *location, l *Listing) error {
	st, err := loc.root.Stat(loc.rel)
	if err != nil {
		return hostErr(err)
	}
	if !st.IsDir() {
		return ErrNotDir
	}
	f, err := loc.root.Open(loc.rel)
	if err != nil {
		return hostErr(err)
	}
	defer f.Close()
	dirents, err := f.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, d := range dirents {
		fi, err := d.Info()
		if err != nil {
			continue
		}
		e := Entry{Name: d.Name(), Type: entryType(fi.Mode()), Size: fi.Size(), Mtime: fi.ModTime(), Mode: fi.Mode().String()}
		if e.Type == "symlink" {
			e.Link, _ = loc.root.Readlink(path.Join(loc.rel, d.Name()))
		}
		if e.Type == "dir" {
			e.Size = 0
		}
		l.Entries = append(l.Entries, e)
	}
	return nil
}

func hostErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func dockerErr(err error) error {
	if docker.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

// listDocker lists a directory through Docker: with stat in the container
// when it is running, otherwise through the archive API.
func (b *Browser) listDocker(ctx context.Context, info docker.ContainerJSON, loc *location, l *Listing) error {
	l.Source = "docker"
	if info.State.Running {
		err := b.listExec(ctx, info.ID, loc, l)
		if !errors.Is(err, errNoShell) {
			return err
		}
		l.Entries = []Entry{}
	}
	return b.listArchive(ctx, info.ID, loc, l)
}

// errNoShell means the container has no sh/stat/find to run.
var errNoShell = errors.New("the container has no shell")

// listScript prints one line per entry of the directory $1: a symlink's
// target as "L<tab>name<tab>target", then "mode<tab>size<tab>mtime<tab>name"
// from stat, where mode is the raw st_mode in hex. It works with GNU
// coreutils and busybox.
const listScript = `[ -e "$1" ] || { echo hlm:notfound; exit 3; }
[ -d "$1" ] || { echo hlm:notdir; exit 3; }
cd -- "$1" || exit 3
set -- * .[!.]* ..?*
for f do [ -L "$f" ] && printf 'L\t%s\t%s\n' "$f" "$(readlink -- "$f")"; done
stat -c '%f` + "\t" + `%s` + "\t" + `%Y` + "\t" + `%n' -- "$@" 2>/dev/null
exit 0`

func (b *Browser) exec(ctx context.Context, id string, cmd []string) (docker.ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	res, err := b.docker.Exec(ctx, id, "0", cmd, maxExecOutput)
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") || strings.Contains(err.Error(), "no such file or directory") {
			return res, errNoShell
		}
		return res, dockerErr(err)
	}
	if res.ExitCode == 126 || res.ExitCode == 127 {
		return res, errNoShell
	}
	return res, nil
}

func (b *Browser) listExec(ctx context.Context, id string, loc *location, l *Listing) error {
	res, err := b.exec(ctx, id, []string{"sh", "-c", listScript, "sh", loc.path})
	if err != nil {
		return err
	}
	out := string(res.Stdout)
	switch {
	case strings.HasPrefix(out, "hlm:notfound"):
		return ErrNotFound
	case strings.HasPrefix(out, "hlm:notdir"):
		return ErrNotDir
	}
	l.Truncated = res.Truncated
	links := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, target, ok := strings.Cut(strings.TrimPrefix(line, "L\t"), "\t"); ok && strings.HasPrefix(line, "L\t") {
			links[name] = target
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		raw, err1 := strconv.ParseUint(f[0], 16, 32)
		size, err2 := strconv.ParseInt(f[1], 10, 64)
		mtime, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		mode := unixMode(uint32(raw))
		e := Entry{Name: f[3], Type: entryType(mode), Size: size, Mtime: time.Unix(mtime, 0), Mode: mode.String()}
		if e.Type == "dir" {
			e.Size = 0
		}
		l.Entries = append(l.Entries, e)
	}
	for i := range l.Entries {
		l.Entries[i].Link = links[l.Entries[i].Name]
	}
	return nil
}

// unixMode converts a raw st_mode to an fs.FileMode.
func unixMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o100000:
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	default:
		mode |= fs.ModeIrregular
	}
	return mode
}

// listArchive lists a directory through Docker's archive API. Docker sends
// the whole tree, contents included, and locks the container meanwhile, so
// the transfer is cut off early.
func (b *Browser) listArchive(ctx context.Context, id string, loc *location, l *Listing) error {
	st, err := b.docker.StatPath(ctx, id, loc.path)
	if err != nil {
		return dockerErr(err)
	}
	if !st.Mode.IsDir() {
		return ErrNotDir
	}
	ctx, cancel := context.WithTimeout(ctx, maxArchiveTime)
	defer cancel() // stops the transfer, and unlocks the container, when we stop reading
	rc, _, err := b.docker.CopyFrom(ctx, id, loc.path)
	if err != nil {
		return dockerErr(err)
	}
	defer rc.Close()

	prefix := ""
	if loc.path != "/" {
		prefix = path.Base(loc.path) + "/"
	}
	counted := &countingReader{r: rc}
	tr := tar.NewReader(counted)
	for n := 0; ; n++ {
		if n > maxArchiveEntries || counted.n > maxArchiveBytes {
			l.Truncated = true
			return nil
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				l.Truncated = true // ran out of time
				return nil
			}
			return err
		}
		name := strings.Trim(strings.TrimPrefix(hdr.Name, "./"), "/")
		rel, ok := strings.CutPrefix(name+"/", prefix)
		rel = strings.TrimSuffix(rel, "/")
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue // the directory itself, or something deeper
		}
		fi := hdr.FileInfo()
		e := Entry{Name: rel, Type: entryType(fi.Mode()), Size: hdr.Size, Mtime: hdr.ModTime, Mode: fi.Mode().String(), Link: hdr.Linkname}
		if e.Type == "dir" {
			e.Size = 0
		}
		l.Entries = append(l.Entries, e)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Read opens a file for editing.
func (b *Browser) Read(ctx context.Context, info docker.ContainerJSON, p string) (File, error) {
	loc, err := b.locate(info, p)
	if err != nil {
		return File{}, err
	}
	defer loc.close()
	if err := checkEditable(loc.path); err != nil {
		return File{}, err
	}
	var (
		data []byte
		f    File
	)
	if loc.root != nil {
		data, f, err = readHost(loc)
		if errors.Is(err, os.ErrPermission) || isEscape(err) {
			loc.dropHost()
		}
	}
	if loc.root == nil {
		data, f, err = b.readDocker(ctx, info.ID, loc)
	}
	if err != nil {
		return File{}, err
	}
	f.Path, f.ReadOnly = loc.path, loc.readOnly()
	if isBinary(data) {
		f.Binary = true
	} else {
		f.Content = string(data)
	}
	return f, nil
}

func isBinary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(data)
}

func readHost(loc *location) ([]byte, File, error) {
	if err := checkHostLinks(loc); err != nil {
		return nil, File{}, err
	}
	fh, err := loc.root.Open(loc.rel)
	if err != nil {
		return nil, File{}, hostErr(err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, File{}, err
	}
	if st.IsDir() {
		return nil, File{}, ErrIsDir
	}
	if !st.Mode().IsRegular() {
		return nil, File{}, ErrNotRegular
	}
	if st.Size() > MaxEditSize {
		return nil, File{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxEditSize+1))
	if err != nil {
		return nil, File{}, err
	}
	return data, File{Size: st.Size(), Mtime: st.ModTime(), Mode: st.Mode().String(), Source: "host"}, nil
}

func (b *Browser) readDocker(ctx context.Context, id string, loc *location) ([]byte, File, error) {
	hdr, data, _, err := b.fetchDocker(ctx, id, loc.path, true, 0)
	if err != nil {
		return nil, File{}, err
	}
	return data, File{Size: hdr.Size, Mtime: hdr.ModTime, Mode: hdr.FileInfo().Mode().String(), Source: "docker"}, nil
}

// fetchDocker reads a single regular file's tar header (and, if withData,
// its contents) through Docker, following symlinks. It also returns the
// path of the file itself, after following links.
func (b *Browser) fetchDocker(ctx context.Context, id, p string, withData bool, depth int) (*tar.Header, []byte, string, error) {
	if depth > 16 {
		return nil, nil, "", errors.New("too many levels of symbolic links")
	}
	if err := checkEditable(p); err != nil {
		return nil, nil, "", err
	}
	st, err := b.docker.StatPath(ctx, id, p)
	if err != nil {
		return nil, nil, "", dockerErr(err)
	}
	if st.Mode.IsDir() {
		return nil, nil, "", ErrIsDir
	}
	if !st.Mode.IsRegular() && st.LinkTarget == "" {
		return nil, nil, "", ErrNotRegular
	}
	if st.Size > MaxEditSize {
		return nil, nil, "", ErrTooLarge
	}
	rc, _, err := b.docker.CopyFrom(ctx, id, p)
	if err != nil {
		return nil, nil, "", dockerErr(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		return nil, nil, "", err
	}
	if hdr.Typeflag != tar.TypeReg {
		// A symlink: Docker returns the link itself, so follow it.
		if hdr.Typeflag == tar.TypeSymlink {
			target := hdr.Linkname
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(p), target)
			}
			return b.fetchDocker(ctx, id, target, withData, depth+1)
		}
		return nil, nil, "", ErrNotRegular
	}
	if !withData {
		return hdr, nil, p, nil
	}
	data, err := io.ReadAll(io.LimitReader(tr, MaxEditSize+1))
	if err != nil {
		return nil, nil, "", err
	}
	return hdr, data, p, nil
}

// Write saves content to an existing file, keeping its owner and mode. If
// the file's modification time no longer matches expected (it changed since
// it was opened), it fails with ErrConflict unless expected is zero.
func (b *Browser) Write(ctx context.Context, info docker.ContainerJSON, p, content string, expected time.Time) (File, error) {
	loc, err := b.locate(info, p)
	if err != nil {
		return File{}, err
	}
	defer loc.close()
	if loc.readOnly() {
		return File{}, ErrReadOnly
	}
	if err := checkEditable(loc.path); err != nil {
		return File{}, err
	}
	data := []byte(content)
	if len(data) > MaxEditSize {
		return File{}, ErrTooLarge
	}
	if loc.root != nil {
		err = writeHost(loc, data, expected)
		if hostCantWrite(err) {
			loc.dropHost()
		} else if err != nil {
			return File{}, err
		}
	}
	if loc.root == nil {
		if err := b.writeDocker(ctx, info.ID, loc, data, expected); err != nil {
			return File{}, err
		}
	}
	return b.Read(ctx, info, loc.path)
}

// hostCantWrite reports whether writing through the host's filesystem
// failed in a way Docker can get around: no permission, a path leading out
// of the data folder, or a host root mounted read-only into hlm's container
// (as docker-compose.yml does, for host metrics). Docker writes through the
// server's own, writable bind mount instead.
func hostCantWrite(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) || isEscape(err)
}

func sameTime(a, b time.Time) bool {
	// Docker reports whole seconds for some files and nanoseconds for others.
	return a.Equal(b) || a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

func writeHost(loc *location, data []byte, expected time.Time) error {
	if err := checkHostLinks(loc); err != nil {
		return err
	}
	st, err := loc.root.Stat(loc.rel)
	if err != nil {
		return hostErr(err)
	}
	if !st.Mode().IsRegular() {
		return ErrNotRegular
	}
	if !expected.IsZero() && !sameTime(st.ModTime(), expected) {
		return ErrConflict
	}
	// Truncate and rewrite in place: the file keeps its owner, mode and any
	// hard links, which matters for servers running as non-root users.
	fh, err := loc.root.OpenFile(loc.rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

func (b *Browser) writeDocker(ctx context.Context, id string, loc *location, data []byte, expected time.Time) error {
	// Write to the file a symlink points at, not over the link.
	hdr, _, target, err := b.fetchDocker(ctx, id, loc.path, false, 0)
	if err != nil {
		return err
	}
	if !expected.IsZero() && !sameTime(hdr.ModTime, expected) {
		return ErrConflict
	}
	// Re-send the file with its original owner and mode.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     path.Base(target),
		Mode:     hdr.Mode,
		Uid:      hdr.Uid,
		Gid:      hdr.Gid,
		Uname:    hdr.Uname,
		Gname:    hdr.Gname,
		ModTime:  time.Now(),
		Size:     int64(len(data)),
	})
	if err == nil {
		_, err = tw.Write(data)
	}
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return err
	}
	return dockerErr(b.docker.CopyTo(ctx, id, path.Dir(target), &buf))
}

// configExts are file extensions game servers commonly use for settings.
var configExts = map[string]bool{
	".json": true, ".ini": true, ".cfg": true, ".conf": true, ".config": true, ".properties": true,
	".yml": true, ".yaml": true, ".toml": true, ".xml": true, ".txt": true, ".env": true, ".lua": true,
	".json5": true, ".jsonc": true, ".cnf": true, ".vdf": true, ".acf": true,
}

// IsConfigName reports whether a file name looks like a settings file.
func IsConfigName(name string) bool {
	return configExts[strings.ToLower(path.Ext(name))]
}

// skipDirs are directories too big or too unlikely to hold settings to be
// worth searching.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true, "steamapps": true,
	"Engine": true, "Content": true, "Binaries": true, "libraries": true,
}

// Search finds files below dir whose name contains query (case-insensitive),
// or, with an empty query, files that look like configuration files.
func (b *Browser) Search(ctx context.Context, info docker.ContainerJSON, dir, query string) ([]Match, bool, error) {
	loc, err := b.locate(info, dir)
	if err != nil {
		return nil, false, err
	}
	defer loc.close()
	q := strings.ToLower(strings.TrimSpace(query))
	matches := func(name string) bool {
		if q == "" {
			return IsConfigName(name)
		}
		return strings.Contains(strings.ToLower(name), q)
	}
	if loc.root != nil {
		return searchHost(ctx, loc, matches)
	}
	if info.State.Running {
		out, truncated, err := b.searchExec(ctx, info.ID, loc, q)
		if !errors.Is(err, errNoShell) {
			return out, truncated, err
		}
	}
	return b.searchArchive(ctx, info.ID, loc, matches)
}

// searchExec runs find in the container.
func (b *Browser) searchExec(ctx context.Context, id string, loc *location, q string) ([]Match, bool, error) {
	cmd := []string{"find", loc.path, "("}
	first := true
	for d := range skipDirs {
		if !first {
			cmd = append(cmd, "-o")
		}
		cmd = append(cmd, "-name", d)
		first = false
	}
	cmd = append(cmd, ")", "-prune", "-o", "-type", "f", "(")
	if q != "" {
		cmd = append(cmd, "-iname", "*"+q+"*")
	} else {
		first = true
		for ext := range configExts {
			if !first {
				cmd = append(cmd, "-o")
			}
			cmd = append(cmd, "-iname", "*"+ext)
			first = false
		}
	}
	cmd = append(cmd, ")", "-exec", "stat", "-c", "%s\t%Y\t%n", "{}", "+")
	res, err := b.exec(ctx, id, cmd)
	if err != nil {
		return nil, false, err
	}
	out := []Match{}
	truncated := res.Truncated
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		if len(out) >= maxSearchResults {
			truncated = true
			break
		}
		size, _ := strconv.ParseInt(f[0], 10, 64)
		mtime, _ := strconv.ParseInt(f[1], 10, 64)
		out = append(out, Match{Path: f[2], Size: size, Mtime: time.Unix(mtime, 0)})
	}
	if len(out) == 0 && strings.Contains(string(res.Stderr), "No such file") {
		return nil, false, ErrNotFound
	}
	return out, truncated, nil
}

func searchHost(ctx context.Context, loc *location, matches func(string) bool) ([]Match, bool, error) {
	out := []Match{}
	visited := 0
	truncated := false
	err := fs.WalkDir(loc.root.FS(), loc.rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == loc.rel {
				return hostErr(err)
			}
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > maxSearchVisited || len(out) >= maxSearchResults {
			truncated = true
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != loc.rel && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !matches(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel := p
		if loc.rel != "." {
			rel = strings.TrimPrefix(p, loc.rel+"/")
		}
		out = append(out, Match{Path: path.Join(loc.path, rel), Size: fi.Size(), Mtime: fi.ModTime()})
		return nil
	})
	return out, truncated, err
}

func (b *Browser) searchArchive(ctx context.Context, id string, loc *location, matches func(string) bool) ([]Match, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, maxArchiveTime)
	defer cancel()
	rc, st, err := b.docker.CopyFrom(ctx, id, loc.path)
	if err != nil {
		return nil, false, dockerErr(err)
	}
	defer rc.Close()
	if !st.Mode.IsDir() {
		return nil, false, ErrNotDir
	}
	parent := path.Dir(loc.path)
	counted := &countingReader{r: rc}
	tr := tar.NewReader(counted)
	out := []Match{}
	for n := 0; ; n++ {
		if counted.n > maxArchiveBytes || n > maxSearchVisited || len(out) >= maxSearchResults {
			return out, true, nil
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, false, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return out, true, nil // ran out of time
			}
			return nil, false, err
		}
		if hdr.Typeflag != tar.TypeReg || !matches(path.Base(hdr.Name)) {
			continue
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		full := path.Join(parent, name)
		if loc.path == "/" {
			full = path.Join("/", name)
		}
		out = append(out, Match{Path: full, Size: hdr.Size, Mtime: hdr.ModTime})
	}
}
