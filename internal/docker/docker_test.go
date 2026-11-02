package docker

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestSplitImageRef(t *testing.T) {
	tests := []struct{ ref, name, tag string }{
		{"nodiaque/steamcmd:enshrouded", "nodiaque/steamcmd", "enshrouded"},
		{"ubuntu", "ubuntu", "latest"},
		{"registry:5000/team/app", "registry:5000/team/app", "latest"},
		{"registry:5000/team/app:v2", "registry:5000/team/app", "v2"},
		{"ghcr.io/x/y@sha256:abc", "ghcr.io/x/y", "sha256:abc"},
	}
	for _, tt := range tests {
		name, tag := SplitImageRef(tt.ref)
		if name != tt.name || tag != tt.tag {
			t.Errorf("SplitImageRef(%q) = %q, %q; want %q, %q", tt.ref, name, tag, tt.name, tt.tag)
		}
	}
}

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hello "))
	buf.Write(frame(2, "from stderr\n"))
	buf.Write(frame(1, ""))
	buf.Write(frame(1, "bye\n"))
	got, err := io.ReadAll(newDemuxReader(io.NopCloser(&buf)))
	if err != nil {
		t.Fatal(err)
	}
	if want := "hello from stderr\nbye\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("1.41", "1.47") || versionLess("1.52", "1.47") || versionLess("1.47", "1.47") {
		t.Error("versionLess is wrong")
	}
}

func TestDemuxLimited(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "out1 "))
	buf.Write(frame(2, "err1"))
	buf.Write(frame(1, "out2 is long"))
	res, err := demuxLimited(&buf, 8)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "out1 out" || string(res.Stderr) != "err1" || !res.Truncated {
		t.Errorf("got stdout=%q stderr=%q truncated=%v", res.Stdout, res.Stderr, res.Truncated)
	}
}
