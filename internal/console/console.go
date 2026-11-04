// Package console sends commands to game servers: by writing to the
// container's standard input, over RCON, or by running a command in the
// container (such as rcon-cli) for each one.
package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/rcon"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// Modes.
const (
	ModeNone  = ""
	ModeStdin = "stdin"
	ModeRCON  = "rcon"
	ModeExec  = "exec"
)

// Placeholder is replaced by the command in an exec command line.
const Placeholder = "{command}"

// MaxCommand is the longest command accepted.
const MaxCommand = 1024

// Settings say how to send a server commands. They're kept by server name,
// so they carry over when the server is updated or reconfigured.
type Settings struct {
	Mode string `json:"mode"`
	// RCON: Port is the container's RCON port; 0 finds it. Host, when set,
	// is an address to reach the port at instead (Port is then the port
	// there). The password is Password if set, or else the value of the
	// container's environment variable PasswordVar.
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	Password    string `json:"password,omitempty"` // secret
	PasswordVar string `json:"passwordVar,omitempty"`
	// Exec is a command line run in the container for each command, with
	// {command} where the command goes (or else at the end), e.g.
	// "rcon-cli {command}".
	Exec string `json:"exec,omitempty"`
	// QuickCommands are commands offered as shortcuts, besides the game's
	// own.
	QuickCommands []string `json:"quickCommands,omitempty"`
}

// Input is Settings as sent to Save: a blank Password keeps the saved one,
// unless ClearPassword is set.
type Input struct {
	Settings
	ClearPassword bool `json:"clearPassword,omitempty"`
}

// Status is what the console tab shows.
type Status struct {
	// Settings are the saved settings, without the password.
	Settings    Settings `json:"settings"`
	HasPassword bool     `json:"hasPassword"`
	// Ready says whether commands can be sent now; Reason says why not.
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
	// Detected is how this server looks like it takes commands, with
	// DetectedBecause saying why; nil if that's unclear.
	Detected        *Settings `json:"detected,omitempty"`
	DetectedBecause string    `json:"detectedBecause,omitempty"`
	// StdinOpen is set when the container accepts input (-i).
	StdinOpen bool `json:"stdinOpen"`
	Running   bool `json:"running"`
	// Game names the game the built-in quick commands are for.
	Game          string   `json:"game,omitempty"`
	QuickCommands []string `json:"quickCommands"`
	// EnvVars are the names of the container's environment variables, to
	// choose the RCON password from.
	EnvVars []string `json:"envVars"`
}

// Result is what a command returned.
type Result struct {
	Mode string `json:"mode"`
	// Output is the reply, over RCON or from the exec command. Commands
	// written to stdin have none here: their output is in the logs.
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode,omitempty"`
}

// Servers is what the service needs from the server manager.
type Servers interface {
	// Inspect returns a managed server's container.
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
}

// Docker runs commands in containers and writes to their input.
type Docker interface {
	WriteStdin(ctx context.Context, id string, data []byte) error
	Exec(ctx context.Context, id, user string, cmd []string, maxOutput int) (docker.ExecResult, error)
}

// Options configures a Service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	Docker  Docker
	// HostAddresses are where the Docker host's published ports can be
	// reached from hlm, in order of preference.
	HostAddresses func() []string
	Log           *slog.Logger
}

// Service keeps the settings and sends commands.
type Service struct {
	opts   Options
	saveMu sync.Mutex

	mu    sync.Mutex
	conns map[string]*rconConn // by container ID
	rate  map[string][]time.Time

	// dial and now are replaced in tests.
	dial func(ctx context.Context, addr, password string) (rconClient, error)
	now  func() time.Time
}

type rconClient interface {
	Exec(ctx context.Context, command string) (string, error)
	Broken() bool
	Close() error
}

type rconConn struct {
	mu       sync.Mutex
	client   rconClient
	addr     string
	password string
	lastUsed time.Time
}

const secretName = "console"

// idleClose is how long an unused RCON connection stays open.
const idleClose = 5 * time.Minute

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.HostAddresses == nil {
		o.HostAddresses = func() []string { return []string{"127.0.0.1"} }
	}
	return &Service{
		opts:  o,
		conns: map[string]*rconConn{},
		rate:  map[string][]time.Time{},
		dial: func(ctx context.Context, addr, password string) (rconClient, error) {
			return rcon.Dial(ctx, addr, password)
		},
		now: time.Now,
	}
}

func (s *Service) all() (map[string]Settings, error) {
	m := map[string]Settings{}
	_, err := s.opts.Secrets.Get(secretName, &m)
	return m, err
}

// settings returns a server's saved settings.
func (s *Service) settings(name string) (Settings, error) {
	all, err := s.all()
	if err != nil {
		return Settings{}, err
	}
	return all[name], nil
}

func serverName(info docker.ContainerJSON) string { return strings.TrimPrefix(info.Name, "/") }

// Status describes how a server takes commands.
func (s *Service) Status(ctx context.Context, id string) (Status, error) {
	info, err := s.opts.Servers.Inspect(ctx, id)
	if err != nil {
		return Status{}, err
	}
	set, err := s.settings(serverName(info))
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Settings:    set,
		HasPassword: set.Password != "",
		StdinOpen:   info.Config.OpenStdin,
		Running:     info.State.Running,
		EnvVars:     []string{},
	}
	st.Settings.Password = ""
	env := envMap(info.Config.Env)
	for k := range env {
		st.EnvVars = append(st.EnvVars, k)
	}
	slices.Sort(st.EnvVars)
	if d, why := Detect(info); d.Mode != ModeNone {
		st.Detected, st.DetectedBecause = &d, why
	}
	st.Game = gameOf(info.Config.Image)
	st.QuickCommands = slices.Clone(gameCommands[st.Game])
	for _, q := range set.QuickCommands {
		if !slices.Contains(st.QuickCommands, q) {
			st.QuickCommands = append(st.QuickCommands, q)
		}
	}
	if st.QuickCommands == nil {
		st.QuickCommands = []string{}
	}
	st.Ready, st.Reason = ready(info, set)
	return st, nil
}

// ready says whether commands can be sent to the server now, and if not,
// why not.
func ready(info docker.ContainerJSON, set Settings) (bool, string) {
	switch {
	case set.Mode == ModeNone:
		return false, "Choose how this server takes commands in the console settings."
	case !info.State.Running:
		return false, "Start the server to send it commands."
	case set.Mode == ModeStdin && !info.Config.OpenStdin:
		return false, "This server's container doesn't accept input: its Docker options turn it off (--interactive=false). Remove that, or choose another way to send commands."
	case set.Mode == ModeRCON && set.Password == "" && envMap(info.Config.Env)[set.PasswordVar] == "":
		return false, "There's no RCON password: choose the variable that holds it, or enter it, in the console settings."
	}
	return true, ""
}

var (
	envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	hostRE    = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)
)

// Save stores a server's settings.
func (s *Service) Save(ctx context.Context, id string, in Input) (Status, error) {
	info, err := s.opts.Servers.Inspect(ctx, id)
	if err != nil {
		return Status{}, err
	}
	name := serverName(info)
	set := in.Settings
	set.Host = strings.TrimSpace(set.Host)
	set.PasswordVar = strings.TrimSpace(set.PasswordVar)
	set.Exec = strings.TrimSpace(set.Exec)
	switch set.Mode {
	case ModeNone, ModeStdin:
		set = Settings{Mode: set.Mode, QuickCommands: set.QuickCommands}
	case ModeRCON:
		set.Exec = ""
		if set.Port < 0 || set.Port > 65535 {
			return Status{}, errors.New("the RCON port must be between 1 and 65535")
		}
		if set.Host != "" {
			if !hostRE.MatchString(set.Host) || strings.Count(set.Host, ":") == 1 {
				return Status{}, errors.New("the address is a host name or IP address, without a port")
			}
			if set.Port == 0 {
				return Status{}, errors.New("enter the RCON port at that address")
			}
		}
		if set.PasswordVar != "" && !envNameRE.MatchString(set.PasswordVar) {
			return Status{}, errors.New("that isn't an environment variable name")
		}
	case ModeExec:
		set.Host, set.Port, set.Password, set.PasswordVar = "", 0, "", ""
		if set.Exec == "" {
			return Status{}, errors.New("enter the command to run, like rcon-cli {command}")
		}
		if len(set.Exec) > 500 {
			return Status{}, errors.New("the command can be at most 500 characters")
		}
		if _, err := splitArgs(set.Exec); err != nil {
			return Status{}, err
		}
	default:
		return Status{}, fmt.Errorf("unknown console mode %q", set.Mode)
	}
	var quick []string
	for _, q := range set.QuickCommands {
		q = strings.TrimSpace(q)
		if q == "" || slices.Contains(quick, q) {
			continue
		}
		if len(q) > 200 {
			return Status{}, errors.New("quick commands can be at most 200 characters")
		}
		quick = append(quick, q)
	}
	if len(quick) > 30 {
		return Status{}, errors.New("there can be at most 30 quick commands")
	}
	set.QuickCommands = quick

	s.saveMu.Lock()
	all, err := s.all()
	if err == nil {
		old := all[name]
		if set.Mode == ModeRCON && set.Password == "" && !in.ClearPassword && old.Mode == ModeRCON {
			set.Password = old.Password
		}
		all[name] = set
		err = s.opts.Secrets.Set(secretName, all)
	}
	s.saveMu.Unlock()
	if err != nil {
		return Status{}, err
	}
	s.drop(info.ID) // the RCON details may have changed
	return s.Status(ctx, id)
}

// ForgetServer removes a deleted server's settings.
func (s *Service) ForgetServer(name string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all, err := s.all()
	if err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	return s.opts.Secrets.Set(secretName, all)
}

// ErrRateLimited is returned when commands come too fast.
var ErrRateLimited = errors.New("too many commands; wait a moment")

// allow reports whether who may send another command to server now: at
// most 5 a second each.
func (s *Service) allow(server, who string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := server + "\x00" + who
	now := s.now()
	recent := slices.DeleteFunc(s.rate[key], func(t time.Time) bool { return now.Sub(t) >= time.Second })
	if len(recent) >= 5 {
		s.rate[key] = recent
		return false
	}
	s.rate[key] = append(recent, now)
	if len(s.rate) > 1000 { // keep the map small
		for k, v := range s.rate {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Second {
				delete(s.rate, k)
			}
		}
	}
	return true
}

// Send sends a command to a server. who identifies the sender, for rate
// limiting and the log.
func (s *Service) Send(ctx context.Context, id, who, command string) (Result, error) {
	command = cleanCommand(command)
	if command == "" {
		return Result{}, errors.New("type a command")
	}
	if len(command) > MaxCommand {
		return Result{}, fmt.Errorf("the command is too long (at most %d bytes)", MaxCommand)
	}
	info, err := s.opts.Servers.Inspect(ctx, id)
	if err != nil {
		return Result{}, err
	}
	name := serverName(info)
	if !s.allow(name, who) {
		return Result{}, ErrRateLimited
	}
	set, err := s.settings(name)
	if err != nil {
		return Result{}, err
	}
	if ok, why := ready(info, set); !ok {
		return Result{}, errors.New(why)
	}
	s.opts.Log.Info("console command", "server", name, "by", who, "mode", set.Mode, "command", truncate(command, 200))
	res := Result{Mode: set.Mode}
	switch set.Mode {
	case ModeStdin:
		err = s.opts.Docker.WriteStdin(ctx, info.ID, []byte(command+"\n"))
	case ModeRCON:
		res.Output, err = s.sendRCON(ctx, info, set, command)
	case ModeExec:
		res.Output, res.ExitCode, err = s.exec(ctx, info, set, command)
	}
	res.Output = clean(res.Output, passwordOf(info, set))
	return res, err
}

// cleanCommand trims a command and drops control characters other than
// tabs, which could do anything on a console.
func cleanCommand(c string) string {
	c = strings.TrimSpace(c)
	return strings.Map(func(r rune) rune {
		if r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0)) {
			return r
		}
		return -1
	}, c)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mcFormat matches Minecraft's § formatting codes.
var mcFormat = regexp.MustCompile(`§[0-9a-fk-orA-FK-OR]`)

// clean tidies a reply for showing: formatting codes go, and the password,
// should a game echo it, is hidden.
func clean(out, password string) string {
	out = mcFormat.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if password != "" && len(password) >= 3 {
		out = strings.ReplaceAll(out, password, "***")
	}
	return strings.TrimRight(out, "\n ")
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func passwordOf(info docker.ContainerJSON, set Settings) string {
	if set.Mode != ModeRCON {
		return ""
	}
	if set.Password != "" {
		return set.Password
	}
	return envMap(info.Config.Env)[set.PasswordVar]
}

// --- exec ---

const maxExecOutput = 256 << 10

func (s *Service) exec(ctx context.Context, info docker.ContainerJSON, set Settings, command string) (string, int, error) {
	args, err := splitArgs(set.Exec)
	if err != nil {
		return "", 0, err
	}
	placed := false
	for i, a := range args {
		if strings.Contains(a, Placeholder) {
			args[i] = strings.ReplaceAll(a, Placeholder, command)
			placed = true
		}
	}
	if !placed {
		args = append(args, command)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.opts.Docker.Exec(ctx, info.ID, "", args, maxExecOutput)
	if err != nil {
		var apiErr *docker.APIError
		if errors.As(err, &apiErr) {
			return "", 0, fmt.Errorf("run %s: %s", args[0], apiErr.Message)
		}
		return "", 0, err
	}
	out := string(res.Stdout)
	if len(res.Stderr) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += string(res.Stderr)
	}
	if res.Truncated {
		out += "\n… (output cut short)"
	}
	return out, res.ExitCode, nil
}

// splitArgs splits a command line into arguments, as a shell would for
// plain words and single or double quotes (without expanding anything).
func splitArgs(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ' || r == '\t':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("the command has an unclosed quote")
	}
	if inArg {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("enter the command to run")
	}
	return args, nil
}

// --- RCON ---

// rconAddresses lists where a server's RCON port might be reached from hlm,
// best first.
func (s *Service) rconAddresses(info docker.ContainerJSON, set Settings) ([]string, error) {
	if set.Host != "" {
		return []string{net.JoinHostPort(set.Host, strconv.Itoa(set.Port))}, nil
	}
	port := set.Port
	if port == 0 {
		port = rconPort(info)
	}
	if port == 0 {
		return nil, errors.New("couldn't find the RCON port; enter it in the console settings")
	}
	p := strconv.Itoa(port)
	var out []string
	add := func(host string) {
		if host == "" {
			return
		}
		a := net.JoinHostPort(host, p)
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if info.HostConfig.NetworkMode == "host" {
		for _, h := range s.opts.HostAddresses() {
			add(h)
		}
		return out, nil
	}
	// Straight to the container, on each of its networks.
	nets := make([]string, 0, len(info.NetworkSettings.Networks))
	for n := range info.NetworkSettings.Networks {
		nets = append(nets, n)
	}
	slices.Sort(nets)
	for _, n := range nets {
		add(info.NetworkSettings.Networks[n].IPAddress)
	}
	// Or through the port published on the host.
	for _, b := range info.HostConfig.PortBindings[p+"/tcp"] {
		if b.HostPort == "" {
			continue
		}
		if b.HostIP != "" && b.HostIP != "0.0.0.0" && b.HostIP != "::" {
			// Published on one address only.
			if a := net.JoinHostPort(b.HostIP, b.HostPort); !slices.Contains(out, a) {
				out = append(out, a)
			}
			continue
		}
		for _, h := range s.opts.HostAddresses() {
			if h == "" {
				continue
			}
			if a := net.JoinHostPort(h, b.HostPort); !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the RCON port can't be reached: the container has no address hlm can use, and the port isn't published")
	}
	return out, nil
}

func (s *Service) drop(containerID string) {
	s.mu.Lock()
	c := s.conns[containerID]
	delete(s.conns, containerID)
	s.mu.Unlock()
	if c != nil {
		c.mu.Lock()
		if c.client != nil {
			c.client.Close()
		}
		c.mu.Unlock()
	}
}

func (s *Service) sendRCON(ctx context.Context, info docker.ContainerJSON, set Settings, command string) (string, error) {
	password := passwordOf(info, set)
	s.mu.Lock()
	c := s.conns[info.ID]
	if c == nil {
		c = &rconConn{}
		s.conns[info.ID] = c
	}
	s.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastUsed = s.now()
	if c.client != nil && (c.client.Broken() || c.password != password) {
		c.client.Close()
		c.client = nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if c.client == nil {
			client, addr, err := s.connect(ctx, info, set, password, c.addr)
			if err != nil {
				return "", err
			}
			c.client, c.addr, c.password = client, addr, password
		}
		out, err := c.client.Exec(ctx, command)
		if err == nil || out != "" || attempt == 1 || ctx.Err() != nil {
			return out, err
		}
		// The server may have closed an idle connection; try a new one.
		c.client.Close()
		c.client = nil
	}
}

// connect signs in to the server's RCON port, trying each address it
// might be at, starting with the one that worked last.
func (s *Service) connect(ctx context.Context, info docker.ContainerJSON, set Settings, password, last string) (rconClient, string, error) {
	addrs, err := s.rconAddresses(info, set)
	if err != nil {
		return nil, "", err
	}
	if i := slices.Index(addrs, last); i > 0 {
		addrs = append([]string{last}, slices.Delete(addrs, i, i+1)...)
	}
	var errs []string
	for _, a := range addrs {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		client, err := s.dial(dctx, a, password)
		cancel()
		if err == nil {
			return client, a, nil
		}
		if errors.Is(err, rcon.ErrAuth) {
			return nil, "", fmt.Errorf("%s: %w", a, err)
		}
		errs = append(errs, fmt.Sprintf("%s: %v", a, err))
	}
	return nil, "", errors.New("couldn't connect to RCON (" + strings.Join(errs, "; ") + "); is RCON turned on in the game's settings?")
}

// Test checks settings before they're saved: for RCON it signs in, for exec
// it checks the program is there, and for stdin that the container takes
// input. A blank password uses the saved one.
func (s *Service) Test(ctx context.Context, id string, in Input) (string, error) {
	info, err := s.opts.Servers.Inspect(ctx, id)
	if err != nil {
		return "", err
	}
	if !info.State.Running {
		return "", errors.New("start the server first")
	}
	set := in.Settings
	if set.Mode == ModeRCON && set.Password == "" && !in.ClearPassword {
		if old, err := s.settings(serverName(info)); err == nil && old.Mode == ModeRCON {
			set.Password = old.Password
		}
	}
	switch set.Mode {
	case ModeStdin:
		if !info.Config.OpenStdin {
			return "", errors.New("this server's container doesn't accept input: its Docker options turn it off (--interactive=false)")
		}
		return "The container accepts input. Commands appear in the console with the server's output.", nil
	case ModeRCON:
		password := passwordOf(info, set)
		if password == "" {
			return "", errors.New("there's no RCON password: choose the variable that holds it, or enter it")
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		client, addr, err := s.connect(ctx, info, set, password, "")
		if err != nil {
			return "", err
		}
		client.Close()
		return "Signed in to RCON at " + addr + ".", nil
	case ModeExec:
		args, err := splitArgs(set.Exec)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		res, err := s.opts.Docker.Exec(ctx, info.ID, "", []string{"sh", "-c", `command -v "$1"`, "sh", args[0]}, 4096)
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("%s isn't in the container", args[0])
		}
		return args[0] + " is in the container at " + strings.TrimSpace(string(res.Stdout)) + ".", nil
	}
	return "", errors.New("choose how this server takes commands")
}

// Run closes idle RCON connections until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			ids := make([]string, 0, len(s.conns))
			for id := range s.conns {
				ids = append(ids, id)
			}
			s.mu.Unlock()
			for _, id := range ids {
				s.drop(id)
			}
			return
		case <-t.C:
		}
		s.mu.Lock()
		var idle []string
		for id, c := range s.conns {
			if c.mu.TryLock() {
				if s.now().Sub(c.lastUsed) > idleClose {
					idle = append(idle, id)
				}
				c.mu.Unlock()
			}
		}
		s.mu.Unlock()
		for _, id := range idle {
			s.drop(id)
		}
	}
}
