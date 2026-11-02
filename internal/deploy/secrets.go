package deploy

import (
	"maps"
	"regexp"
	"slices"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// secretName matches variable names that usually hold secrets:
// SERVER_PASSWORD, RCON_PASS, API_KEY, JWT_SECRET, AUTH_TOKEN and the like.
var secretName = regexp.MustCompile(`(?i)TOKEN|SECRET|PASS|PWD|KEY|CREDENTIAL|AUTH|PRIVATE`)

// Hidden settings that aren't items.
const (
	HiddenExtraParams = "extraParams"
	HiddenPostArgs    = "postArgs"
)

func secretItem(cfg catalog.Config) bool {
	return cfg.Mask || secretName.MatchString(cfg.Target) || secretName.MatchString(cfg.Name)
}

// HideSecrets returns copies of a server's template and settings without
// the values of variables that look like secrets, or that the template
// masks, and without ExtraParams and PostArgs, where `-e PASSWORD=…` or
// `--password …` can hide. It lists what it blanked, by item ID or as
// HiddenExtraParams and HiddenPostArgs. Without a template, the names aren't
// known, so every value is hidden.
func HideSecrets(app *catalog.App, req Request) (*catalog.App, Request, []string) {
	var hidden []string
	req.Values = maps.Clone(req.Values)
	if app != nil {
		a := *app
		a.Config = slices.Clone(app.Config)
		for i, cfg := range a.Config {
			if cfg.Type != catalog.TypeVariable || !secretItem(cfg) {
				continue
			}
			_, set := req.Values[cfg.ID]
			if set || cfg.Value != "" || cfg.Default != "" {
				hidden = append(hidden, cfg.ID)
			}
			if set {
				req.Values[cfg.ID] = ""
			}
			a.Config[i].Value, a.Config[i].Default = "", ""
		}
		if a.ExtraParams != "" {
			a.ExtraParams = ""
			hidden = append(hidden, HiddenExtraParams)
		}
		if a.PostArgs != "" {
			a.PostArgs = ""
			hidden = append(hidden, HiddenPostArgs)
		}
		app = &a
	} else {
		for k := range req.Values {
			req.Values[k] = ""
			hidden = append(hidden, k)
		}
	}
	req.Extra = slices.Clone(req.Extra)
	for i, e := range req.Extra {
		if e.Type != catalog.TypeVariable || !(app == nil || secretName.MatchString(e.Target) || secretName.MatchString(e.Name)) {
			continue
		}
		id := extraID(i)
		req.Extra[i].Value = ""
		if _, set := req.Values[id]; set {
			req.Values[id] = ""
		}
		if !slices.Contains(hidden, id) {
			hidden = append(hidden, id)
		}
	}
	if req.ExtraParams != nil {
		req.ExtraParams = nil
		if !slices.Contains(hidden, HiddenExtraParams) {
			hidden = append(hidden, HiddenExtraParams)
		}
	}
	if req.PostArgs != nil {
		req.PostArgs = nil
		if !slices.Contains(hidden, HiddenPostArgs) {
			hidden = append(hidden, HiddenPostArgs)
		}
	}
	slices.Sort(hidden)
	return app, req, hidden
}
