package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/prefs"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func events(t *testing.T, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		var e manager.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		key := e.Target
		if key == "" {
			key = e.Stage
		}
		out[key] = e.Stage + ": " + e.Message
	}
	return out
}

// Bulk actions go server by server: each is checked against the caller's
// role on it, and ones they can't see are "not found".
func TestBulkActions(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	w := call(h, "operator", "POST", "/api/servers/bulk", `{"servers":["valheim","plex","ghost"],"action":"restart"}`, e.keys)
	got := events(t, w.Body.String())
	if got["valheim"] != "done: Restarted" || !strings.Contains(got["plex"], "not found") || !strings.Contains(got["ghost"], "not found") ||
		got["summary"] != "summary: Restart: 1 done, 2 failed" {
		t.Errorf("operator: %v", got)
	}
	got = events(t, call(h, "viewer", "POST", "/api/servers/bulk", `{"servers":["valheim"],"action":"stop"}`, e.keys).Body.String())
	if !strings.Contains(got["valheim"], "can't do that") {
		t.Errorf("viewer: %v", got)
	}
	got = events(t, call(h, "admin", "POST", "/api/servers/bulk", `{"servers":["valheim","plex"],"action":"update"}`, e.keys).Body.String())
	if got["valheim"] != "done: Already up to date" || got["plex"] != "done: Already up to date" {
		t.Errorf("update with nothing to update: %v", got)
	}
	if w := call(h, "admin", "POST", "/api/servers/bulk", `{"servers":["valheim"],"action":"delete"}`, e.keys); w.Code != 400 {
		t.Errorf("unknown action: %d", w.Code)
	}
}

func TestTagsAndFavourites(t *testing.T) {
	e := setup(t)
	store, _ := secrets.Open(t.TempDir(), "")
	e.opts.Prefs = prefs.New(store)
	h := Handler(e.opts)
	if w := call(h, "operator", "PUT", "/api/servers/valheim/tags", `{"tags":["pvp"]}`, e.keys); w.Code != 403 {
		t.Errorf("operator setting tags: %d", w.Code)
	}
	if w := call(h, "manager", "PUT", "/api/servers/valheim/tags", `{"tags":["pvp","Friends"]}`, e.keys); w.Code != 200 {
		t.Errorf("manager setting tags: %d %s", w.Code, w.Body)
	}
	if w := call(h, "manager", "PUT", "/api/servers/valheim/tags", `{"tags":["<b>"]}`, e.keys); w.Code != 400 {
		t.Errorf("bad tag: %d", w.Code)
	}
	if w := call(h, "viewer", "PUT", "/api/servers/valheim/favourite", `{"favourite":true}`, e.keys); w.Code != 200 {
		t.Errorf("viewer starring: %d %s", w.Code, w.Body)
	}
	list := func(who string) map[string]manager.Server {
		var servers []manager.Server
		json.Unmarshal(call(h, who, "GET", "/api/servers", "", e.keys).Body.Bytes(), &servers)
		out := map[string]manager.Server{}
		for _, s := range servers {
			out[s.Name] = s
		}
		return out
	}
	// Tags are the server's; favourites are each caller's own.
	v, o := list("viewer")["valheim"], list("operator")["valheim"]
	if strings.Join(v.Tags, ",") != "pvp,Friends" || !v.Favourite || o.Favourite || strings.Join(o.Tags, ",") != "pvp,Friends" {
		t.Errorf("viewer %+v / operator %+v", v, o)
	}
}
