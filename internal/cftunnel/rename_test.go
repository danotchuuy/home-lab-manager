package cftunnel

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// A server's hostnames follow it to its new name; routes to an address
// and to other servers stay. Without a tunnel, nothing is applied.
func TestRenameServer(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, nil, nil, "", slog.New(slog.DiscardHandler))
	store.Set(secretRoutes, []Route{
		{Hostname: "valheim.example.com", Server: "valheim", Port: 80},
		{Hostname: "map.example.com", Server: "valheim", Port: 8080},
		{Hostname: "mc.example.com", Server: "mc", Port: 8123},
		{Hostname: "nas.example.com", Address: "192.168.1.5", Port: 5000},
	})
	if err := s.RenameServer(context.Background(), "valheim", "valheim@nas"); err != nil {
		t.Fatal(err)
	}
	if got := s.ServerHostnames("valheim@nas"); !slices.Equal(got, []string{"valheim.example.com", "map.example.com"}) {
		t.Errorf("moved %q", got)
	}
	if len(s.ServerHostnames("valheim")) != 0 || !slices.Equal(s.ServerHostnames("mc"), []string{"mc.example.com"}) {
		t.Error("other routes changed")
	}
	if err := s.RenameServer(context.Background(), "nothing", "x"); err != nil {
		t.Errorf("no routes: %v", err)
	}
}
