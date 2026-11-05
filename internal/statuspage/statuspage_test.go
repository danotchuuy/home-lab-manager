package statuspage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeServers struct{ list []manager.Server }

func (f *fakeServers) List(context.Context) ([]manager.Server, error) { return f.list, nil }

type fakeQuery map[string]gamequery.Status

func (f fakeQuery) Status(name string, names bool) gamequery.Status {
	st := f[name]
	if st.Result != nil && !names {
		r := *st.Result
		r.PlayerNames = nil
		st.Result = &r
	}
	return st
}

func setup(t *testing.T) (*Service, *fakeServers) {
	t.Helper()
	store, _ := secrets.Open(t.TempDir(), "")
	srvs := &fakeServers{list: []manager.Server{
		{ID: "abc123", Name: "valheim", App: "Valheim", Icon: "https://example.com/v.png", Image: "secret/image:tag", State: "running",
			IPAddress: "172.17.0.9", Ports: nil, Request: &deploy.Request{Values: map[string]string{"pw": "hunter2"}}},
		{ID: "def456", Name: "mc@nas", App: "Minecraft", State: "running"},
		{ID: "ghi789", Name: "ark", App: "ARK", State: "exited"},
		{ID: "jkl012", Name: "hidden", App: "Hidden", State: "running"},
	}}
	q := fakeQuery{
		"valheim": {Status: gamequery.StatusOnline, Result: &gamequery.Result{Players: 3, MaxPlayers: 10, HasPlayers: true,
			PlayerNames: []string{"ann", "bo", "cy"}, Version: "0.218", Map: "Midgard", Address: "172.17.0.9:2457"}},
		"mc@nas": {Status: gamequery.StatusStarting},
	}
	s := New(Options{Secrets: store, Servers: srvs, Query: q, Uptime: func(name string, rng time.Duration) (float64, bool) {
		if name == "valheim" {
			return 0.99, true
		}
		return 0, false
	}})
	return s, srvs
}

func TestPage(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := s.Page(ctx, "", now); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := s.Save(Settings{Enabled: true, Message: "Maintenance Saturday", Servers: []Entry{
		{Server: "valheim", DisplayName: "Viking world", ShowPlayers: true, ShowNames: true, ShowVersion: true, Address: "play.example.com:2456", Hint: "Ask Dana for the password"},
		{Server: "mc@nas", ShowPlayers: true},
		{Server: "ark", DisplayName: "Viking world"},
		{Server: "gone", DisplayName: "Old server"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.Page(ctx, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != DefaultTitle || p.Message != "Maintenance Saturday" || len(p.Servers) != 4 {
		t.Fatalf("page = %+v", p)
	}
	v, mc, ark, gone := p.Servers[0], p.Servers[1], p.Servers[2], p.Servers[3]
	if v.Name != "Viking world" || v.Slug != "viking-world" || v.State != StateOnline || *v.Players != 3 || *v.MaxPlayers != 10 ||
		!slices.Equal(v.PlayerNames, []string{"ann", "bo", "cy"}) || v.Version != "0.218" || v.Map != "Midgard" ||
		v.Address != "play.example.com:2456" || *v.Uptime7d != 0.99 || v.Game != "Valheim" {
		t.Errorf("valheim = %+v", v)
	}
	if mc.Name != "Minecraft" || mc.Slug != "mc" || mc.State != StateStarting || mc.Players != nil || mc.Uptime7d != nil {
		t.Errorf("minecraft = %+v", mc)
	}
	if ark.State != StateOffline || ark.Slug != "viking-world-2" {
		t.Errorf("ark = %+v", ark)
	}
	if gone.Name != "Old server" || gone.State != StateOffline || gone.Game != "" {
		t.Errorf("a server that doesn't exist = %+v", gone)
	}

	// Nothing but the allow-listed fields: no server names, images,
	// addresses, settings or anything else hlm knows.
	b, _ := json.Marshal(p)
	for _, leak := range []string{"hunter2", "secret/image", "172.17.0.9", "abc123", `"valheim"`, "hidden", "mc@nas", "request", "image"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("the page has %q: %s", leak, b)
		}
	}
	var raw struct {
		Servers []map[string]any `json:"servers"`
	}
	json.Unmarshal(b, &raw)
	allowed := []string{"slug", "name", "game", "icon", "state", "players", "maxPlayers", "playerNames", "version", "map", "address", "hint", "uptime7d", "uptime30d"}
	for _, srv := range raw.Servers {
		for k := range srv {
			if !slices.Contains(allowed, k) {
				t.Errorf("field %q isn't allow-listed", k)
			}
		}
	}

	// Without players shown, neither counts nor names; names need players.
	s.Save(Settings{Enabled: true, Servers: []Entry{{Server: "valheim", ShowNames: true}}}, nil)
	p, _ = s.Page(ctx, "", now)
	if v := p.Servers[0]; v.Players != nil || v.PlayerNames != nil || v.Version != "" {
		t.Errorf("players hidden = %+v", v)
	}
	s.Save(Settings{Enabled: true, Servers: []Entry{{Server: "valheim", ShowPlayers: true}}}, nil)
	p, _ = s.Page(ctx, "", now)
	if v := p.Servers[0]; *v.Players != 3 || v.PlayerNames != nil {
		t.Errorf("names hidden = %+v", v)
	}
}

func TestCacheAndForget(t *testing.T) {
	s, srvs := setup(t)
	ctx := context.Background()
	now := time.Now()
	s.Save(Settings{Enabled: true, Servers: []Entry{{Server: "valheim"}, {Server: "ark"}}}, nil)
	p, _ := s.Page(ctx, "", now)
	srvs.list[0].State = "exited"
	if p2, _ := s.Page(ctx, "", now.Add(5*time.Second)); p2.Servers[0].State != p.Servers[0].State {
		t.Error("not cached")
	}
	if p3, _ := s.Page(ctx, "", now.Add(CacheFor+time.Second)); p3.Servers[0].State != StateOffline {
		t.Error("cached too long")
	}
	if err := s.ForgetServer("valheim"); err != nil {
		t.Fatal(err)
	}
	set, _ := s.Settings()
	if len(set.Servers) != 1 || set.Servers[0].Server != "ark" {
		t.Errorf("after forgetting: %+v", set.Servers)
	}
	if _, err := s.ServerBySlug(ctx, "", "valheim", now.Add(time.Minute)); !errors.Is(err, ErrDisabled) {
		t.Errorf("forgotten server by slug: %v", err)
	}
	if srv, err := s.ServerBySlug(ctx, "", "ark", now.Add(time.Minute)); err != nil || srv.Name != "ARK" {
		t.Errorf("ark by slug: %+v, %v", srv, err)
	}
}

func TestSaveChecks(t *testing.T) {
	s, _ := setup(t)
	for _, bad := range []Settings{
		{Sites: []Site{{Hostname: "not a host"}}},
		{Sites: []Site{{Hostname: "192.168.1.5"}}},
		{Sites: []Site{{Hostname: "a.example.com"}, {Hostname: "A.example.com"}}},
		{Title: strings.Repeat("x", 201)},
		{Servers: []Entry{{Server: "a", Hint: strings.Repeat("x", 201)}}},
	} {
		if _, err := s.Save(bad, nil); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	set, err := s.Save(Settings{Sites: []Site{{Hostname: "Status.Example.COM."}}, Servers: []Entry{{Server: "a"}, {Server: "a"}, {Server: " "}, {Server: "b@nas", ShowNames: true}}}, nil)
	if err != nil || set.Sites[0].Hostname != "status.example.com" || set.Sites[0].ID == "" || len(set.Servers) != 2 || set.Servers[1].Slug != "b" || set.Servers[1].ShowNames {
		t.Errorf("saved %+v, %v", set, err)
	}
}

func TestBadge(t *testing.T) {
	players, max := 3, 10
	got := Badge(Server{Name: "Viking <world> & co", State: StateOnline, Players: &players, MaxPlayers: &max})
	golden := "testdata/badge.svg"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.WriteFile(golden, got, 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("badge changed:\n%s", got)
	}
	if !strings.Contains(string(got), "Viking &lt;world&gt; &amp; co") || !strings.Contains(string(got), "3/10 online") {
		t.Error("not escaped, or no players")
	}
	if b := string(Badge(Server{Name: "x", State: StateOffline})); !strings.Contains(b, ">offline<") || !strings.Contains(b, "#9f9f9f") {
		t.Errorf("offline badge: %s", b)
	}
}

func TestLimiter(t *testing.T) {
	l := &Limiter{Rate: 1, Burst: 3}
	now := time.Now()
	for i := range 3 {
		if !l.Allow("a", now) {
			t.Fatalf("request %d refused", i)
		}
	}
	if l.Allow("a", now) || !l.Allow("b", now) {
		t.Error("burst not enforced per address")
	}
	if !l.Allow("a", now.Add(1100*time.Millisecond)) {
		t.Error("not refilled")
	}
}
