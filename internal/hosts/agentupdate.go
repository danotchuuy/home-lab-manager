package hosts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
)

// Keeping hosts' agents on hlm's version: an agent is updated the way hlm
// updates itself (see selfupdate.StartHelper), through the agent, on
// request or automatically when it connects on another version than hlm's.

// secretAgentAuto turns automatic agent updates off (they're on unless it
// says false).
const secretAgentAuto = "host-agent-auto-update"

// Waits while an agent is updated, and before retrying an automatic update
// that failed for the same version.
var (
	agentReconnectWait = 3 * time.Minute
	agentAutoDelay     = 5 * time.Second
	agentAutoRetry     = 6 * time.Hour
)

// AgentUpdate is how a host's agent's last update went.
type AgentUpdate struct {
	// State is "updating", "done" or "failed".
	State   string    `json:"state"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
	// Auto says hlm started it, rather than someone asking.
	Auto bool `json:"auto,omitempty"`
}

type agentUpdates struct {
	mu   sync.Mutex
	last map[string]*AgentUpdate
}

func (u *agentUpdates) get(id string) *AgentUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a := u.last[id]; a != nil {
		c := *a
		return &c
	}
	return nil
}

// begin records an update starting, unless one is already going.
func (u *agentUpdates) begin(id string, a AgentUpdate) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil {
		u.last = map[string]*AgentUpdate{}
	}
	if cur := u.last[id]; cur != nil && cur.State == "updating" {
		return false
	}
	u.last[id] = &a
	return true
}

func (u *agentUpdates) set(id string, fn func(*AgentUpdate)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a := u.last[id]; a != nil {
		fn(a)
	}
}

// AgentAutoUpdate reports whether agents are updated to hlm's version
// automatically.
func (r *Registry) AgentAutoUpdate() bool {
	var on *bool
	if _, err := r.opts.Secrets.Get(secretAgentAuto, &on); err != nil || on == nil {
		return true
	}
	return *on
}

// SetAgentAutoUpdate turns automatic agent updates on or off.
func (r *Registry) SetAgentAutoUpdate(on bool) error {
	return r.opts.Secrets.Set(secretAgentAuto, on)
}

// AgentUpdateStatus is how a host's agent's last update went, if it had
// one since hlm started.
func (r *Registry) AgentUpdateStatus(id string) *AgentUpdate { return r.agentUpd.get(id) }

// AgentVersion is the version agents are updated to (hlm's), and why they
// can't be when it isn't a release.
func (r *Registry) AgentVersion() (string, string) {
	v := r.opts.Version
	if !selfupdate.IsRelease(v) {
		return v, fmt.Sprintf("this hlm is a development build (%s), so there's no published image to update agents to", v)
	}
	return v, ""
}

// UpdateAgent updates a host's agent to hlm's version: hlm pulls that
// image there, through the agent, and starts a helper container that swaps
// the agent's container for one on it (putting the old one back if the new
// one doesn't stay up), then waits for the agent to connect again. Lines of
// progress go to emit.
func (r *Registry) UpdateAgent(ctx context.Context, id string, emit func(string)) error {
	return r.updateAgent(ctx, id, false, emit)
}

func (r *Registry) updateAgent(ctx context.Context, id string, auto bool, emit func(string)) error {
	if r.opts.Agents == nil {
		return errors.New("agents can't connect to this hlm")
	}
	version, why := r.AgentVersion()
	if why != "" {
		return errors.New(why)
	}
	h, err := r.Get(id)
	if err != nil {
		return err
	}
	if h.Kind != KindAgent {
		return fmt.Errorf("%s isn't reached through hlm's agent", h.Name)
	}
	st := r.opts.Agents.Status(id)
	if !st.Connected {
		return fmt.Errorf("%s's agent isn't connected", h.Name)
	}
	if st.Version == version {
		emit(fmt.Sprintf("%s's agent is %s already", h.Name, version))
		return nil
	}
	if !r.agentUpd.begin(id, AgentUpdate{State: "updating", From: st.Version, To: version, At: time.Now(), Auto: auto}) {
		return fmt.Errorf("%s's agent is being updated already", h.Name)
	}
	step := func(msg string) {
		r.agentUpd.set(id, func(a *AgentUpdate) { a.Message = msg })
		emit(msg)
	}
	err = r.runAgentUpdate(ctx, id, h, st.Version, version, step)
	r.agentUpd.set(id, func(a *AgentUpdate) {
		a.At = time.Now()
		if err != nil {
			a.State, a.Message = "failed", err.Error()
		} else {
			a.State, a.Message = "done", fmt.Sprintf("Updated from %s to %s", st.Version, version)
		}
	})
	if err != nil {
		r.opts.Log.Warn("update a host's agent", "host", id, "from", st.Version, "to", version, "err", err)
	} else {
		r.opts.Log.Info("updated a host's agent", "host", id, "from", st.Version, "to", version)
	}
	return err
}

func (r *Registry) runAgentUpdate(ctx context.Context, id string, h Host, from, to string, step func(string)) error {
	r.mu.Lock()
	c := r.conns[id]
	r.mu.Unlock()
	if c == nil {
		return fmt.Errorf("%s isn't connected", h.Name)
	}
	d := c.client
	list, err := d.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("list the containers there: %w", err)
	}
	cid, err := selfupdate.AgentContainer(list)
	if err != nil {
		return err
	}
	if cid == "" {
		return errors.New("the agent doesn't run in a container hlm can see there, so update it by hand: run the command from the Hosts page again, or the new hlm binary with the same arguments")
	}
	var current string
	for _, ct := range list {
		if ct.ID == cid {
			current = ct.Image
		}
	}
	image := selfupdate.AgentImage(current, to)
	step("Pulling " + image + " on " + h.Name)
	if err := d.PullImage(ctx, image, nil); err != nil {
		// A copy already there (loaded by hand, say) will do if the
		// registry can't be reached; StartHelper checks its version.
		if _, ierr := d.InspectImage(ctx, image); ierr != nil {
			return fmt.Errorf("pull %s: %w", image, err)
		}
		step(fmt.Sprintf("Couldn't pull %s (%v), so using the copy already on %s", image, err, h.Name))
	}
	started := time.Now()
	helper, err := selfupdate.StartHelper(ctx, d, cid, image, from, to)
	if err != nil {
		return err
	}
	step("Started " + helper + " there: it replaces the agent's container (keeping its settings and credential) and puts the old one back if the new one doesn't stay up")

	// The agent goes, and comes back on the new version (or the old one, if
	// the helper had to put it back). It's done when the helper has seen
	// the new one stay up, and finished.
	deadline := time.Now().Add(agentReconnectWait)
	said := false
	for {
		st := r.opts.Agents.Status(id)
		// A connection made since the helper started is the agent's new
		// (or restored) container.
		away := !st.Connected || st.Since.After(started)
		if st.Connected && st.Since.After(started) {
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			info, err := d.InspectContainer(hctx, helper)
			cancel()
			finished := err == nil && !info.State.Running
			switch {
			case finished && info.State.ExitCode == 0 && st.Version == to:
				step(fmt.Sprintf("%s's agent runs %s", h.Name, to))
				r.cleanupHelpers(d)
				return nil
			case finished && info.State.ExitCode != 0, st.Version != to && finished:
				log := helperLog(ctx, d, helper)
				r.cleanupHelpers(d)
				return fmt.Errorf("the new agent didn't stay up, so the old one (%s) was put back%s", st.Version, log)
			case st.Version == to && !said:
				step(fmt.Sprintf("%s's agent is back on %s; making sure it stays up", h.Name, to))
				said = true
			}
		}
		if time.Now().After(deadline) {
			if !away {
				return fmt.Errorf("the agent didn't restart within %s%s", agentReconnectWait, helperLog(ctx, d, helper))
			}
			return fmt.Errorf("the agent hasn't come back within %s; it keeps trying to connect, so look at %s on the host", agentReconnectWait, h.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// helperLog is the last thing the update helper said, to say what went
// wrong: its last log line's message.
func helperLog(ctx context.Context, d *docker.Client, name string) string {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	rc, err := d.Logs(lctx, name, docker.LogsOptions{Tail: "5"})
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, _ := io.ReadAll(io.LimitReader(rc, 4096))
	if said := lastMessage(string(data)); said != "" {
		return " (the helper said: " + said + ")"
	}
	return ""
}

// lastMessage is the msg of the last line of a slog text log, or the line
// itself if it has none.
func lastMessage(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if _, rest, ok := strings.Cut(last, `msg="`); ok {
		if msg, _, ok := strings.Cut(rest, `"`); ok {
			return msg
		}
	}
	if _, rest, ok := strings.Cut(last, "msg="); ok {
		msg, _, _ := strings.Cut(rest, " ")
		return msg
	}
	return last
}

// cleanupHelpers removes finished update helpers from a host.
func (r *Registry) cleanupHelpers(d *docker.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := selfupdate.CleanupHelpers(ctx, d); err != nil {
		r.opts.Log.Debug("remove agent update helpers", "err", err)
	}
}

// autoUpdateAgent updates an agent that connected on another version than
// hlm's, if that's on: once per version, unless it failed hours ago.
func (r *Registry) autoUpdateAgent(id string) {
	if r.opts.Agents == nil {
		return
	}
	version, why := r.AgentVersion()
	if why != "" {
		return
	}
	time.Sleep(agentAutoDelay) // let it settle in first
	// Read after the wait, so turning it off meanwhile counts.
	if !r.AgentAutoUpdate() {
		return
	}
	st := r.opts.Agents.Status(id)
	if !st.Connected || st.Version == version || st.Version == "" {
		return
	}
	if last := r.agentUpd.get(id); last != nil && last.To == version && last.State != "done" && time.Since(last.At) < agentAutoRetry {
		return // tried for this version not long ago
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentReconnectWait+5*time.Minute)
	defer cancel()
	r.opts.Log.Info("updating a host's agent to hlm's version", "host", id, "from", st.Version, "to", version)
	_ = r.updateAgent(ctx, id, true, func(string) {})
}
