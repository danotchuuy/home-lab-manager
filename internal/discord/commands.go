package discord

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// Limits.
const (
	ratePerMinute = 5
	maxSay        = 200
	maxChoices    = 25
)

func serverOption(what string) CommandOption {
	return CommandOption{Type: 3, Name: "server", Description: "The server to " + what, Required: true, Autocomplete: true}
}

func slashCommands() []Command {
	no := false
	return []Command{
		{Name: "status", Description: "How the game servers are doing", DMPermission: &no},
		{Name: "players", Description: "Who's playing on a server", Options: []CommandOption{serverOption("look at")}, DMPermission: &no},
		{Name: "start", Description: "Start a server", Options: []CommandOption{serverOption("start")}, DMPermission: &no},
		{Name: "stop", Description: "Stop a server", Options: []CommandOption{serverOption("stop")}, DMPermission: &no},
		{Name: "restart", Description: "Restart a server", Options: []CommandOption{serverOption("restart")}, DMPermission: &no},
		{Name: "say", Description: "Say something to the players on a server", DMPermission: &no, Options: []CommandOption{
			serverOption("message"),
			{Type: 3, Name: "message", Description: "What to say", Required: true, MaxLength: maxSay},
		}},
	}
}

// commandAction is the rule action a command needs ("" for status).
func commandAction(name string) (string, bool) {
	switch name {
	case "status":
		return "", true
	case ActionPlayers, ActionStart, ActionStop, ActionRestart, ActionSay:
		return name, true
	}
	return "", false
}

// Embed colours.
const (
	colorUp    = 0x2e7d32
	colorDown  = 0x757575
	colorError = 0xc62828
	colorWait  = 0xef6c00
)

// server is one of the page's servers, with the server behind it.
type server struct {
	name string
	pub  statuspage.Server
}

// servers are the status page's servers, as Discord may see them.
func (s *Service) servers(ctx context.Context, details bool) ([]server, error) {
	pub, names, err := s.opts.Page.All(ctx, s.opts.Now())
	if err != nil {
		return nil, err
	}
	out := make([]server, len(pub))
	for i := range pub {
		p := pub[i]
		if !details {
			p.Address, p.Hint, p.PlayerNames = "", "", nil
		}
		out[i] = server{name: names[i], pub: p}
	}
	return out, nil
}

// caller is who sent an interaction.
type caller struct {
	user  User
	roles []string
}

// may says whether the caller may do an action to a server.
func (s *Service) may(set Settings, c caller, action, server string) bool {
	if action == "" {
		return true
	}
	var groups map[string][]string
	for _, r := range set.Rules {
		if !slices.Contains(r.Actions, action) {
			continue
		}
		if r.RoleID != set.GuildID && !slices.Contains(c.roles, r.RoleID) {
			continue // the server's ID is its @everyone role
		}
		if len(r.Groups) == 0 {
			return true
		}
		if groups == nil {
			if s.opts.Groups == nil {
				return false
			}
			var err error
			if groups, err = s.opts.Groups(); err != nil {
				return false // when in doubt, no
			}
		}
		host := "local"
		if i := strings.LastIndexByte(server, '@'); i >= 0 {
			host = server[i+1:]
		}
		for _, g := range r.Groups {
			if slices.Contains(groups[g], server) || slices.Contains(groups[g], "@"+host) {
				return true
			}
		}
	}
	return false
}

// allow counts a command against the caller's rate limit.
func (s *Service) allow(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	recent := slices.DeleteFunc(s.rate[user], func(t time.Time) bool { return now.Sub(t) >= time.Minute })
	if len(recent) >= ratePerMinute {
		s.rate[user] = recent
		return false
	}
	s.rate[user] = append(recent, now)
	if len(s.rate) > 1000 {
		for k, v := range s.rate {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute {
				delete(s.rate, k)
			}
		}
	}
	return true
}

// handle answers one interaction.
func (s *Service) handle(ctx context.Context, c *Client, in Interaction) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.StartWait+2*time.Minute)
	defer cancel()
	set, err := s.settings()
	if err != nil {
		s.opts.Log.Warn("discord: settings", "err", err)
		return
	}
	reply := func(m Message) {
		if err := c.Respond(ctx, in.ID, in.Token, ResponseMessage, &m); err != nil {
			s.opts.Log.Warn("discord: reply", "err", err)
		}
	}
	private := func(text string) { reply(Message{Content: text, Flags: FlagEphemeral}) }

	if in.GuildID == "" || in.GuildID != set.GuildID || in.Member == nil {
		if in.Type == InteractionCommand {
			private("hlm only answers in its own Discord server.")
		}
		return
	}
	who := caller{user: in.Member.User, roles: in.Member.Roles}
	action, known := commandAction(in.Data.Name)
	if !known {
		if in.Type == InteractionCommand {
			private("hlm doesn't know that command.")
		}
		return
	}

	if in.Type == InteractionAutocomplete {
		s.autocomplete(ctx, c, in, set, who, action)
		return
	}
	if in.Type != InteractionCommand {
		return
	}
	if !s.allow(who.user.ID) {
		private(fmt.Sprintf("Slow down: at most %d commands a minute.", ratePerMinute))
		return
	}
	list, err := s.servers(ctx, set.Details)
	if err != nil {
		private("hlm couldn't look at its servers: " + err.Error())
		return
	}
	if in.Data.Name == "status" {
		reply(statusMessage(list))
		return
	}

	arg, _ := in.Option("server")
	srv, ok := find(list, arg)
	if !ok || !s.may(set, who, action, srv.name) {
		// The same answer for servers that don't exist and ones they may
		// not use, so names can't be probed.
		private(fmt.Sprintf("You can't %s that server here. Pick one from the list as you type.", action))
		return
	}
	switch action {
	case ActionPlayers:
		reply(playersMessage(srv.pub))
	case ActionSay:
		msg, _ := in.Option("message")
		reply(s.say(ctx, srv, who, msg))
	case ActionStart, ActionStop, ActionRestart:
		if err := c.Respond(ctx, in.ID, in.Token, ResponseDeferred, nil); err != nil {
			s.opts.Log.Warn("discord: defer", "err", err)
			return
		}
		m := s.control(ctx, set, srv, who, action)
		if err := c.EditReply(ctx, cmpOr(in.ApplicationID, s.State().ApplicationID), in.Token, m); err != nil {
			s.opts.Log.Warn("discord: edit reply", "err", err)
		}
	}
}

// find is the server an argument names: its slug (what autocomplete
// sends), or its display name.
func find(list []server, arg string) (server, bool) {
	arg = strings.TrimSpace(arg)
	for _, srv := range list {
		if srv.pub.Slug == arg {
			return srv, true
		}
	}
	for _, srv := range list {
		if strings.EqualFold(srv.pub.Name, arg) {
			return srv, true
		}
	}
	return server{}, false
}

// autocomplete offers the servers the caller may use the command on.
func (s *Service) autocomplete(ctx context.Context, c *Client, in Interaction, set Settings, who caller, action string) {
	typed := ""
	for _, o := range in.Data.Options {
		if o.Focused {
			if o.Name != "server" {
				_ = c.Respond(ctx, in.ID, in.Token, ResponseAutocomplete, map[string]any{"choices": []Choice{}})
				return
			}
			typed, _ = o.Value.(string)
		}
	}
	choices := []Choice{}
	if list, err := s.servers(ctx, false); err == nil {
		for _, srv := range list {
			if len(choices) == maxChoices {
				break
			}
			if !s.may(set, who, action, srv.name) {
				continue
			}
			if typed != "" && !strings.Contains(strings.ToLower(srv.pub.Name), strings.ToLower(typed)) {
				continue
			}
			choices = append(choices, Choice{Name: truncate(srv.pub.Name, 100), Value: srv.pub.Slug})
		}
	}
	if err := c.Respond(ctx, in.ID, in.Token, ResponseAutocomplete, map[string]any{"choices": choices}); err != nil {
		s.opts.Log.Warn("discord: autocomplete", "err", err)
	}
}

// stateText says how a server is, in a few words.
func stateText(p statuspage.Server) (string, int) {
	switch p.State {
	case statuspage.StateOnline:
		return "🟢 Online", colorUp
	case statuspage.StateStarting:
		return "🟡 Starting", colorWait
	case statuspage.StateRunning:
		return "🟢 Running", colorUp
	case statuspage.StateAsleep:
		return "💤 Asleep (start it to play)", colorDown
	}
	return "⚫ Offline", colorDown
}

// players says how many are on.
func players(p statuspage.Server) string {
	if p.Players == nil {
		return ""
	}
	if p.MaxPlayers != nil {
		return fmt.Sprintf("%d/%d players", *p.Players, *p.MaxPlayers)
	}
	return fmt.Sprintf("%d players", *p.Players)
}

// line is a server's state in one line.
func line(p statuspage.Server) string {
	parts := []string{}
	st, _ := stateText(p)
	parts = append(parts, st)
	if n := players(p); n != "" {
		parts = append(parts, n)
	}
	if p.Address != "" {
		parts = append(parts, code(p.Address))
	}
	return strings.Join(parts, " · ")
}

func statusMessage(list []server) Message {
	e := Embed{Title: "Game servers", Color: colorUp}
	if len(list) == 0 {
		e.Description = "There are no servers on hlm's status page."
		e.Color = colorDown
	}
	for i, srv := range list {
		if i == 25 {
			e.Footer = &struct {
				Text string `json:"text"`
			}{Text: fmt.Sprintf("and %d more", len(list)-25)}
			break
		}
		e.Fields = append(e.Fields, EmbedField{Name: truncate(srv.pub.Name, 256), Value: line(srv.pub)})
	}
	return Message{Embeds: []Embed{e}}
}

func playersMessage(p statuspage.Server) Message {
	st, color := stateText(p)
	e := Embed{Title: truncate(p.Name, 256), Color: color}
	switch {
	case p.State != statuspage.StateOnline:
		e.Description = st
	case p.Players == nil:
		e.Description = "hlm doesn't show who's playing on this server."
	case *p.Players == 0:
		e.Description = "Nobody is playing."
	case len(p.PlayerNames) > 0:
		names := make([]string, len(p.PlayerNames))
		for i, n := range p.PlayerNames {
			names[i] = escape(n)
		}
		e.Description = players(p) + ": " + truncate(strings.Join(names, ", "), 3500)
	default:
		e.Description = players(p) + "."
	}
	return Message{Embeds: []Embed{e}}
}

// say sends the game's say command.
func (s *Service) say(ctx context.Context, srv server, who caller, msg string) Message {
	msg = clean(msg)
	fail := func(text string) Message { return Message{Content: text, Flags: FlagEphemeral} }
	if msg == "" {
		return fail("Say what?")
	}
	if len(msg) > maxSay {
		return fail(fmt.Sprintf("At most %d characters.", maxSay))
	}
	if srv.pub.State != statuspage.StateOnline && srv.pub.State != statuspage.StateRunning {
		return fail(srv.pub.Name + " isn't running.")
	}
	info, err := s.opts.Servers.Inspect(ctx, srv.name)
	if err != nil {
		return fail("hlm couldn't look at " + srv.pub.Name + ".")
	}
	game := gameprofile.Match(info.Config.Image)
	if game == nil || game.Console.Say == "" {
		return fail("hlm doesn't know how to say things on " + srv.pub.Name + ".")
	}
	name := clean(who.user.Display())
	cmd := fmt.Sprintf("%s [Discord] %s: %s", game.Console.Say, truncate(name, 32), msg)
	_, err = s.opts.Console.Send(ctx, srv.name, "discord:"+who.user.ID, cmd)
	s.record(who, "servers.say", srv.name, map[string]string{"message": msg}, err)
	if err != nil {
		s.opts.Log.Warn("discord: say", "server", srv.name, "err", err)
		return fail("That didn't reach " + srv.pub.Name + ".")
	}
	return Message{Content: fmt.Sprintf("Said on **%s**: %s", escape(srv.pub.Name), escape(msg))}
}

// control starts, stops or restarts a server, waiting for a started one
// to answer.
func (s *Service) control(ctx context.Context, set Settings, srv server, who caller, action string) Message {
	name := escape(srv.pub.Name)
	msg := func(color int, format string, a ...any) Message {
		return Message{Embeds: []Embed{{Description: fmt.Sprintf(format, a...), Color: color}}}
	}
	if s.opts.Servers.JobRunning(srv.name) {
		return msg(colorWait, "**%s** is being installed or updated; try again when that's done.", name)
	}
	running := srv.pub.State != statuspage.StateOffline && srv.pub.State != statuspage.StateAsleep
	var err error
	switch action {
	case ActionStart:
		if running {
			st, _ := stateText(srv.pub)
			return msg(colorUp, "**%s** is already running (%s).", name, st)
		}
		if s.opts.Asleep != nil && s.opts.Wake != nil && s.opts.Asleep(srv.name) {
			err = s.opts.Wake(ctx, srv.name, "discord:"+who.user.Username)
		} else {
			err = s.opts.Servers.Start(ctx, srv.name)
		}
	case ActionStop:
		if !running {
			return msg(colorDown, "**%s** isn't running.", name)
		}
		err = s.opts.Servers.Stop(ctx, srv.name)
	case ActionRestart:
		if !running {
			return msg(colorDown, "**%s** isn't running; use /start.", name)
		}
		err = s.opts.Servers.Restart(ctx, srv.name)
	}
	s.record(who, "servers."+action, srv.name, nil, err)
	if err != nil {
		s.opts.Log.Warn("discord: "+action, "server", srv.name, "err", err)
		return msg(colorError, "%s **%s** failed.", strings.ToUpper(action[:1])+action[1:]+"ing", name)
	}
	s.opts.Page.Invalidate()
	if action == ActionStop {
		was := ""
		if n := players(srv.pub); n != "" && *srv.pub.Players > 0 {
			was = fmt.Sprintf(" (%s were on)", n)
		}
		return msg(colorDown, "Stopped **%s**%s.", name, was)
	}
	return s.waitUp(ctx, set, srv, msg)
}

// waitUp waits for a started server to answer its status query.
func (s *Service) waitUp(ctx context.Context, set Settings, srv server, msg func(int, string, ...any) Message) Message {
	name := escape(srv.pub.Name)
	begin := time.Now()
	for {
		select {
		case <-ctx.Done():
			return msg(colorWait, "**%s** was started; hlm stopped waiting for it.", name)
		case <-time.After(s.opts.Poll):
		}
		s.opts.Page.Invalidate()
		list, err := s.servers(ctx, set.Details)
		if err != nil {
			continue
		}
		cur, ok := find(list, srv.pub.Slug)
		if !ok {
			return msg(colorWait, "**%s** was started.", name)
		}
		switch cur.pub.State {
		case statuspage.StateOnline:
			text := fmt.Sprintf("**%s** is up", name)
			if n := players(cur.pub); n != "" {
				text += ": " + n
			}
			if cur.pub.Address != "" {
				text += " · " + code(cur.pub.Address)
			}
			return msg(colorUp, "%s", text)
		case statuspage.StateOffline:
			if time.Since(begin) > 2*s.opts.Poll {
				return msg(colorError, "**%s** stopped again right after starting. Someone with hlm access should look at its log.", name)
			}
		case statuspage.StateRunning:
			if time.Since(begin) >= time.Minute {
				return msg(colorUp, "**%s** is running (hlm can't ask the game whether it's ready).", name)
			}
		}
		if time.Since(begin) >= s.opts.StartWait {
			return msg(colorWait, "**%s** is running, but hasn't answered after %s; it may still be starting.", name, s.opts.StartWait)
		}
	}
}

// record puts what someone did through Discord in the activity log.
func (s *Service) record(who caller, action, server string, details map[string]string, err error) {
	if s.opts.Audit == nil {
		return
	}
	d := map[string]string{"via": "Discord", "discord_user": who.user.ID}
	for k, v := range details {
		d[k] = v
	}
	e := audit.Entry{Actor: "discord:" + who.user.Username, ActorKind: audit.ActorDiscord, Action: action,
		Target: server, Server: server, Result: audit.ResultOK, Details: d}
	if err != nil {
		e.Result, e.Error = audit.ResultError, err.Error()
	}
	s.opts.Audit.Record(e)
}

// clean drops control characters and trims.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}

// escape keeps text from being read as Discord markdown or mentions.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\*_~`|>#[]()@<:", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// code shows text as code, where Discord doesn't read markdown.
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 1
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xc0 != 0x80 }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
