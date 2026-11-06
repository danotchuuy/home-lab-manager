package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Gateway opcodes.
const (
	gwDispatch       = 0
	gwHeartbeat      = 1
	gwIdentify       = 2
	gwResume         = 6
	gwReconnect      = 7
	gwInvalidSession = 9
	gwHello          = 10
	gwHeartbeatAck   = 11
)

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Ready is what the gateway says once identified.
type Ready struct {
	User             User   `json:"user"`
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
	Application      struct {
		ID string `json:"id"`
	} `json:"application"`
}

// Interaction is a slash command, or its autocomplete.
type Interaction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"` // 2: command, 4: autocomplete
	Token         string `json:"token"`
	GuildID       string `json:"guild_id,omitempty"`
	ChannelID     string `json:"channel_id,omitempty"`
	Member        *struct {
		User  User     `json:"user"`
		Nick  string   `json:"nick,omitempty"`
		Roles []string `json:"roles"`
	} `json:"member,omitempty"`
	User *User `json:"user,omitempty"` // in direct messages
	Data struct {
		Name    string   `json:"name"`
		Options []Option `json:"options,omitempty"`
	} `json:"data"`
}

// Interaction types.
const (
	InteractionCommand      = 2
	InteractionAutocomplete = 4
)

// Option is a command's argument as given.
type Option struct {
	Name    string `json:"name"`
	Type    int    `json:"type"`
	Value   any    `json:"value"`
	Focused bool   `json:"focused,omitempty"`
}

// Option is the value of the named option, as text.
func (in Interaction) Option(name string) (string, bool) {
	for _, o := range in.Data.Options {
		if o.Name == name {
			s, _ := o.Value.(string)
			return s, o.Focused
		}
	}
	return "", false
}

// FatalError is a close the gateway means: connecting again won't help
// (a bad token, say) until the settings change.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

// fatalCloses are gateway close codes not worth reconnecting after.
var fatalCloses = map[int]string{
	4004: "Discord doesn't accept the bot token",
	4010: "invalid shard",
	4011: "the bot needs sharding",
	4012: "invalid gateway version",
	4013: "invalid intents",
	4014: "the bot asked for intents it isn't allowed",
}

// freshCloses end the session: the next connection identifies anew.
var freshCloses = map[int]bool{4007: true, 4009: true}

// gateway keeps one bot session going.
type gateway struct {
	client *Client
	// url, if set, replaces the URL Discord gives (tests).
	url           string
	log           *slog.Logger
	onReady       func(Ready)
	onInteraction func(Interaction)

	seq       atomic.Int64
	sessionID string
	resumeURL string
}

// connect runs one connection until it ends. A *FatalError means stop.
func (g *gateway) connect(ctx context.Context) error {
	base := g.resumeURL
	if g.sessionID == "" || base == "" {
		base = g.url
		if base == "" {
			u, err := g.client.GatewayURL(ctx)
			if err != nil {
				if errors.Is(err, ErrBadToken) {
					return &FatalError{err}
				}
				return err
			}
			base = u
		}
	}
	u := strings.TrimRight(base, "/") + "/?v=10&encoding=json"
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, err := Dial(dctx, u, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to Discord: %w", err)
	}
	defer conn.Close(1000)
	stop := context.AfterFunc(ctx, func() { conn.Close(1000) })
	defer stop()

	// Hello first.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	hello, err := g.read(conn)
	if err != nil {
		return err
	}
	if hello.Op != gwHello {
		return fmt.Errorf("discord: expected hello, got op %d", hello.Op)
	}
	var h struct {
		Interval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &h); err != nil || h.Interval <= 0 {
		return errors.New("discord: bad hello")
	}
	interval := time.Duration(h.Interval) * time.Millisecond

	if g.sessionID != "" {
		err = g.send(conn, gwResume, map[string]any{"token": g.client.Token, "session_id": g.sessionID, "seq": g.seq.Load()})
	} else {
		err = g.send(conn, gwIdentify, map[string]any{
			"token":      g.client.Token,
			"intents":    0, // interactions need none
			"properties": map[string]string{"os": "linux", "browser": "hlm", "device": "hlm"},
		})
	}
	if err != nil {
		return err
	}

	// Heartbeats; a missed ack means the connection is dead.
	var acked atomic.Bool
	acked.Store(true)
	beat := func() error {
		var d any
		if s := g.seq.Load(); s > 0 {
			d = s
		}
		return g.send(conn, gwHeartbeat, d)
	}
	hbCtx, hbStop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wait := time.Duration(rand.Float64() * float64(interval))
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-time.After(wait):
			}
			wait = interval
			if !acked.Swap(false) {
				g.log.Warn("discord: no heartbeat answer; reconnecting")
				conn.Close(4000)
				return
			}
			if beat() != nil {
				return
			}
		}
	}()
	defer func() {
		hbStop()
		wg.Wait()
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(interval + 30*time.Second))
		p, err := g.read(conn)
		if err != nil {
			var ce *CloseError
			if errors.As(err, &ce) {
				if why, ok := fatalCloses[ce.Code]; ok {
					return &FatalError{errors.New(why)}
				}
				if freshCloses[ce.Code] {
					g.sessionID = ""
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if p.S != nil {
			g.seq.Store(*p.S)
		}
		switch p.Op {
		case gwHeartbeatAck:
			acked.Store(true)
		case gwHeartbeat:
			if err := beat(); err != nil {
				return err
			}
		case gwReconnect:
			return errors.New("discord asked for a reconnect")
		case gwInvalidSession:
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				g.sessionID = ""
			}
			return errors.New("discord: invalid session")
		case gwDispatch:
			switch p.T {
			case "READY":
				var r Ready
				if err := json.Unmarshal(p.D, &r); err != nil {
					return fmt.Errorf("discord: bad READY: %w", err)
				}
				g.sessionID, g.resumeURL = r.SessionID, r.ResumeGatewayURL
				if g.onReady != nil {
					g.onReady(r)
				}
			case "RESUMED":
				g.log.Info("discord: session resumed")
			case "INTERACTION_CREATE":
				var in Interaction
				if err := json.Unmarshal(p.D, &in); err != nil {
					g.log.Warn("discord: bad interaction", "err", err)
					continue
				}
				if g.onInteraction != nil {
					go g.onInteraction(in)
				}
			}
		}
	}
}

func (g *gateway) read(conn *Conn) (payload, error) {
	data, err := conn.ReadMessage()
	if err != nil {
		return payload{}, err
	}
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return payload{}, fmt.Errorf("discord: bad message: %w", err)
	}
	return p, nil
}

func (g *gateway) send(conn *Conn, op int, d any) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b, err := json.Marshal(payload{Op: op, D: raw})
	if err != nil {
		return err
	}
	return conn.WriteText(b)
}
