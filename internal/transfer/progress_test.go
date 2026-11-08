package transfer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

func TestCopyTracker(t *testing.T) {
	var got []manager.Event
	c := newCopyTracker(func(e manager.Event) { got = append(got, e) })
	c.every = 0
	// Before the total is known: how much so far.
	c.progress(files.ArchiveStats{Files: 1, Bytes: 1 << 20})
	if p := got[0].Copy; p == nil || p.Total != 0 || !strings.HasSuffix(got[0].Message, "so far") {
		t.Fatalf("first %+v", got[0])
	}
	c.measure(context.Background(), func(context.Context) (int64, error) { return 10 << 20, nil })
	for {
		c.mu.Lock()
		known := c.total != 0
		c.mu.Unlock()
		if known {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	c.progress(files.ArchiveStats{Files: 3, Bytes: 6 << 20})
	p := got[len(got)-1].Copy
	if p.Total != 10<<20 || p.Bytes != 6<<20 || p.Files != 3 {
		t.Errorf("progress %+v", p)
	}
	// 5 MiB in about 0.1 s.
	if p.Rate < 10<<20 || p.Rate > 60<<20 {
		t.Errorf("rate %.0f", p.Rate)
	}
	if msg := got[len(got)-1].Message; msg != "Copied 6.0 MiB of 10.0 MiB (3 files) so far" {
		t.Errorf("message %q", msg)
	}
	// Measured a little short: never more than all of it.
	c.progress(files.ArchiveStats{Files: 4, Bytes: 11 << 20})
	if p := got[len(got)-1].Copy; p.Total != p.Bytes {
		t.Errorf("over the total: %+v", p)
	}

	// Reported about once a second, however often the copy says.
	got = nil
	c2 := newCopyTracker(func(e manager.Event) { got = append(got, e) })
	for i := 0; i < 100; i++ {
		c2.progress(files.ArchiveStats{Bytes: int64(i)})
	}
	if len(got) != 1 {
		t.Errorf("%d reports", len(got))
	}
}
