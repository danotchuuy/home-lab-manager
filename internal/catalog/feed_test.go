package catalog

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func loadTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "applicationFeed.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := New("", dir, slog.New(slog.DiscardHandler))
	if err := c.LoadCache(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseFeedSkipsPluginsAndDeprecated(t *testing.T) {
	c := loadTestCatalog(t)
	if got := c.Count(); got != 2 {
		t.Fatalf("Count() = %d, want 2 (plugin and deprecated entries skipped)", got)
	}
}

func TestEnshroudedTemplate(t *testing.T) {
	c := loadTestCatalog(t)
	apps, total := c.Search(Query{Text: "enshrouded", Category: "GameServers"})
	if total != 2 {
		t.Fatalf("search total = %d, want 2", total)
	}

	var app *App
	for _, a := range apps {
		if a.Repository == "nodiaque/steamcmd:enshrouded" {
			app = a
		}
	}
	if app == nil {
		t.Fatal("nodiaque Enshrouded template not found")
	}
	if got, ok := c.Get(app.ID); !ok || got != app {
		t.Fatalf("Get(%q) did not return the app", app.ID)
	}

	byID := map[string]Config{}
	for _, cfg := range app.Config {
		byID[cfg.ID] = cfg
	}
	port, ok := byID["port:15637/udp"]
	if !ok || port.Value != "15637" || port.Mode != "udp" {
		t.Errorf("udp query port = %+v", port)
	}
	backup := byID["variable:BACKUP"]
	if len(backup.Options) != 2 || backup.Value != "true" {
		t.Errorf("BACKUP options/value = %v/%q, want [false true]/true", backup.Options, backup.Value)
	}
	files := byID["path:/serverdata/serverfiles"]
	if files.Value != "/mnt/user/appdata/enshrouded" || files.Mode != "rw" {
		t.Errorf("serverfiles path = %+v", files)
	}

	local := app.Localize("/srv/hlm/appdata")
	for _, cfg := range local.Config {
		if cfg.ID == "path:/serverdata/serverfiles" && cfg.Value != "/srv/hlm/appdata/enshrouded" {
			t.Errorf("localized path = %q", cfg.Value)
		}
	}
	if app.Config[0].Value == local.Config[0].Value && app.Config[0].Type == TypePath {
		t.Error("Localize modified the original app")
	}
}

func TestBranchShapes(t *testing.T) {
	c := loadTestCatalog(t)
	apps, _ := c.Search(Query{Text: "broccoli"})
	if len(apps) != 1 {
		t.Fatalf("got %d apps", len(apps))
	}
	// This template's Branch is a single object rather than a list.
	if b := apps[0].Branches; len(b) != 1 || b[0].Tag != "wine" {
		t.Errorf("branches = %+v", b)
	}
}

func TestRewriteAppdata(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/mnt/user/appdata/x", "/data/x"},
		{"/mnt/cache/appdata/x/y", "/data/x/y"},
		{"/mnt/user/appdata", "/data"},
		{"/mnt/user/appdatax", "/mnt/user/appdatax"},
		{"/mnt/user/media", "/mnt/user/media"},
	}
	for _, tt := range tests {
		if got := rewriteAppdata(tt.in, "/data/"); got != tt.want {
			t.Errorf("rewriteAppdata(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRepoName(t *testing.T) {
	tests := map[string]string{
		"authelia/authelia":                  "authelia/authelia",
		"docker.io/authelia/authelia:latest": "authelia/authelia",
		"Authelia/Authelia@sha256:abc":       "authelia/authelia",
		"docker.io/library/nginx:1.27":       "nginx",
		"registry:5000/team/app":             "team/app",
		"ghcr.io/authelia/authelia:4.39":     "authelia/authelia",
	}
	for ref, want := range tests {
		if got := repoName(ref); got != want {
			t.Errorf("repoName(%q) = %q, want %q", ref, got, want)
		}
	}
}
