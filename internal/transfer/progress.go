package transfer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// copyTracker reports a data copy's progress about once a second: how much
// is copied, of how much in all (measured alongside the copy, so it doesn't
// have to wait), and how fast it's going lately.
type copyTracker struct {
	emit  func(manager.Event)
	every time.Duration

	mu      sync.Mutex
	total   int64
	last    time.Time
	samples []copySample // the last few seconds', for the speed
}

type copySample struct {
	at    time.Time
	bytes int64
}

// rateWindow is how far back the speed is measured.
const rateWindow = 5 * time.Second

func newCopyTracker(emit func(manager.Event)) *copyTracker {
	return &copyTracker{emit: emit, every: time.Second}
}

// measure works out the copy's total in the background.
func (c *copyTracker) measure(ctx context.Context, size func(context.Context) (int64, error)) {
	go func() {
		n, err := size(ctx)
		if err != nil || n <= 0 {
			return // the progress just doesn't say how much is left
		}
		c.mu.Lock()
		c.total = n
		c.mu.Unlock()
	}()
}

// progress takes the copy's running totals.
func (c *copyTracker) progress(st files.ArchiveStats) {
	now := time.Now()
	c.mu.Lock()
	c.samples = append(c.samples, copySample{now, st.Bytes})
	for len(c.samples) > 2 && now.Sub(c.samples[1].at) > rateWindow {
		c.samples = c.samples[1:]
	}
	if now.Sub(c.last) < c.every {
		c.mu.Unlock()
		return
	}
	c.last = now
	p := manager.CopyProgress{Files: st.Files, Bytes: st.Bytes, Total: c.total}
	if first := c.samples[0]; now.Sub(first.at) > 0 && len(c.samples) > 1 {
		p.Rate = float64(st.Bytes-first.bytes) / now.Sub(first.at).Seconds()
	}
	if p.Total > 0 && p.Bytes > p.Total {
		p.Total = p.Bytes // measured a little short (files growing, say)
	}
	c.mu.Unlock()

	msg := fmt.Sprintf("Copied %d files (%s) so far", p.Files, formatBytes(p.Bytes))
	if p.Total > 0 {
		msg = fmt.Sprintf("Copied %s of %s (%d files) so far", formatBytes(p.Bytes), formatBytes(p.Total), p.Files)
	}
	c.emit(manager.Event{Stage: "copy", Message: msg, Copy: &p})
}
