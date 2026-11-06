// Package selfexe puts hlm's own binary into containers, to run small
// helpers (a wake listener, a status probe) in images that are already on
// a host. That needs a static Linux binary for the image's architecture.
package selfexe

import (
	"archive/tar"
	"context"
	"debug/elf"
	"io"
	"os"
	"runtime"
	"strings"
)

// Check says why the binary at path can't run in a container, or "".
func Check(path string) string {
	if runtime.GOOS != "linux" {
		return "it needs hlm to run on Linux"
	}
	if path == "" {
		return "hlm couldn't find its own binary"
	}
	f, err := elf.Open(path)
	if err != nil {
		return "hlm couldn't read its own binary: " + err.Error()
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "it needs a static build of hlm (CGO_ENABLED=0, as releases are)"
		}
	}
	return ""
}

// Fits says why an image (by its os and architecture) can't run this
// binary, or "".
func Fits(os, arch string) string {
	if (os != "" && os != "linux") || (arch != "" && arch != runtime.GOARCH) {
		return "the image is for " + os + "/" + arch + " and hlm for linux/" + runtime.GOARCH
	}
	return ""
}

// Copier copies a tar stream into a container (docker.Client.CopyTo).
type Copier interface {
	CopyTo(ctx context.Context, id, dir string, r io.Reader) error
}

// CopyInto puts the binary at path into a created container, as dest (an
// absolute path at the container's root).
func CopyInto(ctx context.Context, d Copier, id, path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(dest, "/"), Mode: 0o755, Size: st.Size(),
			ModTime: st.ModTime(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	err = d.CopyTo(ctx, id, "/", pr)
	pr.Close()
	return err
}
