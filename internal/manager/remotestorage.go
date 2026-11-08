package manager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Data folders on another host are measured by a helper container there,
// with the host's root mounted read-only: one container measures them all
// with du, and says which aren't folders or aren't there.

// measureScript prints "dir KiB path", "file 0 path" or "missing 0 path"
// for each folder given (paths on the host, under $R).
const measureScript = `R=/hlm-root
for p in "$@"; do
  if [ -d "$R$p" ]; then
    s=$(du -sk "$R$p" 2>/dev/null | tail -n 1 | cut -f1)
    echo "dir ${s:--1} $p"
  elif [ -e "$R$p" ]; then echo "file 0 $p"
  else echo "missing 0 $p"
  fi
done
echo "hlm:done"`

// remoteMeasurer holds a host's measured folders.
type remoteMeasurer struct {
	sizes map[string]DataFolder
	err   error
}

func (r remoteMeasurer) bind(_ context.Context, p string) DataFolder {
	if f, ok := r.sizes[path.Clean(p)]; ok {
		return f
	}
	f := DataFolder{Path: p, Size: -1}
	if r.err != nil {
		f.Error = r.err.Error()
	}
	return f
}

// measureRemote measures folders on another host.
func (m *Manager) measureRemote(ctx context.Context, host string, paths []string) remoteMeasurer {
	rm := remoteMeasurer{sizes: map[string]DataFolder{}}
	var clean []string
	for _, p := range paths {
		p = path.Clean(p)
		if path.IsAbs(p) && !skipDataPath[p] && !slices.Contains(clean, p) {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return rm
	}
	slices.Sort(clean)
	cmd := append([]string{"sh", "-c", measureScript, "sh"}, clean...)
	code, out, err := m.runHelperOutput(ctx, host, "/:/hlm-root:ro", "all", cmd...)
	switch {
	case err != nil:
		rm.err = err
		return rm
	case code != 0 || !strings.Contains(out, "hlm:done"):
		rm.err = fmt.Errorf("the measuring helper failed (exit status %d): %s", code, lastLines(out))
		return rm
	}
	parsed, err := parseMeasure(out)
	if err != nil {
		rm.err = err
		return rm
	}
	rm.sizes = parsed
	return rm
}

// parseMeasure reads measureScript's output.
func parseMeasure(out string) (map[string]DataFolder, error) {
	sizes := map[string]DataFolder{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		kind, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, p, ok := strings.Cut(rest, " ")
		if !ok || !path.IsAbs(p) {
			continue
		}
		kib, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			continue
		}
		switch kind {
		case "dir":
			f := DataFolder{Path: p, Size: -1}
			if kib >= 0 {
				f.Size = kib * 1024
			} else {
				f.Error = "couldn't be measured"
			}
			sizes[p] = f
		case "file":
			sizes[p] = DataFolder{} // not a data folder
		case "missing":
			sizes[p] = DataFolder{Path: p, Size: 0, Missing: true}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sizes) == 0 {
		return nil, errors.New("the measuring helper said nothing")
	}
	return sizes, nil
}

func lastLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, " / ")
}

// apparentScript prints "bytes path" for each folder, its files' sizes as
// a copy reads them, or "-1 path" if it can't.
const apparentScript = `R=/hlm-root
for p in "$@"; do
  s=$(du -sb "$R$p" 2>/dev/null | tail -n 1 | cut -f1)
  echo "${s:--1} $p"
done
echo "hlm:done"`

// DataSize measures a server's data folders (by their paths in the
// container), as copying them would read them: folders on the host and
// Docker volumes alike, by where they are on its host. Folders inside
// another of them count once. It's for showing a copy's progress, so it
// fails rather than guess when a folder can't be measured.
func (m *Manager) DataSize(ctx context.Context, id string, folders []string) (int64, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return 0, err
	}
	host, _ := SplitID(info.ID)
	var sources []string
	for _, mnt := range info.Mounts {
		if (mnt.Type == "bind" || mnt.Type == "volume") && slices.Contains(folders, path.Clean(mnt.Destination)) && mnt.Source != "" {
			sources = append(sources, path.Clean(mnt.Source))
		}
	}
	// A folder inside another is part of that one's size.
	slices.Sort(sources)
	var outer []string
	for _, s := range sources {
		if len(outer) > 0 && (s == outer[len(outer)-1] || strings.HasPrefix(s, outer[len(outer)-1]+"/")) {
			continue
		}
		outer = append(outer, s)
	}
	if len(outer) == 0 {
		return 0, nil
	}
	// Apparent sizes (du -b), as the copy counts what it reads, rather
	// than disk space, which can be far more for many small files.
	cmd := append([]string{"sh", "-c", apparentScript, "sh"}, outer...)
	code, out, err := m.runHelperOutput(ctx, host, "/:/hlm-root:ro", "all", cmd...)
	if err != nil {
		return 0, err
	}
	if code != 0 || !strings.Contains(out, "hlm:done") {
		return 0, fmt.Errorf("the measuring helper failed (exit status %d): %s", code, lastLines(out))
	}
	var total int64
	for _, line := range strings.Split(out, "\n") {
		size, p, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || line == "hlm:done" {
			continue
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("couldn't measure %s", p)
		}
		total += n
	}
	return total, nil
}
