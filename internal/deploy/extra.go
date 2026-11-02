package deploy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// ExtraConfig is a port, path, variable, label or device the user added to a
// server in addition to the template's own.
type ExtraConfig struct {
	Type   string `json:"type"` // Port, Path, Variable, Label or Device
	Name   string `json:"name,omitempty"`
	Target string `json:"target"` // container port, container path, variable name, label key; unused for devices
	Value  string `json:"value"`  // host port, host path, variable value, label value, host device
	Mode   string `json:"mode,omitempty"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// extraConfigs validates the user's extra items and turns them into config
// items that Build handles exactly like the template's.
func extraConfigs(extra []ExtraConfig) ([]catalog.Config, error) {
	out := make([]catalog.Config, 0, len(extra))
	for i, e := range extra {
		cfg := catalog.Config{
			ID:       "extra:" + strconv.Itoa(i),
			Name:     strings.TrimSpace(e.Name),
			Target:   strings.TrimSpace(e.Target),
			Value:    strings.TrimSpace(e.Value),
			Mode:     strings.ToLower(strings.TrimSpace(e.Mode)),
			Required: true,
		}
		label := func() string {
			if cfg.Name != "" {
				return cfg.Name
			}
			return fmt.Sprintf("additional %s #%d", strings.ToLower(e.Type), i+1)
		}

		switch strings.ToLower(strings.TrimSpace(e.Type)) {
		case "port":
			cfg.Type = catalog.TypePort
			if _, err := parsePortRange(cfg.Target); err != nil {
				return nil, fmt.Errorf("%s: container port: %w", label(), err)
			}
			switch cfg.Mode {
			case "", "tcp":
				cfg.Mode = "tcp"
			case "udp":
			default:
				return nil, fmt.Errorf("%s: protocol must be tcp or udp, not %q", label(), cfg.Mode)
			}
		case "path":
			cfg.Type = catalog.TypePath
			if !strings.HasPrefix(cfg.Target, "/") {
				return nil, fmt.Errorf("%s: container path must be absolute, got %q", label(), cfg.Target)
			}
			switch cfg.Mode {
			case "":
				cfg.Mode = "rw"
			case "rw", "ro", "rw,slave", "rw,shared", "ro,slave", "ro,rslave", "rw,rslave", "rw,rshared":
			default:
				return nil, fmt.Errorf("%s: access mode %q is not supported", label(), cfg.Mode)
			}
		case "variable":
			cfg.Type = catalog.TypeVariable
			if !envName.MatchString(cfg.Target) {
				return nil, fmt.Errorf("%s: %q is not a valid variable name", label(), cfg.Target)
			}
			cfg.Required = false // an empty value is a valid setting
		case "label":
			cfg.Type = catalog.TypeLabel
			if cfg.Target == "" || strings.ContainsAny(cfg.Target, "= \t") {
				return nil, fmt.Errorf("%s: %q is not a valid label key", label(), cfg.Target)
			}
			if strings.HasPrefix(cfg.Target, "io.hlm.") || strings.HasPrefix(cfg.Target, "io.gsh.") {
				return nil, fmt.Errorf("%s: io.hlm.* and io.gsh.* labels are reserved", label())
			}
			cfg.Required = false
		case "device":
			cfg.Type = catalog.TypeDevice
			if !strings.HasPrefix(cfg.Value, "/dev/") {
				return nil, fmt.Errorf("%s: device must be a path under /dev, got %q", label(), cfg.Value)
			}
		default:
			return nil, fmt.Errorf("additional item #%d: unknown type %q", i+1, e.Type)
		}
		if cfg.Name == "" {
			cfg.Name = label()
		}
		out = append(out, cfg)
	}
	return out, nil
}

func orDefault[T any](override *T, def T) T {
	if override != nil {
		return *override
	}
	return def
}
