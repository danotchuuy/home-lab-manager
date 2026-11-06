package deploy

import (
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

func TestEnv(t *testing.T) {
	app := &catalog.App{Config: []catalog.Config{
		{ID: "mods", Type: catalog.TypeVariable, Target: "GAME_MOD_IDS", Value: "1,2"},
		{ID: "pw", Type: catalog.TypeVariable, Target: "PASSWORD", Value: "x"},
	}}
	req := Request{Disabled: []string{"mods"}, Extra: []ExtraConfig{{Type: "variable", Target: "OTHER", Value: "a"}}}
	if _, ok := EnvValue(app, req, "GAME_MOD_IDS"); ok {
		t.Error("a turned-off item has a value")
	}
	SetEnv(app, &req, "GAME_MOD_IDS", "3,4")
	if v, ok := EnvValue(app, req, "GAME_MOD_IDS"); !ok || v != "3,4" || len(req.Disabled) != 0 {
		t.Errorf("template item: %q %v %v", v, ok, req.Disabled)
	}
	SetEnv(app, &req, "OTHER", "b")
	if v, _ := EnvValue(app, req, "OTHER"); v != "b" || len(req.Extra) != 1 {
		t.Errorf("extra: %q %+v", v, req.Extra)
	}
	SetEnv(app, &req, "NEW_VAR", "c")
	if v, ok := EnvValue(app, req, "NEW_VAR"); !ok || v != "c" || len(req.Extra) != 2 {
		t.Errorf("new: %q %+v", v, req.Extra)
	}
	if v, _ := EnvValue(app, req, "PASSWORD"); v != "x" {
		t.Errorf("untouched: %q", v)
	}
}
