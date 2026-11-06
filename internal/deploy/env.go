package deploy

import (
	"slices"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// EnvValue is the value a server's request gives a variable, through the
// template's item for it or an extra one; ok is false when it has none (or
// the template's item is turned off).
func EnvValue(app *catalog.App, req Request, name string) (string, bool) {
	items, err := Items(app, req)
	if err != nil {
		return "", false
	}
	for _, it := range items {
		if it.Type == catalog.TypeVariable && it.Target == name && !it.Disabled {
			return it.Effective, true
		}
	}
	return "", false
}

// SetEnv sets a variable in a server's request: the template's item for it
// (turned on if it was off), an extra variable already there, or a new
// extra one.
func SetEnv(app *catalog.App, req *Request, name, value string) {
	for _, cfg := range app.Config {
		if cfg.Type == catalog.TypeVariable && cfg.Target == name {
			if req.Values == nil {
				req.Values = map[string]string{}
			}
			req.Values[cfg.ID] = value
			req.Disabled = slices.DeleteFunc(slices.Clone(req.Disabled), func(id string) bool { return id == cfg.ID })
			return
		}
	}
	req.Extra = slices.Clone(req.Extra)
	for i, e := range req.Extra {
		if strings.EqualFold(e.Type, catalog.TypeVariable) && strings.TrimSpace(e.Target) == name {
			req.Extra[i].Value = value
			delete(req.Values, extraID(i))
			return
		}
	}
	req.Extra = append(req.Extra, ExtraConfig{Type: catalog.TypeVariable, Name: name, Target: name, Value: value})
}
