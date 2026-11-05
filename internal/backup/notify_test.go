package backup

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

type fakeNotifier struct {
	events  []notify.Event
	cleared []string
}

func (f *fakeNotifier) Notify(e notify.Event) { f.events = append(f.events, e) }
func (f *fakeNotifier) Clear(key string)      { f.cleared = append(f.cleared, key) }

func TestNotifications(t *testing.T) {
	n := &fakeNotifier{}
	s := &Service{}
	s.notifyBackup("valheim", Run{}) // no notifier: nothing happens
	s.UseNotifier(n)

	s.notifyBackup("valheim", Run{Trigger: "schedule", Error: "upload: connection refused"})
	s.notifyBackup("valheim", Run{Trigger: "manual", Key: "hlm/valheim/x.tar.gz", Size: 3 << 20, Files: 12,
		Warnings: []string{"world.sav changed while it was read"}})
	s.notifyBackup("", Run{Trigger: "schedule", Error: "bucket missing"})
	s.notifyRestore("valheim", Run{Files: 3})
	s.notifyRestore("valheim", Run{Error: "boom"})

	if len(n.events) != 5 {
		t.Fatalf("got %d events", len(n.events))
	}
	fail := n.events[0]
	if fail.Kind != notify.KindBackupFailed || fail.Server != "valheim" || fail.Key != "backup.failed/valheim" ||
		fail.Link != "/servers/valheim?tab=backups" || fail.Message != "upload: connection refused" {
		t.Errorf("failure = %+v", fail)
	}
	ok := n.events[1]
	if ok.Kind != notify.KindBackupSucceeded || ok.Fields["Size"] != "3.0 MiB" || ok.Fields["Files"] != "12" ||
		!strings.Contains(ok.Message, "world.sav changed") {
		t.Errorf("success = %+v", ok)
	}
	if len(n.cleared) != 2 || n.cleared[0] != "backup.failed/valheim" || n.cleared[1] != "backup.stale/valheim" {
		t.Errorf("success didn't clear the failure: %v", n.cleared)
	}
	cfg := n.events[2]
	if cfg.Server != "" || cfg.Key != "backup.failed/_hlm" || !strings.Contains(cfg.Title, "hlm's configuration") {
		t.Errorf("config failure = %+v", cfg)
	}
	if n.events[3].Kind != notify.KindRestoreFinished || n.events[4].Kind != notify.KindRestoreFailed {
		t.Errorf("restores = %+v", n.events[3:])
	}
}
