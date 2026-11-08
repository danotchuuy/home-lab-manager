package hosts

import (
	"context"
	"log/slog"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestHTTPSMove(t *testing.T) {
	for _, tt := range []struct {
		old, new, current, want string
	}{
		// HTTPS comes on: plain HTTP agents go to the domain, same port.
		{"", "hlm.example.com", "http://192.168.1.10:8080", "https://hlm.example.com:8080"},
		{"", "hlm.example.com", "http://192.168.1.10", "https://hlm.example.com"},
		// Already HTTPS some other way (a proxy): left alone.
		{"", "hlm.example.com", "https://proxy.example.com", ""},
		// Another domain.
		{"old.example.com", "hlm.example.com", "https://old.example.com:8080", "https://hlm.example.com:8080"},
		{"old.example.com", "hlm.example.com", "https://proxy.example.com", ""},
		// HTTPS goes off: back to plain HTTP at the domain.
		{"hlm.example.com", "", "https://HLM.example.com:8080", "http://hlm.example.com:8080"},
		{"hlm.example.com", "", "http://192.168.1.10:8080", ""},
		// An agent that doesn't say where it connects (an older one).
		{"", "hlm.example.com", "", ""},
	} {
		got, ok := HTTPSMove(tt.old, tt.new)(tt.current)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%q→%q, %q: %q %v, want %q", tt.old, tt.new, tt.current, got, ok, tt.want)
		}
	}
}

func TestMoveAgentsNotConnected(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	hub := agent.NewHub(slog.New(slog.DiscardHandler))
	r := New(Options{Secrets: store, Agents: hub, Log: slog.New(slog.DiscardHandler)})
	e, err := r.CreateEnrolment(Host{ID: "nas", AppdataRoot: "/a", Address: "192.168.1.20"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Enrol(e.Token); err != nil {
		t.Fatal(err)
	}
	moves := r.MoveAgents(context.Background(), func(string) (string, bool) { return "https://hlm.example.com", true })
	if len(moves) != 1 || moves[0].State != "pending" || moves[0].To != "https://hlm.example.com" {
		t.Fatalf("moves = %+v", moves)
	}
	if p := r.pendingMoves(); p["nas"] != "https://hlm.example.com" {
		t.Errorf("pending = %v", p)
	}
	if mv := r.AgentMoveStatus("nas"); mv == nil || mv.State != "pending" {
		t.Errorf("status = %+v", mv)
	}
	// Still not connected when it's tried: kept for later.
	r.movePending("nas")
	if p := r.pendingMoves(); p["nas"] == "" {
		t.Error("pending move dropped")
	}
	// HTTPS changing again before it connects: from where it was going.
	r.MoveAgents(context.Background(), HTTPSMove("hlm.example.com", "new.example.com"))
	if p := r.pendingMoves(); p["nas"] != "https://new.example.com" {
		t.Errorf("pending after another change = %v", p)
	}
}
