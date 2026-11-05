package statuspage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func withPasswords(t *testing.T) *Service {
	t.Helper()
	s, _ := setup(t)
	// A cheap stand-in for argon2id.
	s.opts.HashPassword = func(p string) string { return "h:" + p }
	s.opts.PasswordMatches = func(h, p string) bool { return h == "h:"+p }
	return s
}

func TestAccessLevels(t *testing.T) {
	s := withPasswords(t)
	now := time.Now()
	base := Settings{Enabled: true, Servers: []Entry{{Server: "valheim", ShowPlayers: true, ShowNames: true, Address: "play.example.com:2456", Hint: "ask"}}}
	type want struct {
		level                     int
		viewLocked, detailsLocked bool
	}
	check := func(name, cookie string, w want) {
		t.Helper()
		a, err := s.AccessFor(cookie, now)
		if err != nil || a.Level != w.level || a.ViewLocked != w.viewLocked || a.DetailsLocked != w.detailsLocked {
			t.Errorf("%s: %+v, %v", name, a, err)
		}
	}

	// No passwords: everything, to everyone.
	s.SavePasswords(base, nil, nil, nil)
	check("open", "", want{LevelDetails, false, false})
	if _, _, _, err := s.Unlock("anything", now); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("unlock with no passwords: %v", err)
	}

	// Only a details password: the page is public, the details aren't.
	s.SavePasswords(base, nil, ptr("details-pw"), nil)
	check("details only", "", want{LevelView, false, true})
	lvl, tok, _, err := s.Unlock("details-pw", now)
	if err != nil || lvl != LevelDetails {
		t.Fatalf("unlock details: %d %v", lvl, err)
	}
	check("details unlocked", tok, want{LevelDetails, false, false})

	// Both: nothing without one; the view password sees the page without
	// details; the details password sees everything.
	s.SavePasswords(base, ptr("view-pw"), nil, nil)
	check("both, none given", "", want{LevelNone, true, true})
	check("both, details cookie from before", tok, want{LevelDetails, false, false})
	_, viewTok, _, err := s.Unlock("view-pw", now)
	if err != nil {
		t.Fatal(err)
	}
	check("both, view given", viewTok, want{LevelView, false, true})
	if _, _, _, err := s.Unlock("nope", now); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong password: %v", err)
	}

	// Cookies end with their password, their time, or any tampering.
	check("expired", viewTok, want{LevelView, false, true})
	if a, _ := s.AccessFor(viewTok, now.Add(UnlockFor+time.Minute)); !a.ViewLocked {
		t.Error("expired cookie still works")
	}
	if a, _ := s.AccessFor(strings.Replace(viewTok, "1.", "2.", 1), now); !a.ViewLocked {
		t.Error("tampered cookie works")
	}
	s.SavePasswords(base, ptr("new-view-pw"), nil, nil)
	check("after a new view password", viewTok, want{LevelNone, true, true})
	check("the details cookie still works", tok, want{LevelDetails, false, false})

	// Saving other settings keeps the passwords; "" removes one.
	s.Save(Settings{Enabled: true, Title: "x", Servers: base.Servers, ViewHash: "h:injected"}, nil)
	if set, _ := s.Settings(); set.ViewHash != "h:new-view-pw" || set.DetailsHash != "h:details-pw" {
		t.Errorf("passwords after Save: %q %q", set.ViewHash, set.DetailsHash)
	}
	s.SavePasswords(base, ptr(""), nil, nil)
	check("view password removed", "", want{LevelView, false, true})
	if _, err := s.SavePasswords(base, ptr("abc"), nil, nil); err == nil {
		t.Error("a 3-character password")
	}
}

func TestRedact(t *testing.T) {
	s := withPasswords(t)
	s.SavePasswords(Settings{Enabled: true, Servers: []Entry{{Server: "valheim", ShowPlayers: true, ShowNames: true, Address: "play.example.com:2456", Hint: "ask"}}},
		nil, ptr("details-pw"), nil)
	p, err := s.Page(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	locked := Redact(p, Access{Level: LevelView, DetailsLocked: true})
	if v := locked.Servers[0]; v.Address != "" || v.Hint != "" || v.PlayerNames != nil || v.Players == nil || *v.Players != 3 {
		t.Errorf("redacted = %+v", v)
	}
	if v := Redact(p, Access{Level: LevelDetails}).Servers[0]; v.Address == "" || len(v.PlayerNames) != 3 {
		t.Errorf("unlocked = %+v", v)
	}
	if p.Servers[0].Address == "" {
		t.Error("Redact changed the cached page")
	}
}
