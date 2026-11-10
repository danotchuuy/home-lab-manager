package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Why a server last stopped, from Docker's events: whether someone asked
// for it or it went down on its own. See Server.StopReason.
const (
	// StopRequested: someone stopped it (hlm, a schedule, idle sleep, a
	// person in hlm or with docker stop, or the host shutting down).
	StopRequested = "stopped"
	// StopExited: it exited cleanly (code 0) without anyone asking, such
	// as after an in-game shutdown command.
	StopExited = "exited"
	// StopCrashed: it exited with an error, or ran out of memory, without
	// anyone asking; also a container Docker is restarting.
	StopCrashed = "crashed"
	// StopUnhealthy: hlm restarted it because its health was failing.
	StopUnhealthy = "unhealthy"
	// StopCrashLoop: hlm stopped it because it kept exiting.
	StopCrashLoop = "crash_loop"
	// StopUnknown: it stopped while hlm wasn't watching.
	StopUnknown = "unknown"
)

// ExitReasons are the reasons an exit is counted under (ExitCounts).
var ExitReasons = []string{StopRequested, StopExited, StopCrashed, StopUnhealthy}

// lastExit is how a server's container last exited.
type lastExit struct {
	ID     string    `json:"id"` // the container, so a recreated one starts over
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Code   string    `json:"exitCode,omitempty"`
}

// exitLog keeps servers' last exits and counts their exits by reason, in a
// file so they survive a restart of hlm.
type exitLog struct {
	mu   sync.Mutex
	path string
	data exitData
	// unhealthy marks servers hlm is restarting for failing health, until
	// their exit.
	unhealthy map[string]time.Time
}

type exitData struct {
	Last   map[string]lastExit         `json:"last"`
	Counts map[string]map[string]int64 `json:"counts"` // server → reason → exits
}

func newExitLog() *exitLog {
	return &exitLog{
		data:      exitData{Last: map[string]lastExit{}, Counts: map[string]map[string]int64{}},
		unhealthy: map[string]time.Time{},
	}
}

// UseExitStore keeps servers' last exits, and their exit counts, in path.
func (m *Manager) UseExitStore(path string) error {
	l := m.exits
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d exitData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if d.Last != nil {
		l.data.Last = d.Last
	}
	if d.Counts != nil {
		l.data.Counts = d.Counts
	}
	return nil
}

// save writes the log; l.mu is held.
func (l *exitLog) save() error {
	if l.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// restarting marks a server hlm is about to restart for failing health.
func (l *exitLog) restarting(name string, at time.Time) {
	l.mu.Lock()
	l.unhealthy[name] = at
	l.mu.Unlock()
}

// record notes a server's exit: requested says a kill came just before it.
func (l *exitLog) record(name, id string, at time.Time, requested, oom bool, code string) error {
	reason := StopRequested
	switch {
	case oom || (!requested && code != "0"):
		reason = StopCrashed
	case !requested:
		reason = StopExited
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.unhealthy[name]; ok {
		delete(l.unhealthy, name)
		if reason == StopRequested && at.Sub(t) < expectedStopWindow {
			reason = StopUnhealthy
		}
	}
	l.data.Last[name] = lastExit{ID: id, At: at, Reason: reason, Code: code}
	c := l.data.Counts[name]
	if c == nil {
		c = map[string]int64{}
		l.data.Counts[name] = c
	}
	c[reason]++
	return l.save()
}

func (l *exitLog) last(name string) (lastExit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.data.Last[name]
	return e, ok
}

// forget drops a deleted server.
func (l *exitLog) forget(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, a := l.data.Last[name]
	_, b := l.data.Counts[name]
	if !a && !b {
		return nil
	}
	delete(l.data.Last, name)
	delete(l.data.Counts, name)
	return l.save()
}

// ExitCounts is how many times each server's container has exited, by
// reason (ExitReasons), since hlm started keeping count.
func (m *Manager) ExitCounts() map[string]map[string]int64 {
	l := m.exits
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]map[string]int64, len(l.data.Counts))
	for name, c := range l.data.Counts {
		out[name] = maps.Clone(c)
	}
	return out
}

// fillStopReason says why a server that isn't running stopped.
func (m *Manager) fillStopReason(srv *Server, info docker.ContainerJSON) {
	switch {
	case srv.State == "running" || (info.State.Running && !info.State.Restarting):
		return
	case srv.CrashLoop != nil:
		srv.StopReason = StopCrashLoop
	case info.State.Restarting || srv.State == "restarting":
		srv.StopReason = StopCrashed
	case srv.State == "created":
		srv.StopReason = StopRequested // never started
	default:
		if e, ok := m.exits.last(srv.Name); ok && e.ID == srv.ID {
			srv.StopReason = e.Reason
			at := e.At
			srv.StoppedAt, srv.ExitCode = &at, e.Code
			return
		} else if info.State.OOMKilled {
			srv.StopReason = StopCrashed
		} else {
			srv.StopReason = StopUnknown
		}
	}
	// Otherwise, from Docker.
	if srv.State == "created" || info.State.FinishedAt == "" {
		return
	}
	if at, err := time.Parse(time.RFC3339Nano, info.State.FinishedAt); err == nil && at.Year() > 1 {
		srv.StoppedAt = &at
		srv.ExitCode = strconv.Itoa(info.State.ExitCode)
	}
}
