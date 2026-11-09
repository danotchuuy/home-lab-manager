package manager

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/files"
)

type fakeAgentArchive struct {
	io.Reader
	res files.ArchiveStats
	err error
}

func (fakeAgentArchive) Close() error                          { return nil }
func (f fakeAgentArchive) Result() (files.ArchiveStats, error) { return f.res, f.err }

// An agent's archive is copied entry by entry, counted as it goes, with the
// agent's exclusions, warnings and error.
func TestCopyArchive(t *testing.T) {
	var src bytes.Buffer
	tw := tar.NewWriter(&src)
	tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "data/a", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.WriteHeader(&tar.Header{Name: "data/b", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644})
	tw.Write([]byte("de"))
	tw.Close()

	var out bytes.Buffer
	otw := tar.NewWriter(&out)
	var seen []int
	st, err := copyArchive(fakeAgentArchive{Reader: bytes.NewReader(src.Bytes()), res: files.ArchiveStats{Files: 2, Bytes: 5, Excluded: 4, Warnings: []string{"x changed"}}},
		otw, func(s files.ArchiveStats) { seen = append(seen, s.Files) })
	if err != nil {
		t.Fatal(err)
	}
	otw.Close()
	if st.Files != 2 || st.Bytes != 5 || st.Excluded != 4 || len(st.Warnings) != 1 || len(seen) != 2 {
		t.Errorf("stats %+v, progress %v", st, seen)
	}
	tr := tar.NewReader(&out)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if len(names) != 3 || names[2] != "data/b" {
		t.Errorf("copied %v", names)
	}

	// Cut short: the agent's error, not tar's.
	cut := src.Bytes()[:600]
	_, err = copyArchive(fakeAgentArchive{Reader: bytes.NewReader(cut), err: errors.New("disk on fire")}, tar.NewWriter(io.Discard), nil)
	if err == nil || err.Error() != "disk on fire" {
		t.Errorf("cut short: %v", err)
	}
}
