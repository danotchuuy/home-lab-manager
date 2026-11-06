// Package discord lets people in a Discord server check on hlm's game
// servers, and start, stop, restart and message them, with slash commands
// to a bot.
//
// hlm connects out to Discord's gateway, so nothing needs to reach hlm
// from the internet. Discord sees only the servers on the status page,
// shown as the page shows them; what each Discord role may do, and on which
// server groups, is set by an admin. Everything else is refused.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

const secretName = "discord"

// Actions a rule can allow. Status is allowed to everyone in the Discord
// server.
const (
	ActionPlayers = "players"
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
	ActionSay     = "say"
)

// Actions are the actions, in the order they're shown.
var Actions = []string{ActionPlayers, ActionStart, ActionStop, ActionRestart, ActionSay}

// Settings are the bot's settings.
type Settings struct {
	Enabled bool `json:"enabled"`
	// Guilds are the Discord servers the bot answers in, each with the
	// game servers it sees.
	Guilds []Guild `json:"guilds"`
	// Token is the bot's token; the API never sends it.
	Token string `json:"token,omitempty"`
}

// Guild is a Discord server the bot answers in.
type Guild struct {
	ID string `json:"id"`
	// Name is what it's called here.
	Name string `json:"name,omitempty"`
	// Groups are the hlm server groups whose servers (of those on the
	// status page) it sees; none means every server on the page.
	Groups []string `json:"groups"`
	// Details shows addresses and who's playing, as the status page does
	// behind its details password.
	Details bool `json:"details"`
	// Rules say what each of its roles may do.
	Rules []Rule `json:"rules"`
}

// Rule lets a Discord role do things to some servers.
type Rule struct {
	// RoleID is the Discord role; the Discord server's own ID is
	// @everyone.
	RoleID string `json:"roleId"`
	// RoleName is the role's name when the rule was saved, for showing.
	RoleName string `json:"roleName,omitempty"`
	// Groups narrow it to some hlm server groups; none means every server
	// the Discord server sees.
	Groups  []string `json:"groups"`
	Actions []string `json:"actions"`
}

// guild finds one of the Discord servers by ID.
func (set Settings) guild(id string) (Guild, bool) {
	for _, g := range set.Guilds {
		if g.ID == id {
			return g, true
		}
	}
	return Guild{}, false
}

// State is how the connection is.
type State struct {
	// Status is off, connecting, connected or error.
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	Bot           string    `json:"bot,omitempty"`
	ApplicationID string    `json:"applicationId,omitempty"`
	Since         time.Time `json:"since,omitzero"`
	// Guilds say whether each Discord server has the slash commands.
	Guilds []GuildState `json:"guilds,omitempty"`
}

// GuildState is how one Discord server is.
type GuildState struct {
	ID string `json:"id"`
	// Name is its name in Discord, when the bot can see it.
	Name     string `json:"name,omitempty"`
	Commands bool   `json:"commands"`
	Error    string `json:"error,omitempty"`
}

// Servers is what the bot needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	JobRunning(name string) bool
}

// Page is the status page's view of the servers.
type Page interface {
	All(ctx context.Context, now time.Time) ([]statuspage.Server, []string, error)
	Invalidate()
}

// Console sends commands.
type Console interface {
	Send(ctx context.Context, id, who, command string) (console.Result, error)
}

// Options configures the service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	Page    Page
	Console Console
	// Groups lists the server groups' servers by group ID, with "@host"
	// for hosts a group holds whole.
	Groups func() (map[string][]string, error)
	// Asleep says whether hlm stopped a server while nobody played, and
	// Wake starts it again.
	Asleep func(server string) bool
	Wake   func(ctx context.Context, server, why string) error
	Audit  audit.Recorder
	Log    *slog.Logger
	// API and GatewayURL replace Discord's (tests); HTTP is the client.
	API        string
	GatewayURL string
	HTTP       *http.Client
	// Poll and StartWait are how often and how long a started server is
	// watched until it answers (5s, 10 minutes).
	Poll      time.Duration
	StartWait time.Duration
	Now       func() time.Time
}

// Service runs the bot.
type Service struct {
	opts Options

	mu      sync.Mutex
	state   State
	changed chan struct{}
	rate    map[string][]time.Time // user → recent commands
}

// New creates the service; Run connects.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.API == "" {
		o.API = DefaultAPI
	}
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}
	if o.StartWait == 0 {
		o.StartWait = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{opts: o, state: State{Status: "off"}, changed: make(chan struct{}, 1), rate: map[string][]time.Time{}}
}

func (s *Service) client(token string) *Client {
	return &Client{API: s.opts.API, Token: token, HTTP: s.opts.HTTP}
}

func (s *Service) settings() (Settings, error) {
	var set Settings
	_, err := s.opts.Secrets.Get(secretName, &set)
	if set.Guilds == nil {
		set.Guilds = []Guild{}
	}
	return set, err
}

// Settings are the settings, without the token; hasToken says there is
// one.
func (s *Service) Settings() (set Settings, hasToken bool, err error) {
	set, err = s.settings()
	hasToken = set.Token != ""
	set.Token = ""
	return set, hasToken, err
}

// State is the connection's state.
func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Service) setState(f func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.state)
}

// Limits on the settings.
const (
	maxGuilds   = 20
	maxRules    = 50
	maxTokenLen = 200
)

// Save checks and stores the settings. token, if not nil, replaces the
// bot's token (and is checked with Discord); the rest of in replaces the
// settings. The bot reconnects.
func (s *Service) Save(ctx context.Context, in Settings, token *string) (Settings, error) {
	cur, err := s.settings()
	if err != nil {
		return Settings{}, err
	}
	in.Token = cur.Token
	if token != nil {
		t := strings.TrimSpace(*token)
		t = strings.TrimPrefix(t, "Bot ")
		if len(t) > maxTokenLen || strings.ContainsAny(t, " \t\r\n") {
			return Settings{}, errors.New("that doesn't look like a bot token")
		}
		if t != "" {
			if _, err := s.client(t).Me(ctx); err != nil {
				return Settings{}, err
			}
		}
		in.Token = t
	}
	if len(in.Guilds) > maxGuilds {
		return Settings{}, fmt.Errorf("at most %d Discord servers", maxGuilds)
	}
	if in.Guilds == nil {
		in.Guilds = []Guild{}
	}
	if in.Enabled && (in.Token == "" || len(in.Guilds) == 0) {
		return Settings{}, errors.New("the bot needs its token and at least one Discord server")
	}
	var groups map[string][]string
	checkGroups := func(where string, ids []string) error {
		if len(ids) == 0 || s.opts.Groups == nil {
			return nil
		}
		if groups == nil {
			var err error
			if groups, err = s.opts.Groups(); err != nil {
				return err
			}
		}
		for _, g := range ids {
			if _, ok := groups[g]; !ok {
				return fmt.Errorf("%s: there's no server group %q", where, g)
			}
		}
		return nil
	}
	seen := map[string]bool{}
	for gi := range in.Guilds {
		g := &in.Guilds[gi]
		g.ID = strings.TrimSpace(g.ID)
		if !ValidID(g.ID) {
			return Settings{}, errors.New("a Discord server ID is a number: in Discord, turn on Developer Mode, then right-click the server and Copy Server ID")
		}
		if seen[g.ID] {
			return Settings{}, fmt.Errorf("Discord server %s is there twice", g.ID)
		}
		seen[g.ID] = true
		g.Name = strings.TrimSpace(g.Name)
		if len(g.Name) > 100 {
			g.Name = g.Name[:100]
		}
		where := cmpOr(g.Name, g.ID)
		if g.Groups == nil {
			g.Groups = []string{}
		}
		if err := checkGroups(where, g.Groups); err != nil {
			return Settings{}, err
		}
		if len(g.Rules) > maxRules {
			return Settings{}, fmt.Errorf("%s: at most %d rules", where, maxRules)
		}
		if g.Rules == nil {
			g.Rules = []Rule{}
		}
		roles := map[string]bool{}
		for i := range g.Rules {
			r := &g.Rules[i]
			if !ValidID(r.RoleID) {
				return Settings{}, fmt.Errorf("%s, rule %d: %q isn't a Discord role ID", where, i+1, r.RoleID)
			}
			if roles[r.RoleID] {
				return Settings{}, fmt.Errorf("%s: two rules for the same role", where)
			}
			roles[r.RoleID] = true
			r.RoleName = strings.TrimSpace(r.RoleName)
			if len(r.RoleName) > 100 {
				r.RoleName = r.RoleName[:100]
			}
			for _, a := range r.Actions {
				if !slices.Contains(Actions, a) {
					return Settings{}, fmt.Errorf("%s, rule %d: unknown action %q", where, i+1, a)
				}
			}
			slices.Sort(r.Actions)
			r.Actions = slices.Compact(r.Actions)
			if r.Groups == nil {
				r.Groups = []string{}
			}
			if err := checkGroups(fmt.Sprintf("%s, rule %d", where, i+1), r.Groups); err != nil {
				return Settings{}, err
			}
		}
	}
	if err := s.opts.Secrets.Set(secretName, in); err != nil {
		return Settings{}, err
	}
	s.poke()
	in.Token = ""
	return in, nil
}

// Disconnect takes the bot's commands out of the Discord server, forgets
// the token and turns the bot off.
func (s *Service) Disconnect(ctx context.Context) error {
	set, err := s.settings()
	if err != nil {
		return err
	}
	var cleanup error
	if set.Token != "" && len(set.Guilds) > 0 {
		c := s.client(set.Token)
		app := s.State().ApplicationID
		if app == "" {
			if a, err := c.App(ctx); err == nil {
				app = a.ID
			} else {
				cleanup = err
			}
		}
		if app != "" {
			var failed []string
			for _, g := range set.Guilds {
				if err := c.SetCommands(ctx, app, g.ID, nil); err != nil {
					failed = append(failed, cmpOr(g.Name, g.ID))
					cleanup = err
				}
			}
			if cleanup != nil {
				cleanup = fmt.Errorf("%s: %w", strings.Join(failed, ", "), cleanup)
			}
		}
	}
	set.Token, set.Enabled = "", false
	if err := s.opts.Secrets.Set(secretName, set); err != nil {
		return err
	}
	s.poke()
	if cleanup != nil {
		return fmt.Errorf("the bot is off and its token forgotten, but its commands couldn't be removed from every Discord server: %w", cleanup)
	}
	return nil
}

// Roles lists one of the Discord servers' roles, for choosing rules.
func (s *Service) Roles(ctx context.Context, guild string) ([]Role, error) {
	set, err := s.settings()
	if err != nil {
		return nil, err
	}
	if set.Token == "" {
		return nil, errors.New("save the bot's token first")
	}
	if _, ok := set.guild(guild); !ok {
		return nil, errors.New("save that Discord server first")
	}
	rs, err := s.client(set.Token).Roles(ctx, guild)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound) {
			return nil, errors.New("the bot isn't in that Discord server yet: open its invite link")
		}
		return nil, err
	}
	// Bots' own roles can't be given to people.
	rs = slices.DeleteFunc(rs, func(r Role) bool { return r.Managed })
	slices.SortFunc(rs, func(a, b Role) int { return b.Position - a.Position })
	return rs, nil
}

// poke makes Run pick up new settings.
func (s *Service) poke() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Run keeps the bot connected while it's turned on, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		set, err := s.settings()
		if err != nil || !set.Enabled || set.Token == "" || len(set.Guilds) == 0 {
			s.setState(func(st *State) { *st = State{Status: "off"} })
			select {
			case <-ctx.Done():
				return
			case <-s.changed:
				continue
			}
		}
		connCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.connectLoop(connCtx, set)
		}()
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-s.changed:
			cancel()
			<-done
		}
	}
}

// connectLoop connects and reconnects, backing off, until ctx ends or the
// gateway says connecting again won't help.
func (s *Service) connectLoop(ctx context.Context, set Settings) {
	c := s.client(set.Token)
	g := &gateway{client: c, url: s.opts.GatewayURL, log: s.opts.Log}
	g.onReady = func(r Ready) {
		s.setState(func(st *State) {
			*st = State{Status: "connected", Bot: r.User.Display(), ApplicationID: r.Application.ID, Since: s.opts.Now()}
		})
		s.opts.Log.Info("discord: connected", "bot", r.User.Username)
		go s.register(ctx, c, r.Application.ID, set.Guilds)
	}
	g.onInteraction = func(in Interaction) { s.handle(ctx, c, in) }
	backoff := time.Second
	for ctx.Err() == nil {
		s.setState(func(st *State) {
			if st.Status != "connected" {
				st.Status = "connecting"
			}
		})
		began := time.Now()
		err := g.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		var fatal *FatalError
		if errors.As(err, &fatal) {
			s.opts.Log.Warn("discord: stopped", "err", err)
			s.setState(func(st *State) { *st = State{Status: "error", Error: err.Error()} })
			<-ctx.Done()
			return
		}
		if time.Since(began) > time.Minute {
			backoff = time.Second
		}
		s.opts.Log.Info("discord: connection ended; reconnecting", "err", err, "in", backoff)
		s.setState(func(st *State) {
			st.Status, st.Error = "connecting", fmt.Sprint(err)
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// register puts the slash commands in each Discord server.
func (s *Service) register(ctx context.Context, c *Client, app string, guilds []Guild) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	states := make([]GuildState, len(guilds))
	for i, g := range guilds {
		states[i] = s.registerOne(ctx, c, app, g.ID)
	}
	s.setState(func(st *State) {
		st.Guilds, st.Error = states, ""
	})
}

// registerOne puts the commands in one Discord server.
func (s *Service) registerOne(ctx context.Context, c *Client, app, guild string) GuildState {
	gs := GuildState{ID: guild}
	if g, err := c.Guild(ctx, guild); err == nil {
		gs.Name = g.Name
	}
	if err := c.SetCommands(ctx, app, guild, slashCommands()); err != nil {
		s.opts.Log.Warn("discord: register commands", "guild", guild, "err", err)
		gs.Error = "couldn't add the commands (has the bot been invited to it?): " + err.Error()
		return gs
	}
	gs.Commands = true
	return gs
}

// TestResult is what Test found.
type TestResult struct {
	Bot       string `json:"bot"`
	InviteURL string `json:"inviteUrl"`
	// Guilds say how each Discord server went.
	Guilds []GuildState `json:"guilds"`
}

// Test checks the token, and puts the commands in each Discord server
// again.
func (s *Service) Test(ctx context.Context) (TestResult, error) {
	set, err := s.settings()
	if err != nil {
		return TestResult{}, err
	}
	if set.Token == "" || len(set.Guilds) == 0 {
		return TestResult{}, errors.New("save the bot's token and a Discord server first")
	}
	c := s.client(set.Token)
	me, err := c.Me(ctx)
	if err != nil {
		return TestResult{}, err
	}
	app, err := c.App(ctx)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{Bot: me.Display(), InviteURL: InviteURL(app.ID), Guilds: []GuildState{}}
	for _, g := range set.Guilds {
		gs := s.registerOne(ctx, c, app.ID, g.ID)
		if gs.Name == "" && !gs.Commands {
			gs.Error = "the bot isn't in this Discord server yet: open its invite link, add it, then test again"
		}
		res.Guilds = append(res.Guilds, gs)
	}
	s.setState(func(st *State) {
		if st.Status == "connected" {
			st.Guilds, st.Error = res.Guilds, ""
		}
	})
	return res, nil
}

// InviteLink is the bot's invite link, once its application is known.
func (s *Service) InviteLink() string {
	if app := s.State().ApplicationID; app != "" {
		return InviteURL(app)
	}
	return ""
}
