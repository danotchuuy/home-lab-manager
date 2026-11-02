package deploy

import (
	"cmp"
	"reflect"
	"slices"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// Changing a server's settings can give full control of the Docker host: a
// privileged container, a mount of / or docker.sock, host networking, another
// template with any of those. RestrictedChanges is what keeps someone who
// manages a server from doing that, so anything it doesn't know is safe is
// restricted.

// freeFields are the fields of Request anyone who manages a server may
// change. Values is checked item by item.
var freeFields = map[string]bool{
	"Name":          true, // recreating a server keeps its name
	"Tag":           true, // only the template's own branches are accepted
	"RestartPolicy": true, // the live policy wins, and it has its own switch
}

// lockedFields are the other fields of Request, as what they come to after
// the template's defaults: what's sent can look different and mean the same.
var lockedFields = map[string]func(*catalog.App, Request) any{
	"TemplateID": func(app *catalog.App, r Request) any { return cmp.Or(r.TemplateID, app.ID) },
	"Network": func(app *catalog.App, r Request) any {
		return cmp.Or(strings.TrimSpace(r.Network), strings.TrimSpace(app.Network), "bridge")
	},
	"IPAddress": func(_ *catalog.App, r Request) any { return strings.TrimSpace(r.IPAddress) },
	"Disabled": func(_ *catalog.App, r Request) any {
		ids := slices.Sorted(slices.Values(r.Disabled))
		return strings.Join(slices.Compact(ids), "\x00")
	},
	"Extra": func(_ *catalog.App, r Request) any {
		if len(r.Extra) == 0 {
			return []ExtraConfig(nil)
		}
		return r.Extra
	},
	"ExtraParams": func(app *catalog.App, r Request) any { return orDefault(r.ExtraParams, app.ExtraParams) },
	"PostArgs":    func(app *catalog.App, r Request) any { return orDefault(r.PostArgs, app.PostArgs) },
	"Privileged":  func(app *catalog.App, r Request) any { return orDefault(r.Privileged, app.Privileged) },
}

// RestrictedChanges lists what differs between a server's request and a new
// one that takes an admin to change. Only the values of a template's Variable
// and Port items, the image branch and the restart policy are unrestricted.
func RestrictedChanges(app *catalog.App, old, next Request) []string {
	var out []string
	before, after := reflect.ValueOf(old), reflect.ValueOf(next)
	for _, f := range reflect.VisibleFields(before.Type()) {
		effective, locked := lockedFields[f.Name]
		switch {
		case f.Name == "Values" || freeFields[f.Name]:
		case locked:
			if !reflect.DeepEqual(effective(app, old), effective(app, next)) {
				out = append(out, f.Name)
			}
		case !reflect.DeepEqual(before.FieldByIndex(f.Index).Interface(), after.FieldByIndex(f.Index).Interface()):
			out = append(out, f.Name)
		}
	}
	return append(out, restrictedValues(app, old, next)...)
}

// restrictedValues lists the items in Values that may not change: Build also
// reads them for paths, devices and labels, and for items in Extra, whose
// keys aren't in the template.
func restrictedValues(app *catalog.App, old, next Request) []string {
	var out []string
	known := map[string]bool{}
	for _, cfg := range app.Config {
		known[cfg.ID] = true
		if cfg.Type == catalog.TypeVariable || cfg.Type == catalog.TypePort {
			continue
		}
		value := func(r Request) string {
			v, ok := r.Values[cfg.ID]
			if !ok {
				v = cfg.Value
			}
			return strings.TrimSpace(v)
		}
		if value(old) != value(next) {
			out = append(out, cmp.Or(cfg.Name, cfg.ID))
		}
	}
	keys := map[string]bool{}
	for k := range old.Values {
		keys[k] = true
	}
	for k := range next.Values {
		keys[k] = true
	}
	var other []string
	for k := range keys {
		before, had := old.Values[k]
		after, has := next.Values[k]
		if !known[k] && (had != has || before != after) {
			other = append(other, k)
		}
	}
	slices.Sort(other)
	return append(out, other...)
}
