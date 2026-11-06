package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// wsServer is a server whose image takes Workshop items in a variable.
type wsServer struct {
	*fakeServer
	image    string
	app      *catalog.App
	req      deploy.Request
	started  time.Time
	applied  []string // the variable after each reconfigure
	crashOn  string   // an item that makes it crash
	failNext bool     // the next reconfigure fails
}

func newWS(image string, items, names string) *wsServer {
	app := &catalog.App{Config: []catalog.Config{
		{ID: "mods", Type: catalog.TypeVariable, Target: "GAME_MOD_IDS"},
		{ID: "ids", Type: catalog.TypeVariable, Target: "MOD_WORKSHOP_IDS"},
		{ID: "names", Type: catalog.TypeVariable, Target: "MOD_NAMES"},
	}}
	w := &wsServer{fakeServer: newFake(), image: image, app: app, started: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	w.req = deploy.Request{Name: "game", Values: map[string]string{"mods": items, "ids": items, "names": names}}
	return w
}

func (w *wsServer) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	info, _ := w.fakeServer.Inspect(ctx, id)
	info.Config.Image = w.image
	w.mu.Lock()
	info.State.StartedAt = w.started.Format(time.RFC3339Nano)
	w.mu.Unlock()
	return info, nil
}

func (w *wsServer) Config(ctx context.Context, id string) (*catalog.App, deploy.Request, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.app, w.req, nil
}

func (w *wsServer) Reconfigure(ctx context.Context, id string, req deploy.Request, emit func(manager.Event)) (manager.Server, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failNext {
		w.failNext = false
		return manager.Server{}, errors.New("the image can't be pulled")
	}
	v, _ := deploy.EnvValue(w.app, req, "GAME_MOD_IDS")
	if strings.Contains(w.image, "zomboid") {
		ids, _ := deploy.EnvValue(w.app, req, "MOD_WORKSHOP_IDS")
		names, _ := deploy.EnvValue(w.app, req, "MOD_NAMES")
		v = ids + "|" + names
	}
	w.req = req
	w.applied = append(w.applied, v)
	w.started = w.started.Add(time.Hour)
	// A stopped (crashed) server is recreated stopped.
	w.running = w.running && (w.crashOn == "" || !strings.Contains(v, w.crashOn))
	return manager.Server{}, nil
}

// fakeSteam knows a few items.
func fakeSteam(t *testing.T) *Steam {
	items := map[string]map[string]any{
		"731604991": {"title": "Structures Plus (S+)", "consumer_app_id": 346110, "time_updated": 1680660170, "file_size": "60998180"},
		"1404697612": {"title": "Awesome Spyglass!", "consumer_app_id": 346110,
			"time_updated": time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Unix(), "file_size": "1000"},
		"666":        {"title": "Crashes Everything", "consumer_app_id": 346110, "time_updated": 1, "file_size": "1"},
		"2169435993": {"title": "Common Sense", "consumer_app_id": 108600, "time_updated": 1, "file_size": "1"},
		"880454836":  {"title": "Pippi", "consumer_app_id": 440900, "time_updated": 1, "file_size": "1"},
		"13":         {"title": "Banned Mod", "consumer_app_id": 346110, "time_updated": 1, "file_size": "1", "banned": 1},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		var out []map[string]any
		for i := 0; ; i++ {
			id := r.PostForm.Get(fmt.Sprintf("publishedfileids[%d]", i))
			if id == "" {
				break
			}
			d := map[string]any{"publishedfileid": id, "result": 9}
			if it, ok := items[id]; ok {
				d = map[string]any{"publishedfileid": id, "result": 1}
				for k, v := range it {
					d[k] = v
				}
			}
			out = append(out, d)
		}
		json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": 1, "publishedfiledetails": out}})
	}))
	t.Cleanup(srv.Close)
	return &Steam{API: srv.URL, Client: srv.Client(), UserAgent: "hlm-test"}
}

func wsSetup(t *testing.T, w *wsServer) (*Service, *notes) {
	n := &notes{}
	s := New(Options{Servers: w, Steam: fakeSteam(t), Notifier: n, Dir: t.TempDir(),
		Ready: func(ctx context.Context, server string) (bool, bool) {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.running, true
		},
		Poll: time.Millisecond, Settle: time.Millisecond, StartTimeout: 200 * time.Millisecond,
	})
	return s, n
}

func TestWorkshopOverview(t *testing.T) {
	w := newWS("hermsi/ark-server:latest", "731604991, 1404697612,42", "")
	s, _ := wsSetup(t, w)
	ov, err := s.Workshop(context.Background(), "game")
	if err != nil {
		t.Fatal(err)
	}
	if ov.AppID != 346110 || ov.Env != "GAME_MOD_IDS" || len(ov.Items) != 3 {
		t.Fatalf("overview: %+v", ov)
	}
	if it := ov.Items[0]; it.Title != "Structures Plus (S+)" || it.Size != 60998180 || it.Newer {
		t.Errorf("first: %+v", it)
	}
	if it := ov.Items[1]; !it.Newer {
		t.Errorf("updated since start: %+v", it)
	}
	if it := ov.Items[2]; !it.Missing {
		t.Errorf("unknown: %+v", it)
	}

	// An image hlm doesn't know the variable of.
	w.image = "thmhoag/arkserver"
	if _, err := s.Workshop(context.Background(), "game"); !errors.Is(err, ErrNoWorkshop) {
		t.Errorf("unknown image: %v", err)
	}
}

func TestWorkshopApply(t *testing.T) {
	w := newWS("hermsi/ark-server", "731604991", "")
	s, n := wsSetup(t, w)
	ctx := context.Background()

	// Added by link, in order.
	_, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{
		{ID: "https://steamcommunity.com/sharedfiles/filedetails/?id=1404697612&searchtext="}, {ID: "731604991"},
	}, noEvents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.applied, "|") != "1404697612,731604991" {
		t.Errorf("applied: %v", w.applied)
	}

	for _, tt := range []struct {
		items []WorkshopEntry
		want  string
	}{
		{[]WorkshopEntry{{ID: "1404697612"}, {ID: "731604991"}}, "nothing to change"},
		{[]WorkshopEntry{{ID: "99"}}, "Steam has no Workshop item 99"},
		{[]WorkshopEntry{{ID: "2169435993"}}, "for another game, not ARK"},
		{[]WorkshopEntry{{ID: "13"}}, "banned"},
		{[]WorkshopEntry{{ID: "1;rm"}}, "isn't a Workshop item"},
		{[]WorkshopEntry{{ID: "731604991"}, {ID: "731604991"}}, "twice"},
		{[]WorkshopEntry{{ID: "731604991", Name: "x"}}, "doesn't take mod names"},
	} {
		if _, err := s.ApplyWorkshop(ctx, "game", "dan", tt.items, noEvents); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%+v: %v, want %q", tt.items, err, tt.want)
		}
	}
	if len(w.applied) != 1 {
		t.Errorf("refused changes were applied: %v", w.applied)
	}

	// One that crashes the server: the old list is put back.
	w.crashOn = "666"
	res, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "1404697612"}, {ID: "666"}}, noEvents)
	if err == nil || !res.RolledBack || !strings.Contains(err.Error(), "put the old list back") {
		t.Fatalf("crash: %+v, %v", res, err)
	}
	if strings.Join(w.applied, "|") != "1404697612,731604991|1404697612,666|1404697612,731604991" || !w.running {
		t.Errorf("applied: %v, running %v", w.applied, w.running)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindModsFailed {
		t.Errorf("notified: %+v", n.events)
	}

	// A reconfigure that fails changes nothing.
	w.failNext = true
	if _, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "731604991"}}, noEvents); err == nil || !strings.Contains(err.Error(), "can't be pulled") {
		t.Errorf("failed reconfigure: %v", err)
	}

	// A stopped server is changed and left stopped.
	w.running = false
	res, err = s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "731604991"}}, noEvents)
	if err != nil || !strings.Contains(res.Warning, "next starts") {
		t.Errorf("stopped: %+v, %v", res, err)
	}
}

func TestWorkshopNames(t *testing.T) {
	w := newWS("renegademaster/zomboid-dedicated-server", "", "")
	s, _ := wsSetup(t, w)
	ctx := context.Background()
	if _, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "2169435993"}}, noEvents); err == nil || !strings.Contains(err.Error(), "needs its mod name") {
		t.Errorf("no name: %v", err)
	}
	if _, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "2169435993", Name: "a;b"}}, noEvents); err == nil {
		t.Error("a name with the separator")
	}
	if _, err := s.ApplyWorkshop(ctx, "game", "dan", []WorkshopEntry{{ID: "2169435993", Name: "CommonSense"}}, noEvents); err != nil {
		t.Fatal(err)
	}
	if got := w.applied[len(w.applied)-1]; got != "2169435993|CommonSense" {
		t.Errorf("applied: %q", got)
	}
	ov, err := s.Workshop(ctx, "game")
	if err != nil || ov.NamesEnv != "MOD_NAMES" || len(ov.Items) != 1 || ov.Items[0].Name != "CommonSense" || ov.Items[0].Title != "Common Sense" {
		t.Errorf("overview: %+v, %v", ov, err)
	}
}

func TestWorkshopID(t *testing.T) {
	for in, want := range map[string]string{
		"731604991": "731604991",
		"https://steamcommunity.com/sharedfiles/filedetails/?id=731604991":           "731604991",
		"https://steamcommunity.com/workshop/filedetails/?l=german&id=731604991&x=1": "731604991",
		" 731604991 ":                       "731604991",
		"https://example.com/?id=731604991": "",
		"abc":                               "",
	} {
		got, ok := WorkshopID(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}
