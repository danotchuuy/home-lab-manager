package gameprofile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const myGame = `id: mygame
name: My Game
match: [[mygame]]
query:
  a2s_ports: [27016]
console:
  rcon_port: 27017
  say: say
  save: save
`

func TestStore(t *testing.T) {
	t.Cleanup(func() { Use(nil) })
	dir := filepath.Join(t.TempDir(), "game-profiles")
	s := NewStore(dir)
	// No folder yet is fine: the built-in profiles are used.
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); len(st.Profiles) != len(Default().All()) || len(st.Errors) != 0 {
		t.Fatalf("state = %d profiles, %v", len(st.Profiles), st.Errors)
	}

	// Your own game.
	p, err := s.Import([]byte(myGame))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != Custom || p.File != "mygame.yaml" || p.Replaces {
		t.Errorf("imported = %+v", p)
	}
	if got := Match("someone/mygame-server:latest"); got == nil || got.ID != "mygame" || got.Console.RCONPort != 27017 {
		t.Errorf("Match = %+v", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "mygame.yaml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file: %v %v", info, err)
	}
	if src, _ := s.Source("mygame"); string(src) != myGame {
		t.Errorf("Source = %q", src)
	}

	// One that replaces a built-in profile.
	rust, _ := Builtin("rust")
	mine := strings.Replace(string(rust), "name: Rust", "name: Rust (mine)", 1)
	if p, err = s.Import([]byte(mine)); err != nil {
		t.Fatal(err)
	}
	if !p.Replaces || Match("didstopia/rust-server").Name != "Rust (mine)" {
		t.Errorf("replaced = %+v", p)
	}
	if b, _ := Builtin("rust"); string(b) != string(rust) {
		t.Error("the built-in source changed")
	}

	// Files with mistakes are reported, and the rest still used.
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("id: broken\nname: Broken\nmatch: [[x]]\nnope: 1\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "zz-again.yaml"), []byte(strings.Replace(myGame, "My Game", "Again", 1)), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a profile"), 0o600)
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if len(st.Errors) != 2 || st.Errors[0].File != "broken.yaml" || st.Errors[0].Error != "line 4: field nope not found" ||
		st.Errors[1].File != "zz-again.yaml" || !strings.Contains(st.Errors[1].Error, "mygame.yaml") {
		t.Errorf("errors = %+v", st.Errors)
	}
	if Match("mygame").Name != "My Game" {
		t.Error("the first file with an id should win")
	}

	// Importing again replaces the same file.
	if _, err := s.Import([]byte(strings.Replace(myGame, "My Game", "My Game 2", 1))); err != nil {
		t.Fatal(err)
	}
	if Match("mygame").Name != "My Game 2" {
		t.Error("not replaced")
	}
	if _, err := s.Import([]byte("id: Bad\n")); err == nil {
		t.Error("a bad profile was imported")
	}
	if _, err := s.Import([]byte("id: x\nname: " + strings.Repeat("x", MaxSize) + "\n")); err == nil {
		t.Error("an oversized profile was imported")
	}

	// Deleting your own brings back the built-in one.
	if err := s.Delete("rust"); err != nil {
		t.Fatal(err)
	}
	if got := Match("didstopia/rust-server"); got.Name != "Rust" || got.Source != BuiltIn {
		t.Errorf("after delete = %+v", got)
	}
	if err := s.Delete("rust"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a built-in = %v", err)
	}
	// The other file with its id is used once the first is gone.
	if err := s.Delete("mygame"); err != nil || Match("mygame").Name != "Again" || len(s.State().Errors) != 1 {
		t.Errorf("delete mygame: %v", err)
	}
	if err := s.Delete("mygame"); err != nil || Match("mygame") != nil {
		t.Errorf("delete mygame again: %v", err)
	}
	if _, err := s.Source("mygame"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Source of deleted = %v", err)
	}
}
