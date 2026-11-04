package hostmetrics

import (
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

type fakeNotifier struct {
	events  []notify.Event
	cleared []string
}

func (f *fakeNotifier) Notify(e notify.Event) { f.events = append(f.events, e) }
func (f *fakeNotifier) Clear(key string)      { f.cleared = append(f.cleared, key) }

func TestDiskAlerts(t *testing.T) {
	const gib = 1 << 30
	disk := func(used, free uint64) []Filesystem {
		return []Filesystem{{Device: "/dev/sda1", Mount: "/", Used: used * gib, Free: free * gib}}
	}
	n := &fakeNotifier{}
	w := &diskWatch{over: map[string]bool{}}

	w.check(disk(80, 20), 90, n) // 80%
	w.check(disk(91, 9), 90, n)  // 91%: warn
	w.check(disk(92, 8), 90, n)  // still over: no repeat
	w.check(disk(87, 13), 90, n) // under, but not by enough to reset
	w.check(disk(91, 9), 90, n)  // still counts as the same episode
	if len(n.events) != 1 || n.events[0].Title != "/ is 91% full" || n.events[0].Key != "disk.low//" {
		t.Fatalf("events = %+v", n.events)
	}
	if n.events[0].Message != "The filesystem at / (/dev/sda1) on the Docker host has 9.0 GiB free of 100.0 GiB." {
		t.Errorf("message = %q", n.events[0].Message)
	}
	w.check(disk(80, 20), 90, n) // well under: reset
	w.check(disk(95, 5), 90, n)  // over again: warn again
	if len(n.events) != 2 || len(n.cleared) != 1 {
		t.Errorf("events = %d, cleared = %v", len(n.events), n.cleared)
	}
}
