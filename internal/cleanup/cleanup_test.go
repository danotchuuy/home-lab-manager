package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeManager struct{ runs []string }

func (f *fakeManager) HostIDs() []string { return []string{"local", "nas"} }
func (f *fakeManager) CleanupLeftovers(_ context.Context, host string) (manager.CleanupResult, error) {
	f.runs = append(f.runs, host)
	if host == "nas" {
		return manager.CleanupResult{}, errors.New("can't be reached")
	}
	return manager.CleanupResult{Freed: 3e9, Removed: []manager.CleanupItem{{Kind: manager.CleanOldImage, Name: "game"}}}, nil
}

type recorder struct{ entries []audit.Entry }

func (r *recorder) Record(e audit.Entry) { r.entries = append(r.entries, e) }

func TestWeeklyCleanup(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	m, rec := &fakeManager{}, &recorder{}
	s := New(store, m, rec, slog.New(slog.DiscardHandler))
	now := time.Now()

	if !s.RemoveOldImages() {
		t.Error("old images are kept by default")
	}
	if s.RunIfDue(context.Background(), now) || len(m.runs) != 0 {
		t.Error("ran while off")
	}
	if _, err := s.Save(Settings{Weekly: true, KeepOldImages: true}); err != nil {
		t.Fatal(err)
	}
	if s.RemoveOldImages() {
		t.Error("removes old images while told to keep them")
	}
	if !s.RunIfDue(context.Background(), now) || len(m.runs) != 2 {
		t.Fatalf("first run: %q", m.runs)
	}
	set, _ := s.Settings()
	if set.LastRun == nil || !set.LastRun.Equal(now) || set.LastFreed != 3e9 || set.LastError != "nas: can't be reached" {
		t.Errorf("after the run: %+v", set)
	}
	if len(rec.entries) != 1 || rec.entries[0].Action != "host.cleanup.weekly" || rec.entries[0].Details["freed"] != "3000000000" {
		t.Errorf("audit: %+v", rec.entries)
	}
	if s.RunIfDue(context.Background(), now.Add(6*24*time.Hour)) {
		t.Error("ran again within a week")
	}
	// Saving settings keeps the last run.
	s.Save(Settings{Weekly: true})
	if !s.RunIfDue(context.Background(), now.Add(Week)) || len(m.runs) != 4 {
		t.Errorf("a week later: %q", m.runs)
	}
}
