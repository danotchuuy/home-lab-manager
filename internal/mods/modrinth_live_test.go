package mods

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestModrinthLive talks to the real Modrinth API; it runs only with
// HLM_LIVE_MODRINTH=1.
func TestModrinthLive(t *testing.T) {
	if os.Getenv("HLM_LIVE_MODRINTH") != "1" {
		t.Skip("set HLM_LIVE_MODRINTH=1 to talk to Modrinth")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m := NewModrinth("danotchuuy/home-lab-manager/test")
	fit := Fit{Loaders: []string{"fabric"}, GameVersion: "1.21.4"}
	hits, err := m.Search(ctx, "lithium", fit, 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v, %d hits", err, len(hits))
	}
	t.Logf("search: %s (%s), server %s", hits[0].Title, hits[0].ProjectID, hits[0].ServerSide)
	plugins, err := m.Search(ctx, "luckperms", Fit{Loaders: []string{"paper", "spigot", "bukkit"}, GameVersion: "1.21.4"}, 5)
	if err != nil || len(plugins) == 0 {
		t.Fatalf("plugin search: %v, %d hits", err, len(plugins))
	}
	t.Logf("plugins: %s", plugins[0].Title)
	vs, err := m.Versions(ctx, hits[0].ProjectID, fit)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := Best(vs)
	if !ok {
		t.Fatal("no version")
	}
	f, _ := v.File()
	t.Logf("best: %s %s %s %d deps %v", v.VersionNumber, v.VersionType, f.Filename, len(v.Dependencies), v.Dependencies)
	if err := m.checkURL(f.URL); err != nil {
		t.Fatal(err)
	}
	byHash, err := m.ByHash(ctx, []string{f.Hashes["sha512"]})
	if err != nil || byHash[f.Hashes["sha512"]].ID != v.ID {
		t.Fatalf("by hash: %v %+v", err, byHash)
	}
	latest, err := m.Latest(ctx, []string{f.Hashes["sha512"]}, Fit{Loaders: fit.Loaders})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("latest for any game version: %s %v", latest[f.Hashes["sha512"]].VersionNumber, latest[f.Hashes["sha512"]].GameVersions)
	ps, err := m.Projects(ctx, []string{"sodium", "fabric-api"})
	if err != nil || len(ps) != 2 {
		t.Fatalf("projects: %v %+v", err, ps)
	}
	p, err := m.Download(ctx, f, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("downloaded to %s", p)
}
