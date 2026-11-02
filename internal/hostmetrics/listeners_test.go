package hostmetrics

import (
	"net/netip"
	"path/filepath"
	"testing"
)

func TestListeners(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "stat"), "cpu 1 2 3 4\n")
	head := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	writeFile(t, filepath.Join(dir, "1", "net", "tcp"), head+
		"   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1\n"+ // 0.0.0.0:8080 listening
		"   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2\n"+ // 127.0.0.1:53 listening
		"   2: 0501A8C0:0016 0A01A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 3\n") // established: skipped
	writeFile(t, filepath.Join(dir, "1", "net", "udp6"), head+
		"   0: 00000000000000000000000000000000:0998 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 4\n") // [::]:2456
	c := New(Config{ProcPath: dir}, "")
	ls, err := c.Listeners()
	if err != nil {
		t.Fatal(err)
	}
	want := []Listener{
		{netip.MustParseAddr("0.0.0.0"), 8080, "tcp"},
		{netip.MustParseAddr("127.0.0.1"), 53, "tcp"},
		{netip.MustParseAddr("::"), 2456, "udp"},
	}
	if len(ls) != len(want) {
		t.Fatalf("got %+v", ls)
	}
	for i := range want {
		if ls[i] != want[i] {
			t.Errorf("listener %d = %+v, want %+v", i, ls[i], want[i])
		}
	}
	if _, err := New(Config{ProcPath: dir}, "remote").Listeners(); err == nil {
		t.Error("read listeners of an unavailable host")
	}
}
