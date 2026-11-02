package catalog

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CustomCategory holds templates made from Docker images.
const CustomCategory = "Custom"

// ImageInfo is what FromImage reads from an image's configuration.
type ImageInfo struct {
	Ref          string            // the image reference it was pulled as
	Env          []string          // KEY=value defaults
	ExposedPorts []string          // e.g. "25565/tcp"
	Volumes      []string          // container paths the image declares as volumes
	Labels       map[string]string // OCI and other labels
	User         string
}

// envSkip are variables every base image sets that no one configures.
var envSkip = map[string]bool{
	"PATH": true, "HOME": true, "HOSTNAME": true, "TERM": true, "SHLVL": true, "PWD": true,
	"GPG_KEY": true, "GPG_KEYS": true, "DEBIAN_FRONTEND": true,
}

// envAdvanced matches variables that describe how the image was built (tool
// versions, locales, install locations) rather than settings to change.
var envAdvanced = regexp.MustCompile(`^(LANG|LANGUAGE|LC_.*|TZ|.*_VERSION|.*_HOME|.*_SHA(1|256|512)?|.*_URL|.*_DIR|.*PATH|JAVA_.*|PYTHON.*|NODE_.*|NPM_.*|GO.*|PIP_.*|DOTNET_.*|ASPNETCORE_.*|S6_.*)$`)

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

func imageSlug(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	base := path.Base(name)
	s := strings.Trim(nonSlugChars.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if s == "" {
		s = "image"
	}
	return s
}

// CustomID is the template ID for an image reference: importing the same
// reference again replaces its template.
func CustomID(ref string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(ref)))
	return "image-" + imageSlug(ref) + "-" + hex.EncodeToString(sum[:])[:8]
}

// FromImage builds a template from an image's own defaults: its exposed
// ports become ports published on the same host ports, its volumes become
// data folders under appdata, and its environment becomes variables.
func FromImage(info ImageInfo) *App {
	l := info.Labels
	slug := imageSlug(info.Ref)
	name := firstNonEmpty(l["org.opencontainers.image.title"], l["org.label-schema.name"], slug)
	app := &App{
		ID:         CustomID(info.Ref),
		Name:       name,
		Repository: strings.TrimSpace(info.Ref),
		FromImage:  strings.TrimSpace(info.Ref),
		Author:     firstNonEmpty(l["org.opencontainers.image.vendor"], l["org.opencontainers.image.authors"], l["maintainer"], l["org.label-schema.vendor"]),
		Project:    firstNonEmpty(l["org.opencontainers.image.url"], l["org.opencontainers.image.source"], l["org.label-schema.url"]),
		Support:    firstNonEmpty(l["org.opencontainers.image.documentation"], l["org.opencontainers.image.source"]),
		Icon:       l["net.unraid.docker.icon"],
		WebUI:      l["net.unraid.docker.webui"],
		Network:    "bridge",
		Categories: []string{CustomCategory},
		Config:     []Config{},
	}
	desc := firstNonEmpty(l["org.opencontainers.image.description"], l["org.label-schema.description"])
	note := "Made from the defaults built into " + app.Repository + ": its exposed ports, volumes and environment. " +
		"Check them before installing, since images don't say which settings matter."
	if info.User != "" && info.User != "root" && info.User != "0" {
		note += " The image runs as user " + info.User + ", so its data folders must be writable by that user."
	}
	app.Overview = strings.TrimSpace(desc + "\n\n" + note)

	ports := append([]string(nil), info.ExposedPorts...)
	sort.Slice(ports, func(i, j int) bool { return portLess(ports[i], ports[j]) })
	for _, p := range ports {
		port, proto, _ := strings.Cut(p, "/")
		if proto == "" {
			proto = "tcp"
		}
		if _, err := strconv.Atoi(port); err != nil {
			continue
		}
		app.Config = append(app.Config, Config{
			ID: "port:" + port + "/" + proto, Name: strings.ToUpper(proto) + " port " + port, Type: TypePort,
			Target: port, Default: port, Value: port, Mode: proto, Display: "always", Required: false,
			Description: "Exposed by the image. Published on the same port of the host by default.",
		})
	}

	vols := append([]string(nil), info.Volumes...)
	sort.Strings(vols)
	for _, v := range vols {
		v = path.Clean(v)
		if !path.IsAbs(v) || v == "/" {
			continue
		}
		folder := strings.Trim(nonSlugChars.ReplaceAllString(strings.ToLower(v), "-"), "-")
		host := "/mnt/user/appdata/" + slug + "/" + folder
		app.Config = append(app.Config, Config{
			ID: "path:" + v, Name: "Data " + v, Type: TypePath, Target: v, Default: host, Value: host,
			Mode: "rw", Display: "always", Required: true,
			Description: "Declared as a volume by the image, so its data should live outside the container.",
		})
	}

	seen := map[string]bool{}
	for _, kv := range info.Env {
		key, val, _ := strings.Cut(kv, "=")
		if key == "" || envSkip[key] || seen[key] {
			continue
		}
		seen[key] = true
		display := "always"
		if envAdvanced.MatchString(key) {
			display = "advanced"
		}
		lower := strings.ToLower(key)
		mask := strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.HasSuffix(lower, "_key")
		app.Config = append(app.Config, Config{
			ID: "variable:" + key, Name: key, Type: TypeVariable, Target: key, Default: val, Value: val,
			Display: display, Mask: mask,
			Description: "The image's default. Left unchanged, it isn't set on the container, so newer image versions can change it.",
		})
	}
	app.indexText()
	return app
}

func portLess(a, b string) bool {
	pa, _, _ := strings.Cut(a, "/")
	pb, _, _ := strings.Cut(b, "/")
	na, _ := strconv.Atoi(pa)
	nb, _ := strconv.Atoi(pb)
	if na != nb {
		return na < nb
	}
	return a < b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// UseCustomStore loads custom templates from path (if it exists) and saves
// new ones there.
func (c *Catalog) UseCustomStore(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.customPath = path
	c.custom = map[string]*App{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c.rebuild()
		return nil
	}
	if err != nil {
		return err
	}
	var list []*App
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, a := range list {
		a.indexText()
		c.custom[a.ID] = a
	}
	c.rebuild()
	return nil
}

func (c *Catalog) saveCustom() error {
	if c.customPath == "" {
		return errors.New("no store for custom templates")
	}
	list := make([]*App, 0, len(c.custom))
	for _, a := range c.custom {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.customPath), 0o755); err != nil {
		return err
	}
	tmp := c.customPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.customPath)
}

// AddCustom stores a custom template, replacing one with the same ID.
func (c *Catalog) AddCustom(app *App) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.custom == nil {
		c.custom = map[string]*App{}
	}
	prev, had := c.custom[app.ID]
	c.custom[app.ID] = app
	if err := c.saveCustom(); err != nil {
		if had {
			c.custom[app.ID] = prev
		} else {
			delete(c.custom, app.ID)
		}
		return err
	}
	c.rebuild()
	return nil
}

// ErrNotCustom is returned when deleting a template that came from the feed.
var ErrNotCustom = errors.New("only templates made from Docker images can be deleted")

// DeleteCustom removes a custom template.
func (c *Catalog) DeleteCustom(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.custom[id]
	if !ok {
		return ErrNotCustom
	}
	delete(c.custom, id)
	if err := c.saveCustom(); err != nil {
		c.custom[id] = prev
		return err
	}
	c.rebuild()
	return nil
}
