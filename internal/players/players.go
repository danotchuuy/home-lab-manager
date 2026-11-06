// Package players manages a game server's players: who's on, kicking and
// banning them, and the allow, ban and admin lists, with the commands and
// files from the server's game profile.
package players

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	logName = "players"
	// keepRecords is how many actions are kept per server.
	keepRecords = 100
)

// Servers is what the service needs from the server manager.
type Servers interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
	SearchFiles(ctx context.Context, id, dir, query string) ([]files.Match, bool, error)
	ReadFile(ctx context.Context, id, path string) (files.File, error)
	WriteFile(ctx context.Context, id, path, content string, expected time.Time) (files.File, error)
}

// Console sends servers commands.
type Console interface {
	Query(ctx context.Context, id, command string) (string, error)
	Send(ctx context.Context, id, who, command string) (console.Result, error)
}

// Options configures a Service.
type Options struct {
	Secrets *secrets.Store
	Servers Servers
	Console Console
	// Names are the players the status query last saw, for when the
	// console can't list them.
	Names    func(server string) []string
	Notifier notify.Notifier
	Log      *slog.Logger
	Now      func() time.Time
}

// Service manages players.
type Service struct {
	opts   Options
	saveMu sync.Mutex
}

// New creates the service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{opts: o}
}

// Player is someone on a server, or on one of its lists.
type Player struct {
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
	// Reason is why they're on the ban list, if it says.
	Reason string `json:"reason,omitempty"`
}

// List is one of a server's player lists, from its file.
type List struct {
	File    string   `json:"file"`
	Path    string   `json:"path,omitempty"` // where it was found
	Format  string   `json:"format"`
	Players []Player `json:"players"`
	Error   string   `json:"error,omitempty"`
	// Editable says hlm can change the file itself now (a lines file,
	// while the server is stopped).
	Editable bool `json:"editable"`
}

// Record is something done to a player through hlm.
type Record struct {
	Time   time.Time `json:"time"`
	By     string    `json:"by"`
	Action string    `json:"action"`
	Player string    `json:"player"`
	Reason string    `json:"reason,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// Overview is a server's players, as the Players tab shows them.
type Overview struct {
	Game    string `json:"game,omitempty"`
	Running bool   `json:"running"`
	// Actions are the actions the game's console takes; NeedsID those that
	// take a player's ID rather than their name.
	Actions []string `json:"actions"`
	NeedsID []string `json:"needsId,omitempty"`
	// Online are the players on now, from the console when it can list them
	// (with IDs, if the game gives them) or the last status query.
	Online      []Player `json:"online"`
	OnlineFrom  string   `json:"onlineFrom,omitempty"` // "console" or "query"
	OnlineError string   `json:"onlineError,omitempty"`
	// Lists are the game's player lists, by list.
	Lists map[string]List `json:"lists,omitempty"`
	// Log is what was done through hlm, newest first.
	Log []Record `json:"log"`
}

// ErrNoPlayers is a server whose game hlm can't manage players for.
var ErrNoPlayers = errors.New("hlm doesn't know how to manage this game's players: its game profile has no player actions or lists")

// Overview reads a server's players and lists.
func (s *Service) Overview(ctx context.Context, name string) (Overview, error) {
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Running: info.State.Running, Actions: []string{}, Online: []Player{}, Log: s.records(name)}
	game := gameprofile.Match(info.Config.Image)
	if game == nil || (len(game.Console.Actions) == 0 && len(game.PlayerLists) == 0) {
		return ov, ErrNoPlayers
	}
	ov.Game = game.ID
	for _, a := range gameprofile.Actions {
		if cmd, ok := game.Console.Actions[a]; ok {
			ov.Actions = append(ov.Actions, a)
			if strings.Contains(cmd, "{id}") && !strings.Contains(cmd, "{player}") {
				ov.NeedsID = append(ov.NeedsID, a)
			}
		}
	}
	if ov.Running {
		ov.Online, ov.OnlineFrom, ov.OnlineError = s.online(ctx, name, game)
	}
	if len(game.PlayerLists) > 0 {
		ov.Lists = map[string]List{}
		for l, f := range game.PlayerLists {
			ov.Lists[l] = s.readList(ctx, info, f)
		}
	}
	return ov, nil
}

// online lists who's on: through the console if it can, with IDs, or
// from the last status query.
func (s *Service) online(ctx context.Context, name string, game *gameprofile.Profile) ([]Player, string, string) {
	out := []Player{}
	var why string
	if p := game.Console.Players; p != nil && s.opts.Console != nil {
		reply, err := s.opts.Console.Query(ctx, name, p.Command)
		if err == nil {
			var list gameprofile.PlayerList
			if list, err = p.Parse(reply); err == nil {
				for i, n := range list.Names {
					pl := Player{Name: n}
					if i < len(list.IDs) {
						pl.ID = list.IDs[i]
					}
					out = append(out, pl)
				}
				return out, "console", ""
			}
		}
		why = err.Error()
	}
	if s.opts.Names != nil {
		for _, n := range s.opts.Names(name) {
			out = append(out, Player{Name: n})
		}
		return out, "query", why
	}
	return out, "", why
}

// --- what's sent ---

var (
	// Names go into a command as one word: nothing that could end it or
	// start another (spaces, ; quotes, newlines, control characters).
	nameRE = regexp.MustCompile(`^[^\s;"'\x60\\{}]{1,64}$`)
	idRE   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	// Reasons are words, on one line, without what ends a command.
	reasonRE = regexp.MustCompile(`^[^\x00-\x1f\x7f;"'\x60\\{}]{0,200}$`)
)

// CheckName, CheckID and CheckReason say why a value can't go into a
// command.
func CheckName(n string) error {
	if !nameRE.MatchString(n) {
		return fmt.Errorf("%q can't be sent in a command: a name is one word of up to 64 characters, without ; quotes or backslashes (use the console for anything else)", n)
	}
	return nil
}

func CheckID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("%q isn't a player ID: letters, digits, _ . : and - only", id)
	}
	return nil
}

func CheckReason(r string) error {
	if !reasonRE.MatchString(r) {
		return errors.New("a reason is one line of up to 200 characters, without ; quotes or backslashes")
	}
	return nil
}

// Command fills in an action's command, checking the name if it goes in.
// The ID and reason must have been checked.
func Command(tmpl string, p Player, reason string) (string, error) {
	if strings.Contains(tmpl, "{id}") && p.ID == "" {
		return "", errors.New("this game needs the player's ID for that, and hlm doesn't know it")
	}
	if strings.Contains(tmpl, "{player}") {
		if p.Name == "" {
			return "", errors.New("this game needs the player's name for that")
		}
		if err := CheckName(p.Name); err != nil {
			return "", err
		}
	}
	if reason == "" {
		for _, ph := range []string{` "{reason}"`, ` '{reason}'`, ` {reason}`, `{reason}`} {
			tmpl = strings.ReplaceAll(tmpl, ph, "")
		}
	}
	return strings.NewReplacer("{player}", p.Name, "{id}", p.ID, "{reason}", reason).Replace(tmpl), nil
}

// Act does something to a player: through the console while the server
// runs, or by changing a list's file (lines files) while it's stopped.
// who is the person, for the console's log and hlm's records.
func (s *Service) Act(ctx context.Context, name, who, action string, p Player, reason string) (string, error) {
	p.Name, p.ID, reason = strings.TrimSpace(p.Name), strings.TrimSpace(p.ID), strings.TrimSpace(reason)
	// Names are checked where they go into a command (some games' commands
	// take only the ID, and their names can have spaces); here only that
	// it's a name at all.
	if len(p.Name) > 100 || strings.ContainsFunc(p.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("that isn't a player's name")
	}
	if p.ID != "" {
		if err := CheckID(p.ID); err != nil {
			return "", err
		}
	}
	if p.Name == "" && p.ID == "" {
		return "", errors.New("say which player")
	}
	if err := CheckReason(reason); err != nil {
		return "", err
	}
	if !slices.Contains(gameprofile.Actions, action) {
		return "", fmt.Errorf("unknown action %q", action)
	}
	info, err := s.opts.Servers.Inspect(ctx, name)
	if err != nil {
		return "", err
	}
	game := gameprofile.Match(info.Config.Image)
	if game == nil {
		return "", ErrNoPlayers
	}
	// A name that can't go into the command is refused before anything's
	// recorded.
	if tmpl, ok := game.Console.Actions[action]; ok && info.State.Running && strings.Contains(tmpl, "{player}") {
		if err := CheckName(p.Name); err != nil {
			return "", err
		}
	}
	who = strings.TrimSpace(who)
	// On the request's activity log entry.
	audit.Add(ctx, "player", cmpOr(p.Name, p.ID))
	audit.Add(ctx, "reason", reason)
	rec := Record{Time: s.opts.Now(), By: who, Action: action, Player: cmpOr(p.Name, p.ID), Reason: reason}
	out, err := s.act(ctx, info, name, who, game, action, p, reason)
	if err != nil {
		rec.Error = err.Error()
	}
	s.record(name, rec)
	if err == nil && action == gameprofile.ActionBan && s.opts.Notifier != nil {
		msg := fmt.Sprintf("%s banned %s on %s.", cmpOr(who, "Someone"), rec.Player, name)
		if reason != "" {
			msg += " Reason: " + reason
		}
		s.opts.Notifier.Notify(notify.Event{Kind: notify.KindPlayerBanned, Severity: notify.Info, Title: rec.Player + " was banned",
			Message: msg, Server: name, Link: "/servers/" + name + "?tab=players", Time: rec.Time})
	}
	return out, err
}

func (s *Service) act(ctx context.Context, info docker.ContainerJSON, name, who string, game *gameprofile.Profile, action string, p Player, reason string) (string, error) {
	tmpl, hasCommand := game.Console.Actions[action]
	if info.State.Running {
		if !hasCommand {
			if list, _ := gameprofile.ActionList(action); game.PlayerLists[list].Format == gameprofile.FormatLines {
				return "", fmt.Errorf("%s reads its lists when it starts: stop it to change them", game.Name)
			}
			return "", fmt.Errorf("%s's console has no command for that", game.Name)
		}
		cmd, err := Command(tmpl, p, reason)
		if err != nil {
			return "", err
		}
		res, err := s.opts.Console.Send(ctx, name, who, cmd)
		return res.Output, err
	}
	// Stopped: only a list file can change.
	list, remove := gameprofile.ActionList(action)
	f, ok := game.PlayerLists[list]
	if !ok || f.Format != gameprofile.FormatLines {
		return "", errors.New("start the server to do that: it goes through its console")
	}
	entry := p.ID
	if entry == "" {
		if err := CheckName(p.Name); err != nil {
			return "", err
		}
		entry = p.Name
	}
	return "", s.editLines(ctx, info, f, entry, remove)
}

// --- records ---

func (s *Service) records(name string) []Record {
	all := map[string][]Record{}
	s.opts.Secrets.Get(logName, &all)
	out := slices.Clone(all[name])
	slices.Reverse(out)
	if out == nil {
		out = []Record{}
	}
	return out
}

func (s *Service) record(name string, r Record) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all := map[string][]Record{}
	if _, err := s.opts.Secrets.Get(logName, &all); err != nil {
		s.opts.Log.Warn("read player records", "err", err)
		return
	}
	list := append(all[name], r)
	if len(list) > keepRecords {
		list = list[len(list)-keepRecords:]
	}
	all[name] = list
	if err := s.opts.Secrets.Set(logName, all); err != nil {
		s.opts.Log.Warn("save player records", "err", err)
	}
}

// ForgetServer drops a deleted server's records.
func (s *Service) ForgetServer(name string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	all := map[string][]Record{}
	if _, err := s.opts.Secrets.Get(logName, &all); err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	return s.opts.Secrets.Set(logName, all)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// findFile finds a list's file in the server's data folders: the one
// under Prefer, or the shallowest.
func (s *Service) findFile(ctx context.Context, info docker.ContainerJSON, f gameprofile.ListFile) (string, error) {
	var matches []string
	for _, m := range info.Mounts {
		if m.Type == "tmpfs" || m.Destination == "" || m.Destination == "/" {
			continue
		}
		ms, _, err := s.opts.Servers.SearchFiles(ctx, info.ID, m.Destination, f.File)
		if err != nil {
			continue
		}
		for _, x := range ms {
			if strings.EqualFold(path.Base(x.Path), f.File) && !slices.Contains(matches, x.Path) {
				matches = append(matches, x.Path)
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%s isn't in the server's data folders yet (the game makes it when it first starts)", f.File)
	}
	slices.SortFunc(matches, func(a, b string) int {
		pa, pb := f.Prefer != "" && strings.Contains(a, f.Prefer), f.Prefer != "" && strings.Contains(b, f.Prefer)
		switch {
		case pa && !pb:
			return -1
		case pb && !pa:
			return 1
		}
		if d := strings.Count(a, "/") - strings.Count(b, "/"); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	return matches[0], nil
}
