package rbac

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	sec, err := secrets.Open(t.TempDir(), "test key")
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(sec, slog.New(slog.DiscardHandler))
}

func local(name string) Principal {
	return Principal{Name: name, Role: Member, Subjects: []Subject{{SubjectLocal, name}}}
}

func TestRoleOrder(t *testing.T) {
	order := []Role{Member, Viewer, Operator, Manager, Admin}
	for i, r := range order {
		for j, want := range order {
			if got := r.AtLeast(want); got != (i >= j) {
				t.Errorf("%s.AtLeast(%s) = %v", r, want, got)
			}
		}
	}
	for _, r := range []Role{None, "owner"} {
		if r.AtLeast(Member) || r.AtLeast(None) {
			t.Errorf("role %q should include nothing", r)
		}
		if r.Valid() {
			t.Errorf("role %q should not be valid", r)
		}
	}
}

func TestAccess(t *testing.T) {
	groups := []Group{
		{ID: "1", Name: "games", Servers: []string{"valheim", "shared"}, Grants: []Grant{
			{Subject{SubjectLocal, "Alice"}, Viewer},
			{Subject{SubjectOIDCGroup, "Friends"}, Operator},
			{Subject{SubjectKey, "k1"}, Viewer},
		}},
		{ID: "2", Name: "media", Servers: []string{"plex", "shared"}, Grants: []Grant{
			{Subject{SubjectLocal, "alice"}, Manager},
			{Subject{SubjectOIDCUser, "bob@example.com"}, Viewer},
		}},
	}
	tests := []struct {
		name   string
		p      Principal
		server string
		want   Role
	}{
		{"grant", local("alice"), "valheim", Viewer},
		{"username is case-insensitive", local("ALICE"), "valheim", Viewer},
		{"highest role across groups", local("alice"), "shared", Manager},
		{"not in any of their groups", local("alice"), "other", None},
		{"no grants", local("carol"), "valheim", None},
		{"oidc group", Principal{Role: Member, Subjects: []Subject{{SubjectOIDCGroup, "Friends"}}}, "valheim", Operator},
		{"oidc group is case-sensitive", Principal{Role: Member, Subjects: []Subject{{SubjectOIDCGroup, "friends"}}}, "valheim", None},
		{"oidc email", Principal{Role: Member, Subjects: []Subject{{SubjectOIDCUser, "Bob@example.com"}}}, "plex", Viewer},
		{"a kind doesn't match another", Principal{Role: Member, Subjects: []Subject{{SubjectOIDCUser, "alice"}}}, "valheim", None},
		{"api key", Principal{Role: Member, Subjects: []Subject{{SubjectKey, "k1"}}}, "valheim", Viewer},
		{"admin", Principal{Role: Admin}, "anything", Admin},
		{"unknown global role gets no grants", Principal{Role: "owner", Subjects: []Subject{{SubjectLocal, "alice"}}}, "valheim", None},
		{"no role gets no grants", Principal{Subjects: []Subject{{SubjectLocal, "alice"}}}, "valheim", None},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := computeAccess(tt.p, groups).Role(tt.server); got != tt.want {
				t.Errorf("role = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccessHidesOtherGroups(t *testing.T) {
	groups := []Group{
		{ID: "1", Name: "games", Servers: []string{"shared"}, Grants: []Grant{{Subject{SubjectLocal, "alice"}, Viewer}}},
		{ID: "2", Name: "secret", Servers: []string{"shared"}, Grants: []Grant{{Subject{SubjectLocal, "bob"}, Viewer}}},
	}
	a := computeAccess(local("alice"), groups)
	if got := a.GroupsOf("shared"); len(got) != 1 || got[0] != "games" {
		t.Errorf("groups of shared = %v", got)
	}
	if got := a.Groups(); len(got) != 1 || got[0].Name != "games" || len(got[0].Grants) != 0 {
		t.Errorf("groups = %+v", got)
	}
	if got := computeAccess(Principal{Role: Admin}, groups).GroupsOf("shared"); len(got) != 2 {
		t.Errorf("an admin should see both groups, got %v", got)
	}
}

func TestStore(t *testing.T) {
	s := testStore(t)
	g, err := s.Create(Group{Name: " games ", Servers: []string{"valheim", "valheim"}, Grants: []Grant{{Subject{SubjectLocal, "alice"}, Operator}}})
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "games" || g.ID == "" || len(g.Servers) != 1 {
		t.Fatalf("created %+v", g)
	}
	if _, err := s.Create(Group{Name: "GAMES"}); !errors.Is(err, ErrGroupExists) {
		t.Errorf("duplicate name: %v", err)
	}
	if !s.Access(local("alice")).Can("valheim", Operator) {
		t.Error("alice should have access")
	}

	// A new server with a deleted server's name doesn't inherit its access.
	if err := s.ForgetServer("valheim"); err != nil {
		t.Fatal(err)
	}
	if s.Access(local("alice")).Role("valheim") != None {
		t.Error("access should end with the server")
	}

	other, err := s.Create(Group{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetServerGroups("plex", []string{g.ID, other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetServerGroups("plex", []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	if !s.Access(local("alice")).Can("plex", Operator) {
		t.Error("alice should have access to plex")
	}
	if err := s.SetServerGroups("plex", []string{"nope"}); !errors.Is(err, ErrNoGroup) {
		t.Errorf("unknown group: %v", err)
	}

	// A new account with a deleted account's name doesn't inherit its grants.
	if err := s.ForgetSubject(Subject{SubjectLocal, "ALICE"}); err != nil {
		t.Fatal(err)
	}
	if s.Access(local("alice")).Role("plex") != None {
		t.Error("access should end with the account")
	}

	if err := s.Delete(g.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(g.ID); !errors.Is(err, ErrNoGroup) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := s.Update("nope", Group{Name: "x"}); !errors.Is(err, ErrNoGroup) {
		t.Errorf("update unknown: %v", err)
	}
}

func TestGroupValidation(t *testing.T) {
	s := testStore(t)
	bad := []Group{
		{},
		{Name: "x", Servers: []string{"../etc"}},
		{Name: "x", Grants: []Grant{{Subject{"robot", "a"}, Viewer}}},
		{Name: "x", Grants: []Grant{{Subject{SubjectLocal, " "}, Viewer}}},
		{Name: "x", Grants: []Grant{{Subject{SubjectLocal, "a"}, Admin}}},
		{Name: "x", Grants: []Grant{{Subject{SubjectLocal, "a"}, "owner"}}},
		{Name: "x", Grants: []Grant{{Subject{SubjectLocal, "a"}, Viewer}, {Subject{SubjectLocal, "A"}, Manager}}},
	}
	for _, g := range bad {
		if _, err := s.Create(g); err == nil {
			t.Errorf("accepted %+v", g)
		}
	}
}
