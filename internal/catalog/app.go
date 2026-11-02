// Package catalog loads Unraid Community Applications templates and turns
// them into a searchable list of apps that can be deployed as containers.
package catalog

import (
	"strings"
)

// App is a normalized Community Applications Docker template.
type App struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Author         string   `json:"author,omitempty"`
	Maintainer     string   `json:"maintainer,omitempty"`
	Repo           string   `json:"repo,omitempty"` // CA template repository, e.g. "Nodiaque's Repository"
	Repository     string   `json:"repository"`     // Docker image reference
	Registry       string   `json:"registry,omitempty"`
	Overview       string   `json:"overview,omitempty"`
	Requires       string   `json:"requires,omitempty"`
	Icon           string   `json:"icon,omitempty"`
	Screenshots    []string `json:"screenshots,omitempty"`
	Support        string   `json:"support,omitempty"`
	Project        string   `json:"project,omitempty"`
	ReadMe         string   `json:"readme,omitempty"`
	TemplateURL    string   `json:"templateUrl,omitempty"`
	WebUI          string   `json:"webui,omitempty"`
	Network        string   `json:"network,omitempty"`
	Privileged     bool     `json:"privileged"`
	Shell          string   `json:"shell,omitempty"`
	ExtraParams    string   `json:"extraParams,omitempty"`
	PostArgs       string   `json:"postArgs,omitempty"`
	Categories     []string `json:"categories"`
	Branches       []Branch `json:"branches,omitempty"`
	DefaultTagDesc string   `json:"defaultTagDescription,omitempty"`
	Config         []Config `json:"config"`
	Downloads      int64    `json:"downloads,omitempty"`
	Stars          int64    `json:"stars,omitempty"`
	LastUpdate     int64    `json:"lastUpdate,omitempty"`
	FirstSeen      int64    `json:"firstSeen,omitempty"`
	Official       bool     `json:"official,omitempty"`
	// FromImage is set for templates made from a Docker image's own defaults
	// (see FromImage) rather than taken from the CA feed.
	FromImage  string `json:"fromImage,omitempty"`
	searchText string
}

// Branch is an alternative image tag the template offers.
type Branch struct {
	Tag         string `json:"tag"`
	Description string `json:"description,omitempty"`
	Repository  string `json:"repository,omitempty"` // full image ref, when it differs from Repository with Tag swapped in
}

// Config types used by Unraid templates.
const (
	TypePort     = "Port"
	TypePath     = "Path"
	TypeVariable = "Variable"
	TypeDevice   = "Device"
	TypeLabel    = "Label"
)

// Config is one configurable item of a template: a port, path, environment
// variable, device or label.
type Config struct {
	// ID identifies the item within its template. Install requests use it to
	// key values.
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Target      string   `json:"target"` // container port, container path, env var name, label key
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"` // allowed values, when Default is a "a|b|c" list
	Value       string   `json:"value"`
	Mode        string   `json:"mode,omitempty"` // tcp/udp for ports, rw/ro for paths
	Description string   `json:"description,omitempty"`
	Display     string   `json:"display,omitempty"` // "always" or "advanced"
	Required    bool     `json:"required"`
	Mask        bool     `json:"mask"`
}

// Summary is the short form of an App used in listings.
type Summary struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Author     string   `json:"author,omitempty"`
	Repo       string   `json:"repo,omitempty"`
	Repository string   `json:"repository"`
	Icon       string   `json:"icon,omitempty"`
	Overview   string   `json:"overview,omitempty"`
	Categories []string `json:"categories"`
	Downloads  int64    `json:"downloads,omitempty"`
	Stars      int64    `json:"stars,omitempty"`
	LastUpdate int64    `json:"lastUpdate,omitempty"`
	Official   bool     `json:"official,omitempty"`
}

// Summary returns the listing form of the app.
func (a *App) Summary() Summary {
	overview := a.Overview
	if len(overview) > 300 {
		overview = strings.TrimSpace(overview[:300]) + "…"
	}
	return Summary{
		ID:         a.ID,
		Name:       a.Name,
		Author:     a.Author,
		Repo:       a.Repo,
		Repository: a.Repository,
		Icon:       a.Icon,
		Overview:   overview,
		Categories: a.Categories,
		Downloads:  a.Downloads,
		Stars:      a.Stars,
		LastUpdate: a.LastUpdate,
		Official:   a.Official,
	}
}

// HasCategory reports whether the app is in category or one of its
// subcategories ("MediaApp" matches "MediaApp-Video").
func (a *App) HasCategory(category string) bool {
	for _, c := range a.Categories {
		if strings.EqualFold(c, category) || strings.HasPrefix(strings.ToLower(c), strings.ToLower(category)+"-") {
			return true
		}
	}
	return false
}

// Unraid's default appdata locations, rewritten to the local appdata root.
var unraidAppdataPrefixes = []string{"/mnt/user/appdata", "/mnt/cache/appdata"}

// Localize returns a copy of the app with Unraid-specific default host paths
// rewritten to live under appdataRoot.
func (a *App) Localize(appdataRoot string) *App {
	c := *a
	c.Config = make([]Config, len(a.Config))
	for i, cfg := range a.Config {
		if cfg.Type == TypePath {
			cfg.Value = rewriteAppdata(cfg.Value, appdataRoot)
			cfg.Default = rewriteAppdata(cfg.Default, appdataRoot)
		}
		c.Config[i] = cfg
	}
	return &c
}

func rewriteAppdata(p, root string) string {
	if root == "" {
		return p
	}
	for _, prefix := range unraidAppdataPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return strings.TrimRight(root, "/") + strings.TrimPrefix(p, prefix)
		}
	}
	return p
}
