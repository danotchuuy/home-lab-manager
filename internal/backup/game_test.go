package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestCheckGameFolders(t *testing.T) {
	got, err := checkGameFolders([]GameFolder{{Path: " /pal/Saved/0/*/backup/world/ "}, {Path: ""}, {Path: "/pal/Saved/0/*/backup/world"}, {Path: "/worlds", Match: "*_backup_*"}})
	if err != nil || len(got) != 2 || got[0].Path != "/pal/Saved/0/*/backup/world" || got[1].Match != "*_backup_*" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []GameFolder{{Path: "saves"}, {Path: "/"}, {Path: "/a/../b"}, {Path: "/a/w*/b"}, {Path: "/a", Match: "x/y"}, {Path: "/a", Match: "["}} {
		if _, err := checkGameFolders([]GameFolder{bad}); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestGameKeys(t *testing.T) {
	g := gameSeries(Settings{Prefix: "hlm"}, "pal")
	at := time.Unix(1700000000, 0)
	k := g.key("/serverdata/backup/world", "2024.01.01-10.00.00", at)
	if k != "hlm/pal/game/serverdata/backup/world/2024.01.01-10.00.00@1700000000.tar.gz" {
		t.Fatal(k)
	}
	if !g.has(k) || g.source(k) != "/serverdata/backup/world/2024.01.01-10.00.00" || gameKeyTime(k) != 1700000000 {
		t.Errorf("has %v source %q time %d", g.has(k), g.source(k), gameKeyTime(k))
	}
	for _, bad := range []string{"hlm/pal/pal-20240101-100000.tar.gz", "hlm/other/game/x@1.tar.gz", "hlm/pal/game/../x@1.tar.gz", "hlm/pal/game/x.tar.gz"} {
		if g.has(bad) {
			t.Errorf("%s taken as a game backup", bad)
		}
	}
}

func TestGameDue(t *testing.T) {
	now := time.Now()
	plan := Plan{GameFolders: []GameFolder{{Path: "/b"}}}
	if gameDue(Plan{}, true, time.Time{}, now) {
		t.Error("due without folders")
	}
	if !gameDue(plan, false, time.Time{}, now) {
		t.Error("not due the first time")
	}
	plan.LastGame = &GameSync{Started: now.Add(-5 * time.Minute)}
	if gameDue(plan, true, time.Time{}, now) {
		t.Error("due again after 5 minutes")
	}
	plan.LastGame.Started = now.Add(-gameEvery)
	if !gameDue(plan, true, time.Time{}, now) {
		t.Error("not due after gameEvery while running")
	}
	// Stopped: once, after it settles, for the last backups it wrote.
	stopped := now.Add(-5 * time.Minute)
	plan.LastGame.Started = stopped.Add(-30 * time.Minute)
	if !gameDue(plan, false, stopped, now) {
		t.Error("not due after it stopped")
	}
	if gameDue(plan, false, now.Add(-time.Minute), now) {
		t.Error("due before it settled")
	}
	plan.LastGame.Started = stopped.Add(gameSettle + time.Second)
	if gameDue(plan, false, stopped, now) {
		t.Error("due again while stopped")
	}
}

// A game's backups against SeaweedFS: each one uploaded once, a rewritten
// one again, those still being written left for later, old ones the game
// dropped deleted past GameKeep, and each one restorable on its own.
func TestSeaweedFSGameBackups(t *testing.T) {
	endpoint := seaweed(t)
	ctx := context.Background()
	store, _ := secrets.Open(t.TempDir(), "test-key")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	dir := "pal/Saved/0/W1/backup/world/"
	servers := &fakeServers{running: true, files: map[string]string{
		dir + "a/Level.sav": "a", dir + "b/Level.sav": "b", dir + "c/Level.sav": "c (being written)",
		"pal/Saved/0/W1/Level.sav": "live",
	}, mtime: map[string]time.Time{dir + "a": old, dir + "b": old.Add(time.Minute), dir + "c": time.Now()}}
	s := New(store, servers, slog.New(slog.DiscardHandler))
	if _, err := s.Save(ctx, Settings{Settings: s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret"}, Prefix: "hlm"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncGame(ctx, "srv"); err == nil {
		t.Error("synced with no folders")
	}
	if _, err := s.SetPlan(ctx, "srv", Plan{GameFolders: []GameFolder{{Path: "/pal/Saved/0/*/backup/world"}}, GameKeep: 1}); err != nil {
		t.Fatal(err)
	}
	sync := func() GameSync {
		t.Helper()
		if err := s.SyncGame(ctx, "srv"); err != nil {
			t.Fatal(err)
		}
		o := wait(t, s)
		if o.Plan.LastGame == nil || o.Plan.LastGame.Error != "" {
			t.Fatalf("last = %+v", o.Plan.LastGame)
		}
		return *o.Plan.LastGame
	}
	if g := sync(); g.Found != 3 || g.Uploaded != 2 || g.Deleted != 0 {
		t.Errorf("first: %+v", g)
	}
	if g := sync(); g.Uploaded != 0 {
		t.Errorf("again: %+v", g)
	}
	// The game writes b again (rotating its backups), and c is done.
	servers.mu.Lock()
	servers.files[dir+"b/Level.sav"] = "b, later"
	servers.mtime[dir+"b"] = old.Add(30 * time.Minute)
	servers.mtime[dir+"c"] = old.Add(20 * time.Minute)
	servers.mu.Unlock()
	// b's first copy is gone from the folder and past GameKeep: deleted.
	if g := sync(); g.Uploaded != 2 || g.Deleted != 1 {
		t.Errorf("rewritten: %+v", g)
	}
	// The game drops a: past GameKeep, it goes too.
	servers.mu.Lock()
	delete(servers.files, dir+"a/Level.sav")
	servers.mu.Unlock()
	if g := sync(); g.Uploaded != 0 || g.Deleted != 1 {
		t.Errorf("pruned: %+v", g)
	}
	// A listing cut short (a stopped container's big folder, through
	// Docker): what's listed is uploaded, nothing is cleaned up.
	servers.mu.Lock()
	servers.files[dir+"d/Level.sav"] = "d"
	servers.mtime[dir+"d"] = old.Add(5 * time.Minute)
	servers.files[dir+"e/Level.sav"] = "e"
	servers.mtime[dir+"e"] = old.Add(10 * time.Minute)
	servers.truncate = 3 // b, c and d: e isn't listed
	servers.mu.Unlock()
	if g := sync(); g.Uploaded != 1 || g.Deleted != 0 || !slices.ContainsFunc(g.Warnings, func(w string) bool { return strings.Contains(w, "weren't cleaned up") }) {
		t.Errorf("cut short: %+v", g)
	}
	servers.mu.Lock()
	servers.truncate = 0
	servers.mu.Unlock()
	// In full again: e goes up; all four are still in the folder, so kept.
	if g := sync(); g.Uploaded != 1 || g.Deleted != 0 {
		t.Errorf("listed in full again: %+v", g)
	}
	// The game drops d and e, older than b (the newest): they go.
	servers.mu.Lock()
	for _, k := range []string{"d", "e"} {
		delete(servers.files, dir+k+"/Level.sav")
	}
	servers.mu.Unlock()
	if g := sync(); g.Deleted != 2 {
		t.Errorf("dropped: %+v", g)
	}

	o, _ := s.Overview(ctx, "srv")
	if len(o.GameBackups) != 2 || !strings.HasSuffix(o.GameBackups[0].Key, "/b@"+strconv.FormatInt(old.Add(30*time.Minute).Unix(), 10)+".tar.gz") {
		t.Fatalf("game backups: %+v", o.GameBackups)
	}
	if len(o.Backups) != 0 {
		t.Errorf("game backups listed as hlm's: %+v", o.Backups)
	}
	all, _ := s.All(ctx, func(string) bool { return true }, true)
	if len(all.Servers) != 1 || len(all.Servers[0].GameStored) != 2 || len(all.Servers[0].Stored) != 0 || all.Servers[0].LastGame == nil {
		t.Errorf("all: %+v", all.Servers)
	}

	// Each holds just its own folder, named by its path in the container.
	rc, _, err := s.Download(ctx, "srv", o.GameBackups[0].Key, "")
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(rc)
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	rc.Close()
	if len(got) != 1 || got[dir+"b/Level.sav"] != "b, later" {
		t.Errorf("contents %v", got)
	}

	// Restoring one puts it back where the game keeps it.
	servers.mu.Lock()
	delete(servers.files, dir+"b/Level.sav")
	servers.mu.Unlock()
	if err := s.Restore(ctx, "srv", o.GameBackups[0].Key, "", false); err != nil {
		t.Fatal(err)
	}
	wait(t, s)
	if servers.file(dir+"b/Level.sav") != "b, later" {
		t.Errorf("restored %q", servers.file(dir+"b/Level.sav"))
	}
}
