package console

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/rcon"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func container(name, image string, env ...string) docker.ContainerJSON {
	var c docker.ContainerJSON
	c.ID = "id-" + name
	c.Name = "/" + name
	c.Config.Image = image
	c.Config.Env = env
	c.Config.ExposedPorts = map[string]struct{}{}
	c.HostConfig.PortBindings = map[string][]docker.PortBinding{}
	c.State.Running = true
	return c
}

type fakeServers map[string]docker.ContainerJSON

func (f fakeServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	if c, ok := f[id]; ok {
		return c, nil
	}
	return docker.ContainerJSON{}, errors.New("not found")
}

type fakeDocker struct {
	mu     sync.Mutex
	stdin  []string
	execs  [][]string
	result docker.ExecResult
}

func (f *fakeDocker) WriteStdin(_ context.Context, id string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stdin = append(f.stdin, id+":"+string(data))
	return nil
}

func (f *fakeDocker) Exec(_ context.Context, id, user string, cmd []string, max int) (docker.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	return f.result, nil
}

type fakeRCON struct {
	addr   string
	broken bool
	closed bool
	reply  string
}

func (f *fakeRCON) Exec(_ context.Context, cmd string) (string, error) {
	if f.closed {
		return "", errors.New("use of closed connection")
	}
	return f.reply + cmd, nil
}
func (f *fakeRCON) Broken() bool { return f.broken }
func (f *fakeRCON) Close() error { f.closed = true; return nil }

func newService(t *testing.T, servers fakeServers) (*Service, *fakeDocker) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDocker{}
	s := New(Options{Secrets: store, Servers: servers, Docker: d, HostAddresses: func() []string { return []string{"127.0.0.1", "192.168.1.10"} }})
	return s, d
}

func openStdin(c docker.ContainerJSON) docker.ContainerJSON {
	c.Config.OpenStdin = true
	return c
}

func TestDetect(t *testing.T) {
	withPort := func(c docker.ContainerJSON, port string) docker.ContainerJSON {
		c.Config.ExposedPorts[port] = struct{}{}
		return c
	}
	stdin := container("terraria", "ryshe/terraria")
	stdin.Config.OpenStdin = true
	tests := []struct {
		name string
		c    docker.ContainerJSON
		want Settings
	}{
		{"itzg", container("mc", "itzg/minecraft-server:java21", "RCON_PASSWORD=x"), Settings{Mode: ModeExec, Exec: "rcon-cli {command}"}},
		{"itzg without rcon", withPort(container("mc", "itzg/minecraft-server", "ENABLE_RCON=false"), "25575/tcp"), Settings{}},
		{"bedrock", container("mcb", "itzg/minecraft-bedrock-server"), Settings{Mode: ModeExec, Exec: "send-command {command}"}},
		{"palworld with rcon", container("pal", "thijsvanloef/palworld-server-docker:latest"), Settings{Mode: ModeExec, Exec: "rcon-cli {command}"}},
		{"palworld rest by variable", container("pal", "thijsvanloef/palworld-server-docker:latest", "REST_API_ENABLED=true", "ADMIN_PASSWORD=pw"), Settings{Mode: ModeREST, PasswordVar: "ADMIN_PASSWORD"}},
		{"palworld rest by port", withPort(container("pal", "jammsen/palworld-dedicated-server", "ADMIN_PASSWORD=pw"), "8212/tcp"), Settings{Mode: ModeREST, PasswordVar: "ADMIN_PASSWORD"}},
		{"palworld rest off", withPort(container("pal", "thijsvanloef/palworld-server-docker", "REST_API_ENABLED=false", "ADMIN_PASSWORD=pw"), "8212/tcp"), Settings{Mode: ModeExec, Exec: "rcon-cli {command}"}},
		{"palworld rest without password", container("pal", "thijsvanloef/palworld-server-docker", "REST_API_ENABLED=true"), Settings{Mode: ModeREST}},
		{"rcon by known port", withPort(container("rust", "didstopia/rust-server", "RCON_PASSWORD=pw"), "28016/tcp"), Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD"}},
		{"rcon by port variable", container("ark", "hermsi/ark-server", "ADMIN_PASSWORD=pw", "RCON_PORT=32330"), Settings{Mode: ModeRCON, PasswordVar: "ADMIN_PASSWORD"}},
		{"password but no port", container("x", "some/game", "RCON_PASSWORD=pw"), Settings{}},
		{"game's usual port", container("mc", "ghcr.io/x/minecraft-paper", "RCON_PASSWORD=pw"), Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD"}},
		{"stdin", stdin, Settings{Mode: ModeStdin}},
		{"nothing", container("plex", "plexinc/pms-docker"), Settings{}},
		{"open input alone", openStdin(container("plex", "plexinc/pms-docker")), Settings{}},
	}
	for _, tt := range tests {
		got, why := Detect(tt.c)
		if got.Mode != tt.want.Mode || got.Exec != tt.want.Exec || got.PasswordVar != tt.want.PasswordVar {
			t.Errorf("%s: Detect = %+v (%s), want %+v", tt.name, got, why, tt.want)
		}
		if got.Mode != ModeNone && why == "" {
			t.Errorf("%s: no reason given", tt.name)
		}
	}
}

func TestGameOf(t *testing.T) {
	tests := map[string]string{
		"itzg/minecraft-server:java21":        "minecraft",
		"itzg/minecraft-bedrock-server":       "minecraft-bedrock",
		"thijsvanloef/palworld-server-docker": "palworld",
		"didstopia/rust-server":               "rust",
		"rustdesk/rustdesk-server":            "",
		"hermsi/ark-server":                   "ark",
		"linuxserver/darktable":               "",
		"ghcr.io/x/factorio:stable":           "factorio",
		"plexinc/pms-docker":                  "",
	}
	for image, want := range tests {
		if got := gameOf(image); got != want {
			t.Errorf("gameOf(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestSaveAndStatus(t *testing.T) {
	c := container("rust", "didstopia/rust-server", "RCON_PASSWORD=envpw")
	c.Config.ExposedPorts["28016/tcp"] = struct{}{}
	s, _ := newService(t, fakeServers{"rust": c})
	ctx := context.Background()

	st, err := s.Status(ctx, "rust")
	if err != nil {
		t.Fatal(err)
	}
	if st.Ready || st.Detected == nil || st.Detected.Mode != ModeRCON || st.Game != "rust" ||
		!slices.Contains(st.QuickCommands, "server.save") || !slices.Contains(st.EnvVars, "RCON_PASSWORD") {
		t.Errorf("status before setup = %+v", st)
	}

	bad := []Settings{
		{Mode: "telnet"},
		{Mode: ModeRCON, Port: 70000},
		{Mode: ModeRCON, Host: "10.0.0.5:28016", Port: 28016},
		{Mode: ModeRCON, Host: "10.0.0.5"},
		{Mode: ModeRCON, PasswordVar: "NOT VALID"},
		{Mode: ModeExec},
		{Mode: ModeExec, Exec: `rcon-cli "{command}`},
	}
	for _, b := range bad {
		if _, err := s.Save(ctx, "rust", Input{Settings: b}); err == nil {
			t.Errorf("saved %+v", b)
		}
	}

	st, err = s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, Password: "override", QuickCommands: []string{" kick bob ", "", "kick bob", "server.save"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ready || !st.HasPassword || st.Settings.Password != "" || !slices.Equal(st.Settings.QuickCommands, []string{"kick bob", "server.save"}) ||
		st.QuickCommands[len(st.QuickCommands)-1] != "kick bob" || len(st.QuickCommands) != len(gameCommands["rust"])+1 {
		t.Errorf("status after saving = %+v", st)
	}
	// A blank password keeps the saved one; ClearPassword removes it.
	if _, err := s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.settings("rust"); set.Password != "override" {
		t.Errorf("password after saving without one = %q", set.Password)
	}
	st, _ = s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD"}, ClearPassword: true})
	if set, _ := s.settings("rust"); set.Password != "" || st.HasPassword || !st.Ready {
		t.Errorf("after clearing: %+v, %+v", set, st)
	}
	// Without a password anywhere, it isn't ready.
	st, _ = s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, PasswordVar: "MISSING"}})
	if st.Ready || !strings.Contains(st.Reason, "password") {
		t.Errorf("ready without a password: %+v", st)
	}
	// Switching to exec drops the RCON settings.
	if _, err := s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeExec, Exec: "rcon-cli {command}", Password: "x"}}); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.settings("rust"); set.Password != "" || set.Exec != "rcon-cli {command}" {
		t.Errorf("exec settings = %+v", set)
	}

	if err := s.ForgetServer("rust"); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.settings("rust"); set.Mode != ModeNone {
		t.Errorf("settings after forgetting = %+v", set)
	}
}

func TestSendStdin(t *testing.T) {
	c := container("terraria", "ryshe/terraria")
	c.Config.OpenStdin = true
	closed := container("closed", "ryshe/terraria")
	stopped := container("stopped", "ryshe/terraria")
	stopped.Config.OpenStdin = true
	stopped.State.Running = false
	s, d := newService(t, fakeServers{"terraria": c, "closed": closed, "stopped": stopped})
	ctx := context.Background()
	for _, name := range []string{"terraria", "closed", "stopped"} {
		if _, err := s.Save(ctx, name, Input{Settings: Settings{Mode: ModeStdin}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.Send(ctx, "terraria", "alice", "  say hi\x1b[2J\tthere\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeStdin || res.Output != "" || len(d.stdin) != 1 || d.stdin[0] != "id-terraria:say hi[2J\tthere\n" {
		t.Errorf("result %+v, stdin %q", res, d.stdin)
	}
	if _, err := s.Send(ctx, "closed", "alice", "save"); err == nil || !strings.Contains(err.Error(), "doesn't accept input") {
		t.Errorf("container without stdin: %v", err)
	}
	if _, err := s.Send(ctx, "stopped", "alice", "save"); err == nil || !strings.Contains(err.Error(), "Start the server") {
		t.Errorf("stopped server: %v", err)
	}
	if _, err := s.Send(ctx, "terraria", "alice", strings.Repeat("a", MaxCommand+1)); err == nil {
		t.Error("over-long command sent")
	}
	if _, err := s.Send(ctx, "terraria", "alice", " \x07 "); err == nil {
		t.Error("empty command sent")
	}
}

func TestSendExec(t *testing.T) {
	c := container("mc", "itzg/minecraft-server")
	s, d := newService(t, fakeServers{"mc": c})
	ctx := context.Background()
	d.result = docker.ExecResult{Stdout: []byte("§6There are §c2§6 players online\r\n"), Stderr: []byte("warning"), ExitCode: 0}
	if _, err := s.Save(ctx, "mc", Input{Settings: Settings{Mode: ModeExec, Exec: `rcon-cli --config '/data/my rcon.yml' {command}`}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(ctx, "mc", "alice", "say hello world")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rcon-cli", "--config", "/data/my rcon.yml", "say hello world"}
	if !slices.Equal(d.execs[0], want) {
		t.Errorf("ran %q, want %q", d.execs[0], want)
	}
	if res.Output != "There are 2 players online\nwarning" {
		t.Errorf("output = %q", res.Output)
	}
	// Without the placeholder, the command goes at the end.
	if _, err := s.Save(ctx, "mc", Input{Settings: Settings{Mode: ModeExec, Exec: "mc-send-to-console"}}); err != nil {
		t.Fatal(err)
	}
	s.Send(ctx, "mc", "alice", "list")
	if !slices.Equal(d.execs[1], []string{"mc-send-to-console", "list"}) {
		t.Errorf("ran %q", d.execs[1])
	}
}

func TestSendRCON(t *testing.T) {
	c := container("rust", "didstopia/rust-server", "RCON_PASSWORD=sekrit")
	c.Config.ExposedPorts["28016/tcp"] = struct{}{}
	c.HostConfig.PortBindings["28016/tcp"] = []docker.PortBinding{{HostPort: "38016"}}
	c.NetworkSettings.Networks = map[string]docker.EndpointSettings{"bridge": {IPAddress: "172.17.0.5"}}
	s, _ := newService(t, fakeServers{"rust": c})
	ctx := context.Background()

	var mu sync.Mutex
	var tried []string
	var made []*fakeRCON
	reachable := "127.0.0.1:38016"
	s.dial = func(_ context.Context, addr, password string) (rconClient, error) {
		mu.Lock()
		defer mu.Unlock()
		tried = append(tried, addr)
		if password != "sekrit" {
			return nil, rcon.ErrAuth
		}
		if addr != reachable {
			return nil, fmt.Errorf("connection refused")
		}
		f := &fakeRCON{addr: addr, reply: "§areply to sekrit: "}
		made = append(made, f)
		return f, nil
	}
	if _, err := s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(ctx, "rust", "alice", "status")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "reply to ***: status" {
		t.Errorf("output = %q", res.Output)
	}
	if want := []string{"172.17.0.5:28016", "127.0.0.1:38016"}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v", tried, want)
	}
	// The connection is kept for the next command.
	s.Send(ctx, "rust", "alice", "players")
	if len(made) != 1 {
		t.Errorf("made %d connections for two commands", len(made))
	}
	// A broken connection is replaced, starting with the address that worked.
	made[0].broken = true
	tried = nil
	if _, err := s.Send(ctx, "rust", "alice", "save"); err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 || !made[0].closed || tried[0] != "127.0.0.1:38016" {
		t.Errorf("after a broken connection: made %d, tried %v", len(made), tried)
	}
	// A wrong password stops at the first address.
	if _, err := s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, Password: "wrong"}}); err != nil {
		t.Fatal(err)
	}
	tried = nil
	if _, err := s.Send(ctx, "rust", "alice", "status"); !errors.Is(err, rcon.ErrAuth) || len(tried) != 1 {
		t.Errorf("wrong password: %v, tried %v", err, tried)
	}
	// An address given by hand is the only one tried.
	if _, err := s.Save(ctx, "rust", Input{Settings: Settings{Mode: ModeRCON, PasswordVar: "RCON_PASSWORD", Host: "10.0.0.9", Port: 1234}}); err != nil {
		t.Fatal(err)
	}
	tried = nil
	s.Send(ctx, "rust", "alice", "status")
	if !slices.Equal(tried, []string{"10.0.0.9:1234"}) {
		t.Errorf("with a given address, tried %v", tried)
	}
}

func TestRCONAddresses(t *testing.T) {
	s, _ := newService(t, nil)
	c := container("x", "game", "RCON_PORT=25575")
	c.HostConfig.PortBindings["25575/tcp"] = []docker.PortBinding{{HostIP: "127.0.0.1", HostPort: "25580"}}
	got, err := s.rconAddresses(c, Settings{Mode: ModeRCON})
	if err != nil || !slices.Equal(got, []string{"127.0.0.1:25580"}) {
		t.Errorf("published on one address: %v, %v", got, err)
	}
	h := container("x", "game", "RCON_PORT=25575")
	h.HostConfig.NetworkMode = "host"
	got, _ = s.rconAddresses(h, Settings{Mode: ModeRCON})
	if !slices.Equal(got, []string{"127.0.0.1:25575", "192.168.1.10:25575"}) {
		t.Errorf("host networking: %v", got)
	}
	if _, err := s.rconAddresses(container("x", "game"), Settings{Mode: ModeRCON}); err == nil {
		t.Error("no port found, but no error")
	}
}

func TestRateLimit(t *testing.T) {
	c := container("t", "ryshe/terraria")
	c.Config.OpenStdin = true
	s, _ := newService(t, fakeServers{"t": c})
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	s.Save(ctx, "t", Input{Settings: Settings{Mode: ModeStdin}})
	for i := range 5 {
		if _, err := s.Send(ctx, "t", "alice", "save"); err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
	}
	if _, err := s.Send(ctx, "t", "alice", "save"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("sixth command in a second: %v", err)
	}
	if _, err := s.Send(ctx, "t", "bob", "save"); err != nil {
		t.Errorf("another user: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := s.Send(ctx, "t", "alice", "save"); err != nil {
		t.Errorf("a second later: %v", err)
	}
}

func TestSplitArgs(t *testing.T) {
	tests := map[string][]string{
		"rcon-cli {command}":         {"rcon-cli", "{command}"},
		`a "b c" 'd e'  f`:           {"a", "b c", "d e", "f"},
		`say "{command}"`:            {"say", "{command}"},
		`x --opt="a b"`:              {"x", "--opt=a b"},
		`it's"`:                      nil,
		"   ":                        nil,
		"send-command\t{command}\t ": {"send-command", "{command}"},
	}
	for in, want := range tests {
		got, err := splitArgs(in)
		if want == nil {
			if err == nil {
				t.Errorf("splitArgs(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
