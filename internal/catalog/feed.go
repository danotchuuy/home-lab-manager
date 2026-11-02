package catalog

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// The CA application feed is generated from Unraid's XML templates, so many
// fields come through with inconsistent JSON types: a value might be a string
// in one app and a list or object in another. Fields like that are decoded as
// raw JSON and normalized by hand.

type rawFeed struct {
	LastUpdated string            `json:"last_updated"`
	Apps        []json.RawMessage `json:"applist"`
}

type rawApp struct {
	Name                  string          `json:"Name"`
	Author                string          `json:"Author"`
	Maintainer            json.RawMessage `json:"Maintainer"`
	Repo                  string          `json:"Repo"`
	Repository            string          `json:"Repository"`
	Registry              string          `json:"Registry"`
	Overview              string          `json:"Overview"`
	Description           string          `json:"Description"`
	Requires              string          `json:"Requires"`
	Icon                  string          `json:"Icon"`
	Screenshot            json.RawMessage `json:"Screenshot"`
	Support               string          `json:"Support"`
	Project               string          `json:"Project"`
	ReadMe                string          `json:"ReadMe"`
	TemplateURL           string          `json:"TemplateURL"`
	TemplatePath          string          `json:"TemplatePath"`
	WebUI                 string          `json:"WebUI"`
	Network               string          `json:"Network"`
	Privileged            json.RawMessage `json:"Privileged"`
	Shell                 json.RawMessage `json:"Shell"`
	ExtraParams           string          `json:"ExtraParams"`
	PostArgs              string          `json:"PostArgs"`
	Category              string          `json:"Category"`
	CategoryList          []string        `json:"CategoryList"`
	Branch                json.RawMessage `json:"Branch"`
	DefaultTagDescription string          `json:"DefaultTagDescription"`
	Config                json.RawMessage `json:"Config"`
	Downloads             json.RawMessage `json:"downloads"`
	Stars                 json.RawMessage `json:"stars"`
	LastUpdate            json.RawMessage `json:"LastUpdate"`
	FirstSeen             json.RawMessage `json:"FirstSeen"`
	Official              json.RawMessage `json:"Official"`
	Plugin                json.RawMessage `json:"Plugin"`
	Blacklist             json.RawMessage `json:"Blacklist"`
	Deprecated            json.RawMessage `json:"Deprecated"`
}

type rawConfig struct {
	Attributes struct {
		Name        string `json:"Name"`
		Target      string `json:"Target"`
		Default     string `json:"Default"`
		Mode        string `json:"Mode"`
		Description string `json:"Description"`
		Type        string `json:"Type"`
		Display     string `json:"Display"`
		Required    string `json:"Required"`
		Mask        string `json:"Mask"`
	} `json:"@attributes"`
	Value json.RawMessage `json:"value"`
}

// parseFeed decodes the CA application feed and returns the Docker apps in it.
// Plugins, blacklisted and deprecated apps are left out.
func parseFeed(r io.Reader) ([]*App, string, error) {
	var feed rawFeed
	if err := json.NewDecoder(r).Decode(&feed); err != nil {
		return nil, "", fmt.Errorf("decode feed: %w", err)
	}
	apps := make([]*App, 0, len(feed.Apps))
	seen := make(map[string]bool, len(feed.Apps))
	for _, raw := range feed.Apps {
		var ra rawApp
		if err := json.Unmarshal(raw, &ra); err != nil {
			continue // skip malformed entries rather than failing the whole feed
		}
		if ra.Name == "" || ra.Repository == "" ||
			asBool(ra.Plugin) || asBool(ra.Blacklist) || asBool(ra.Deprecated) {
			continue
		}
		app := normalize(&ra)
		if seen[app.ID] {
			continue
		}
		seen[app.ID] = true
		apps = append(apps, app)
	}
	return apps, feed.LastUpdated, nil
}

func normalize(ra *rawApp) *App {
	overview := ra.Overview
	if overview == "" {
		overview = ra.Description
	}
	app := &App{
		ID:             appID(ra),
		Name:           ra.Name,
		Author:         ra.Author,
		Maintainer:     maintainerName(ra.Maintainer),
		Repo:           ra.Repo,
		Repository:     strings.TrimSpace(ra.Repository),
		Registry:       ra.Registry,
		Overview:       cleanText(overview),
		Requires:       cleanText(ra.Requires),
		Icon:           ra.Icon,
		Screenshots:    asStrings(ra.Screenshot),
		Support:        ra.Support,
		Project:        ra.Project,
		ReadMe:         ra.ReadMe,
		TemplateURL:    ra.TemplateURL,
		WebUI:          ra.WebUI,
		Network:        ra.Network,
		Privileged:     asBool(ra.Privileged),
		Shell:          asString(ra.Shell),
		ExtraParams:    ra.ExtraParams,
		PostArgs:       ra.PostArgs,
		Categories:     categories(ra),
		Branches:       branches(ra.Branch),
		DefaultTagDesc: ra.DefaultTagDescription,
		Config:         configs(ra.Config),
		Downloads:      asInt(ra.Downloads),
		Stars:          asInt(ra.Stars),
		LastUpdate:     asInt(ra.LastUpdate),
		FirstSeen:      asInt(ra.FirstSeen),
		Official:       asBool(ra.Official),
	}
	if app.Author == "" {
		app.Author = app.Maintainer
	}
	app.indexText()
	return app
}

// indexText prepares the text Search matches against.
func (a *App) indexText() {
	a.searchText = strings.ToLower(strings.Join([]string{
		a.Name, a.Author, a.Repo, a.Repository, a.Overview,
	}, "\n"))
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// appID builds an ID that is readable and stays the same across feed
// refreshes: a slug of the name plus a short hash of the template's path.
func appID(ra *rawApp) string {
	key := ra.TemplatePath
	if key == "" {
		key = ra.Repo + "\x00" + ra.Name + "\x00" + ra.Repository
	}
	sum := sha1.Sum([]byte(key))
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(ra.Name), "-"), "-")
	if slug == "" {
		slug = "app"
	}
	return slug + "-" + hex.EncodeToString(sum[:])[:10]
}

func categories(ra *rawApp) []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		// The legacy Category field looks like "Tools:System Backup:"; the newer
		// CategoryList uses "Tools-System".
		c = strings.ReplaceAll(strings.TrimSuffix(strings.TrimSpace(c), ":"), ":", "-")
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range ra.CategoryList {
		for _, part := range strings.Split(c, ";") {
			add(part)
		}
	}
	if len(out) == 0 {
		for _, c := range strings.Fields(ra.Category) {
			add(c)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func branches(raw json.RawMessage) []Branch {
	type rawBranch struct {
		Tag            string `json:"Tag"`
		TagDescription string `json:"TagDescription"`
		Repository     string `json:"Repository"`
	}
	var list []rawBranch
	if json.Unmarshal(raw, &list) != nil {
		var one rawBranch
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []rawBranch{one}
	}
	var out []Branch
	for _, b := range list {
		if b.Tag == "" {
			continue
		}
		out = append(out, Branch{Tag: b.Tag, Description: b.TagDescription, Repository: b.Repository})
	}
	return out
}

func configs(raw json.RawMessage) []Config {
	var list []rawConfig
	if json.Unmarshal(raw, &list) != nil {
		var one rawConfig
		if json.Unmarshal(raw, &one) != nil {
			return []Config{}
		}
		list = []rawConfig{one}
	}

	out := make([]Config, 0, len(list))
	ids := map[string]int{}
	for _, rc := range list {
		at := rc.Attributes
		typ := normalizeType(at.Type)
		if typ == "" {
			continue
		}
		c := Config{
			Name:        at.Name,
			Type:        typ,
			Target:      strings.TrimSpace(at.Target),
			Default:     at.Default,
			Value:       asString(rc.Value),
			Mode:        strings.ToLower(strings.TrimSpace(at.Mode)),
			Description: cleanText(at.Description),
			Display:     normalizeDisplay(at.Display),
			Required:    strings.EqualFold(at.Required, "true"),
			Mask:        strings.EqualFold(at.Mask, "true") || strings.EqualFold(at.Display, "password"),
		}
		if c.Name == "" {
			c.Name = c.Target
		}
		// A default such as "false|true" lists the allowed values.
		if strings.Contains(c.Default, "|") {
			c.Options = strings.Split(c.Default, "|")
			c.Default = c.Options[0]
		}
		if c.Value == "" && c.Options == nil {
			c.Value = c.Default
		} else if c.Value == "" {
			c.Value = c.Options[0]
		}
		if c.Type == TypePort && c.Mode != "udp" {
			c.Mode = "tcp"
		}
		if c.Type == TypePath && c.Mode == "" {
			c.Mode = "rw"
		}

		base := strings.ToLower(c.Type) + ":" + c.Target
		if c.Type == TypePort {
			base += "/" + c.Mode
		}
		if c.Target == "" {
			base = strings.ToLower(c.Type) + ":" + c.Name
		}
		ids[base]++
		c.ID = base
		if n := ids[base]; n > 1 {
			c.ID = base + "#" + strconv.Itoa(n)
		}
		out = append(out, c)
	}
	return out
}

func normalizeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "port":
		return TypePort
	case "path":
		return TypePath
	case "variable", "textarea":
		return TypeVariable
	case "device":
		return TypeDevice
	case "label":
		return TypeLabel
	}
	return ""
}

func normalizeDisplay(d string) string {
	if strings.HasPrefix(strings.ToLower(d), "advanced") || strings.EqualFold(d, "hidden") {
		return "advanced"
	}
	return "always"
}

func maintainerName(raw json.RawMessage) string {
	if s := asString(raw); s != "" {
		return s
	}
	var m struct {
		Name string `json:"Name"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Name
}

// cleanText tidies up template descriptions, which often contain Windows
// line endings and leftover HTML line breaks.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	for _, br := range []string{"<br>", "<br/>", "<br />", "[br]"} {
		s = strings.ReplaceAll(s, br, "\n")
	}
	return strings.TrimSpace(s)
}

func asString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return list[0]
	}
	return ""
}

func asStrings(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	if s := asString(raw); s != "" {
		return []string{s}
	}
	return nil
}

func asBool(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	return strings.EqualFold(asString(raw), "true")
}

func asInt(raw json.RawMessage) int64 {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int64(f)
	}
	n, _ := strconv.ParseInt(asString(raw), 10, 64)
	return n
}
