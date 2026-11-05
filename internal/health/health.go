// Package health keeps servers' health settings (crash-loop limits, custom
// health checks, restarting servers whose health fails), and restarts
// servers that have been unhealthy for too long.
package health

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "health"

// Kinds of custom check.
const (
	CheckImage   = ""        // the image's (or template's) own, if any
	CheckNone    = "none"    // no check, even if the image has one
	CheckTCP     = "tcp"     // a TCP port in the container accepts connections
	CheckHTTP    = "http"    // an HTTP GET in the container answers 2xx or 3xx
	CheckCommand = "command" // a shell command in the container exits 0
)

// Signals a restart can go by.
const (
	SignalDocker = ""      // Docker's health check says unhealthy
	SignalQuery  = "query" // the game's status query stopped answering
)

// Defaults for custom checks: game servers can take many minutes to start.
const (
	DefaultInterval    = 60 // seconds
	DefaultTimeout     = 10 // seconds
	DefaultRetries     = 3
	DefaultStartPeriod = 10 // minutes
)

// CrashLoop sets a server's crash-loop limit.
type CrashLoop struct {
	Off bool `json:"off,omitempty"`
	// Exits within Minutes stop the server; 0 uses the default.
	Exits   int `json:"exits,omitempty"`
	Minutes int `json:"minutes,omitempty"`
}

// Check is a custom health check.
type Check struct {
	Kind    string `json:"kind"`
	Port    int    `json:"port,omitempty"`    // tcp, http: in the container
	Path    string `json:"path,omitempty"`    // http
	Command string `json:"command,omitempty"` // command, run with /bin/sh -c
	// Timings; 0 uses the defaults.
	Interval int `json:"interval,omitempty"` // seconds between checks
	Timeout  int `json:"timeout,omitempty"`  // seconds a check may take
	Retries  int `json:"retries,omitempty"`  // failures in a row that make it unhealthy
	// StartPeriod is the minutes after a start when failures don't count;
	// nil uses the default (0 is none).
	StartPeriod *int `json:"startPeriod,omitempty"`
}

// Settings are one server's health settings.
type Settings struct {
	CrashLoop CrashLoop `json:"crashLoop"`
	Check     Check     `json:"check"`
	// RestartAfter restarts the server once it has been unhealthy for this
	// many minutes; 0 is off.
	RestartAfter int    `json:"restartAfter,omitempty"`
	Signal       string `json:"signal,omitempty"`
}

// Store keeps servers' health settings. It's the manager's HealthPolicy.
type Store struct {
	secrets *secrets.Store
	mu      sync.Mutex
}

// NewStore returns a store keeping settings in s.
func NewStore(s *secrets.Store) *Store { return &Store{secrets: s} }

var _ manager.HealthPolicy = (*Store)(nil)

func (s *Store) all() (map[string]Settings, error) {
	m := map[string]Settings{}
	_, err := s.secrets.Get(secretName, &m)
	return m, err
}

// Get returns a server's settings.
func (s *Store) Get(server string) (Settings, error) {
	all, err := s.all()
	return all[server], err
}

// All returns every server's settings that aren't the defaults.
func (s *Store) All() (map[string]Settings, error) { return s.all() }

// Set saves a server's settings, after checking them.
func (s *Store) Set(server string, set Settings) (Settings, error) {
	set, err := set.Validate()
	if err != nil {
		return set, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.all()
	if err != nil {
		return set, err
	}
	if set == (Settings{}) {
		delete(all, server)
	} else {
		all[server] = set
	}
	return set, s.secrets.Set(secretName, all)
}

// ForgetServer removes a deleted server's settings.
func (s *Store) ForgetServer(server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.all()
	if err != nil {
		return err
	}
	if _, ok := all[server]; !ok {
		return nil
	}
	delete(all, server)
	return s.secrets.Set(secretName, all)
}

// CrashLoopLimit implements manager.HealthPolicy.
func (s *Store) CrashLoopLimit(server string) (int, time.Duration) {
	set, _ := s.Get(server)
	c := set.CrashLoop.withDefaults()
	if c.Off {
		return 0, 0
	}
	return c.Exits, time.Duration(c.Minutes) * time.Minute
}

// Healthcheck implements manager.HealthPolicy.
func (s *Store) Healthcheck(server string) *docker.Healthcheck {
	set, _ := s.Get(server)
	return set.Check.Healthcheck()
}

func (c CrashLoop) withDefaults() CrashLoop {
	if c.Exits == 0 {
		c.Exits = manager.DefaultCrashLoopExits
	}
	if c.Minutes == 0 {
		c.Minutes = int(manager.DefaultCrashLoopWindow / time.Minute)
	}
	return c
}

// A path is put in single quotes in a shell command, so it can't have one.
var validPath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&()*+,;=:@%/?#\[\]-]*$`)

// Validate checks settings and tidies them up.
func (set Settings) Validate() (Settings, error) {
	c := &set.CrashLoop
	if c.Exits != 0 && (c.Exits < 2 || c.Exits > 100) {
		return set, errors.New("the crash-loop limit is 2 to 100 exits")
	}
	if c.Minutes != 0 && (c.Minutes < 1 || c.Minutes > 1440) {
		return set, errors.New("the crash-loop window is 1 to 1440 minutes")
	}
	// Defaults are stored as 0, so changing a default changes them.
	if c.Exits == manager.DefaultCrashLoopExits {
		c.Exits = 0
	}
	if c.Minutes == int(manager.DefaultCrashLoopWindow/time.Minute) {
		c.Minutes = 0
	}
	if c.Off {
		c.Exits, c.Minutes = 0, 0
	}

	k := &set.Check
	k.Path, k.Command = strings.TrimSpace(k.Path), strings.TrimSpace(k.Command)
	switch k.Kind {
	case CheckImage, CheckNone:
		*k = Check{Kind: k.Kind}
	case CheckTCP, CheckHTTP:
		if k.Port < 1 || k.Port > 65535 {
			return set, errors.New("enter the port to check, as the server listens on it in its container")
		}
		k.Command = ""
		if k.Kind == CheckTCP {
			k.Path = ""
		} else {
			if k.Path == "" {
				k.Path = "/"
			}
			if !validPath.MatchString(k.Path) {
				return set, errors.New("the path starts with / and can't have spaces or quotes")
			}
		}
	case CheckCommand:
		if k.Command == "" {
			return set, errors.New("enter the command to run")
		}
		if len(k.Command) > 1000 {
			return set, errors.New("the command is too long")
		}
		k.Port, k.Path = 0, ""
	default:
		return set, fmt.Errorf("unknown kind of health check %q", k.Kind)
	}
	if k.Kind != CheckImage && k.Kind != CheckNone {
		for _, r := range []struct {
			name     string
			v        *int
			min, max int
			def      int
		}{
			{"interval", &k.Interval, 5, 3600, DefaultInterval},
			{"timeout", &k.Timeout, 1, 300, DefaultTimeout},
			{"retries", &k.Retries, 1, 20, DefaultRetries},
		} {
			if *r.v == r.def {
				*r.v = 0
			}
			if *r.v != 0 && (*r.v < r.min || *r.v > r.max) {
				return set, fmt.Errorf("the %s is %d to %d", r.name, r.min, r.max)
			}
		}
		if p := k.StartPeriod; p != nil {
			if *p < 0 || *p > 120 {
				return set, errors.New("the start period is 0 to 120 minutes")
			}
			if *p == DefaultStartPeriod {
				k.StartPeriod = nil
			}
		}
	}

	if set.RestartAfter != 0 && (set.RestartAfter < 1 || set.RestartAfter > 1440) {
		return set, errors.New("restart after 1 to 1440 minutes of failing health")
	}
	switch set.Signal {
	case SignalDocker, SignalQuery:
	default:
		return set, fmt.Errorf("unknown health signal %q", set.Signal)
	}
	return set, nil
}

// Healthcheck is the Docker health check for a custom check, or nil to keep
// the image's.
func (k Check) Healthcheck() *docker.Healthcheck {
	var cmd string
	switch k.Kind {
	case CheckNone:
		return &docker.Healthcheck{Test: []string{"NONE"}}
	case CheckTCP:
		// Images have nc, or bash with its /dev/tcp, more often than not.
		cmd = fmt.Sprintf(`if command -v nc >/dev/null; then nc -z -w %[2]d 127.0.0.1 %[1]d; `+
			`else bash -c 'exec 3<>/dev/tcp/127.0.0.1/%[1]d'; fi`, k.Port, or(k.Timeout, DefaultTimeout))
	case CheckHTTP:
		url := fmt.Sprintf("http://127.0.0.1:%d%s", k.Port, k.Path)
		cmd = fmt.Sprintf(`if command -v curl >/dev/null; then curl -fsS -o /dev/null --max-time %[2]d '%[1]s'; `+
			`else wget -q -O /dev/null -T %[2]d '%[1]s'; fi`, url, or(k.Timeout, DefaultTimeout))
	case CheckCommand:
		cmd = k.Command
	default:
		return nil
	}
	return &docker.Healthcheck{
		Test:        []string{"CMD-SHELL", cmd},
		Interval:    int64(time.Duration(or(k.Interval, DefaultInterval)) * time.Second),
		Timeout:     int64(time.Duration(or(k.Timeout, DefaultTimeout)) * time.Second),
		Retries:     or(k.Retries, DefaultRetries),
		StartPeriod: int64(time.Duration(startPeriod(k.StartPeriod)) * time.Minute),
	}
}

func startPeriod(p *int) int {
	if p == nil {
		return DefaultStartPeriod
	}
	return *p
}

func or(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
