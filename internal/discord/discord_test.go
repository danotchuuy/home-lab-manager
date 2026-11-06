package discord_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/discord"
	"github.com/danotchuuy/home-lab-manager/internal/discord/discordtest"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

const friends = "300000000000000001"

// world is the servers behind the bot.
type world struct {
	mu       sync.Mutex
	pub      []statuspage.Server
	names    []string
	started  []string
	stopped  []string
	sent     []string
	asleep   map[string]bool
	woken    []string
	startsUp bool // a started server comes online
	audit    []audit.Entry
}

func (w *world) All(ctx context.Context, now time.Time) ([]statuspage.Server, []string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]statuspage.Server(nil), w.pub...), append([]string(nil), w.names...), nil
}
func (w *world) Invalidate() {}

func (w *world) set(name, state string) {
	for i, n := range w.names {
		if n == name {
			w.pub[i].State = state
		}
	}
}

func (w *world) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	info.Config.Image = "itzg/minecraft-server"
	if id == "valheim" {
		info.Config.Image = "lloesche/valheim-server"
	}
	return info, nil
}
func (w *world) Start(ctx context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = append(w.started, id)
	if w.startsUp {
		w.set(id, statuspage.StateOnline)
	}
	return nil
}
func (w *world) Stop(ctx context.Context, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = append(w.stopped, id)
	w.set(id, statuspage.StateOffline)
	return nil
}
func (w *world) Restart(ctx context.Context, id string) error { return w.Start(ctx, id) }
func (w *world) JobRunning(string) bool                       { return false }
func (w *world) Send(ctx context.Context, id, who, command string) (console.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, id+"|"+who+"|"+command)
	return console.Result{}, nil
}
func (w *world) Record(e audit.Entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.audit = append(w.audit, e)
}

func intp(n int) *int { return &n }

func setup(t *testing.T) (*discord.Service, *discordtest.Server, *world) {
	t.Helper()
	fake := discordtest.New()
	t.Cleanup(fake.Close)
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		names: []string{"mc", "valheim", "secret-mc"},
		pub: []statuspage.Server{
			{Slug: "minecraft", Name: "Minecraft", State: statuspage.StateOnline, Players: intp(2), MaxPlayers: intp(20),
				PlayerNames: []string{"alex", "sam_*"}, Address: "mc.example.com"},
			{Slug: "valheim", Name: "Valheim", State: statuspage.StateOffline},
			{Slug: "creative", Name: "Creative", State: statuspage.StateOffline},
		},
		asleep: map[string]bool{"valheim": true}, startsUp: true,
	}
	s := discord.New(discord.Options{
		Secrets: store, Servers: w, Page: w, Console: w, Audit: w,
		Groups: func() (map[string][]string, error) {
			return map[string][]string{"survival": {"mc", "valheim"}, "private": {"secret-mc"}}, nil
		},
		Asleep: func(name string) bool { w.mu.Lock(); defer w.mu.Unlock(); return w.asleep[name] },
		Wake: func(ctx context.Context, name, why string) error {
			w.mu.Lock()
			w.woken = append(w.woken, name+"|"+why)
			w.asleep[name] = false
			w.mu.Unlock()
			return w.Start(ctx, name)
		},
		API: fake.URL, GatewayURL: fake.GatewayURL(), Poll: 10 * time.Millisecond, StartWait: time.Second,
	})
	return s, fake, w
}

func run(t *testing.T, s *discord.Service) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, ok func() bool, debug ...func()) {
	t.Helper()
	defer func() {
		if t.Failed() {
			for _, d := range debug {
				d()
			}
		}
	}()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func save(t *testing.T, s *discord.Service, set discord.Settings) {
	t.Helper()
	tok := discordtest.Token
	if _, err := s.Save(context.Background(), set, &tok); err != nil {
		t.Fatal(err)
	}
}

func connected(t *testing.T, s *discord.Service) {
	waitFor(t, "connected with commands", func() bool { st := s.State(); return st.Status == "connected" && st.Commands })
}

var nextID = 400000000000000000

// command sends a slash command and returns the bot's answers to it.
func command(t *testing.T, fake *discordtest.Server, user string, roles []string, guild, name string, opts map[string]string, wantCalls int) []discordtest.Call {
	t.Helper()
	nextID++
	id := strings.TrimSpace(strings.Repeat(" ", 0) + itoa(nextID))
	token := "tok-" + id
	var options []map[string]any
	for k, v := range opts {
		options = append(options, map[string]any{"name": k, "type": 3, "value": v})
	}
	in := map[string]any{"id": id, "application_id": discordtest.AppID, "type": 2, "token": token, "guild_id": guild,
		"member": map[string]any{"user": map[string]any{"id": userID(user), "username": user}, "roles": roles},
		"data":   map[string]any{"name": name, "options": options}}
	if guild == "" {
		delete(in, "guild_id")
		delete(in, "member")
		in["user"] = map[string]any{"id": userID(user), "username": user}
	}
	if err := fake.Interaction(in); err != nil {
		t.Fatal(err)
	}
	var got []discordtest.Call
	waitFor(t, name+" answered", func() bool {
		got = nil
		for _, c := range fake.Calls() {
			if strings.Contains(c.Path, token) {
				got = append(got, c)
			}
		}
		return len(got) >= wantCalls
	}, func() {
		for _, c := range fake.Calls() {
			t.Logf("%s %s %s", c.Method, c.Path, text(c))
		}
	})
	return got
}

// userID is a test user's Discord ID.
func userID(name string) string {
	return map[string]string{"dan": "500000000000000001", "eve": "500000000000000002", "sue": "500000000000000003"}[name]
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// text is everything a reply says.
func text(c discordtest.Call) string {
	b, _ := json.Marshal(c.Body)
	return string(b)
}

func TestConnectAndRegister(t *testing.T) {
	s, fake, _ := setup(t)
	run(t, s)
	if st := s.State(); st.Status != "off" {
		t.Fatalf("before settings: %+v", st)
	}
	save(t, s, discord.Settings{Enabled: true, GuildID: discordtest.GuildID})
	connected(t, s)
	if st := s.State(); st.Bot != "hlm" || st.ApplicationID != discordtest.AppID {
		t.Errorf("state: %+v", st)
	}
	var names []string
	for _, c := range fake.Calls() {
		if strings.HasSuffix(c.Path, "/guilds/"+discordtest.GuildID+"/commands") {
			for _, cmd := range c.List {
				names = append(names, cmd.(map[string]any)["name"].(string))
			}
		}
	}
	if strings.Join(names, ",") != "status,players,start,stop,restart,say" {
		t.Errorf("commands: %v", names)
	}
	ops := fake.Ops()
	if len(ops) == 0 || ops[0]["op"] != float64(2) || ops[0]["d"].(map[string]any)["intents"] != float64(0) {
		t.Errorf("identify: %+v", ops)
	}

	// A dropped connection resumes the session.
	fake.Drop()
	waitFor(t, "resume", func() bool {
		for _, op := range fake.Ops() {
			if op["op"] == float64(6) && op["d"].(map[string]any)["session_id"] == "session-1" {
				return true
			}
		}
		return false
	})
	if fake.Sessions() != 2 {
		t.Errorf("sessions: %d", fake.Sessions())
	}

	// Roles for the settings page, without bots' own.
	roles, err := s.Roles(context.Background())
	if err != nil || len(roles) != 2 || roles[0].Name != "Friends" {
		t.Errorf("roles: %+v, %v", roles, err)
	}

	// Turning it off takes the commands away and forgets the token.
	if err := s.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if last := calls[len(calls)-1]; last.Method != "PUT" || len(last.List) != 0 {
		t.Errorf("unregister: %+v", last)
	}
	if _, has, _ := s.Settings(); has {
		t.Error("token kept")
	}
	waitFor(t, "off", func() bool { return s.State().Status == "off" })
}

func TestBadToken(t *testing.T) {
	s, _, _ := setup(t)
	bad := "nope"
	if _, err := s.Save(context.Background(), discord.Settings{Enabled: true, GuildID: discordtest.GuildID}, &bad); err == nil ||
		!strings.Contains(err.Error(), "doesn't accept the bot token") {
		t.Errorf("bad token saved: %v", err)
	}
	for _, set := range []discord.Settings{
		{GuildID: "abc"},
		{Enabled: true},
		{GuildID: discordtest.GuildID, Rules: []discord.Rule{{RoleID: "x"}}},
		{GuildID: discordtest.GuildID, Rules: []discord.Rule{{RoleID: friends, Actions: []string{"delete"}}}},
		{GuildID: discordtest.GuildID, Rules: []discord.Rule{{RoleID: friends, Groups: []string{"nope"}}}},
	} {
		if _, err := s.Save(context.Background(), set, nil); err == nil {
			t.Errorf("saved %+v", set)
		}
	}
}

func TestCommands(t *testing.T) {
	s, fake, w := setup(t)
	run(t, s)
	save(t, s, discord.Settings{Enabled: true, GuildID: discordtest.GuildID, Rules: []discord.Rule{
		{RoleID: friends, Groups: []string{"survival"}, Actions: []string{"players", "start", "say"}},
		{RoleID: discordtest.GuildID, Actions: []string{"players"}}, // @everyone
	}})
	connected(t, s)
	g := discordtest.GuildID

	// Status: every server on the page, without addresses or names
	// (details are off), and without internal names.
	got := command(t, fake, "dan", nil, g, "status", nil, 1)
	out := text(got[0])
	if !strings.Contains(out, "Minecraft") || !strings.Contains(out, "2/20 players") || !strings.Contains(out, "Creative") {
		t.Errorf("status: %s", out)
	}
	for _, leak := range []string{"mc.example.com", "alex", "secret-mc", `"mc"`} {
		if strings.Contains(out, leak) {
			t.Errorf("status shows %q: %s", leak, out)
		}
	}

	// Players: everyone may; names stay hidden without details.
	got = command(t, fake, "dan", nil, g, "players", map[string]string{"server": "minecraft"}, 1)
	if out := text(got[0]); !strings.Contains(out, "2/20 players.") || strings.Contains(out, "alex") {
		t.Errorf("players: %s", out)
	}

	// Start: friends only, and only their groups' servers. Refusals are
	// the same for servers they can't use and ones that don't exist.
	got = command(t, fake, "eve", nil, g, "start", map[string]string{"server": "valheim"}, 1)
	refusal := text(got[0])
	if !strings.Contains(refusal, "can't start that server") || got[0].Body["data"].(map[string]any)["flags"] != float64(64) {
		t.Errorf("start without the role: %s", refusal)
	}
	got = command(t, fake, "eve", []string{friends}, g, "start", map[string]string{"server": "creative"}, 1)
	if text(got[0]) != refusal {
		t.Errorf("start outside the group: %s", text(got[0]))
	}
	got = command(t, fake, "eve", []string{friends}, g, "start", map[string]string{"server": "nonexistent"}, 1)
	if text(got[0]) != refusal {
		t.Errorf("unknown server: %s", text(got[0]))
	}
	w.mu.Lock()
	if len(w.started) != 0 {
		t.Errorf("started: %v", w.started)
	}
	w.mu.Unlock()

	// Valheim is asleep: it's woken, and the reply waits until it's up.
	got = command(t, fake, "eve", []string{friends}, g, "start", map[string]string{"server": "Valheim"}, 2)
	if got[0].Body["type"] != float64(5) || got[1].Method != "PATCH" || !strings.Contains(text(got[1]), "**Valheim** is up") {
		t.Errorf("start: %+v / %s", got[0].Body, text(got[1]))
	}
	w.mu.Lock()
	if len(w.woken) != 1 || w.woken[0] != "valheim|discord:eve" {
		t.Errorf("woken: %v", w.woken)
	}
	var startEntry *audit.Entry
	for i := range w.audit {
		if w.audit[i].Action == "servers.start" {
			startEntry = &w.audit[i]
		}
	}
	w.mu.Unlock()
	if startEntry == nil || startEntry.Actor != "discord:eve" || startEntry.ActorKind != audit.ActorDiscord || startEntry.Server != "valheim" ||
		startEntry.Details["via"] != "Discord" {
		t.Errorf("audit: %+v", startEntry)
	}

	// Stop isn't in their rule.
	got = command(t, fake, "eve", []string{friends}, g, "stop", map[string]string{"server": "minecraft"}, 1)
	if !strings.Contains(text(got[0]), "can't stop that server") {
		t.Errorf("stop: %s", text(got[0]))
	}

	// Say: the game's command, with who said it, control characters gone.
	got = command(t, fake, "sue", []string{friends}, g, "say", map[string]string{"server": "minecraft", "message": "back in 5\nstop"}, 1)
	w.mu.Lock()
	sent := append([]string(nil), w.sent...)
	w.mu.Unlock()
	if len(sent) != 1 || sent[0] != "mc|discord:500000000000000003|say [Discord] sue: back in 5 stop" {
		t.Errorf("sent: %q", sent)
	}
	if !strings.Contains(text(got[0]), "Said on **Minecraft**") {
		t.Errorf("say reply: %s", text(got[0]))
	}

	// Only in the configured server; never in direct messages.
	got = command(t, fake, "sue", []string{friends}, "", "status", nil, 1)
	if !strings.Contains(text(got[0]), "only answers in its own Discord server") {
		t.Errorf("DM: %s", text(got[0]))
	}
	got = command(t, fake, "sue", []string{friends}, "200000000000000999", "status", nil, 1)
	if !strings.Contains(text(got[0]), "only answers in its own Discord server") {
		t.Errorf("other server: %s", text(got[0]))
	}
}

func TestDetailsAndRateLimit(t *testing.T) {
	s, fake, _ := setup(t)
	run(t, s)
	save(t, s, discord.Settings{Enabled: true, GuildID: discordtest.GuildID, Details: true,
		Rules: []discord.Rule{{RoleID: discordtest.GuildID, Actions: []string{"players"}}}})
	connected(t, s)
	g := discordtest.GuildID
	got := command(t, fake, "dan", nil, g, "players", map[string]string{"server": "minecraft"}, 1)
	// Names are shown, escaped so they aren't read as markdown.
	if out := text(got[0]); !strings.Contains(out, `alex, sam\\_\\*`) {
		t.Errorf("players with details: %s", out)
	}
	got = command(t, fake, "dan", nil, g, "status", nil, 1)
	if out := text(got[0]); !strings.Contains(out, "`mc.example.com`") {
		t.Errorf("status with details: %s", out)
	}
	for range 3 {
		command(t, fake, "dan", nil, g, "status", nil, 1)
	}
	got = command(t, fake, "dan", nil, g, "status", nil, 1)
	if !strings.Contains(text(got[0]), "Slow down") {
		t.Errorf("sixth command: %s", text(got[0]))
	}
}

func TestAutocomplete(t *testing.T) {
	s, fake, _ := setup(t)
	run(t, s)
	save(t, s, discord.Settings{Enabled: true, GuildID: discordtest.GuildID, Rules: []discord.Rule{
		{RoleID: friends, Groups: []string{"survival"}, Actions: []string{"start"}},
	}})
	connected(t, s)
	ask := func(roles []string, typed string) string {
		nextID++
		id := itoa(nextID)
		fake.Interaction(map[string]any{"id": id, "application_id": discordtest.AppID, "type": 4, "token": "ac-" + id,
			"guild_id": discordtest.GuildID,
			"member":   map[string]any{"user": map[string]any{"id": "500000000000000001", "username": "eve"}, "roles": roles},
			"data": map[string]any{"name": "start", "options": []map[string]any{
				{"name": "server", "type": 3, "value": typed, "focused": true}}}})
		var out string
		waitFor(t, "autocomplete", func() bool {
			for _, c := range fake.Calls() {
				if strings.Contains(c.Path, "ac-"+id) {
					b, _ := json.Marshal(c.Body["data"])
					out = string(b)
					return true
				}
			}
			return false
		})
		return out
	}
	if got := ask([]string{friends}, ""); got != `{"choices":[{"name":"Minecraft","value":"minecraft"},{"name":"Valheim","value":"valheim"}]}` {
		t.Errorf("friends: %s", got)
	}
	if got := ask([]string{friends}, "VAL"); got != `{"choices":[{"name":"Valheim","value":"valheim"}]}` {
		t.Errorf("typed: %s", got)
	}
	if got := ask(nil, ""); got != `{"choices":[]}` {
		t.Errorf("no role: %s", got)
	}
}
