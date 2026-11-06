// Package discordtest is a fake Discord, REST API and gateway, for tests.
package discordtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/discord"
)

// Token is the bot token the fake accepts.
const Token = "good-token"

// IDs the fake uses.
const (
	AppID   = "100000000000000001"
	BotID   = "100000000000000002"
	GuildID = "200000000000000001"
)

// Call is a REST call the fake received.
type Call struct {
	Method, Path string
	Body         map[string]any
	List         []any
}

// Server is the fake.
type Server struct {
	*httptest.Server
	// Roles are the guild's roles.
	Roles []discord.Role

	mu       sync.Mutex
	calls    []Call
	conn     *discord.Conn
	ops      []map[string]any // what the bot sent on the gateway
	sessions int
	seq      int64
	// Interval is the heartbeat interval sent in hello.
	Interval time.Duration
}

// New starts a fake.
func New() *Server {
	s := &Server{Interval: 45 * time.Second, Roles: []discord.Role{
		{ID: GuildID, Name: "@everyone"}, {ID: "300000000000000001", Name: "Friends", Position: 1},
		{ID: "300000000000000002", Name: "hlm bot", Managed: true, Position: 2},
	}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// GatewayURL is the gateway's address.
func (s *Server) GatewayURL() string { return "ws" + strings.TrimPrefix(s.URL, "http") + "/gateway" }

// Calls are the REST calls so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Ops are the gateway messages the bot sent.
func (s *Server) Ops() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.ops...)
}

// Sessions counts gateway connections.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gateway/" || r.URL.Path == "/gateway" {
		s.gateway(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bot "+Token {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message": "401: Unauthorized", "code": 0}`)
		return
	}
	data, _ := io.ReadAll(r.Body)
	c := Call{Method: r.Method, Path: r.URL.Path}
	if len(data) > 0 {
		if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
			json.Unmarshal(data, &c.List)
		} else {
			json.Unmarshal(data, &c.Body)
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	p := r.URL.Path
	switch {
	case p == "/gateway/bot":
		reply(map[string]string{"url": s.GatewayURL()})
	case p == "/users/@me":
		reply(discord.User{ID: BotID, Username: "hlm", Bot: true})
	case p == "/oauth2/applications/@me":
		reply(discord.Application{ID: AppID, Name: "hlm"})
	case p == "/guilds/"+GuildID+"/roles":
		reply(s.Roles)
	case p == "/guilds/"+GuildID:
		reply(discord.Guild{ID: GuildID, Name: "Friends"})
	case strings.HasPrefix(p, "/applications/"+AppID+"/guilds/") && r.Method == http.MethodPut:
		reply(c.List)
	case strings.HasPrefix(p, "/interactions/") && strings.HasSuffix(p, "/callback"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/webhooks/"+AppID+"/") && r.Method == http.MethodPatch:
		reply(map[string]string{"id": "1"})
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message": "Unknown", "code": 10000}`)
	}
}

func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	conn, err := discord.Accept(w, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.sessions++
	s.mu.Unlock()
	s.send(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": s.Interval.Milliseconds()}})
	for {
		data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		s.mu.Lock()
		s.ops = append(s.ops, m)
		s.mu.Unlock()
		d, _ := m["d"].(map[string]any)
		switch m["op"] {
		case float64(1):
			s.send(conn, map[string]any{"op": 11})
		case float64(2):
			if d["token"] != Token {
				conn.Close(4004)
				return
			}
			s.dispatch(conn, "READY", map[string]any{
				"v": 10, "session_id": "session-1", "resume_gateway_url": "ws" + strings.TrimPrefix(s.URL, "http") + "/gateway",
				"user": map[string]any{"id": BotID, "username": "hlm", "bot": true}, "application": map[string]any{"id": AppID},
			})
		case float64(6):
			s.dispatch(conn, "RESUMED", map[string]any{})
		}
	}
}

func (s *Server) send(conn *discord.Conn, v any) {
	b, _ := json.Marshal(v)
	conn.WriteText(b)
}

func (s *Server) dispatch(conn *discord.Conn, t string, d any) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	s.send(conn, map[string]any{"op": 0, "t": t, "s": seq, "d": d})
}

// Interaction sends an interaction to the bot.
func (s *Server) Interaction(in map[string]any) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("no gateway connection")
	}
	s.dispatch(conn, "INTERACTION_CREATE", in)
	return nil
}

// Drop closes the gateway connection as a network failure would.
func (s *Server) Drop() {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		conn.Close(4000)
	}
}
