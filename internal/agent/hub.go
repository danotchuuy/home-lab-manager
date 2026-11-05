// Package agent connects Docker hosts to hlm from the other side: "hlm
// agent" runs on a host, dials out to hlm over HTTPS (a WebSocket, so it
// works through NAT, reverse proxies and tunnels, with no Docker port
// open), and serves hlm the host's Docker API over that one connection,
// limited to the calls hlm makes. The Hub is hlm's end.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// ErrNotConnected is returned when a host's agent isn't connected.
var ErrNotConnected = errors.New("the host's agent isn't connected")

// CredentialPrefix starts an agent's credential: hlma_<host>_<secret>.
const CredentialPrefix = "hlma_"

// Credential is what an agent signs in with.
func Credential(host, secret string) string { return CredentialPrefix + host + "_" + secret }

// ParseCredential splits a credential into the host and the secret.
func ParseCredential(s string) (host, secret string, ok bool) {
	rest, ok := strings.CutPrefix(s, CredentialPrefix)
	if !ok {
		return "", "", false
	}
	// Host IDs have no underscores.
	i := strings.IndexByte(rest, '_')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// Status is an agent's connection.
type Status struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitzero"`
	// LastSeen is when it was last connected (or now, while it is).
	LastSeen time.Time `json:"lastSeen,omitzero"`
	Version  string    `json:"version,omitempty"`
	Remote   string    `json:"remote,omitempty"`
}

// Hub keeps the agents' connections.
type Hub struct {
	// Authenticate checks an agent's host and secret.
	Authenticate func(host, secret string) bool
	// OnConnect hears when a host's agent connects or goes (for logging
	// and refreshing what's listed).
	OnConnect func(host string, connected bool)
	Log       *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	seen     map[string]Status
}

type session struct {
	mux    *yamux.Session
	status Status
}

// NewHub makes a hub; set Authenticate before serving.
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{Log: log, sessions: map[string]*session{}, seen: map[string]Status{}}
}

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	// Pings keep proxies from dropping a quiet connection (Cloudflare's
	// idle timeout is 100 s) and notice a dead one.
	c.KeepAliveInterval = 25 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.StreamOpenTimeout = 30 * time.Second
	c.MaxStreamWindowSize = 1 << 20
	return c
}

// ServeHTTP takes an agent's connection: GET with the WebSocket upgrade
// and "Authorization: Bearer <credential>".
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cred, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	host, secret, ok := ParseCredential(cred)
	if !ok || h.Authenticate == nil || !h.Authenticate(host, secret) {
		h.Log.Warn("refused an agent's connection", "remote", r.RemoteAddr, "host", host)
		http.Error(w, `{"error":"this agent isn't enrolled (any more): enrol it again with a new command from hlm"}`, http.StatusUnauthorized)
		return
	}
	if !isUpgrade(r) {
		http.Error(w, `{"error":"agents connect with a WebSocket"}`, http.StatusBadRequest)
		return
	}
	ws, err := accept(w, r)
	if err != nil {
		h.Log.Warn("agent connection", "host", host, "err", err)
		return
	}
	mux, err := yamux.Client(ws, muxConfig())
	if err != nil {
		ws.Close()
		return
	}
	now := time.Now().UTC()
	s := &session{mux: mux, status: Status{
		Connected: true, Since: now, LastSeen: now, Version: r.Header.Get("X-HLM-Agent-Version"), Remote: r.RemoteAddr,
	}}
	h.mu.Lock()
	old := h.sessions[host]
	h.sessions[host] = s
	h.mu.Unlock()
	if old != nil {
		old.mux.Close()
	}
	h.Log.Info("agent connected", "host", host, "version", s.status.Version, "remote", r.RemoteAddr)
	if h.OnConnect != nil {
		h.OnConnect(host, true)
	}
	go func() {
		<-mux.CloseChan()
		h.mu.Lock()
		current := h.sessions[host] == s
		if current {
			delete(h.sessions, host)
			st := s.status
			st.Connected, st.LastSeen = false, time.Now().UTC()
			h.seen[host] = st
		}
		h.mu.Unlock()
		if current {
			h.Log.Info("agent disconnected", "host", host)
			if h.OnConnect != nil {
				h.OnConnect(host, false)
			}
		}
	}()
}

// Dial opens a connection to a host's Docker through its agent.
func (h *Hub) Dial(ctx context.Context, host string) (net.Conn, error) {
	h.mu.Lock()
	s := h.sessions[host]
	h.mu.Unlock()
	if s == nil {
		return nil, ErrNotConnected
	}
	type result struct {
		c   net.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		st, err := s.mux.OpenStream()
		done <- result{st, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, ErrNotConnected
		}
		return res.c, nil
	case <-ctx.Done():
		go func() {
			if res := <-done; res.c != nil {
				res.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// HostMetrics asks a host's agent for the host's CPU, memory, network and
// disk metrics.
func (h *Hub) HostMetrics(ctx context.Context, host string) (hostmetrics.Snapshot, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return h.Dial(ctx, host) },
		DisableKeepAlives: true,
	}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+HostMetricsPath, nil)
	if err != nil {
		return hostmetrics.Snapshot{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrNotConnected) {
			return hostmetrics.Snapshot{}, ErrNotConnected
		}
		return hostmetrics.Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// An agent from before host metrics passes the path to Docker's
		// allow-list, which refuses it.
		return hostmetrics.Snapshot{}, errors.New("the host's agent doesn't report host metrics: update it (run its command again with the newer image)")
	}
	var snap hostmetrics.Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&snap); err != nil {
		return hostmetrics.Snapshot{}, err
	}
	return snap, nil
}

// Status is a host's agent's connection.
func (h *Hub) Status(host string) Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[host]; ok {
		st := s.status
		st.LastSeen = time.Now().UTC()
		return st
	}
	return h.seen[host]
}

// Drop disconnects a host's agent (when the host is removed).
func (h *Hub) Drop(host string) {
	h.mu.Lock()
	s := h.sessions[host]
	delete(h.sessions, host)
	delete(h.seen, host)
	h.mu.Unlock()
	if s != nil {
		s.mux.Close()
	}
}

// Close disconnects every agent.
func (h *Hub) Close() {
	h.mu.Lock()
	list := h.sessions
	h.sessions = map[string]*session{}
	h.mu.Unlock()
	for _, s := range list {
		s.mux.Close()
	}
}
