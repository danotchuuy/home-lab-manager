package manager

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// Crash-loop protection: a server that keeps exiting on its own (with
// "start on boot", Docker restarts it every time) is stopped by hlm once it
// has exited too often in a short time, and stays stopped until someone
// starts it again.

// Default crash-loop limit: this many unexpected exits within the window.
const (
	DefaultCrashLoopExits  = 5
	DefaultCrashLoopWindow = 10 * time.Minute
)

// LabelHealth marks a container whose health check hlm set (from its health
// settings), rather than the image or template.
const LabelHealth = "io.hlm.health"

// HealthPolicy gives each server's crash-loop limit and custom health check.
type HealthPolicy interface {
	// CrashLoopLimit is how many unexpected exits within window stop a
	// server; 0 exits turns crash-loop protection off for it.
	CrashLoopLimit(server string) (exits int, window time.Duration)
	// Healthcheck is the check to give the server's container when it's
	// next created, or nil to keep the image's (or template's).
	Healthcheck(server string) *docker.Healthcheck
}

// UseHealthPolicy sets where crash-loop limits and custom health checks
// come from. Without one, every server has the default limit.
func (m *Manager) UseHealthPolicy(p HealthPolicy) { m.healthPolicy = p }

// UseAudit records what hlm does to servers on its own (stopping a crash
// loop) in the audit log.
func (m *Manager) UseAudit(r audit.Recorder) { m.audit = r }

// CrashLoop is a server hlm stopped because it kept exiting.
type CrashLoop struct {
	Since time.Time `json:"since"` // when hlm stopped it
	Exits int       `json:"exits"` // within the window
	// WindowMinutes is the window the exits were counted in.
	WindowMinutes int    `json:"windowMinutes"`
	ExitCode      string `json:"exitCode,omitempty"` // of the last exit
	Reason        string `json:"reason,omitempty"`   // of the last exit
}

// crashTracker counts servers' unexpected exits, and keeps the servers
// stopped for a crash loop (in a file, so the badge survives a restart of
// hlm).
type crashTracker struct {
	mu      sync.Mutex
	path    string
	exits   map[string][]time.Time // by server name, oldest first
	stopped map[string]CrashLoop   // by server name
}

func newCrashTracker() *crashTracker {
	return &crashTracker{exits: map[string][]time.Time{}, stopped: map[string]CrashLoop{}}
}

// UseCrashLoopStore keeps the servers stopped for a crash loop in path.
func (m *Manager) UseCrashLoopStore(path string) error {
	c := m.crashes
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &c.stopped); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if c.stopped == nil {
		c.stopped = map[string]CrashLoop{}
	}
	return nil
}

// save writes the stopped set; c.mu is held.
func (c *crashTracker) save() error {
	if c.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.stopped, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// add records an exit at t and returns how many there were within window.
func (c *crashTracker) add(name string, t time.Time, window time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := append(c.exits[name], t)
	cut := 0
	for cut < len(list) && t.Sub(list[cut]) > window {
		cut++
	}
	list = list[cut:]
	c.exits[name] = list
	return len(list)
}

// recent is how many exits a server had within window before now.
func (c *crashTracker) recent(name string, now time.Time, window time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.exits[name] {
		if now.Sub(t) <= window {
			n++
		}
	}
	return n
}

func (c *crashTracker) get(name string) (CrashLoop, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.stopped[name]
	return cl, ok
}

func (c *crashTracker) set(name string, cl CrashLoop) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped[name] = cl
	delete(c.exits, name)
	return c.save()
}

// clear forgets a server's exits and crash loop.
func (c *crashTracker) clear(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.exits, name)
	if _, ok := c.stopped[name]; !ok {
		return nil
	}
	delete(c.stopped, name)
	return c.save()
}

// CrashLoopOf returns the crash loop a server was stopped for, if it was.
func (m *Manager) CrashLoopOf(name string) (CrashLoop, bool) { return m.crashes.get(name) }

// RecentExits is how many unexpected exits (and restarts for failing
// health) a server had within its crash-loop window.
func (m *Manager) RecentExits(name string) int {
	_, window := m.crashLimit(name)
	return m.crashes.recent(name, time.Now(), window)
}

// ClearCrashLoop forgets a server's crash loop and the exits counted
// towards one: starting a server does this.
func (m *Manager) ClearCrashLoop(name string) {
	if err := m.crashes.clear(name); err != nil {
		m.log.Warn("clear crash loop", "server", name, "err", err)
	}
}

func (m *Manager) crashLimit(name string) (int, time.Duration) {
	if m.healthPolicy == nil {
		return DefaultCrashLoopExits, DefaultCrashLoopWindow
	}
	n, w := m.healthPolicy.CrashLoopLimit(name)
	if w <= 0 {
		w = DefaultCrashLoopWindow
	}
	return n, w
}

// countExit counts an unexpected exit of a server, and reports whether it
// makes a crash loop.
func (m *Manager) countExit(name string, at time.Time) (CrashLoop, bool) {
	limit, window := m.crashLimit(name)
	if limit <= 0 {
		return CrashLoop{}, false
	}
	if _, ok := m.crashes.get(name); ok {
		return CrashLoop{}, false // already stopped for one
	}
	n := m.crashes.add(name, at, window)
	return CrashLoop{Exits: n, WindowMinutes: int(window / time.Minute)}, n >= limit
}

// stopCrashLoop stops a crash-looping server, remembers why and reports it.
func (m *Manager) stopCrashLoop(ctx context.Context, id, name string, cl CrashLoop) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	err := m.docker.StopContainer(ctx, id, -1)
	if err != nil && !docker.IsNotFound(err) {
		m.log.Error("stop crash-looping server", "server", name, "err", err)
	}
	// After the stop: a start after this time means someone started it
	// again.
	cl.Since = time.Now()
	if err := m.crashes.set(name, cl); err != nil {
		m.log.Warn("save crash loop", "server", name, "err", err)
	}
	m.log.Warn("server stopped: crash loop", "server", name, "exits", cl.Exits, "minutes", cl.WindowMinutes)
	if m.audit != nil {
		e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: "server.crash_loop_stop", Target: name, Server: name,
			Result: audit.ResultOK, Details: map[string]string{
				"exits": fmt.Sprint(cl.Exits), "minutes": fmt.Sprint(cl.WindowMinutes), "lastExit": cl.Reason,
			}}
		if err != nil {
			e.Result, e.Error = audit.ResultError, err.Error()
		}
		m.audit.Record(e)
	}
	// Restarts for failing health count too.
	msg := fmt.Sprintf("%s exited (or had to be restarted) %d times in %s, so hlm stopped it. It stays stopped until someone starts it again.\n\nLast: %s.",
		name, cl.Exits, plural(cl.WindowMinutes, "minute"), cl.Reason)
	if err != nil {
		msg = fmt.Sprintf("%s exited (or had to be restarted) %d times in %s. hlm tried to stop it, but couldn't: %v.\n\nLast: %s.",
			name, cl.Exits, plural(cl.WindowMinutes, "minute"), err, cl.Reason)
	}
	if tail := m.outputTail(ctx, id, 50, 3000); tail != "" {
		msg += "\n\nLast output:\n" + tail
	}
	m.notify(notify.Event{
		Kind: notify.KindServerCrashLoop, Title: name + " is crash-looping: stopped by hlm", Message: msg, Server: name,
		Link: "/servers/" + name, Time: cl.Since,
		Fields: map[string]string{"Exits": fmt.Sprintf("%d in %d min", cl.Exits, cl.WindowMinutes), "Exit code": cmp.Or(cl.ExitCode, "unknown")},
		Key:    notify.DedupeKey(notify.KindServerCrashLoop, name) + "/" + cl.Since.Format(time.RFC3339),
	})
}

// exited handles a server's unexpected exit (a die event nobody asked for).
func (m *Manager) exited(ctx context.Context, id, name string, at time.Time, code string, oom bool) {
	cl, loop := m.countExit(name, at)
	if !loop {
		return
	}
	cl.ExitCode = code
	cl.Reason = "exit code " + cmp.Or(code, "unknown")
	if oom {
		cl.Reason = "ran out of memory"
	}
	go m.stopCrashLoop(ctx, id, name, cl)
}

// RestartUnhealthy restarts a server whose health has been failing, which
// counts towards its crash-loop limit: if this restart would reach it, the
// server is stopped instead and stopped is true.
func (m *Manager) RestartUnhealthy(ctx context.Context, name, reason string) (stopped bool, err error) {
	info, err := m.inspect(ctx, name)
	if err != nil {
		return false, err
	}
	if cl, loop := m.countExit(name, time.Now()); loop {
		cl.Reason = reason
		m.stopCrashLoop(ctx, info.ID, name, cl)
		return true, nil
	}
	return false, m.docker.RestartContainer(ctx, info.ID, -1)
}

// fillCrashLoop marks a server stopped for a crash loop, and forgets the
// crash loop of one that has been started since (with Docker, say).
func (m *Manager) fillCrashLoop(srv *Server, info docker.ContainerJSON) {
	cl, ok := m.crashes.get(srv.Name)
	if !ok {
		return
	}
	if info.State.Running || info.State.Restarting {
		started, err := time.Parse(time.RFC3339Nano, info.State.StartedAt)
		if err == nil && started.After(cl.Since) {
			m.ClearCrashLoop(srv.Name)
			return
		}
	}
	srv.CrashLoop = &cl
}

// applyHealthcheck gives a server the custom health check from its
// settings, if it has one.
func (m *Manager) applyHealthcheck(spec *deploy.Spec) {
	if m.healthPolicy == nil {
		return
	}
	hc := m.healthPolicy.Healthcheck(spec.Name)
	if hc == nil {
		return
	}
	spec.Create.Healthcheck = hc
	if spec.Create.Labels == nil {
		spec.Create.Labels = map[string]string{}
	}
	spec.Create.Labels[LabelHealth] = "custom"
}

// HealthcheckApplied reports whether a server's container has the health
// check its settings ask for: it's set when the container is created, so a
// change waits for the next update or reconfigure.
func (m *Manager) HealthcheckApplied(info docker.ContainerJSON, name string) bool {
	var want *docker.Healthcheck
	if m.healthPolicy != nil {
		want = m.healthPolicy.Healthcheck(name)
	}
	custom := info.Config.Labels[LabelHealth] == "custom"
	if want == nil {
		return !custom
	}
	have := info.Config.Healthcheck
	return custom && have != nil && slices.Equal(have.Test, want.Test) && have.Interval == want.Interval &&
		have.Timeout == want.Timeout && have.StartPeriod == want.StartPeriod && have.Retries == want.Retries
}

// RunCheck runs a health check command (CMD-SHELL) in a running server
// once, as Docker would, and returns its exit code and output.
func (m *Manager) RunCheck(ctx context.Context, id string, hc docker.Healthcheck) (int, string, error) {
	info, err := m.inspect(ctx, id)
	if err != nil {
		return 0, "", err
	}
	if !info.State.Running {
		return 0, "", errors.New("the server isn't running")
	}
	var cmd []string
	switch {
	case len(hc.Test) == 2 && hc.Test[0] == "CMD-SHELL":
		cmd = []string{"/bin/sh", "-c", hc.Test[1]}
	case len(hc.Test) > 1 && hc.Test[0] == "CMD":
		cmd = hc.Test[1:]
	default:
		return 0, "", errors.New("there's no command to run")
	}
	timeout := time.Duration(hc.Timeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	res, err := m.docker.Exec(ctx, info.ID, "", cmd, 4096)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", fmt.Errorf("the check didn't finish within %s", timeout)
		}
		return 0, "", err
	}
	out := strings.TrimSpace(stripANSI(string(res.Stdout) + string(res.Stderr)))
	return res.ExitCode, out, nil
}

// outputTail returns up to lines of a container's last output, at most max
// bytes (keeping the end).
func (m *Manager) outputTail(ctx context.Context, id string, lines, max int) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := m.docker.Logs(ctx, id, docker.LogsOptions{Tail: fmt.Sprint(lines)})
	if err != nil {
		return ""
	}
	defer r.Close()
	data, _ := io.ReadAll(io.LimitReader(r, 256<<10))
	out := stripANSI(strings.TrimSpace(string(data)))
	if len(out) > max {
		out = "…" + out[len(out)-max:]
	}
	return out
}

// plural is n things, in words.
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}
