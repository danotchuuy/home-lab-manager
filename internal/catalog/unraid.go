package catalog

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"strings"
)

// Unraid keeps the template each of its containers was made from, filled
// in with the user's values, on its flash drive: the Community
// Applications template, plus whatever the user added or changed in
// Unraid's Docker page. hlm reads it when it adopts a container Unraid
// made, to keep its settings as the template describes them.

// UnraidTemplatesDir is where Unraid keeps its containers' templates;
// each is named my-<container name>.xml.
const UnraidTemplatesDir = "/boot/config/plugins/dockerMan/templates-user"

// UnraidManagedLabel is the label Unraid puts on the containers it made
// ("dockerman").
const UnraidManagedLabel = "net.unraid.docker.managed"

// UnraidTemplate is one of Unraid's user templates: the template it
// describes (with the user's values as each item's Value) and what is
// only in the user's copy.
type UnraidTemplate struct {
	App *App
	// Name is the container's name.
	Name string
	// MyIP is the container's fixed address on its network, if any.
	MyIP string
	// CPUSet is Unraid's CPU pinning (docker run --cpuset-cpus).
	CPUSet string
}

type unraidXML struct {
	XMLName     xml.Name `xml:"Container"`
	Name        string   `xml:"Name"`
	Repository  string   `xml:"Repository"`
	Registry    string   `xml:"Registry"`
	Network     string   `xml:"Network"`
	MyIP        string   `xml:"MyIP"`
	Shell       string   `xml:"Shell"`
	Privileged  string   `xml:"Privileged"`
	Support     string   `xml:"Support"`
	Project     string   `xml:"Project"`
	Overview    string   `xml:"Overview"`
	WebUI       string   `xml:"WebUI"`
	TemplateURL string   `xml:"TemplateURL"`
	Icon        string   `xml:"Icon"`
	ExtraParams string   `xml:"ExtraParams"`
	PostArgs    string   `xml:"PostArgs"`
	CPUset      string   `xml:"CPUset"`
	Requires    string   `xml:"Requires"`
	Config      []struct {
		Name        string `xml:"Name,attr"`
		Target      string `xml:"Target,attr"`
		Default     string `xml:"Default,attr"`
		Mode        string `xml:"Mode,attr"`
		Description string `xml:"Description,attr"`
		Type        string `xml:"Type,attr"`
		Display     string `xml:"Display,attr"`
		Required    string `xml:"Required,attr"`
		Mask        string `xml:"Mask,attr"`
		Value       string `xml:",chardata"`
	} `xml:"Config"`
}

// ParseUnraidTemplate reads one of Unraid's user templates. The App it
// returns has the user's values as its items' values, and the same item
// IDs as the Community Applications template it came from.
func ParseUnraidTemplate(data []byte) (UnraidTemplate, error) {
	var x unraidXML
	if err := xml.Unmarshal(data, &x); err != nil {
		return UnraidTemplate{}, errors.New("not an Unraid container template: " + err.Error())
	}
	if strings.TrimSpace(x.Repository) == "" {
		return UnraidTemplate{}, errors.New("the Unraid template names no image")
	}
	// Built as the CA feed has it, so IDs come out the same.
	cfgs := make([]rawConfig, 0, len(x.Config))
	for _, c := range x.Config {
		var rc rawConfig
		rc.Attributes.Name, rc.Attributes.Target, rc.Attributes.Default = c.Name, c.Target, c.Default
		rc.Attributes.Mode, rc.Attributes.Description, rc.Attributes.Type = c.Mode, c.Description, c.Type
		rc.Attributes.Display, rc.Attributes.Required, rc.Attributes.Mask = c.Display, c.Required, c.Mask
		rc.Value, _ = json.Marshal(c.Value)
		cfgs = append(cfgs, rc)
	}
	cfgJSON, _ := json.Marshal(cfgs)
	priv, _ := json.Marshal(strings.TrimSpace(x.Privileged))
	shell, _ := json.Marshal(x.Shell)
	ra := rawApp{
		Name: strings.TrimSpace(x.Name), Repository: x.Repository, Registry: x.Registry, Overview: x.Overview,
		Requires: x.Requires, Icon: x.Icon, Support: x.Support, Project: x.Project, TemplateURL: strings.TrimSpace(x.TemplateURL),
		WebUI: x.WebUI, Network: strings.TrimSpace(x.Network), Privileged: priv, Shell: shell,
		ExtraParams: x.ExtraParams, PostArgs: x.PostArgs, Config: cfgJSON,
	}
	app := normalize(&ra)
	// A user's value of "" is a value, not "use the default" (configs fills
	// those in for the feed, where Value is the template's suggestion).
	for i, c := range x.Config {
		if i < len(app.Config) && strings.TrimSpace(c.Value) == "" && normalizeType(c.Type) != "" {
			app.Config[i].Value = ""
		}
	}
	return UnraidTemplate{App: app, Name: ra.Name, MyIP: strings.TrimSpace(x.MyIP), CPUSet: strings.TrimSpace(x.CPUset)}, nil
}

// FindTemplate finds the Community Applications template an Unraid
// container was made from: the one at its template URL, or else the only
// one for its image (with the same name, if several are). Tag is the
// template's branch that image is, "" for its default image. Templates
// for another image don't count: the container would change.
func (c *Catalog) FindTemplate(templateURL, name, image string) (app *App, tag string, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var byURL, byImage, byName []*App
	tags := map[*App]string{}
	for _, a := range c.feedApps {
		t, same := imageTag(a, image)
		if !same {
			continue
		}
		tags[a] = t
		byImage = append(byImage, a)
		if templateURL != "" && a.TemplateURL == templateURL {
			byURL = append(byURL, a)
		}
		if strings.EqualFold(a.Name, name) {
			byName = append(byName, a)
		}
	}
	var pick *App
	switch {
	case len(byURL) > 0:
		pick = byURL[0]
	case len(byName) > 0:
		pick = byName[0]
	case len(byImage) == 1:
		pick = byImage[0]
	default:
		return nil, "", false
	}
	return pick, tags[pick], true
}

// imageTag says whether image is one of the template's: its default image
// (tag "") or one of its branches.
func imageTag(a *App, image string) (string, bool) {
	want := NormalizeImage(image)
	if NormalizeImage(a.Repository) == want {
		return "", true
	}
	for _, b := range a.Branches {
		ref := b.Repository
		if ref == "" {
			ref = withTag(a.Repository, b.Tag)
		}
		if NormalizeImage(ref) == want {
			return b.Tag, true
		}
	}
	return "", false
}

func withTag(ref, tag string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(ref), "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name + ":" + tag
}

// NormalizeImage writes an image reference the one way Docker reads it:
// docker.io and library/ spelled out, and :latest when no tag is given.
func NormalizeImage(ref string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	name, digest, hasDigest := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i <= strings.LastIndex(name, "/") && !hasDigest {
		name += ":latest"
	}
	host, rest, ok := strings.Cut(name, "/")
	switch {
	case !ok:
		name = "docker.io/library/" + name
	case host == "index.docker.io" || host == "docker.io":
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
		name = "docker.io/" + rest
	case !strings.ContainsAny(host, ".:") && host != "localhost":
		name = "docker.io/" + name
	}
	if hasDigest {
		name += "@" + digest
	}
	return name
}
