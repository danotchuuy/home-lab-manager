package files

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// hostOnly is a container whose /data mount is a temp dir served from the
// host, so no Docker daemon is needed.
func hostOnly(t *testing.T) (*Browser, docker.ContainerJSON, string) {
	t.Helper()
	dir := t.TempDir()
	var info docker.ContainerJSON
	info.Mounts = []docker.MountPoint{{Type: "bind", Source: dir, Destination: "/data", RW: true}}
	return New(nil, func(p string) string { return p }), info, dir
}

func TestEditSizeLimit(t *testing.T) {
	b, info, dir := hostOnly(t)
	ctx := context.Background()
	limit := strings.Repeat("a", MaxEditSize)
	os.WriteFile(filepath.Join(dir, "exact.cfg"), []byte(limit), 0o644)
	os.WriteFile(filepath.Join(dir, "big.log"), []byte(limit+"b"), 0o644)

	if f, err := b.Read(ctx, info, "/data/exact.cfg"); err != nil || len(f.Content) != MaxEditSize {
		t.Errorf("1 MiB file: len=%d err=%v", len(f.Content), err)
	}
	if _, err := b.Read(ctx, info, "/data/big.log"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("1 MiB + 1 byte: err = %v, want ErrTooLarge", err)
	}
	if _, err := b.Write(ctx, info, "/data/exact.cfg", limit+"b", time.Time{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("saving 1 MiB + 1 byte: err = %v, want ErrTooLarge", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "exact.cfg")); len(got) != MaxEditSize {
		t.Errorf("file changed by a rejected save: %d bytes", len(got))
	}
	if _, err := b.Write(ctx, info, "/data/exact.cfg", "small", time.Time{}); err != nil {
		t.Errorf("small save: %v", err)
	}
}

func TestDLLsAreNotEditable(t *testing.T) {
	b, info, dir := hostOnly(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(dir, "steam_api64.dll"), []byte("text-looking contents"), 0o644)
	os.WriteFile(filepath.Join(dir, "Mod.DLL"), []byte("x"), 0o644)
	os.Symlink("steam_api64.dll", filepath.Join(dir, "innocent.cfg"))
	os.WriteFile(filepath.Join(dir, "settings.ini"), []byte("a=1"), 0o644)

	for _, p := range []string{"/data/steam_api64.dll", "/data/Mod.DLL", "/data/innocent.cfg"} {
		if _, err := b.Read(ctx, info, p); !errors.Is(err, ErrNotEditable) {
			t.Errorf("read %s: err = %v, want ErrNotEditable", p, err)
		}
		if _, err := b.Write(ctx, info, p, "pwned", time.Time{}); !errors.Is(err, ErrNotEditable) {
			t.Errorf("write %s: err = %v, want ErrNotEditable", p, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "steam_api64.dll")); string(got) != "text-looking contents" {
		t.Errorf("dll was changed: %q", got)
	}
	if _, err := b.Read(ctx, info, "/data/settings.ini"); err != nil {
		t.Errorf("settings.ini: %v", err)
	}
	if !Editable("server.json") || Editable("bin/Game.dll") {
		t.Error("Editable is wrong")
	}
}

func TestHostCantWrite(t *testing.T) {
	readOnly := &fs.PathError{Op: "open", Path: "/host/rootfs/srv/hlm/appdata/mc/server.properties", Err: syscall.EROFS}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"read-only host mount": {readOnly, true},
		"permission":           {&fs.PathError{Op: "open", Err: fs.ErrPermission}, true},
		"saved":                {nil, false},
		"changed meanwhile":    {ErrConflict, false},
		"disk full":            {&fs.PathError{Op: "write", Err: syscall.ENOSPC}, false},
	} {
		if got := hostCantWrite(c.err); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
