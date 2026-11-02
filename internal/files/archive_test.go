package files

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// readTar returns an archive's entries and the contents of its files.
func readTar(t *testing.T, data []byte) ([]string, map[string]string) {
	t.Helper()
	var names []string
	contents := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			contents[hdr.Name] = string(b)
		}
	}
	sort.Strings(names)
	return names, contents
}

func TestNormalizeFolders(t *testing.T) {
	got, err := NormalizeFolders([]string{"/data/saves/", "/data", "/config", "/data/../config/x", "/datafiles"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/config", "/data", "/datafiles"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := NormalizeFolders([]string{"/"}); !errors.Is(err, ErrRootFolder) {
		t.Errorf("/: err = %v", err)
	}
	if _, err := NormalizeFolders([]string{"data"}); !errors.Is(err, ErrBadPath) {
		t.Errorf("relative: err = %v", err)
	}
}

func TestArchiveHost(t *testing.T) {
	dir, saves := t.TempDir(), t.TempDir()
	write := func(root, name, content string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dir, "server.cfg", "port=1")
	write(dir, "logs/latest.log", "hello")
	// Hidden by the mount at /data/saves; must not show up.
	write(dir, "saves/stale.sav", "old")
	write(saves, "world.sav", "world")
	os.Symlink("server.cfg", filepath.Join(dir, "link.cfg"))

	var info docker.ContainerJSON
	info.Mounts = []docker.MountPoint{
		{Type: "bind", Source: dir, Destination: "/data", RW: true},
		{Type: "bind", Source: saves, Destination: "/data/saves", RW: true},
	}
	b := New(nil, func(p string) string { return p })

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	calls := 0
	st, err := b.Archive(context.Background(), info, []string{"/data", "/data/logs"}, tw, func(ArchiveStats) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	names, contents := readTar(t, buf.Bytes())
	want := []string{"data/", "data/link.cfg", "data/logs/", "data/logs/latest.log", "data/saves/", "data/saves/world.sav", "data/server.cfg"}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %v, want %v", names, want)
	}
	if contents["data/saves/world.sav"] != "world" || contents["data/server.cfg"] != "port=1" {
		t.Errorf("contents = %v", contents)
	}
	if st.Files != 3 || st.Bytes != 16 || calls != 3 {
		t.Errorf("stats = %+v, progress calls = %d", st, calls)
	}

	if _, err := b.Archive(context.Background(), info, []string{"/data/missing"}, tar.NewWriter(io.Discard), nil); err == nil {
		t.Error("a missing folder didn't fail")
	}
}

func TestRestoreSkips(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	for _, name := range []string{"data/ok.cfg", "ro/locked.cfg", "../escape", "data/../../etc/x", "/abs"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
	}
	tw.WriteHeader(&tar.Header{Name: "data/fifo", Typeflag: tar.TypeFifo})
	tw.Close()

	var info docker.ContainerJSON
	info.Mounts = []docker.MountPoint{
		{Type: "bind", Source: "/a", Destination: "/data", RW: true},
		{Type: "bind", Source: "/b", Destination: "/ro", RW: false},
	}
	var out bytes.Buffer
	var st ArchiveStats
	if err := rewrite(info, tar.NewReader(&in), tar.NewWriter(&out), &st, func(ArchiveStats) {}); err != nil {
		t.Fatal(err)
	}
	names, _ := readTar(t, out.Bytes())
	if !slices.Equal(names, []string{"data/ok.cfg"}) || st.Files != 1 || len(st.Warnings) != 4 {
		t.Errorf("kept %v, stats %+v", names, st)
	}
}
