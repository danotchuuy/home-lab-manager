package manager

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// UseNotifier sets where the manager reports crashes, updates and the like.
func (m *Manager) UseNotifier(n notify.Notifier) { m.notifier = n }

func (m *Manager) notify(e notify.Event) {
	if m.notifier != nil {
		m.notifier.Notify(e)
	}
}

func (m *Manager) clearNotice(key string) {
	if m.notifier != nil {
		m.notifier.Clear(key)
	}
}

// expectedStopWindow is how long after a kill event (sent by docker stop,
// restart and kill, whoever asked) a container's exit counts as asked for.
const expectedStopWindow = 2 * time.Minute

// watchState follows containers between events.
type watchState struct {
	mu     sync.Mutex
	killed map[string]time.Time // container ID → last kill event
	oom    map[string]bool      // container ID → ran out of memory
	// unhealthy marks containers reported unhealthy, so the alert is sent
	// again only after they've been healthy.
	unhealthy map[string]bool
	last      int64 // TimeNano of the last event handled
}

func newWatchState() *watchState {
	return &watchState{killed: map[string]time.Time{}, oom: map[string]bool{}, unhealthy: map[string]bool{}}
}

// WatchEvents follows Docker's container events until ctx ends, reporting
// servers that crash or turn unhealthy and connection containers that stop
// on their own. It reconnects when the stream drops.
func (m *Manager) WatchEvents(ctx context.Context) {
	st := newWatchState()
	filters := map[string][]string{
		"type":  {"container"},
		"event": {"kill", "die", "oom", "health_status"},
	}
	wait := 5 * time.Second
	var since time.Time
	for {
		start := time.Now()
		err := m.docker.Events(ctx, since, filters, func(e docker.EventMessage) {
			m.handleEvent(ctx, st, e)
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			wait = 5 * time.Second
		}
		m.log.Debug("docker event stream ended; reconnecting", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
		// Pick up where it left off; events already handled are skipped.
		st.mu.Lock()
		if st.last != 0 {
			since = time.Unix(0, st.last)
		} else {
			since = start
		}
		st.mu.Unlock()
	}
}

// handleEvent looks at one container event.
func (m *Manager) handleEvent(ctx context.Context, st *watchState, e docker.EventMessage) {
	st.mu.Lock()
	nano := e.When().UnixNano()
	if nano <= st.last {
		st.mu.Unlock()
		return // seen before a reconnect
	}
	st.last = nano
	id := e.Actor.ID
	action := e.Action
	var crashed, oom bool
	switch {
	case action == "kill":
		st.killed[id] = e.When()
	case action == "oom":
		st.oom[id] = true
	case action == "die":
		killedAt, killed := st.killed[id]
		oom = st.oom[id]
		delete(st.killed, id)
		delete(st.oom, id)
		crashed = oom || !killed || e.When().Sub(killedAt) > expectedStopWindow
	case strings.HasPrefix(action, "health_status"):
		status := strings.TrimSpace(strings.TrimPrefix(action, "health_status:"))
		was := st.unhealthy[id]
		st.unhealthy[id] = status == "unhealthy"
		if status != "unhealthy" || was {
			st.mu.Unlock()
			if status == "healthy" && was {
				m.clearNotice(notify.DedupeKey(notify.KindServerUnhealthy, e.Actor.Attributes["name"]))
			}
			return
		}
	}
	// Forget containers that are gone for good.
	if action == "die" {
		delete(st.unhealthy, id)
	}
	st.mu.Unlock()

	attrs := e.Actor.Attributes
	name := attrs["name"]
	switch {
	case id == m.selfID:
		return
	case attrs[deploy.LabelManaged] == "true":
		m.serverEvent(ctx, e, name, crashed, oom)
	default:
		if !crashed {
			return
		}
		for label, k := range m.connections {
			if _, ok := attrs[label]; ok {
				m.notify(notify.Event{
					Kind:    notify.KindTunnelDown,
					Title:   k.Title + " stopped",
					Message: fmt.Sprintf("The %s container (%s) stopped on its own with exit code %s.", k.Title, name, cmp.Or(attrs["exitCode"], "unknown")),
					Link:    "/connections/" + name,
					Key:     notify.DedupeKey(notify.KindTunnelDown, name),
					Time:    e.When(),
				})
				return
			}
		}
	}
}

// serverEvent reports a server's crash or failing health check.
func (m *Manager) serverEvent(ctx context.Context, e docker.EventMessage, name string, crashed, oom bool) {
	if m.jobs.running(name) {
		return // an install or update reports its own outcome
	}
	code := e.Actor.Attributes["exitCode"]
	switch {
	case e.Action == "die" && crashed && (oom || code != "0"):
		msg := fmt.Sprintf("%s stopped on its own with exit code %s.", name, cmp.Or(code, "unknown"))
		if oom {
			msg = fmt.Sprintf("%s ran out of memory and was stopped (exit code %s).", name, cmp.Or(code, "unknown"))
		}
		if tail := m.lastOutput(ctx, e.Actor.ID); tail != "" {
			msg += "\n\nLast output:\n" + tail
		}
		m.notify(notify.Event{
			Kind: notify.KindServerCrashed, Title: name + " crashed", Message: msg, Server: name,
			Link: "/servers/" + name + "?tab=console", Time: e.When(),
			Fields: map[string]string{"Exit code": cmp.Or(code, "unknown")},
		})
	case strings.HasPrefix(e.Action, "health_status"):
		m.notify(notify.Event{
			Kind: notify.KindServerUnhealthy, Title: name + " is unhealthy",
			Message: name + "'s health check is failing.", Server: name, Link: "/servers/" + name, Time: e.When(),
		})
	}
}

// lastOutput returns the end of a container's output, for crash reports.
func (m *Manager) lastOutput(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := m.docker.Logs(ctx, id, docker.LogsOptions{Tail: "15"})
	if err != nil {
		return ""
	}
	defer r.Close()
	data, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	out := stripANSI(strings.TrimSpace(string(data)))
	if len(out) > 1200 {
		out = "…" + out[len(out)-1200:]
	}
	return out
}

// stripANSI removes terminal escape codes, which notifications can't show.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			continue
		}
		if c == '\r' || (c < 0x20 && c != '\n' && c != '\t') {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
