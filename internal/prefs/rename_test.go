package prefs

import (
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRenameServer(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store)
	store.Set(secretTags, map[string][]string{"valheim": {"survival"}})
	store.Set(secretFavourites, map[string][]string{"ann": {"mc", "valheim"}, "bob": {"mc"}})
	if err := s.RenameServer("valheim", "valheim@nas"); err != nil {
		t.Fatal(err)
	}
	tags, _ := s.load(secretTags)
	favs, _ := s.load(secretFavourites)
	if _, ok := tags["valheim"]; ok || !slices.Equal(tags["valheim@nas"], []string{"survival"}) {
		t.Errorf("tags %v", tags)
	}
	if !slices.Equal(favs["ann"], []string{"mc", "valheim@nas"}) || !slices.Equal(favs["bob"], []string{"mc"}) {
		t.Errorf("favourites %v", favs)
	}
}
