package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Install modes.
const (
	ModeBinary    = "binary"
	ModeContainer = "container"
)

// Config configures the updater.
type Config struct {
	Version string // running version, e.g. v1.2.0 or "dev"
	Repo    string // GitHub owner/name that publishes releases
	APIBase string // GitHub API, normally https://api.github.com
	Image   string // image repository releases are pushed to, e.g. owner/name on Docker Hub
	// Registry replaces the image registry's address (for tests).
	Registry string
	DataDir  string // where the outcome of an update is recorded
	Docker   *docker.Client
	Log      *slog.Logger
	// Restart replaces the process with the executable at path after a
	// binary update. It doesn't return on success. (The path is passed in
	// because /proc/self/exe now points at the renamed old binary.)
	Restart func(path string) error
	// Busy, if set, reports why an update must wait (e.g. a server is being
	// installed), or "" if it may go ahead.
	Busy func() string
}

// Event reports update progress.
type Event struct {
	Stage    string               `json:"stage"` // download, pull, verify, install, restart, done, error
	Message  string               `json:"message,omitempty"`
	Progress *docker.PullProgress `json:"progress,omitempty"`
}

// Result is the outcome of the last update, kept across the restart.
type Result struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// Status describes the running version and any available update.
type Status struct {
	Current    string     `json:"current"`
	Mode       string     `json:"mode"`
	Latest     *Release   `json:"latest,omitempty"`
	Available  bool       `json:"available"`
	CanUpdate  bool       `json:"canUpdate"`
	Reason     string     `json:"reason,omitempty"` // why updating isn't possible
	Target     string     `json:"target,omitempty"` // image or file an update would replace
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
	CheckError string     `json:"checkError,omitempty"`
	Updating   bool       `json:"updating"`
	LastResult *Result    `json:"lastResult,omitempty"`
}

// Updater checks for and installs new versions.
type Updater struct {
	cfg  Config
	http *http.Client

	mode        string
	reason      string // why updating is impossible, if it is
	target      string
	containerID string
	image       string // image reference the hlm container was created from
	// ownImage is the image hlm's container runs, read at startup. Once an
	// update moves the tag, Docker may no longer be able to inspect it, but
	// cloning the container needs to know which settings came from it.
	ownImage *docker.Image
	exe      string

	mu        sync.Mutex
	latest    *Release
	checkedAt time.Time
	checkErr  string
	updating  bool
	result    *Result
}

// ContainerID is the ID of the container hlm runs in, or "" if it doesn't
// run in one (on this Docker host or any other).
func (u *Updater) ContainerID() string { return u.containerID }

// New creates an updater and works out how hlm is installed.
func New(ctx context.Context, cfg Config) *Updater {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	if cfg.Image == "" && cfg.Repo != "" {
		// Docker Hub, under the same owner/name as the GitHub repository.
		cfg.Image = strings.ToLower(cfg.Repo)
	}
	u := &Updater{cfg: cfg, http: &http.Client{Timeout: 5 * time.Minute}}

	if id := ownContainerID(); id != "" {
		u.mode, u.containerID = ModeContainer, id
		info, err := cfg.Docker.InspectContainer(ctx, id)
		if err != nil {
			u.reason = "hlm runs in a container but can't inspect it through Docker: " + err.Error()
		}
		u.image, u.target = info.Config.Image, info.Config.Image
		if err == nil {
			if img, err := cfg.Docker.InspectImage(ctx, info.Image); err != nil {
				u.reason = "can't read hlm's own image (" + err.Error() + "); restart hlm and try again"
			} else {
				u.ownImage = &img
			}
		}
	} else {
		u.mode = ModeBinary
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		switch {
		case err != nil:
			u.reason = "can't find the running executable: " + err.Error()
		case strings.Contains(exe, string(filepath.Separator)+"go-build"):
			u.reason = "hlm is running through `go run`; update the source instead"
		case !dirWritable(filepath.Dir(exe)):
			u.reason = fmt.Sprintf("hlm can't write to %s; run it as a user that can, or update it manually", filepath.Dir(exe))
		}
		u.exe, u.target = exe, exe
	}
	if _, ok := parseVersion(cfg.Version); !ok && u.reason == "" {
		u.reason = fmt.Sprintf("this is a development build (%s); updates only work for release builds", cfg.Version)
	}
	if u.reason == "" && (u.mode == ModeBinary && cfg.Repo == "" || u.mode == ModeContainer && cfg.Image == "") {
		u.reason = "no release repository is configured"
	}
	u.loadResult()
	return u
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".hlm-write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// Status returns the current state.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.result == nil {
		// After a container update the helper records the outcome only once
		// it has watched this new container for a while, so look again.
		u.loadResult()
	}
	s := Status{
		Current:    u.cfg.Version,
		Mode:       u.mode,
		Latest:     u.latest,
		CanUpdate:  u.reason == "",
		Reason:     u.reason,
		Target:     u.target,
		CheckError: u.checkErr,
		Updating:   u.updating,
		LastResult: u.result,
	}
	if u.latest != nil {
		s.Available = newer(u.latest.Version, u.cfg.Version)
	}
	if !u.checkedAt.IsZero() {
		t := u.checkedAt
		s.CheckedAt = &t
	}
	return s
}

// Check looks for the latest version: in container mode the newest version
// tag of the release image, otherwise the latest GitHub release.
func (u *Updater) Check(ctx context.Context) Status {
	var rel *Release
	var err error
	if u.mode == ModeContainer {
		rel, err = latestImageTag(ctx, u.http, u.cfg.Image, u.cfg.Registry)
		if err == nil && u.cfg.Repo != "" {
			// Show the release notes, if a GitHub release was made too.
			if gh, err := releaseByTag(ctx, u.http, u.cfg.APIBase, u.cfg.Repo, rel.Version); err == nil {
				rel.Name, rel.Notes, rel.URL, rel.Published = gh.Name, gh.Notes, gh.URL, gh.Published
			}
		}
		if err != nil {
			err = fmt.Errorf("check %s: %w", u.cfg.Image, err)
		}
	} else {
		rel, err = latestRelease(ctx, u.http, u.cfg.APIBase, u.cfg.Repo)
	}
	u.mu.Lock()
	u.checkedAt = time.Now()
	if err != nil {
		u.checkErr = err.Error()
	} else {
		u.latest, u.checkErr = rel, ""
	}
	u.mu.Unlock()
	return u.Status()
}

// Run checks for updates shortly after startup and then every interval.
func (u *Updater) Run(ctx context.Context, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	for {
		st := u.Check(ctx)
		if st.Available {
			u.cfg.Log.Info("a new version of hlm is available", "current", st.Current, "latest", st.Latest.Version)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// DismissResult forgets the outcome of the last update.
func (u *Updater) DismissResult() {
	u.mu.Lock()
	u.result = nil
	u.mu.Unlock()
	os.Remove(u.resultPath())
}

// Apply installs the latest release. In binary mode the process restarts
// shortly after Apply returns; in container mode a helper container takes
// over and this container is stopped.
func (u *Updater) Apply(ctx context.Context, emit func(Event)) error {
	st := u.Status()
	switch {
	case !st.CanUpdate:
		return errors.New(st.Reason)
	case st.Latest == nil:
		return errors.New("check for updates first")
	case !st.Available:
		return fmt.Errorf("already running the latest version (%s)", st.Current)
	}
	if u.cfg.Busy != nil {
		if why := u.cfg.Busy(); why != "" {
			return errors.New(why)
		}
	}
	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		return errors.New("an update is already in progress")
	}
	u.updating = true
	u.mu.Unlock()

	var err error
	if u.mode == ModeContainer {
		err = u.applyContainer(ctx, st.Latest, emit)
	} else {
		err = u.applyBinary(ctx, st.Latest, emit)
	}
	if err != nil {
		u.mu.Lock()
		u.updating = false
		u.mu.Unlock()
	}
	return err
}

func (u *Updater) resultPath() string {
	return filepath.Join(u.cfg.DataDir, "selfupdate-result.json")
}

func writeResult(path string, r Result) error {
	data, _ := json.Marshal(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write and rename, so a reader never sees half a file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (u *Updater) loadResult() {
	data, err := os.ReadFile(u.resultPath())
	if err != nil {
		return
	}
	var r Result
	if json.Unmarshal(data, &r) == nil {
		u.result = &r
	}
}
