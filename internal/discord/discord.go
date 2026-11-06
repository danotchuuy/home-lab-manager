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
	// GuildID is the Discord server the bot answers in.
	GuildID string `json:"guildId"`
	// Rules say what each Discord role may do.
	Rules []Rule `json:"rules"`
	// Details shows addresses and who's playing, as the status page does
	// behind its details password.
	Details bool `json:"details"`
	// Token is the bot's token; the API never sends it.
	Token string `json:"token,omitempty"`
}

// Rule lets a Discord role do things to some servers.
type Rule struct {
	// RoleID is the Discord role; the server's own ID is @everyone.
	RoleID string `json:"roleId"`
	// RoleName is the role's name when the rule was saved, for showing.
	RoleName string `json:"roleName,omitempty"`
	// Groups are hlm server groups; none means every server on the page.
	Groups  []string `json:"groups"`
	Actions []string `json:"actions"`
}

// State is how the connection is.
type State struct {
	// Status is off, connecting, connected or error.
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	Bot           string    `json:"bot,omitempty"`
	ApplicationID string    `json:"applicationId,omitempty"`
	Since         time.Time `json:"since,omitzero"`
	// Commands says the slash commands are in the Discord server.
	Commands bool `json:"commands"`
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
	if set.Rules == nil {
		set.Rules = []Rule{}
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
	in.GuildID = strings.TrimSpace(in.GuildID)
	if in.GuildID != "" && !ValidID(in.GuildID) {
		return Settings{}, errors.New("the Discord server ID is a number: in Discord, turn on Developer Mode, then right-click the server and Copy Server ID")
	}
	if in.Enabled && (in.Token == "" || in.GuildID == "") {
		return Settings{}, errors.New("the bot needs its token and the Discord server's ID")
	}
	if len(in.Rules) > maxRules {
		return Settings{}, fmt.Errorf("at most %d rules", maxRules)
	}
	if in.Rules == nil {
		in.Rules = []Rule{}
	}
	for i := range in.Rules {
		r := &in.Rules[i]
		if !ValidID(r.RoleID) {
			return Settings{}, fmt.Errorf("rule %d: %q isn't a Discord role ID", i+1, r.RoleID)
		}
		r.RoleName = strings.TrimSpace(r.RoleName)
		if len(r.RoleName) > 100 {
			r.RoleName = r.RoleName[:100]
		}
		for _, a := range r.Actions {
			if !slices.Contains(Actions, a) {
				return Settings{}, fmt.Errorf("rule %d: unknown action %q", i+1, a)
			}
		}
		slices.Sort(r.Actions)
		r.Actions = slices.Compact(r.Actions)
		if r.Groups == nil {
			r.Groups = []string{}
		}
		if len(r.Groups) > 0 && s.opts.Groups != nil {
			all, err := s.opts.Groups()
			if err != nil {
				return Settings{}, err
			}
			for _, g := range r.Groups {
				if _, ok := all[g]; !ok {
					return Settings{}, fmt.Errorf("rule %d: there's no server group %q", i+1, g)
				}
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
	if set.Token != "" && set.GuildID != "" {
		if app := s.State().ApplicationID; app != "" {
			cleanup = s.client(set.Token).SetCommands(ctx, app, set.GuildID, nil)
		} else if a, err := s.client(set.Token).App(ctx); err == nil {
			cleanup = s.client(set.Token).SetCommands(ctx, a.ID, set.GuildID, nil)
		} else {
			cleanup = err
		}
	}
	set.Token, set.Enabled = "", false
	if err := s.opts.Secrets.Set(secretName, set); err != nil {
		return err
	}
	s.poke()
	if cleanup != nil {
		return fmt.Errorf("the bot is off and its token forgotten, but its commands couldn't be removed from the Discord server: %w", cleanup)
	}
	return nil
}

// Roles lists the Discord server's roles, for choosing rules.
func (s *Service) Roles(ctx context.Context) ([]Role, error) {
	set, err := s.settings()
	if err != nil {
		return nil, err
	}
	if set.Token == "" || set.GuildID == "" {
		return nil, errors.New("save the bot's token and the Discord server's ID first")
	}
	rs, err := s.client(set.Token).Roles(ctx, set.GuildID)
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
		if err != nil || !set.Enabled || set.Token == "" || set.GuildID == "" {
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
		go s.register(ctx, c, r.Application.ID, set.GuildID)
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

// register puts the slash commands in the Discord server.
func (s *Service) register(ctx context.Context, c *Client, app, guild string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := c.SetCommands(ctx, app, guild, slashCommands())
	s.setState(func(st *State) {
		st.Commands = err == nil
		st.Error = ""
		if err != nil {
			st.Error = "couldn't add the commands to the Discord server (has the bot been invited to it?): " + err.Error()
		}
	})
	if err != nil {
		s.opts.Log.Warn("discord: register commands", "err", err)
	}
}

// TestResult is what Test found.
type TestResult struct {
	Bot       string `json:"bot"`
	Guild     string `json:"guild"`
	InviteURL string `json:"inviteUrl"`
	// Commands says the slash commands were put in the Discord server.
	Commands bool `json:"commands"`
}

// Test checks the token and the Discord server, and puts the commands in
// it again.
func (s *Service) Test(ctx context.Context) (TestResult, error) {
	set, err := s.settings()
	if err != nil {
		return TestResult{}, err
	}
	if set.Token == "" || set.GuildID == "" {
		return TestResult{}, errors.New("save the bot's token and the Discord server's ID first")
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
	res := TestResult{Bot: me.Display(), InviteURL: InviteURL(app.ID)}
	g, err := c.Guild(ctx, set.GuildID)
	if err != nil {
		return res, errors.New("the bot isn't in that Discord server yet: open its invite link, add it, then test again")
	}
	res.Guild = g.Name
	if err := c.SetCommands(ctx, app.ID, set.GuildID, slashCommands()); err != nil {
		return res, fmt.Errorf("couldn't add the commands to %s: %w", g.Name, err)
	}
	res.Commands = true
	s.setState(func(st *State) {
		if st.Status == "connected" {
			st.Commands, st.Error = true, ""
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
