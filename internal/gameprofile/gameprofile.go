// Package gameprofile is what hlm knows about particular games, as data:
// how to recognise a game from its image, which ports answer status
// queries, its console's commands and RCON port, how to read its player
// list, and its settings files. Each game is one YAML profile; the built-in
// ones are embedded. Profiles only point hlm's own features at things (a
// port, a command, a file name, a pattern): they don't run code.
package gameprofile

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Profile is one game.
type Profile struct {
	// ID names the game (lower-case letters, digits and dashes); the
	// console reports it, and the web UI keys things by it.
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// Match are sets of image words: the profile fits an image that has all
	// the words of any one set.
	Match         [][]string     `yaml:"match" json:"match"`
	Query         Query          `yaml:"query" json:"query"`
	Console       Console        `yaml:"console" json:"console"`
	SettingsFiles []SettingsFile `yaml:"settings_files" json:"settingsFiles,omitempty"`
	// PlayerLists are the files the game keeps its allow, ban and admin
	// lists in, by list.
	PlayerLists map[string]ListFile `yaml:"player_lists" json:"playerLists,omitempty"`
	// Mods is how it takes mods.
	Mods *Mods `yaml:"mods" json:"mods,omitempty"`
	// LaunchOptions are launch arguments a server of the game can have
	// turned on or off from its page, such as a status endpoint.
	LaunchOptions []LaunchOption `yaml:"launch_options" json:"launchOptions,omitempty"`
	// ArgsVars are the variables images of the game take launch arguments
	// in (GAME_PARAMS, …), tried in order; without one of them, launch
	// options go in the server's post arguments (its command).
	ArgsVars []string `yaml:"args_vars" json:"argsVars,omitempty"`
	// GameBackups are where the game keeps its own backups, so hlm can
	// upload each new one (see backup.GameFolder).
	GameBackups []GameBackupFolder `yaml:"game_backups" json:"gameBackups,omitempty"`
	// JoinProtocols are the protocols players join the game over ("udp",
	// "tcp"). A sleeping server is only woken by a connection over one of
	// them: a game that's all UDP isn't woken by a scanner knocking on its
	// TCP ports. Empty means any.
	JoinProtocols []string `yaml:"join_protocols" json:"joinProtocols,omitempty"`

	// Source is where it came from (BuiltIn or Custom), File its file, and
	// Replaces whether a custom profile takes a built-in one's place.
	Source   string `yaml:"-" json:"source"`
	File     string `yaml:"-" json:"file,omitempty"`
	Replaces bool   `yaml:"-" json:"replaces,omitempty"`
	// Origin is where your own profile was imported from, if from a link.
	Origin *OriginInfo `yaml:"-" json:"origin,omitempty"`
	raw    []byte
}

// Where profiles come from.
const (
	BuiltIn = "built-in"
	Custom  = "custom"
)

// YAML is the profile as it was written.
func (p *Profile) YAML() []byte { return bytes.Clone(p.raw) }

// Ref names a profile, for showing which one a server uses.
type Ref struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
	// Mods says hlm can install mods for the game.
	Mods bool `json:"mods,omitempty"`
}

// Ref is the profile's Ref; nil for no profile.
func (p *Profile) Ref() *Ref {
	if p == nil {
		return nil
	}
	return &Ref{ID: p.ID, Name: p.Name, Source: p.Source, Mods: p.Mods != nil && (p.Mods.Modrinth != nil || p.Mods.Workshop != nil)}
}

// Query is how a game answers status queries.
type Query struct {
	// ConsoleFirst asks through the console (names and counts) before any
	// query port.
	ConsoleFirst bool `yaml:"console_first" json:"consoleFirst,omitempty"`
	// Protocol, if set, is the game's own status protocol ("minecraft",
	// "bedrock", or "http-status" for a JSON status page like
	// Nightingale's /status) on Port, or on the port in the variable
	// PortEnv, or on the port a launch argument PortArg gives
	// (-statusPort=7778).
	Protocol string `yaml:"protocol" json:"protocol,omitempty"`
	Port     int    `yaml:"port" json:"port,omitempty"`
	PortEnv  string `yaml:"port_env" json:"portEnv,omitempty"`
	PortArg  string `yaml:"port_arg" json:"portArg,omitempty"`
	// Ports are more container ports the protocol may answer on, tried
	// after the one above: ones some images set themselves (an image that
	// starts the game with its own -statusPort, which wins over hlm's).
	Ports []int `yaml:"ports" json:"ports,omitempty"`
	// MaxPlayers is the game's player limit, for protocols that don't say.
	MaxPlayers int `yaml:"max_players" json:"maxPlayers,omitempty"`
	// A2SPorts are the game's usual Steam query ports (container ports).
	A2SPorts []int `yaml:"a2s_ports" json:"a2sPorts,omitempty"`
}

// Console is how a game takes commands.
type Console struct {
	// RCONPort is its usual RCON port, for images that don't say.
	RCONPort int `yaml:"rcon_port" json:"rconPort,omitempty"`
	// Stdin means the server reads commands from its console.
	Stdin bool `yaml:"stdin" json:"stdin,omitempty"`
	// Commands are shortcuts; a trailing space means something goes after.
	Commands []string `yaml:"commands" json:"commands,omitempty"`
	// Say and Save are the commands scheduled tasks use to warn players and
	// to save the world.
	Say  string `yaml:"say" json:"say,omitempty"`
	Save string `yaml:"save" json:"save,omitempty"`
	// SaveBeforeStop sends Save before hlm stops, restarts or recreates
	// the server, for games that don't save when they're told to stop.
	SaveBeforeStop bool `yaml:"save_before_stop" json:"saveBeforeStop,omitempty"`
	// Exec are images that bring a command that sends commands for you.
	Exec []Exec `yaml:"exec" json:"exec,omitempty"`
	// REST is a built-in console driver that replaces RCON when the image
	// has it.
	REST *REST `yaml:"rest" json:"rest,omitempty"`
	// Players reads the player list through the console.
	Players *Players `yaml:"players" json:"players,omitempty"`
	// Actions are the commands that manage players, by action (Actions
	// lists them): {player} is the player's name, {id} their ID, {reason}
	// an optional reason.
	Actions map[string]string `yaml:"actions" json:"actions,omitempty"`
}

// Player actions, and the list each one changes.
const (
	ActionKick     = "kick"
	ActionBan      = "ban"
	ActionUnban    = "unban"
	ActionAllow    = "allow"
	ActionDisallow = "disallow"
	ActionAdmin    = "admin"
	ActionUnadmin  = "unadmin"
)

// Actions are the player actions, in the order they're shown.
var Actions = []string{ActionKick, ActionBan, ActionUnban, ActionAllow, ActionDisallow, ActionAdmin, ActionUnadmin}

// Player lists.
const (
	ListAllow = "allow"
	ListBan   = "ban"
	ListAdmin = "admin"
)

// ActionList is the list an action adds to (or, with remove, takes off).
func ActionList(action string) (list string, remove bool) {
	switch action {
	case ActionBan, ActionUnban:
		return ListBan, action == ActionUnban
	case ActionAllow, ActionDisallow:
		return ListAllow, action == ActionDisallow
	case ActionAdmin, ActionUnadmin:
		return ListAdmin, action == ActionUnadmin
	}
	return "", false
}

// List formats.
const (
	// FormatMinecraftJSON is Minecraft's whitelist.json, ops.json and
	// banned-players.json: an array of objects with name (and reason).
	FormatMinecraftJSON = "minecraft-json"
	// FormatLines is one name or ID per line; // starts a comment.
	FormatLines = "lines"
)

// GameBackupFolder is a folder a game keeps its own backups in. Path is
// relative to a data folder of the server (images mount the game's files
// in different places); * stands for one folder's name, such as a world's
// ID. Match, if set, picks the backups out by name, for games that keep
// them beside the live save.
type GameBackupFolder struct {
	Path  string `yaml:"path" json:"path"`
	Match string `yaml:"match" json:"match,omitempty"`
	// Note says what's in it, for the person choosing.
	Note string `yaml:"note" json:"note,omitempty"`
}

// ListFile is a file a game keeps a player list in.
type ListFile struct {
	File   string `yaml:"file" json:"file"`
	Format string `yaml:"format" json:"format"`
	// Prefer picks among several files of that name, as for settings
	// files.
	Prefer string `yaml:"prefer" json:"prefer,omitempty"`
}

var placeholderRE = regexp.MustCompile(`\{[a-z]+\}`)

// Exec is an image's own command sender.
type Exec struct {
	// Image is part of the image's name, e.g. itzg/minecraft-server.
	Image string `yaml:"image" json:"image"`
	// Command runs in the container; {command} is the command sent.
	Command string `yaml:"command" json:"command"`
	Why     string `yaml:"why" json:"why"`
	// OffWhen are variables (and values, any case) that turn it off.
	OffWhen map[string]string `yaml:"off_when" json:"offWhen,omitempty"`
}

// Off reports whether the container's variables turn the sender off.
func (e Exec) Off(env map[string]string) bool {
	for k, v := range e.OffWhen {
		if strings.EqualFold(env[k], v) {
			return true
		}
	}
	return false
}

// REST is a built-in REST console driver.
type REST struct {
	// Driver names it; "palworld" is the only one.
	Driver   string   `yaml:"driver" json:"driver"`
	Commands []string `yaml:"commands" json:"commands,omitempty"`
}

// Drivers are the built-in REST console drivers.
var Drivers = []string{"palworld"}

// SettingsFile is a game's usual settings file.
type SettingsFile struct {
	Names []string `yaml:"names" json:"names"`
	// Prefer picks among several files of that name: the one whose path
	// contains it.
	Prefer string `yaml:"prefer" json:"prefer,omitempty"`
	// Words, if set, also have to be in the image's words (e.g. a mod's
	// own file).
	Words []string `yaml:"words" json:"words,omitempty"`
}

var (
	idRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	wordRE = regexp.MustCompile(`[a-z0-9]+`)
	envRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

	goTypeRE = regexp.MustCompile(` in type [\w.*\[\]]+`)
)

// Words splits an image reference into lower-case words: its repository
// name and tag (ich777's images name the game in the tag, as in
// steamcmd:valheim), without the registry and owner.
func Words(image string) []string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return wordRE.FindAllString(strings.ToLower(image), -1)
}

func validPort(p int) bool { return p > 0 && p < 65536 }

// Validate checks a profile and compiles its patterns.
func (p *Profile) Validate() error {
	if !idRE.MatchString(p.ID) {
		return fmt.Errorf("id %q: use lower-case letters, digits and dashes", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("give it a name")
	}
	if len(p.Match) == 0 {
		return errors.New("match needs at least one set of image words")
	}
	for _, set := range p.Match {
		if len(set) == 0 {
			return errors.New("a match set is empty")
		}
		for _, w := range set {
			if got := wordRE.FindAllString(w, -1); len(got) != 1 || got[0] != w {
				return fmt.Errorf("match word %q: one lower-case word of letters and digits", w)
			}
		}
	}
	q := p.Query
	switch q.Protocol {
	case "":
		if q.Port != 0 || q.PortEnv != "" || q.PortArg != "" {
			return errors.New("query: port, port_env and port_arg go with a protocol")
		}
	case "minecraft", "bedrock":
		if !validPort(q.Port) {
			return fmt.Errorf("query: %s needs its port", q.Protocol)
		}
	case "http-status":
		if !validPort(q.Port) && q.PortArg == "" {
			return errors.New("query: http-status needs its port, or the launch argument that sets it (port_arg)")
		}
		if q.Port != 0 && !validPort(q.Port) {
			return fmt.Errorf("query: port %d", q.Port)
		}
	default:
		return fmt.Errorf("query: unknown protocol %q (minecraft, bedrock or http-status)", q.Protocol)
	}
	if q.PortArg != "" && !argRE.MatchString(q.PortArg) {
		return fmt.Errorf("query: port_arg %q should be a launch argument such as -statusPort", q.PortArg)
	}
	if q.MaxPlayers < 0 {
		return errors.New("query: max_players can't be negative")
	}
	for _, port := range q.Ports {
		if !validPort(port) {
			return fmt.Errorf("query: ports: %d", port)
		}
	}
	if len(q.Ports) > 0 && q.Protocol == "" {
		return errors.New("query: ports go with a protocol")
	}
	if err := p.validateLaunchOptions(); err != nil {
		return err
	}
	for _, g := range p.GameBackups {
		if g.Path == "" || strings.HasPrefix(g.Path, "/") || slices.Contains(strings.Split(g.Path, "/"), "..") {
			return fmt.Errorf("game_backups: %q should be a path inside a data folder, such as Pal/Saved/SaveGames/0/*/backup/world", g.Path)
		}
		if g.Match != "" {
			if _, err := path.Match(g.Match, ""); err != nil || strings.Contains(g.Match, "/") {
				return fmt.Errorf("game_backups: match %q isn't a name pattern", g.Match)
			}
		}
	}
	for _, proto := range p.JoinProtocols {
		if proto != "udp" && proto != "tcp" {
			return fmt.Errorf("join_protocols: %q should be udp or tcp", proto)
		}
	}
	if q.PortEnv != "" && !envRE.MatchString(q.PortEnv) {
		return fmt.Errorf("query: port_env %q isn't a variable name", q.PortEnv)
	}
	for _, port := range q.A2SPorts {
		if !validPort(port) {
			return fmt.Errorf("query: a2s port %d", port)
		}
	}
	c := p.Console
	if c.SaveBeforeStop && strings.TrimSpace(c.Save) == "" {
		return errors.New("console: save_before_stop needs the save command")
	}
	if c.RCONPort != 0 && !validPort(c.RCONPort) {
		return fmt.Errorf("console: rcon_port %d", c.RCONPort)
	}
	for _, e := range c.Exec {
		if strings.TrimSpace(e.Image) == "" || !strings.Contains(e.Command, "{command}") {
			return errors.New("console: an exec entry needs an image and a command with {command} in it")
		}
		for k := range e.OffWhen {
			if !envRE.MatchString(k) {
				return fmt.Errorf("console: off_when %q isn't a variable name", k)
			}
		}
	}
	if c.REST != nil && !slices.Contains(Drivers, c.REST.Driver) {
		return fmt.Errorf("console: unknown REST driver %q (%s)", c.REST.Driver, strings.Join(Drivers, ", "))
	}
	if c.Players != nil {
		if err := c.Players.compile(); err != nil {
			return fmt.Errorf("console: players: %w", err)
		}
	}
	for a, cmd := range c.Actions {
		if !slices.Contains(Actions, a) {
			return fmt.Errorf("console: unknown action %q (%s)", a, strings.Join(Actions, ", "))
		}
		if !strings.Contains(cmd, "{player}") && !strings.Contains(cmd, "{id}") {
			return fmt.Errorf("console: action %s needs {player} or {id} in it", a)
		}
		for _, ph := range placeholderRE.FindAllString(cmd, -1) {
			if ph != "{player}" && ph != "{id}" && ph != "{reason}" {
				return fmt.Errorf("console: action %s: unknown %s ({player}, {id} or {reason})", a, ph)
			}
		}
		if strings.ContainsAny(cmd, "\r\n") {
			return fmt.Errorf("console: action %s is more than one line", a)
		}
	}
	for l, f := range p.PlayerLists {
		if l != ListAllow && l != ListBan && l != ListAdmin {
			return fmt.Errorf("player_lists: unknown list %q (allow, ban or admin)", l)
		}
		if f.File == "" || strings.ContainsAny(f.File, "/\\") {
			return fmt.Errorf("player_lists: %s: %q isn't a file name", l, f.File)
		}
		if f.Format != FormatMinecraftJSON && f.Format != FormatLines {
			return fmt.Errorf("player_lists: %s: unknown format %q (minecraft-json or lines)", l, f.Format)
		}
	}
	if p.Mods != nil {
		if err := p.Mods.validate(); err != nil {
			return fmt.Errorf("mods: %w", err)
		}
	}
	for _, f := range p.SettingsFiles {
		if len(f.Names) == 0 {
			return errors.New("settings_files: an entry needs names")
		}
		for _, n := range f.Names {
			if n == "" || strings.ContainsAny(n, "/\\") {
				return fmt.Errorf("settings_files: %q isn't a file name", n)
			}
		}
	}
	return nil
}

// Parse reads one profile (YAML), refusing fields it doesn't know.
func Parse(data []byte) (*Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Profile
	if err := dec.Decode(&p); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			// "line 4: field players not found", without Go's type names.
			msgs := make([]string, len(te.Errors))
			for i, m := range te.Errors {
				msgs[i] = goTypeRE.ReplaceAllString(m, "")
			}
			return nil, errors.New(strings.Join(msgs, "; "))
		}
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p.raw = bytes.Clone(data)
	return &p, nil
}

// MaxSize is the largest profile file read.
const MaxSize = 64 << 10

// LoadError is a profile file that couldn't be used.
type LoadError struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// read parses every *.yaml in a directory of fsys, marking them as from
// source. Files that can't be used are returned as errors beside the rest;
// of profiles with the same id, the first file (by name) is kept.
func read(fsys fs.FS, dir, source string) ([]*Profile, []LoadError, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, err
	}
	var (
		out  []*Profile
		errs []LoadError
	)
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".yaml" {
			continue
		}
		fail := func(err error) { errs = append(errs, LoadError{File: e.Name(), Error: err.Error()}) }
		if info, err := e.Info(); err != nil {
			fail(err)
			continue
		} else if info.Size() > MaxSize {
			fail(fmt.Errorf("larger than %d KiB", MaxSize>>10))
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			fail(err)
			continue
		}
		p, err := Parse(data)
		if err != nil {
			fail(err)
			continue
		}
		if i := slices.IndexFunc(out, func(o *Profile) bool { return o.ID == p.ID }); i >= 0 {
			fail(fmt.Errorf("%s already has the id %q", out[i].File, p.ID))
			continue
		}
		p.Source, p.File = source, e.Name()
		out = append(out, p)
	}
	return out, errs, nil
}

// Set is a set of profiles.
type Set struct {
	profiles []*Profile
}

// Load reads every *.yaml in a directory of fsys as built-in profiles.
// A file that can't be used, or two with the same id, are an error.
func Load(fsys fs.FS, dir string) (*Set, error) {
	ps, errs, err := read(fsys, dir, BuiltIn)
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", errs[0].File, errs[0].Error)
	}
	return newSet(ps), nil
}

func newSet(ps []*Profile) *Set {
	ps = slices.Clone(ps)
	slices.SortFunc(ps, func(a, b *Profile) int { return strings.Compare(a.ID, b.ID) })
	return &Set{profiles: ps}
}

// Merge is base with custom profiles added; a custom profile replaces the
// base one with the same id.
func Merge(base *Set, custom []*Profile) *Set {
	var ps []*Profile
	for _, p := range base.profiles {
		if !slices.ContainsFunc(custom, func(c *Profile) bool { return c.ID == p.ID }) {
			ps = append(ps, p)
		}
	}
	for _, c := range custom {
		cp := *c
		cp.Replaces = base.ByID(c.ID) != nil
		ps = append(ps, &cp)
	}
	return newSet(ps)
}

// All are the profiles, by id.
func (s *Set) All() []*Profile { return s.profiles }

// ByID finds a profile.
func (s *Set) ByID(id string) *Profile {
	for _, p := range s.profiles {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Match finds the profile for an image: among those with a match set whose
// words are all in the image's, the one with the most words (Minecraft
// Bedrock's [minecraft, bedrock] before Minecraft's [minecraft]); nil if
// none fits.
func (s *Set) Match(image string) *Profile {
	ws := Words(image)
	var best *Profile
	bestLen := 0
	for _, p := range s.profiles {
		for _, set := range p.Match {
			if len(set) > bestLen && !slices.ContainsFunc(set, func(w string) bool { return !slices.Contains(ws, w) }) {
				best, bestLen = p, len(set)
			}
		}
	}
	return best
}

//go:embed profiles/*.yaml
var builtin embed.FS

var (
	defaultOnce sync.Once
	defaultSet  *Set
)

// Default is the built-in profiles. They're checked by the tests, so a bad
// one can't be released; it panics if one is broken anyway.
func Default() *Set {
	defaultOnce.Do(func() {
		s, err := Load(builtin, "profiles")
		if err != nil {
			panic("built-in game profiles: " + err.Error())
		}
		defaultSet = s
	})
	return defaultSet
}

var current atomic.Pointer[Set]

// Current is the profiles in use: the built-in ones and, once a Store has
// loaded them, your own.
func Current() *Set {
	if s := current.Load(); s != nil {
		return s
	}
	return Default()
}

// Use makes s the profiles in use; nil goes back to the built-in ones.
func Use(s *Set) { current.Store(s) }

// Match is Current().Match.
func Match(image string) *Profile { return Current().Match(image) }

// LaunchOption is a launch argument (or a few) that a server of the game
// can have turned on or off, such as a status endpoint. Turning it on adds
// Args to the server's launch arguments and, with a Port, gives the server
// that port, which is never forwarded or relayed to the internet.
type LaunchOption struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
	// Args are added to the launch arguments; {port} is Port.
	Args string `yaml:"args" json:"args"`
	// Port, if set, is the port (in the container) it opens, over Proto
	// (tcp unless udp).
	Port  int    `yaml:"port" json:"port,omitempty"`
	Proto string `yaml:"proto" json:"proto,omitempty"`
}

// argRE matches a launch argument's name, such as -statusPort or --port.
var argRE = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9_.:-]*$`)

// Rendered is the option's arguments, with its port.
func (o LaunchOption) Rendered() string {
	return strings.ReplaceAll(o.Args, "{port}", strconv.Itoa(o.Port))
}

// Protocol is the option's port's protocol.
func (o LaunchOption) Protocol() string {
	if o.Proto == "udp" {
		return "udp"
	}
	return "tcp"
}

// LaunchOption finds one of the profile's launch options by ID.
func (p *Profile) LaunchOption(id string) (LaunchOption, bool) {
	for _, o := range p.LaunchOptions {
		if o.ID == id {
			return o, true
		}
	}
	return LaunchOption{}, false
}

func (p *Profile) validateLaunchOptions() error {
	seen := map[string]bool{}
	for _, o := range p.LaunchOptions {
		switch {
		case !idRE.MatchString(o.ID):
			return fmt.Errorf("launch option id %q: use lower-case letters, digits and dashes", o.ID)
		case seen[o.ID]:
			return fmt.Errorf("launch option %q is there twice", o.ID)
		case strings.TrimSpace(o.Name) == "":
			return fmt.Errorf("launch option %q needs a name", o.ID)
		case strings.TrimSpace(o.Args) == "":
			return fmt.Errorf("launch option %q needs its args", o.ID)
		case o.Port != 0 && !validPort(o.Port):
			return fmt.Errorf("launch option %q: port %d", o.ID, o.Port)
		case strings.Contains(o.Args, "{port}") && o.Port == 0:
			return fmt.Errorf("launch option %q uses {port} but has no port", o.ID)
		case o.Proto != "" && o.Proto != "tcp" && o.Proto != "udp":
			return fmt.Errorf("launch option %q: proto %q (tcp or udp)", o.ID, o.Proto)
		}
		seen[o.ID] = true
	}
	for _, v := range p.ArgsVars {
		if !envRE.MatchString(v) {
			return fmt.Errorf("args_vars: %q isn't a variable name", v)
		}
	}
	return nil
}
