package deploy

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// TestBuildWholeFeed builds every game server template in a real CA feed with
// its default values. Run it with HLM_FEED_FILE=path/to/applicationFeed.json.
func TestBuildWholeFeed(t *testing.T) {
	path := os.Getenv("HLM_FEED_FILE")
	if path == "" {
		t.Skip("HLM_FEED_FILE not set")
	}
	dir := t.TempDir()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "applicationFeed.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cat := catalog.New("", dir, slog.New(slog.DiscardHandler))
	if err := cat.LoadCache(); err != nil {
		t.Fatal(err)
	}

	apps, total := cat.Search(catalog.Query{Category: "GameServers"})
	failed := 0
	warnings := map[string]int{}
	for _, app := range apps {
		spec, err := Build(app.Localize("/srv/appdata"), Request{TemplateID: app.ID, Name: "test"}, Env{})
		if err != nil {
			failed++
			t.Logf("%s (%s): %v", app.Name, app.ID, err)
			continue
		}
		for _, w := range spec.Warnings {
			warnings[strings.SplitN(w, ":", 2)[0]]++
		}
	}
	t.Logf("%d/%d game server templates build with defaults; warnings: %v", total-failed, total, warnings)
}
