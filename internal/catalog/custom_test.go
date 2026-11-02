package catalog

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func minecraftLike() ImageInfo {
	return ImageInfo{
		Ref: "itzg/minecraft-server:java21",
		Env: []string{
			"PATH=/usr/local/sbin:/usr/bin", "HOME=/root", "JAVA_VERSION=21.0.5", "LANG=C.UTF-8",
			"EULA=", "TYPE=VANILLA", "MEMORY=1G", "RCON_PASSWORD=minecraft", "UID=1000",
		},
		ExposedPorts: []string{"25575/tcp", "25565/tcp", "19132/udp"},
		Volumes:      []string{"/data", "/downloads"},
		Labels: map[string]string{
			"org.opencontainers.image.title":       "Minecraft Server",
			"org.opencontainers.image.description": "Minecraft server for Java Edition",
			"org.opencontainers.image.source":      "https://github.com/itzg/docker-minecraft-server",
			"org.opencontainers.image.authors":     "itzg",
		},
		User: "1000",
	}
}

func TestFromImage(t *testing.T) {
	app := FromImage(minecraftLike())
	if app.Name != "Minecraft Server" || app.Author != "itzg" || app.FromImage != "itzg/minecraft-server:java21" ||
		app.Project != "https://github.com/itzg/docker-minecraft-server" || !app.HasCategory(CustomCategory) {
		t.Errorf("app = %+v", app)
	}
	if !strings.Contains(app.Overview, "Minecraft server for Java Edition") || !strings.Contains(app.Overview, "user 1000") {
		t.Errorf("overview = %q", app.Overview)
	}
	if !strings.HasPrefix(app.ID, "image-minecraft-server-") || app.ID != CustomID("itzg/minecraft-server:java21") {
		t.Errorf("id = %s", app.ID)
	}

	byID := map[string]Config{}
	var order []string
	for _, c := range app.Config {
		byID[c.ID] = c
		order = append(order, c.ID)
	}
	want := []string{
		"port:19132/udp", "port:25565/tcp", "port:25575/tcp",
		"path:/data", "path:/downloads",
		"variable:JAVA_VERSION", "variable:LANG", "variable:EULA", "variable:TYPE", "variable:MEMORY", "variable:RCON_PASSWORD", "variable:UID",
	}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("config order:\n got %v\nwant %v", order, want)
	}
	if p := byID["port:25565/tcp"]; p.Value != "25565" || p.Mode != "tcp" || p.Required {
		t.Errorf("port = %+v", p)
	}
	if p := byID["path:/data"]; p.Value != "/mnt/user/appdata/minecraft-server/data" || !p.Required {
		t.Errorf("path = %+v", p)
	}
	if local := app.Localize("/srv/hlm"); local.Config[3].Value != "/srv/hlm/minecraft-server/data" {
		t.Errorf("localized path = %s", local.Config[3].Value)
	}
	if v := byID["variable:JAVA_VERSION"]; v.Display != "advanced" {
		t.Errorf("JAVA_VERSION should be advanced: %+v", v)
	}
	if v := byID["variable:TYPE"]; v.Display != "always" || v.Default != "VANILLA" {
		t.Errorf("TYPE = %+v", v)
	}
	if !byID["variable:RCON_PASSWORD"].Mask {
		t.Error("RCON_PASSWORD should be masked")
	}
	if _, ok := byID["variable:PATH"]; ok {
		t.Error("PATH should be left out")
	}
}

func TestCustomStore(t *testing.T) {
	dir := t.TempDir()
	c := loadTestCatalog(t) // the feed fixture: 2 apps
	store := filepath.Join(dir, "templates.json")
	if err := c.UseCustomStore(store); err != nil {
		t.Fatal(err)
	}
	app := FromImage(minecraftLike())
	if err := c.AddCustom(app); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Get(app.ID); !ok || got.Name != "Minecraft Server" {
		t.Fatal("custom template not found")
	}
	if res, n := c.Search(Query{Text: "minecraft", Category: CustomCategory}); n != 1 || res[0].ID != app.ID {
		t.Errorf("search = %d results", n)
	}
	// A feed refresh keeps custom templates.
	if err := c.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(app.ID); !ok || c.Count() != 3 {
		t.Errorf("after feed reload: count %d", c.Count())
	}
	// So does a restart.
	c2 := New("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err := c2.UseCustomStore(store); err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Get(app.ID); !ok || len(got.Config) != len(app.Config) {
		t.Error("custom template not reloaded")
	}
	if res, n := c2.Search(Query{Text: "java edition"}); n != 1 || res[0].ID != app.ID {
		t.Error("reloaded template isn't searchable")
	}
	// Importing the same image again replaces the template.
	info := minecraftLike()
	info.Env = append(info.Env, "DIFFICULTY=easy")
	if err := c.AddCustom(FromImage(info)); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(app.ID); len(got.Config) != len(app.Config)+1 || c.Count() != 3 {
		t.Errorf("re-import: %d settings, %d apps", len(got.Config), c.Count())
	}

	// Only custom templates can be deleted.
	feedApp, _ := c.Search(Query{Text: "enshrouded"})
	if err := c.DeleteCustom(feedApp[0].ID); !errors.Is(err, ErrNotCustom) {
		t.Errorf("deleting a feed app: %v", err)
	}
	if err := c.DeleteCustom(app.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(app.ID); ok {
		t.Error("deleted template still there")
	}
	data, _ := os.ReadFile(store)
	if strings.Contains(string(data), app.ID) {
		t.Error("deleted template still stored")
	}
}
