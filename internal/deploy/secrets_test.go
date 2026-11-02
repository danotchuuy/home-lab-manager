package deploy

import (
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

func TestHideSecrets(t *testing.T) {
	app := &catalog.App{ExtraParams: "-e ADMIN_PASSWORD=x", Config: []catalog.Config{
		{ID: "pw", Name: "Server password", Type: catalog.TypeVariable, Target: "SERVER_PASS"},
		{ID: "steam", Name: "Steam", Type: catalog.TypeVariable, Target: "STEAM_API_KEY", Value: "default-key"},
		{ID: "masked", Name: "Thing", Type: catalog.TypeVariable, Target: "THING", Mask: true},
		{ID: "motd", Name: "Message", Type: catalog.TypeVariable, Target: "MOTD"},
		{ID: "data", Name: "Data", Type: catalog.TypePath, Target: "/data"},
	}}
	extra, post := "--rcon-password x", "--password y"
	req := Request{
		Values:      map[string]string{"pw": "a", "masked": "b", "motd": "hello", "data": "/srv/data", "extra:0": "c", "extra:1": "d"},
		Extra:       []ExtraConfig{{Type: catalog.TypeVariable, Target: "DB_PASSWORD", Value: "c"}, {Type: catalog.TypeVariable, Target: "TZ", Value: "d"}},
		ExtraParams: &extra, PostArgs: &post,
	}
	shown, got, hidden := HideSecrets(app, req)
	want := []string{"extra:0", HiddenExtraParams, "masked", HiddenPostArgs, "pw", "steam"}
	slices.Sort(want)
	if !slices.Equal(hidden, want) {
		t.Errorf("hidden = %v, want %v", hidden, want)
	}
	for id, v := range map[string]string{"pw": "", "masked": "", "extra:0": "", "motd": "hello", "data": "/srv/data", "extra:1": "d"} {
		if got.Values[id] != v {
			t.Errorf("%s = %q, want %q", id, got.Values[id], v)
		}
	}
	if got.Extra[0].Value != "" || got.Extra[1].Value != "d" || got.ExtraParams != nil || got.PostArgs != nil {
		t.Errorf("extra = %+v", got)
	}
	if shown.Config[1].Value != "" || shown.ExtraParams != "" {
		t.Errorf("the template's own secrets show: %+v", shown)
	}
	// The originals are untouched.
	if req.Values["pw"] != "a" || req.Extra[0].Value != "c" || app.Config[1].Value != "default-key" {
		t.Error("changed what it was given")
	}
	// Without a template, nothing's known to be safe.
	_, got, hidden = HideSecrets(nil, Request{Values: map[string]string{"x": "1"}})
	if got.Values["x"] != "" || !slices.Contains(hidden, "x") {
		t.Errorf("without a template: %+v %v", got, hidden)
	}
}
