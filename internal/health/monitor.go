package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// MonitorInterval is how often the monitor looks at servers' health.
const MonitorInterval = 30 * time.Second

// Servers is what the monitor needs from the server manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	JobRunning(name string) bool
	RestartUnhealthy(ctx context.Context, name, reason string) (stopped bool, err error)
}

// Options configures a Monitor.
type Options struct {
	Store   *Store
	Servers Servers
	// Responding says whether a server answers its game's status query:
	// ok is false while that isn't known (it's starting, or can't be asked).
	Responding func(name string) (responding, ok bool)
	// Busy says whether a server is being backed up or restored.
	Busy     func(name string) bool
	Notifier notify.Notifier
	Audit    audit.Recorder
	Log      *slog.Logger
	Now      func() time.Time // for tests
}

// Monitor restarts servers that have been unhealthy for longer than their
// settings allow.
type Monitor struct {
	opts Options
	mu   sync.Mutex
	// since is when each server was first seen unhealthy, by name and
	// container (a recreated server starts over).
	since map[string]unhealthy
}

type unhealthy struct {
	id    string
	since time.Time
}

// NewMonitor returns a monitor.
func NewMonitor(o Options) *Monitor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Monitor{opts: o, since: map[string]unhealthy{}}
}

// UnhealthySince is when a server's health started failing, as far as the
// monitor has seen (only servers set to restart are watched).
func (m *Monitor) UnhealthySince(name string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.since[name]
	return u.since, ok
}

// Run checks servers every MonitorInterval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(MonitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Tick looks at every server once.
func (m *Monitor) Tick(ctx context.Context) {
	all, err := m.opts.Store.All()
	if err != nil {
		m.opts.Log.Warn("health: read settings", "err", err)
		return
	}
	watched := false
	for _, set := range all {
		watched = watched || set.RestartAfter > 0
	}
	if !watched {
		m.mu.Lock()
		clear(m.since)
		m.mu.Unlock()
		return
	}
	list, err := m.opts.Servers.List(ctx)
	if err != nil {
		m.opts.Log.Debug("health: list servers", "err", err)
		return
	}
	now := m.opts.Now()
	seen := map[string]bool{}
	for _, srv := range list {
		set := all[srv.Name]
		if set.RestartAfter <= 0 || srv.State != "running" {
			continue
		}
		bad, why := m.failing(srv, set)
		if !bad {
			continue
		}
		seen[srv.Name] = true
		m.mu.Lock()
		u, ok := m.since[srv.Name]
		if !ok || u.id != srv.ID {
			u = unhealthy{id: srv.ID, since: now}
			m.since[srv.Name] = u
		}
		m.mu.Unlock()
		limit := time.Duration(set.RestartAfter) * time.Minute
		if now.Sub(u.since) < limit {
			continue
		}
		if m.opts.Servers.JobRunning(srv.Name) || (m.opts.Busy != nil && m.opts.Busy(srv.Name)) {
			continue // try again once it's done
		}
		m.restart(ctx, srv.Name, fmt.Sprintf("%s for %s", why, minutes(set.RestartAfter)))
		delete(seen, srv.Name)
	}
	m.mu.Lock()
	for name := range m.since {
		if !seen[name] {
			delete(m.since, name)
		}
	}
	m.mu.Unlock()
}

// failing says whether a server's health signal is failing, and how.
func (m *Monitor) failing(srv manager.Server, set Settings) (bool, string) {
	if set.Signal == SignalQuery {
		if m.opts.Responding == nil {
			return false, ""
		}
		responding, ok := m.opts.Responding(srv.Name)
		return ok && !responding, "not answering its game's status query"
	}
	return srv.Health == "unhealthy", "unhealthy"
}

func (m *Monitor) restart(ctx context.Context, name, reason string) {
	restartServer(ctx, m.opts.Servers, m.opts.Audit, m.opts.Notifier, m.opts.Log, m.opts.Now(), name, reason)
	m.mu.Lock()
	delete(m.since, name)
	m.mu.Unlock()
}

// restartServer restarts a server whose health is failing, records it and
// says so; failed is true when the restart didn't happen.
func restartServer(ctx context.Context, servers Servers, rec audit.Recorder, n notify.Notifier, log *slog.Logger,
	now time.Time, name, reason string) (failed bool) {
	stopped, err := servers.RestartUnhealthy(ctx, name, reason)
	if stopped {
		return false // the manager reports the crash loop
	}
	log.Warn("restarted unhealthy server", "server", name, "reason", reason, "err", err)
	if rec != nil {
		e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: "server.unhealthy_restart", Target: name, Server: name,
			Result: audit.ResultOK, Details: map[string]string{"reason": reason}}
		if err != nil {
			e.Result, e.Error = audit.ResultError, err.Error()
		}
		rec.Record(e)
	}
	if n != nil {
		e := notify.Event{
			Kind: notify.KindServerUnhealthy, Title: name + " was restarted: its health was failing",
			Message: fmt.Sprintf("%s was restarted: %s.", name, reason), Server: name,
			Link: "/servers/" + name, Time: now,
			Key: notify.DedupeKey(notify.KindServerUnhealthy, name) + "/restart",
		}
		if err != nil {
			e.Title = name + " is unhealthy, and restarting it failed"
			e.Message = fmt.Sprintf("%s: %s. hlm tried to restart it, but couldn't: %v", name, reason, err)
		}
		n.Notify(e)
	}
	return err != nil
}

// minutes is n minutes, in words.
func minutes(n int) string {
	if n == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", n)
}
