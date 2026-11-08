package rbac

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRenameServer(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := NewStore(store, slog.New(slog.DiscardHandler))
	s.save([]Group{{ID: "a", Servers: []string{"valheim", "mc"}}, {ID: "b", Servers: []string{"mc"}}})
	if err := s.RenameServer("valheim", "valheim@nas"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.load()
	if !slices.Equal(list[0].Servers, []string{"valheim@nas", "mc"}) || !slices.Equal(list[1].Servers, []string{"mc"}) {
		t.Errorf("groups %+v", list)
	}
	if err := s.RenameServer("valheim@nas", "bad name"); err == nil {
		t.Error("renamed to an invalid name")
	}
}
