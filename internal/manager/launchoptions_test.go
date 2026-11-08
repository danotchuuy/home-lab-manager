package manager

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

func nightingale(t *testing.T) (*gameprofile.Profile, gameprofile.LaunchOption) {
	t.Helper()
	g := gameprofile.Match("ghcr.io/ekman/nightingale:1")
	if g == nil {
		t.Fatal("no Nightingale profile")
	}
	o, ok := g.LaunchOption("status")
	if !ok {
		t.Fatal("no status option")
	}
	return g, o
}

const statusArgs = "-statusPort=7778 -ini:Engine:[HTTPServer.Listeners]:+ListenerOverrides=(Port=7778,BindAddress=0.0.0.0)"

// In the variable the image takes the game's arguments in, with the
// server's own arguments kept; the port as an extra one; and back out.
func TestLaunchOptionInVariable(t *testing.T) {
	g, o := nightingale(t)
	app := &catalog.App{ID: "n", Config: []catalog.Config{
		{ID: "game", Name: "Game port", Type: catalog.TypePort, Target: "7777", Mode: "udp", Value: "7777"},
		{ID: "params", Name: "Game parameters", Type: catalog.TypeVariable, Target: "GAME_PARAMS", Value: "-log"},
	}}
	req := deploy.Request{TemplateID: "n", Name: "ng", Values: map[string]string{"params": "-log -EnableCheats"}}
	if _, args, where := launchArgs(app, req, g); args != "-log -EnableCheats" || where != "the variable GAME_PARAMS" {
		t.Fatalf("args %q in %q", args, where)
	}
	on, err := applyLaunchOption(app, req, g, o, true, 7779)
	if err != nil {
		t.Fatal(err)
	}
	if got := on.Values["params"]; got != "-log -EnableCheats "+statusArgs {
		t.Errorf("params %q", got)
	}
	if len(on.Extra) != 1 || on.Extra[0].Target != "7778" || on.Extra[0].Mode != "tcp" || on.Extra[0].Value != "7779" || on.PostArgs != nil {
		t.Errorf("extra %+v, post %v", on.Extra, on.PostArgs)
	}
	if req.Values["params"] != "-log -EnableCheats" {
		t.Error("the original request changed")
	}
	// On twice is on once.
	again, _ := applyLaunchOption(app, on, g, o, true, 7779)
	if again.Values["params"] != on.Values["params"] || len(again.Extra) != 1 {
		t.Errorf("twice: %q %+v", again.Values["params"], again.Extra)
	}
	_, args, _ := launchArgs(app, on, g)
	if !optionOn(args, o) || optionOn("-log", o) {
		t.Error("optionOn")
	}
	off, _ := applyLaunchOption(app, on, g, o, false, 0)
	if got := off.Values["params"]; got != "-log -EnableCheats" || len(off.Extra) != 0 {
		t.Errorf("off: %q %+v", got, off.Extra)
	}
	// A different port set by hand is taken out too.
	hand := req
	hand.Values = map[string]string{"params": "-log -statusPort=9000 -ini:Engine:[HTTPServer.Listeners]:+ListenerOverrides=(Port=9000,BindAddress=0.0.0.0)"}
	if off, _ := applyLaunchOption(app, hand, g, o, false, 0); off.Values["params"] != "-log" {
		t.Errorf("by hand: %q", off.Values["params"])
	}
}

// Without such a variable, in its post arguments (the container's
// command); a template port it already has isn't added again.
func TestLaunchOptionInPostArgs(t *testing.T) {
	g, o := nightingale(t)
	app := &catalog.App{ID: "n", PostArgs: "-log", Config: []catalog.Config{
		{ID: "status", Name: "Status", Type: catalog.TypePort, Target: "7778", Mode: "tcp", Value: "7778"},
	}}
	req := deploy.Request{TemplateID: "n", Name: "ng"}
	on, err := applyLaunchOption(app, req, g, o, true, 7778)
	if err != nil {
		t.Fatal(err)
	}
	if on.PostArgs == nil || *on.PostArgs != "-log "+statusArgs || len(on.Extra) != 0 {
		t.Errorf("post %v, extra %+v", on.PostArgs, on.Extra)
	}
	off, _ := applyLaunchOption(app, on, g, o, false, 0)
	if *off.PostArgs != "-log" {
		t.Errorf("off: %q", *off.PostArgs)
	}
	// The arguments split as Build reads them.
	spec, err := deploy.Build(&catalog.App{ID: "n", Repository: "x"}, deploy.Request{Name: "ng", PostArgs: on.PostArgs}, deploy.Env{})
	if err != nil || len(spec.Create.Cmd) != 3 || !strings.HasPrefix(spec.Create.Cmd[2], "-ini:Engine:[HTTPServer.Listeners]") {
		t.Errorf("cmd %q %v", spec.Create.Cmd, err)
	}
}
