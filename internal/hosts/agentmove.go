package hosts

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
)

// Moving agents to hlm's new address. An agent keeps the address it
// enrolled at; when hlm's address changes (its HTTPS domain, say), hlm tells
// each agent the new one over its connection. The agent checks it can reach
// hlm there before switching, so a wrong address never strands it; one that
// isn't connected is told when it next connects.

// secretAgentMoves keeps the addresses agents that weren't connected are to
// be moved to.
const secretAgentMoves = "host-agent-moves"

// AgentMove is how moving a host's agent to hlm's new address went.
type AgentMove struct {
	Host string `json:"host"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// State is "moved", "pending" (it's told when it connects) or "failed".
	State string    `json:"state"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// agentMoves are the last moves, by host.
type agentMoves struct {
	mu   sync.Mutex
	last map[string]AgentMove
}

func (m *agentMoves) set(mv AgentMove) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string]AgentMove{}
	}
	m.last[mv.Host] = mv
}

func (m *agentMoves) get(id string) *AgentMove {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mv, ok := m.last[id]; ok {
		return &mv
	}
	return nil
}

// AgentMoveStatus is how the last move of a host's agent went, if there was
// one.
func (r *Registry) AgentMoveStatus(id string) *AgentMove { return r.agentMv.get(id) }

// CheckAgentServer tidies hlm's address for agents: http or https, with a
// host and nothing after it but a path.
func CheckAgentServer(s string) (string, error) { return agentServer(s) }

// MoveAgents tells each agent hlm's new address: to says where an agent at
// current goes (false to leave it). Agents that aren't connected are told
// when they next connect.
func (r *Registry) MoveAgents(ctx context.Context, to func(current string) (string, bool)) []AgentMove {
	if r.opts.Agents == nil {
		return nil
	}
	r.agentMvMu.Lock()
	defer r.agentMvMu.Unlock()
	list, err := r.List()
	if err != nil {
		return nil
	}
	pending := r.pendingMoves()
	var out []AgentMove
	for _, h := range list {
		if h.Kind != KindAgent {
			continue
		}
		st := r.opts.Agents.Status(h.ID)
		current := st.Server
		if p, ok := pending[h.ID]; ok && !st.Connected {
			current = p // where it was already going
		}
		target, ok := to(current)
		if !ok {
			continue
		}
		mv := AgentMove{Host: h.ID, From: st.Server, To: target, At: time.Now().UTC()}
		switch {
		case !st.Connected:
			mv.State, mv.Error = "pending", "not connected: it's told when it next connects"
			pending[h.ID] = target
		default:
			mv = r.moveAgent(ctx, h.ID, st.Server, target)
			if mv.State == "moved" {
				delete(pending, h.ID)
			}
		}
		r.agentMv.set(mv)
		out = append(out, mv)
	}
	r.savePendingMoves(pending)
	return out
}

func (r *Registry) moveAgent(ctx context.Context, id, from, to string) AgentMove {
	mv := AgentMove{Host: id, From: from, To: to, State: "moved", At: time.Now().UTC()}
	if from == to {
		return mv
	}
	if err := r.opts.Agents.MoveTo(ctx, id, to); err != nil {
		mv.State, mv.Error = "failed", err.Error()
		if errors.Is(err, agent.ErrNotConnected) {
			mv.State = "pending"
		}
		r.opts.Log.Warn("move an agent to hlm's new address", "host", id, "to", to, "err", err)
		return mv
	}
	r.opts.Log.Info("moved an agent to hlm's new address", "host", id, "from", from, "to", to)
	return mv
}

func (r *Registry) pendingMoves() map[string]string {
	m := map[string]string{}
	_, _ = r.opts.Secrets.Get(secretAgentMoves, &m)
	return m
}

func (r *Registry) savePendingMoves(m map[string]string) {
	var err error
	if len(m) == 0 {
		err = r.opts.Secrets.Delete(secretAgentMoves)
	} else {
		err = r.opts.Secrets.Set(secretAgentMoves, m)
	}
	if err != nil {
		r.opts.Log.Warn("save agents' moves", "err", err)
	}
}

// movePending moves an agent that has just connected, if it was to be
// moved while it wasn't.
func (r *Registry) movePending(id string) {
	r.agentMvMu.Lock()
	defer r.agentMvMu.Unlock()
	pending := r.pendingMoves()
	to, ok := pending[id]
	if !ok {
		return
	}
	st := r.opts.Agents.Status(id)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mv := r.moveAgent(ctx, id, st.Server, to)
	r.agentMv.set(mv)
	if mv.State != "pending" {
		// Moved, or it refused (it can't reach that address): either way
		// it's been told; the Hosts page says how it went.
		delete(pending, id)
		r.savePendingMoves(pending)
	}
}

// HTTPSMove is where an agent goes when hlm's HTTPS changes: from plain
// HTTP (or the old domain) to https://domain on the same port when it comes
// on, and back to plain HTTP at the domain when it goes off. Agents reaching
// hlm some other way (through a proxy, say) are left alone.
func HTTPSMove(oldDomain, newDomain string) func(current string) (string, bool) {
	return func(current string) (string, bool) {
		u, err := url.Parse(current)
		if err != nil || u.Host == "" {
			return "", false
		}
		host := strings.ToLower(u.Hostname())
		withHost := func(scheme, h string) string {
			v := *u
			v.Scheme = scheme
			if p := u.Port(); p != "" {
				v.Host = h + ":" + p
			} else {
				v.Host = h
			}
			return strings.TrimRight(v.String(), "/")
		}
		switch {
		case newDomain != "" && u.Scheme == "http":
			return withHost("https", newDomain), true
		case newDomain != "" && u.Scheme == "https" && oldDomain != "" && host == strings.ToLower(oldDomain) && oldDomain != newDomain:
			return withHost("https", newDomain), true
		case newDomain == "" && oldDomain != "" && u.Scheme == "https" && host == strings.ToLower(oldDomain):
			return withHost("http", oldDomain), true
		}
		return "", false
	}
}
