package manager

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// A game profile's launch options on a server: arguments, like
// Nightingale's status endpoint, that a server can have turned on or off.
// They go in the variable the server's image takes the game's arguments in
// (one of the profile's args_vars), or else in its post arguments, and an
// option's port becomes one of the server's ports.

// LaunchOptionState is one of a game's launch options on a server.
type LaunchOptionState struct {
	gameprofile.LaunchOption
	On bool `json:"on"`
	// Where says where its arguments go ("the variable GAME_PARAMS", or
	// "its post arguments").
	Where string `json:"where"`
}

// ErrNoLaunchOptions is returned for a server whose game has none.
var ErrNoLaunchOptions = errors.New("this server's game has no launch options")

// LaunchOptions lists a server's game's launch options, and which are on.
func (m *Manager) LaunchOptions(ctx context.Context, id string) ([]LaunchOptionState, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	game := gameprofile.Match(info.Config.Image)
	if game == nil || len(game.LaunchOptions) == 0 {
		return []LaunchOptionState{}, nil
	}
	app, req, err := m.Config(ctx, id)
	if err != nil {
		return nil, err
	}
	_, args, where := launchArgs(app, req, game)
	out := make([]LaunchOptionState, 0, len(game.LaunchOptions))
	for _, o := range game.LaunchOptions {
		out = append(out, LaunchOptionState{LaunchOption: o, On: optionOn(args, o), Where: where})
	}
	return out, nil
}

// SetLaunchOption turns one of a server's game's launch options on or off,
// recreating the server with it (and so restarting it, if it runs). admin
// lets an admin take a confined server out of confinement, as
// ReconfigureAsAdmin does. It returns the server, and the option's port's
// key (as port options name it, "7778/tcp") when it has one.
func (m *Manager) SetLaunchOption(ctx context.Context, id, option string, on, admin bool, emit func(Event)) (Server, string, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return Server{}, "", err
	}
	game := gameprofile.Match(info.Config.Image)
	if game == nil {
		return Server{}, "", ErrNoLaunchOptions
	}
	opt, ok := game.LaunchOption(option)
	if !ok {
		return Server{}, "", fmt.Errorf("%s has no launch option %q", game.Name, option)
	}
	app, req, err := m.Config(ctx, id)
	if err != nil {
		return Server{}, "", err
	}
	host := HostOf(info.ID)
	// The option's port, as the server gets it: on its own address (host
	// networking or a LAN network), the port itself; published, a free one.
	hostPort := opt.Port
	direct := info.Config.Labels[deploy.LabelDirectIP] == "true" || info.HostConfig.NetworkMode == "host"
	if on && opt.Port > 0 && !direct {
		used, err := m.HostPortsInUse(ctx, host)
		if err != nil {
			return Server{}, "", err
		}
		for used[fmt.Sprintf("%d/%s", hostPort, opt.Protocol())] && hostPort < 65535 {
			hostPort++
		}
	}
	req, err = applyLaunchOption(app, req, game, opt, on, hostPort)
	if err != nil {
		return Server{}, "", err
	}
	verb := "Turning off"
	if on {
		verb = "Turning on"
	}
	emit(Event{Stage: "create", Message: verb + " " + opt.Name})
	var srv Server
	if admin {
		srv, err = m.ReconfigureAsAdmin(ctx, info.ID, req, emit)
	} else {
		srv, err = m.Reconfigure(ctx, info.ID, req, emit)
	}
	if err != nil || opt.Port == 0 {
		return srv, "", err
	}
	return srv, fmt.Sprintf("%d/%s", hostPort, opt.Protocol()), nil
}

// launchArgs finds where a server's game takes its launch arguments: the
// first of the profile's args_vars the server has (its item's ID and
// value), or else its post arguments (ID "").
func launchArgs(app *catalog.App, req deploy.Request, game *gameprofile.Profile) (string, string, string) {
	items, _ := deploy.Items(app, req)
	for _, name := range game.ArgsVars {
		for _, it := range items {
			if it.Type == catalog.TypeVariable && it.Target == name && !it.Disabled {
				return it.ID, it.Effective, "the variable " + name
			}
		}
	}
	post := app.PostArgs
	if req.PostArgs != nil {
		post = *req.PostArgs
	}
	return "", post, "its post arguments"
}

// argToken matches one of an option's arguments in a server's, with any
// port in place of {port}.
func argToken(tok string) *regexp.Regexp {
	parts := strings.Split(tok, "{port}")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`^` + strings.Join(parts, `\d+`) + `$`)
}

// optionOn reports whether a server's arguments have an option's first
// argument, with whatever port.
func optionOn(args string, o gameprofile.LaunchOption) bool {
	toks := strings.Fields(o.Args)
	if len(toks) == 0 {
		return false
	}
	re := argToken(toks[0])
	for _, a := range strings.Fields(args) {
		if re.MatchString(strings.Trim(a, `"'`)) {
			return true
		}
	}
	return false
}

// withoutOption takes an option's arguments (with any port) out of args,
// leaving the rest as they were.
func withoutOption(args string, o gameprofile.LaunchOption) string {
	var res []*regexp.Regexp
	for _, t := range strings.Fields(o.Args) {
		res = append(res, argToken(t))
	}
	var keep []string
	for _, a := range strings.Fields(args) {
		drop := false
		for _, re := range res {
			if re.MatchString(strings.Trim(a, `"'`)) {
				drop = true
			}
		}
		if !drop {
			keep = append(keep, a)
		}
	}
	return strings.Join(keep, " ")
}

// optionPortName names the port an option gives a server.
func optionPortName(o gameprofile.LaunchOption) string { return o.Name + " (hlm)" }

// applyLaunchOption turns an option on or off in a server's request: its
// arguments in (or out of) where the game takes them, and its port as one
// of the server's ports, published on hostPort.
func applyLaunchOption(app *catalog.App, req deploy.Request, game *gameprofile.Profile, o gameprofile.LaunchOption, on bool, hostPort int) (deploy.Request, error) {
	id, args, _ := launchArgs(app, req, game)
	args = withoutOption(args, o)
	if on {
		args = strings.TrimSpace(args + " " + o.Rendered())
	}
	if id != "" {
		values := map[string]string{}
		for k, v := range req.Values {
			values[k] = v
		}
		values[id] = args
		req.Values = values
	} else {
		req.PostArgs = &args
	}
	if o.Port == 0 {
		return req, nil
	}
	port, proto := strconv.Itoa(o.Port), o.Protocol()
	extra := make([]deploy.ExtraConfig, 0, len(req.Extra)+1)
	for _, e := range req.Extra {
		if !(strings.EqualFold(e.Type, catalog.TypePort) && e.Target == port && strings.EqualFold(e.Mode, proto) && e.Name == optionPortName(o)) {
			extra = append(extra, e)
		}
	}
	req.Extra = extra
	if !on {
		return req, nil
	}
	// A port the template has already is the server's anyway.
	items, err := deploy.Items(app, req)
	if err != nil {
		return req, err
	}
	for _, it := range items {
		if it.Type == catalog.TypePort && it.Target == port && strings.EqualFold(it.Mode, proto) && !it.Disabled {
			return req, nil
		}
	}
	req.Extra = append(req.Extra, deploy.ExtraConfig{Type: catalog.TypePort, Name: optionPortName(o), Target: port, Mode: proto, Value: strconv.Itoa(hostPort)})
	return req, nil
}
