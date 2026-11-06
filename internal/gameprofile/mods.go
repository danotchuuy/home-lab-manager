package gameprofile

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Mods is how a game takes mods.
type Mods struct {
	// Modrinth installs mods from modrinth.com into a folder.
	Modrinth *ModrinthMods `yaml:"modrinth" json:"modrinth,omitempty"`
	// Workshop lists Steam Workshop items in the image's own variable; the
	// image downloads them.
	Workshop *WorkshopMods `yaml:"workshop" json:"workshop,omitempty"`
	// StartTimeout is how long the server gets, after a change, to answer
	// its status query again before hlm puts the mods back (default
	// DefaultStartTimeout).
	StartTimeout time.Duration `yaml:"start_timeout" json:"startTimeout,omitempty"`
}

// DefaultStartTimeout is how long a server gets to come back after a mod
// change, unless its profile says otherwise.
const DefaultStartTimeout = 5 * time.Minute

// Timeout is StartTimeout, or the default.
func (m *Mods) Timeout() time.Duration {
	if m == nil || m.StartTimeout <= 0 {
		return DefaultStartTimeout
	}
	return m.StartTimeout
}

// ModrinthMods says where a server keeps its mods, and which ones fit it.
type ModrinthMods struct {
	// LoaderEnv is the variable naming the server's mod loader (itzg's
	// TYPE), and LoaderDefault its value when it isn't set.
	LoaderEnv     string `yaml:"loader_env" json:"loaderEnv"`
	LoaderDefault string `yaml:"loader_default" json:"loaderDefault,omitempty"`
	// VersionEnv is the variable with the game version (itzg's VERSION).
	// When it's unset or names no version (LATEST), the version the
	// server reports to its status query is used.
	VersionEnv string `yaml:"version_env" json:"versionEnv,omitempty"`
	// Loaders are the loader variable's values (any case) that take mods,
	// and how.
	Loaders map[string]ModLoader `yaml:"loaders" json:"loaders"`
	// WarnEnv are variables that make the image install mods itself;
	// hlm warns when one is set, since the two can undo each other.
	WarnEnv []string `yaml:"warn_env" json:"warnEnv,omitempty"`
}

// ModLoader is one loader.
type ModLoader struct {
	// Modrinth are the Modrinth loaders whose files it loads, best first
	// (Quilt loads Fabric mods; Paper loads Spigot and Bukkit plugins).
	Modrinth []string `yaml:"modrinth" json:"modrinth"`
	// Dir is the folder (a container path) the files go in.
	Dir string `yaml:"dir" json:"dir"`
	// Plugins says they're called plugins (Paper and the like), not mods.
	Plugins bool `yaml:"plugins" json:"plugins,omitempty"`
}

var modrinthLoaderRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Loader finds the loader for the server's variables: its name (the
// variable's value, upper-case) and settings; ok is false when the server
// doesn't take mods.
func (m *ModrinthMods) Loader(env map[string]string) (string, ModLoader, bool) {
	name := strings.ToUpper(strings.TrimSpace(env[m.LoaderEnv]))
	if name == "" {
		name = strings.ToUpper(m.LoaderDefault)
	}
	for k, l := range m.Loaders {
		if strings.EqualFold(k, name) {
			return name, l, true
		}
	}
	return name, ModLoader{}, false
}

// WorkshopMods is how images of a game take Steam Workshop items.
type WorkshopMods struct {
	// AppID is the game's Steam app: items must be for it.
	AppID int `yaml:"app_id" json:"appId"`
	// Images are the images that take a list of items, and how.
	Images []WorkshopImage `yaml:"images" json:"images"`
}

// WorkshopImage is one image's variable for Workshop items.
type WorkshopImage struct {
	// Image is part of the image's name, e.g. hermsi/ark-server.
	Image string `yaml:"image" json:"image"`
	// Env is the variable with the items' IDs, Separator between them.
	Env       string `yaml:"env" json:"env"`
	Separator string `yaml:"separator" json:"separator"`
	// NamesEnv, if set, is a second variable with each item's mod name
	// (Project Zomboid's), in the same order.
	NamesEnv string `yaml:"names_env" json:"namesEnv,omitempty"`
}

// For finds the entry for an image, if any.
func (w *WorkshopMods) For(image string) (WorkshopImage, bool) {
	if w == nil {
		return WorkshopImage{}, false
	}
	for _, i := range w.Images {
		if strings.Contains(strings.ToLower(image), strings.ToLower(i.Image)) {
			return i, true
		}
	}
	return WorkshopImage{}, false
}

func (m *Mods) validate() error {
	if m.StartTimeout < 0 || m.StartTimeout > time.Hour {
		return errors.New("start_timeout: up to an hour")
	}
	if w := m.Workshop; w != nil {
		if w.AppID <= 0 {
			return errors.New("workshop: app_id is the game's Steam app ID")
		}
		if len(w.Images) == 0 {
			return errors.New("workshop: images needs at least one")
		}
		for _, i := range w.Images {
			if strings.TrimSpace(i.Image) == "" || !envRE.MatchString(i.Env) {
				return errors.New("workshop: an image needs its image and env")
			}
			if i.NamesEnv != "" && !envRE.MatchString(i.NamesEnv) {
				return fmt.Errorf("workshop: names_env %q isn't a variable name", i.NamesEnv)
			}
			if len(i.Separator) != 1 || strings.ContainsAny(i.Separator, "0123456789 \t\n") {
				return errors.New("workshop: separator is one character, such as , or ;")
			}
		}
	}
	mr := m.Modrinth
	if mr == nil {
		return nil
	}
	if !envRE.MatchString(mr.LoaderEnv) {
		return fmt.Errorf("modrinth: loader_env %q isn't a variable name", mr.LoaderEnv)
	}
	if mr.VersionEnv != "" && !envRE.MatchString(mr.VersionEnv) {
		return fmt.Errorf("modrinth: version_env %q isn't a variable name", mr.VersionEnv)
	}
	for _, e := range mr.WarnEnv {
		if !envRE.MatchString(e) {
			return fmt.Errorf("modrinth: warn_env %q isn't a variable name", e)
		}
	}
	if len(mr.Loaders) == 0 {
		return errors.New("modrinth: loaders needs at least one")
	}
	for k, l := range mr.Loaders {
		if !envRE.MatchString(k) {
			return fmt.Errorf("modrinth: loader %q: use letters, digits and _", k)
		}
		if len(l.Modrinth) == 0 {
			return fmt.Errorf("modrinth: loader %s needs its modrinth loaders", k)
		}
		for _, ml := range l.Modrinth {
			if !modrinthLoaderRE.MatchString(ml) {
				return fmt.Errorf("modrinth: loader %s: %q isn't a Modrinth loader name", k, ml)
			}
		}
		if !path.IsAbs(l.Dir) || path.Clean(l.Dir) != l.Dir || l.Dir == "/" {
			return fmt.Errorf("modrinth: loader %s: dir %q must be a clean absolute path below /", k, l.Dir)
		}
	}
	return nil
}
