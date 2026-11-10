package manager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestStopReasons(t *testing.T) {
	m, _ := eventsManager(t)
	path := filepath.Join(t.TempDir(), "exits.json")
	if err := m.UseExitStore(path); err != nil {
		t.Fatal(err)
	}
	f := &eventFeed{m: m, st: newWatchState(), t: time.Now()}
	reason := func(m *Manager, id, name, state string) string {
		t.Helper()
		srv := Server{ID: id, Name: name, State: state}
		m.fillStopReason(&srv, docker.ContainerJSON{})
		return srv.StopReason
	}

	// Stopped on purpose: a kill, then the exit.
	f.send("kill", "a", server("valheim", "signal", "15"))
	f.send("die", "a", server("valheim", "exitCode", "143"))
	if got := reason(m, "a", "valheim", "exited"); got != StopRequested {
		t.Errorf("stopped: %q", got)
	}
	// Exited cleanly, nobody asked.
	f.send("die", "a", server("valheim", "exitCode", "0"))
	if got := reason(m, "a", "valheim", "exited"); got != StopExited {
		t.Errorf("exited: %q", got)
	}
	// Crashed: an error, or out of memory even after a kill.
	f.send("die", "a", server("valheim", "exitCode", "1"))
	if got := reason(m, "a", "valheim", "exited"); got != StopCrashed {
		t.Errorf("crashed: %q", got)
	}
	srv := Server{ID: "a", Name: "valheim", State: "exited"}
	m.fillStopReason(&srv, docker.ContainerJSON{})
	if srv.ExitCode != "1" || srv.StoppedAt == nil || !srv.StoppedAt.Equal(f.t) {
		t.Errorf("when and how: %v %q", srv.StoppedAt, srv.ExitCode)
	}
	f.send("kill", "a", server("valheim"))
	f.send("oom", "a", server("valheim"))
	f.send("die", "a", server("valheim", "exitCode", "137"))
	if got := reason(m, "a", "valheim", "exited"); got != StopCrashed {
		t.Errorf("oom: %q", got)
	}
	// Restarted by hlm for failing health.
	m.exits.restarting("valheim", f.t)
	f.send("kill", "a", server("valheim"))
	f.send("die", "a", server("valheim", "exitCode", "143"))
	// Running: no reason; Docker restarting it: crashed.
	if got := reason(m, "a", "valheim", "running"); got != "" {
		t.Errorf("running: %q", got)
	}
	if got := reason(m, "a", "valheim", "restarting"); got != StopCrashed {
		t.Errorf("restarting: %q", got)
	}
	// A recreated container, never seen to exit; one never started.
	if got := reason(m, "b", "valheim", "exited"); got != StopUnknown {
		t.Errorf("another container: %q", got)
	}
	if got := reason(m, "b", "valheim", "created"); got != StopRequested {
		t.Errorf("created: %q", got)
	}

	want := map[string]int64{StopRequested: 1, StopExited: 1, StopCrashed: 2, StopUnhealthy: 1}
	check := func(m *Manager) {
		t.Helper()
		got := m.ExitCounts()["valheim"]
		for k, v := range want {
			if got[k] != v {
				t.Errorf("counts %v, want %v", got, want)
				return
			}
		}
	}
	check(m)
	// Kept across a restart of hlm.
	m2, _ := eventsManager(t)
	if err := m2.UseExitStore(path); err != nil {
		t.Fatal(err)
	}
	check(m2)
	if got := reason(m2, "a", "valheim", "exited"); got != StopUnhealthy {
		t.Errorf("after a restart of hlm: %q", got)
	}
}
