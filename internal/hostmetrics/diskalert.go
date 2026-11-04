package hostmetrics

import (
	"context"
	"fmt"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// diskClearMargin is how far below the threshold a filesystem has to drop
// before another warning about it can be sent straight away.
const diskClearMargin = 5

// diskWatch remembers which filesystems are over the threshold.
type diskWatch struct {
	over map[string]bool // by mount point
}

// WatchDisks warns n when a filesystem gets fuller than threshold() percent,
// checking every interval until ctx ends.
func (c *Collector) WatchDisks(ctx context.Context, interval time.Duration, threshold func() int, n notify.Notifier) {
	if !c.Available() || n == nil {
		return
	}
	w := &diskWatch{over: map[string]bool{}}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.check(c.Snapshot().Disks, threshold(), n)
	}
}

func usedPercent(fs Filesystem) float64 {
	// As df counts it: space reserved for root doesn't count as free.
	total := fs.Used + fs.Free
	if total == 0 {
		return 0
	}
	return float64(fs.Used) * 100 / float64(total)
}

func (w *diskWatch) check(disks []Filesystem, threshold int, n notify.Notifier) {
	for _, fs := range disks {
		pct := usedPercent(fs)
		key := notify.DedupeKey(notify.KindDiskLow, fs.Mount)
		switch {
		case pct >= float64(threshold) && !w.over[fs.Mount]:
			w.over[fs.Mount] = true
			n.Notify(notify.Event{
				Kind:  notify.KindDiskLow,
				Title: fmt.Sprintf("%s is %.0f%% full", fs.Mount, pct),
				Message: fmt.Sprintf("The filesystem at %s (%s) on the Docker host has %s free of %s.",
					fs.Mount, fs.Device, formatBytes(fs.Free), formatBytes(fs.Used+fs.Free)),
				Link:   "/host",
				Key:    key,
				Fields: map[string]string{"Used": fmt.Sprintf("%.0f%%", pct)},
			})
		case pct < float64(threshold-diskClearMargin) && w.over[fs.Mount]:
			delete(w.over, fs.Mount)
			n.Clear(key)
		}
	}
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
