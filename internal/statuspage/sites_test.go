package statuspage

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// Each site shows its groups' servers, behind its own passwords.
func TestSites(t *testing.T) {
	s := withPasswords(t)
	s.opts.Groups = func() (map[string][]string, error) {
		return map[string][]string{"g-friends": {"valheim"}, "g-public": {"mc@nas", "ark"}}, nil
	}
	ctx := context.Background()
	now := time.Now()
	in := Settings{Enabled: true, Title: "All servers",
		Servers: []Entry{{Server: "valheim", Address: "v.example.com:2456"}, {Server: "mc@nas", Address: "mc.example.com"}, {Server: "ark"}},
		Sites: []Site{
			{Hostname: "friends.example.com", Groups: []string{"g-friends"}, Title: "Friends"},
			{Hostname: "public.example.com", Groups: []string{"g-public"}},
			{Hostname: "everything.example.com"},
		},
	}
	set, err := s.SavePasswords(in, ptr("main-view"), nil, []SitePasswords{{View: ptr("friends-view"), Details: ptr("friends-details")}, {}, {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := func(host string) []string {
		p, err := s.Page(ctx, host, now)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		var out []string
		for _, srv := range p.Servers {
			out = append(out, p.Title+": "+srv.Name)
		}
		return out
	}
	if got := names("friends.example.com"); !slices.Equal(got, []string{"Friends: Valheim"}) {
		t.Errorf("friends: %q", got)
	}
	if got := names("PUBLIC.example.com:443"); !slices.Equal(got, []string{"All servers: Minecraft", "All servers: ARK"}) {
		t.Errorf("public: %q", got)
	}
	if got := names("everything.example.com"); len(got) != 3 {
		t.Errorf("a site without groups: %q", got)
	}
	if got := names("hlm.lan"); len(got) != 3 {
		t.Errorf("any other address: %q", got)
	}
	if _, err := s.ServerBySlug(ctx, "friends.example.com", "minecraft", now); !errors.Is(err, ErrDisabled) {
		t.Errorf("another site's server by slug: %v", err)
	}
	if !s.IsSiteHost("Friends.example.com.") || s.IsSiteHost("hlm.lan") {
		t.Error("IsSiteHost")
	}

	// The passwords are the host's.
	check := func(host, cookie string, level int) {
		t.Helper()
		if a, _ := s.AccessFor(host, cookie, now); a.Level != level {
			t.Errorf("%s: level %d, want %d", host, a.Level, level)
		}
	}
	check("friends.example.com", "", LevelNone)
	check("public.example.com", "", LevelDetails) // no passwords there
	check("hlm.lan", "", LevelNone)
	if _, _, _, err := s.Unlock("friends.example.com", "main-view", now); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("the main password on a site: %v", err)
	}
	if _, _, _, err := s.Unlock("hlm.lan", "friends-view", now); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("a site's password elsewhere: %v", err)
	}
	_, friends, _, err := s.Unlock("friends.example.com", "friends-view", now)
	if err != nil {
		t.Fatal(err)
	}
	check("friends.example.com", friends, LevelView)
	_, main, _, _ := s.Unlock("hlm.lan", "main-view", now)
	check("hlm.lan", main, LevelDetails)
	// A cookie is for its site only.
	check("hlm.lan", friends, LevelNone)
	check("friends.example.com", main, LevelNone)

	// Saving again (the form sends the IDs) keeps each site's passwords,
	// even with a new domain; a site without its ID starts without.
	set.Sites[0].Hostname = "pals.example.com"
	set.Sites = append(set.Sites, Site{Hostname: "new.example.com"})
	if _, err := s.Save(set, nil); err != nil {
		t.Fatal(err)
	}
	check("pals.example.com", friends, LevelView)
	if _, _, _, err := s.Unlock("pals.example.com", "friends-details", now); err != nil {
		t.Errorf("details password after renaming: %v", err)
	}
	check("new.example.com", "", LevelDetails)

	// Groups must exist.
	if _, err := s.Save(Settings{Sites: []Site{{Hostname: "x.example.com", Groups: []string{"nope"}}}}, nil); err == nil {
		t.Error("a group that doesn't exist")
	}
}

// A group holding a whole Docker host ("@host") brings its servers to a
// site.
func TestSiteGroupsWithHosts(t *testing.T) {
	s := withPasswords(t)
	s.opts.Groups = func() (map[string][]string, error) {
		return map[string][]string{"g-nas": {"@nas"}, "g-here": {"@local"}}, nil
	}
	ctx := context.Background()
	in := Settings{Enabled: true,
		Servers: []Entry{{Server: "valheim"}, {Server: "mc@nas"}, {Server: "ark@pi"}},
		Sites:   []Site{{Hostname: "nas.example.com", Groups: []string{"g-nas"}}, {Hostname: "here.example.com", Groups: []string{"g-here"}}},
	}
	if _, err := s.SavePasswords(in, nil, nil, []SitePasswords{{}, {}}, nil); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string][]string{"nas.example.com": {"Minecraft"}, "here.example.com": {"Valheim"}} {
		p, err := s.Page(ctx, host, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, srv := range p.Servers {
			got = append(got, srv.Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", host, got, want)
		}
	}
}
