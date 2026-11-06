package players

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeServer struct {
	mu      sync.Mutex
	info    docker.ContainerJSON
	files   map[string]string
	replies map[string]string
	sent    []string
}

func (f *fakeServer) Inspect(context.Context, string) (docker.ContainerJSON, error) {
	return f.info, nil
}

func (f *fakeServer) SearchFiles(_ context.Context, _, dir, q string) ([]files.Match, bool, error) {
	var out []files.Match
	for p := range f.files {
		if strings.HasPrefix(p, dir+"/") && strings.Contains(path.Base(p), q) {
			out = append(out, files.Match{Path: p})
		}
	}
	return out, false, nil
}

func (f *fakeServer) ReadFile(_ context.Context, _, p string) (files.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	if !ok {
		return files.File{}, errors.New("no such file")
	}
	return files.File{Path: p, Content: c}, nil
}

func (f *fakeServer) WriteFile(_ context.Context, _, p, content string, _ time.Time) (files.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = content
	return files.File{Path: p, Content: content}, nil
}

func (f *fakeServer) Query(_ context.Context, _, cmd string) (string, error) {
	r, ok := f.replies[cmd]
	if !ok {
		return "", errors.New("the console isn't set up")
	}
	return r, nil
}

func (f *fakeServer) Send(_ context.Context, _, _, cmd string) (console.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, cmd)
	return console.Result{Output: "done: " + cmd}, nil
}

type notifier struct{ events []notify.Event }

func (n *notifier) Notify(e notify.Event) { n.events = append(n.events, e) }
func (n *notifier) Clear(string)          {}

func newService(t *testing.T, image string, running bool) (*Service, *fakeServer, *notifier) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{files: map[string]string{}, replies: map[string]string{}}
	f.info.ID = "abc"
	f.info.Config.Image = image
	f.info.State.Running = running
	f.info.Mounts = []docker.MountPoint{{Type: "bind", Destination: "/data"}}
	n := &notifier{}
	s := New(Options{Secrets: store, Servers: f, Console: f, Notifier: n,
		Names: func(string) []string { return []string{"from-query"} }})
	return s, f, n
}

func TestChecks(t *testing.T) {
	for _, n := range []string{"alex", "Mr.Smith_99", "Ünïcødé", "x-y"} {
		if err := CheckName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", "bob smith", "x;stop", "a\nb", "q\"uote", "it's", "back\\slash", "{id}", strings.Repeat("a", 65)} {
		if CheckName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
	for _, id := range []string{"steam_76561198000000001", "0002abcdef", "Steam_7656:1"} {
		if err := CheckID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, id := range []string{"", "a b", "x;y", "ü"} {
		if CheckID(id) == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if CheckReason("griefing the spawn, twice") != nil || CheckReason("a;b") == nil || CheckReason("line\nbreak") == nil ||
		CheckReason(strings.Repeat("x", 201)) == nil {
		t.Error("reasons")
	}
}

func TestCommand(t *testing.T) {
	for _, tt := range []struct {
		tmpl, reason, want string
	}{
		{"kick {player} {reason}", "griefing", "kick alex griefing"},
		{"kick {player} {reason}", "", "kick alex"},
		{`kick {player} "{reason}"`, "", "kick alex"},
		{`kick {player} "{reason}"`, "spam", `kick alex "spam"`},
		{"KickPlayer {id}", "ignored", "KickPlayer steam_1"},
	} {
		got, err := Command(tt.tmpl, Player{Name: "alex", ID: "steam_1"}, tt.reason)
		if err != nil || got != tt.want {
			t.Errorf("%q, %q = %q %v", tt.tmpl, tt.reason, got, err)
		}
	}
	if _, err := Command("KickPlayer {id}", Player{Name: "alex"}, ""); err == nil {
		t.Error("no ID, no error")
	}
}

func TestMinecraft(t *testing.T) {
	s, f, n := newService(t, "itzg/minecraft-server", true)
	ctx := context.Background()
	f.replies["list"] = "There are 2 of a max of 20 players online: alex, sam"
	f.files["/data/whitelist.json"] = `[{"uuid":"u1","name":"alex"},{"uuid":"u2","name":"sam"}]`
	f.files["/data/banned-players.json"] = `[{"uuid":"u3","name":"griefer","reason":"lava"}]`
	f.files["/data/world/ops.json"] = `[]`
	f.files["/data/backup/ops.json"] = `[{"uuid":"u9","name":"old"}]`
	f.files["/data/world/backup/ops.json"] = `[{"uuid":"u9","name":"older"}]`

	ov, err := s.Overview(ctx, "mc")
	if err != nil {
		t.Fatal(err)
	}
	if ov.Game != "minecraft" || len(ov.Actions) != 7 || len(ov.NeedsID) != 0 || ov.OnlineFrom != "console" ||
		len(ov.Online) != 2 || ov.Online[1].Name != "sam" {
		t.Errorf("overview = %+v", ov)
	}
	if l := ov.Lists["ban"]; len(l.Players) != 1 || l.Players[0].Reason != "lava" || l.Editable {
		t.Errorf("bans = %+v", l)
	}
	if l := ov.Lists["allow"]; len(l.Players) != 2 || l.Path != "/data/whitelist.json" {
		t.Errorf("allow = %+v", l)
	}
	// The shallowest of several files of that name.
	if l := ov.Lists["admin"]; l.Path != "/data/backup/ops.json" && l.Path != "/data/world/ops.json" {
		t.Errorf("admin = %+v", l)
	}

	if _, err := s.Act(ctx, "mc", "dan", "kick", Player{Name: "alex"}, "griefing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Act(ctx, "mc", "dan", "ban", Player{Name: "sam"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Act(ctx, "mc", "dan", "kick", Player{Name: "alex; stop"}, ""); err == nil {
		t.Error("a crafted name was sent")
	}
	if strings.Join(f.sent, " | ") != "kick alex griefing | ban sam" {
		t.Errorf("sent = %q", f.sent)
	}
	if len(n.events) != 1 || n.events[0].Kind != notify.KindPlayerBanned || !strings.Contains(n.events[0].Message, "dan banned sam on mc") {
		t.Errorf("events = %+v", n.events)
	}
	if ov, _ := s.Overview(ctx, "mc"); len(ov.Log) != 2 || ov.Log[0].Action != "ban" || ov.Log[1].Reason != "griefing" {
		t.Errorf("log = %+v", ov.Log)
	}

	// Stopped: its lists only change through the console.
	f.info.State.Running = false
	if _, err := s.Act(ctx, "mc", "dan", "allow", Player{Name: "kim"}, ""); err == nil || !strings.Contains(err.Error(), "start the server") {
		t.Errorf("stopped: %v", err)
	}
	if err := s.ForgetServer("mc"); err != nil || len(s.records("mc")) != 0 {
		t.Errorf("forget: %v", err)
	}
}

func TestPalworldIDs(t *testing.T) {
	s, f, _ := newService(t, "thijsvanloef/palworld-server-docker", true)
	ctx := context.Background()
	f.replies["ShowPlayers"] = "name,playeruid,steamid\nAlex,123,steam_76561198000000001\n"
	ov, err := s.Overview(ctx, "pal")
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Online) != 1 || ov.Online[0].ID != "steam_76561198000000001" || len(ov.NeedsID) != 3 {
		t.Errorf("overview = %+v", ov)
	}
	if _, err := s.Act(ctx, "pal", "dan", "kick", Player{Name: "Alex"}, ""); err == nil {
		t.Error("kicked without an ID")
	}
	// A name with a space is fine when only the ID goes into the command.
	if _, err := s.Act(ctx, "pal", "dan", "kick", Player{Name: "Alex the Great", ID: "steam_76561198000000001"}, ""); err != nil {
		t.Fatal(err)
	}
	if f.sent[0] != "KickPlayer steam_76561198000000001" {
		t.Errorf("sent = %q", f.sent)
	}
	if _, err := s.Act(ctx, "pal", "dan", "admin", Player{ID: "steam_1"}, ""); err == nil {
		t.Error("an action the game doesn't have")
	}

	// Without a console, names come from the status query.
	delete(f.replies, "ShowPlayers")
	if ov, _ := s.Overview(ctx, "pal"); ov.OnlineFrom != "query" || ov.Online[0].Name != "from-query" || ov.OnlineError == "" {
		t.Errorf("overview = %+v", ov)
	}
}

func TestValheimLists(t *testing.T) {
	s, f, _ := newService(t, "lloesche/valheim-server", false)
	ctx := context.Background()
	f.files["/data/adminlist.txt"] = "// List admin players ID  ONE per line\r\nSteam_76561198000000001\r\n"
	f.files["/data/bannedlist.txt"] = ""
	f.files["/data/permittedlist.txt"] = "// List permitted players ID ONE per line\n"

	ov, err := s.Overview(ctx, "vh")
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Actions) != 0 || ov.Lists["admin"].Players[0].ID != "Steam_76561198000000001" || !ov.Lists["admin"].Editable {
		t.Errorf("overview = %+v", ov)
	}
	if _, err := s.Act(ctx, "vh", "dan", "admin", Player{ID: "Steam_76561198000000002"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.files["/data/adminlist.txt"]; got != "// List admin players ID  ONE per line\r\nSteam_76561198000000001\r\nSteam_76561198000000002\r\n" {
		t.Errorf("adminlist = %q", got)
	}
	if _, err := s.Act(ctx, "vh", "dan", "admin", Player{ID: "Steam_76561198000000002"}, ""); err == nil {
		t.Error("added twice")
	}
	if _, err := s.Act(ctx, "vh", "dan", "unadmin", Player{ID: "Steam_76561198000000001"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.files["/data/adminlist.txt"]; got != "// List admin players ID  ONE per line\r\nSteam_76561198000000002\r\n" {
		t.Errorf("adminlist = %q", got)
	}
	if _, err := s.Act(ctx, "vh", "dan", "ban", Player{ID: "Steam_76561198000000003"}, ""); err != nil || f.files["/data/bannedlist.txt"] != "Steam_76561198000000003\n" {
		t.Errorf("ban: %v %q", err, f.files["/data/bannedlist.txt"])
	}
	if _, err := s.Act(ctx, "vh", "dan", "kick", Player{ID: "Steam_1"}, ""); err == nil {
		t.Error("kicked from a stopped server")
	}

	// Running: Valheim reads them at start.
	f.info.State.Running = true
	if _, err := s.Act(ctx, "vh", "dan", "allow", Player{ID: "Steam_1"}, ""); err == nil || !strings.Contains(err.Error(), "stop it") {
		t.Errorf("running: %v", err)
	}
	if _, err := s.Overview(ctx, "vh"); err != nil {
		t.Fatal(err)
	}

	// A game hlm can't manage players for.
	f.info.Config.Image = "nginx"
	if _, err := s.Overview(ctx, "vh"); !errors.Is(err, ErrNoPlayers) {
		t.Errorf("nginx: %v", err)
	}
}
