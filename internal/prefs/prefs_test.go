package prefs

import (
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestTagsAndFavourites(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store)
	tags, err := s.SetTags("valheim", []string{" survival ", "Friends", "friends", "", "pvp  zone"})
	if err != nil || !slices.Equal(tags, []string{"survival", "Friends", "pvp zone"}) {
		t.Fatalf("tags = %q, %v", tags, err)
	}
	for _, bad := range [][]string{{"<script>"}, {"-dash"}, {"a-very-long-tag-name-that-goes-on-and-on"}, {"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}} {
		if _, err := s.SetTags("valheim", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	s.SetTags("ark", []string{"survival"})
	if _, err := s.SetFavourite("local:alice", "valheim", true); err != nil {
		t.Fatal(err)
	}
	s.SetFavourite("local:alice", "ark", true)
	s.SetFavourite("local:bob", "valheim", true)
	if f, _ := s.Favourites("local:alice"); !slices.Equal(f, []string{"ark", "valheim"}) {
		t.Errorf("alice = %q", f)
	}
	s.SetFavourite("local:alice", "ark", false)
	if f, _ := s.Favourites("local:alice"); !slices.Equal(f, []string{"valheim"}) {
		t.Errorf("alice after unstarring = %q", f)
	}
	if err := s.ForgetServer("valheim"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Tags()
	a, _ := s.Favourites("local:alice")
	b, _ := s.Favourites("local:bob")
	if _, ok := all["valheim"]; ok || len(a)+len(b) != 0 || !slices.Equal(all["ark"], []string{"survival"}) {
		t.Errorf("after forgetting: tags %v, favourites %q %q", all, a, b)
	}
	if _, err := s.SetTags("ark", nil); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Tags(); len(all) != 0 {
		t.Errorf("empty tags kept: %v", all)
	}
}
