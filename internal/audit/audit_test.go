package audit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func open(t *testing.T) (*Log, *time.Time) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(filepath.Join(t.TempDir(), "audit"), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	t.Cleanup(func() { l.Close() })
	return l, &now
}

func actions(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Action)
	}
	return out
}

func TestRecordAndQuery(t *testing.T) {
	l, now := open(t)
	rec := func(d time.Duration, e Entry) {
		e.Time = now.Add(d)
		l.Record(e)
	}
	// Two months: October and November.
	rec(-20*24*time.Hour, Entry{Actor: "alice", ActorKind: ActorLocal, Action: "servers.create", Target: "valheim", Server: "valheim"})
	rec(-2*time.Hour, Entry{Actor: "sso:bob", ActorKind: ActorSSO, Action: "servers.stop", Target: "valheim", Server: "valheim"})
	rec(-time.Hour, Entry{Actor: "key:ci", ActorKind: ActorKey, Action: "servers.delete", Target: "minecraft", Server: "minecraft",
		Result: ResultDenied, Status: 403, Error: "you don't have access to do this"})
	rec(0, Entry{Actor: "alice", ActorKind: ActorLocal, Action: "auth.login", IP: "192.168.1.20"})

	all, next, err := l.Query(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actions(all), []string{"auth.login", "servers.delete", "servers.stop", "servers.create"}) || !next.IsZero() {
		t.Errorf("all = %q, next %v", actions(all), next)
	}
	files, _ := l.Files()
	if len(files) != 2 || !strings.HasSuffix(files[0], "audit-2026-11.jsonl") || !strings.HasSuffix(files[1], "audit-2026-10.jsonl") {
		t.Errorf("files = %q", files)
	}

	tests := []struct {
		name string
		f    Filter
		want []string
	}{
		{"by actor", Filter{Actor: "ALICE"}, []string{"auth.login", "servers.create"}},
		{"by action prefix", Filter{Action: "servers."}, []string{"servers.delete", "servers.stop", "servers.create"}},
		{"by server", Filter{Server: "valheim"}, []string{"servers.stop", "servers.create"}},
		{"by result", Filter{Result: ResultDenied}, []string{"servers.delete"}},
		{"by text", Filter{Text: "192.168.1.20"}, []string{"auth.login"}},
		{"by text in error", Filter{Text: "don't have access"}, []string{"servers.delete"}},
		{"since", Filter{Since: now.Add(-3 * time.Hour)}, []string{"auth.login", "servers.delete", "servers.stop"}},
	}
	for _, tt := range tests {
		got, _, err := l.Query(tt.f)
		if err != nil || !slices.Equal(actions(got), tt.want) {
			t.Errorf("%s: %q, %v; want %q", tt.name, actions(got), err, tt.want)
		}
	}

	// Paging, across the months.
	page1, next, _ := l.Query(Filter{Limit: 3})
	if len(page1) != 3 || next.IsZero() {
		t.Fatalf("page 1: %q, next %v", actions(page1), next)
	}
	page2, next2, _ := l.Query(Filter{Limit: 3, Before: next})
	if !slices.Equal(actions(page2), []string{"servers.create"}) || !next2.IsZero() {
		t.Errorf("page 2: %q, next %v", actions(page2), next2)
	}
}

func TestRedactionAndDamage(t *testing.T) {
	l, now := open(t)
	l.Record(Entry{Actor: "alice", Action: "integrations.backup.set", Result: ResultError,
		Error: "PUT http://key:topsecret@s3.lan/b: refused", Details: map[string]string{"command": "login token=abc123"}})
	// A line cut short by a crash, and junk.
	f, _ := os.OpenFile(l.path(monthOf(*now)), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"time":"2026-11-04T12:00:00Z","actor":"x","act` + "\nnot json\n")
	f.Close()
	l.Record(Entry{Actor: "alice", Action: "servers.start"})
	got, _, err := l.Query(Filter{})
	if err != nil || !slices.Equal(actions(got), []string{"servers.start", "integrations.backup.set"}) {
		t.Fatalf("%q, %v", actions(got), err)
	}
	if strings.Contains(got[1].Error, "topsecret") || strings.Contains(got[1].Details["command"], "abc123") {
		t.Errorf("not redacted: %+v", got[1])
	}
	if got[0].Result != ResultOK || got[0].Time.IsZero() {
		t.Errorf("defaults: %+v", got[0])
	}
	var nilLog *Log
	nilLog.Record(Entry{Action: "x"}) // must not panic
}

func TestRetention(t *testing.T) {
	l, now := open(t)
	for m := range 14 {
		l.Record(Entry{Action: "a", Time: time.Date(2025, time.Month(10+m), 15, 0, 0, 0, 0, time.UTC)})
	}
	if err := l.prune(); err != nil {
		t.Fatal(err)
	}
	months, _ := l.months()
	if len(months) != 12 || months[0] != "2026-11" || months[11] != "2025-12" {
		t.Errorf("kept %q", months)
	}
	if err := l.SetSettings(Settings{RetentionMonths: 2, Mirror: true}); err != nil {
		t.Fatal(err)
	}
	months, _ = l.months()
	if !slices.Equal(months, []string{"2026-11", "2026-10"}) {
		t.Errorf("after 2 months: %q", months)
	}
	if !l.Settings().Mirror || l.Settings().Months() != 2 {
		t.Errorf("settings = %+v", l.Settings())
	}
	if err := l.SetSettings(Settings{RetentionMonths: 500}); err == nil {
		t.Error("accepted 500 months")
	}
	_ = now
}

func TestDetails(t *testing.T) {
	ctx, d := WithDetails(context.Background())
	Add(ctx, "path", "/data/server.properties")
	Add(ctx, "empty", "")
	Addf(ctx, "count", "%d files", 3)
	Add(context.Background(), "nowhere", "x") // no collector: ignored
	if len(d) != 2 || d["path"] != "/data/server.properties" || d["count"] != "3 files" {
		t.Errorf("details = %v", d)
	}
}
