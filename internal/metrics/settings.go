package metrics

import (
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretSettings = "metrics"

// Settings are /metrics' settings.
type Settings struct {
	// AllowFrom lists addresses and ranges that may scrape /metrics without
	// a key. Empty (the default): a key is always needed.
	AllowFrom []string `json:"allowFrom"`
}

// Config keeps the settings in the secrets store.
type Config struct {
	store *secrets.Store
	mu    sync.Mutex
	cache *Settings
}

// NewConfig returns the settings kept in store.
func NewConfig(store *secrets.Store) *Config { return &Config{store: store} }

// Get returns the settings.
func (c *Config) Get() Settings {
	if c == nil {
		return Settings{AllowFrom: []string{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		var s Settings
		if c.store != nil {
			_, _ = c.store.Get(secretSettings, &s)
		}
		c.cache = &s
	}
	out := *c.cache
	if out.AllowFrom == nil {
		out.AllowFrom = []string{}
	}
	return out
}

// Set checks and saves the settings.
func (c *Config) Set(in Settings) (Settings, error) {
	list, err := auth.ParseAllowFrom(in.AllowFrom)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{AllowFrom: list}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.Set(secretSettings, s); err != nil {
		return Settings{}, err
	}
	c.cache = &s
	if s.AllowFrom == nil {
		s.AllowFrom = []string{}
	}
	return s, nil
}
