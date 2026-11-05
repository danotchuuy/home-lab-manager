package hostmetrics

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// source is where a sample's files come from: this machine's filesystem,
// or a Dump taken on another host.
type source interface {
	// proc reads a file in the host's procfs, such as "stat" or "1/net/dev".
	proc(name string) ([]byte, error)
	// root reads a file on the host's root filesystem; an error when it
	// can't be told apart from hlm's own (when it runs in a container
	// without the host's root mounted).
	root(path string) ([]byte, error)
	// statfs is a filesystem's size, used and free (to unprivileged users)
	// bytes, by its mount point on the host.
	statfs(mount string) (total, used, free uint64, err error)
}

var errNoRoot = errors.New("the host's root filesystem isn't mounted")

// localSource reads this machine's files.
type localSource struct{ c *Collector }

func (l localSource) proc(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.c.cfg.ProcPath, filepath.FromSlash(name)))
}

func (l localSource) root(p string) ([]byte, error) {
	if l.c.cfg.RootPath == "" || l.c.cfg.RootPath == "/" {
		return nil, errNoRoot
	}
	return os.ReadFile(l.c.HostPath(p))
}

func (l localSource) statfs(mount string) (uint64, uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(l.c.HostPath(mount), &st); err != nil {
		return 0, 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, (st.Blocks - st.Bfree) * bs, st.Bavail * bs, nil
}

// Dump is what DumpScript printed on another host: its procfs files, its
// /etc/hostname and its filesystems' sizes.
type Dump struct {
	Files map[string][]byte    // "proc:stat", "root:/etc/hostname"
	FS    map[string][3]uint64 // by mount point: total, used, free bytes
}

func (d *Dump) proc(name string) ([]byte, error) { return d.file("proc:" + name) }

func (d *Dump) root(p string) ([]byte, error) { return d.file("root:" + p) }

func (d *Dump) file(key string) ([]byte, error) {
	b, ok := d.Files[key]
	if !ok || len(bytes.TrimSpace(b)) == 0 {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (d *Dump) statfs(mount string) (uint64, uint64, uint64, error) {
	v, ok := d.FS[mount]
	if !ok {
		return 0, 0, 0, os.ErrNotExist
	}
	return v[0], v[1], v[2], nil
}

// dumpFiles are the procfs files a sample reads.
var dumpFiles = []string{"stat", "meminfo", "loadavg", "uptime", "diskstats", "1/net/dev", "1/mounts", "sys/kernel/hostname", "sys/kernel/osrelease"}

// DumpScript prints what a sample needs, for ParseDump: run it with sh on
// the host (or in a container with the host's procfs and root mounted), with
// the procfs path, the root's path ("" for /), and the filesystem types to
// measure as arguments (DumpArgs). Busybox's sh, cat and df are enough.
const DumpScript = `P="$1"; R="$2"; T=" $3 "
for f in ` + "stat meminfo loadavg uptime diskstats 1/net/dev 1/mounts sys/kernel/hostname sys/kernel/osrelease" + `; do
  echo "==hlm:proc $f"; cat "$P/$f" 2>/dev/null; echo
done
if [ -n "$R" ]; then echo "==hlm:root /etc/hostname"; cat "$R/etc/hostname" 2>/dev/null; echo; fi
echo "==hlm:df"
while read -r dev mnt typ rest; do
  case "$T" in *" $typ "*) ;; *) continue ;; esac
  case "$mnt" in /var/lib/docker/*|/snap/*|*\\*) continue ;; esac
  df -Pk "$R$mnt" 2>/dev/null | tail -n 1 | while read -r _ total used avail _; do echo "$mnt $total $used $avail"; done
done < "$P/1/mounts"
echo "==hlm:end"`

// DumpArgs are DumpScript's arguments.
func DumpArgs(procPath, rootPath string) []string {
	types := make([]string, 0, len(realFS))
	for t := range realFS {
		types = append(types, t)
	}
	slices.Sort(types)
	return []string{procPath, rootPath, strings.Join(types, " ")}
}

// ParseDump reads DumpScript's output.
func ParseDump(out []byte) (*Dump, error) {
	d := &Dump{Files: map[string][]byte{}, FS: map[string][3]uint64{}}
	var key string
	var buf bytes.Buffer
	inDF, ended := false, false
	flush := func() {
		if key != "" {
			d.Files[key] = bytes.Clone(buf.Bytes())
		}
		key = ""
		buf.Reset()
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "==hlm:proc "):
			flush()
			key = "proc:" + strings.TrimPrefix(line, "==hlm:proc ")
		case strings.HasPrefix(line, "==hlm:root "):
			flush()
			key = "root:" + strings.TrimPrefix(line, "==hlm:root ")
		case line == "==hlm:df":
			flush()
			inDF = true
		case line == "==hlm:end":
			flush()
			inDF, ended = false, true
		case inDF:
			f := strings.Fields(line)
			if len(f) != 4 {
				continue
			}
			total, err1 := strconv.ParseUint(f[1], 10, 64)
			used, err2 := strconv.ParseUint(f[2], 10, 64)
			avail, err3 := strconv.ParseUint(f[3], 10, 64)
			if err1 == nil && err2 == nil && err3 == nil {
				d.FS[f[0]] = [3]uint64{total * 1024, used * 1024, avail * 1024}
			}
		case key != "":
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !ended {
		return nil, errors.New("the host's figures were cut short")
	}
	if _, err := d.proc("stat"); err != nil {
		return nil, errors.New("couldn't read the host's /proc/stat")
	}
	return d, nil
}

// Fetcher runs DumpScript somewhere and returns its output.
type Fetcher func(ctx context.Context, script string, args []string) ([]byte, error)

// FetchWith makes a NewRemote fetch from a Fetcher.
func FetchWith(run Fetcher, procPath, rootPath string) func(ctx context.Context) (*Dump, error) {
	args := DumpArgs(procPath, rootPath)
	return func(ctx context.Context) (*Dump, error) {
		out, err := run(ctx, DumpScript, args)
		if err != nil {
			return nil, err
		}
		return ParseDump(out)
	}
}
