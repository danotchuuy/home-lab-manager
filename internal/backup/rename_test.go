package backup

import (
	"log/slog"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// A server that moves to another host keeps its plan, and its backups'
// series: the old one's, wherever it moves next.
func TestRenameServer(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, &fakeServers{}, slog.New(slog.DiscardHandler))
	store.Set(secretPlans, map[string]Plan{"valheim": {IntervalHours: 24, Keep: 7}, "other": {Keep: 1}})
	if err := s.RenameServer("valheim", "valheim@nas"); err != nil {
		t.Fatal(err)
	}
	plans, _ := s.Plans()
	if _, ok := plans["valheim"]; ok || plans["valheim@nas"].Keep != 7 || plans["other"].Keep != 1 {
		t.Errorf("plans %+v", plans)
	}
	if got := s.seriesName("valheim@nas"); got != "valheim" {
		t.Errorf("series %q", got)
	}
	if got := s.seriesName("other"); got != "other" {
		t.Errorf("unmoved series %q", got)
	}
	// Moving on keeps the original series; moving back drops the alias.
	s.RenameServer("valheim@nas", "valheim@pi")
	if got := s.seriesName("valheim@pi"); got != "valheim" || s.seriesName("valheim@nas") != "valheim@nas" {
		t.Errorf("after the second move: %q", got)
	}
	s.RenameServer("valheim@pi", "valheim")
	var m map[string]string
	store.Get(secretSeries, &m)
	if len(m) != 0 || s.seriesName("valheim") != "valheim" {
		t.Errorf("back home: %v", m)
	}
	if serverSeries(Settings{}, s.seriesName("valheim")).file != "valheim-" {
		t.Error("series file")
	}
}
