package gameprofile

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// Every built-in profile parses and validates (Default panics otherwise).
func TestBuiltinProfiles(t *testing.T) {
	s := Default()
	if len(s.All()) < 17 {
		t.Fatalf("%d profiles", len(s.All()))
	}
	for _, p := range s.All() {
		if p.Console.Players != nil && p.Console.Players.Format != "csv" && p.Console.Players.re == nil {
			t.Errorf("%s: players pattern not compiled", p.ID)
		}
	}
}

func TestMatch(t *testing.T) {
	for image, want := range map[string]string{
		"itzg/minecraft-server:java21":              "minecraft",
		"itzg/minecraft-bedrock-server":             "minecraft-bedrock", // the more specific set wins
		"thijsvanloef/palworld-server-docker:v1.2":  "palworld",
		"ich777/steamcmd:valheim":                   "valheim", // the game in the tag
		"ich777/steamcmd:arkse":                     "ark",
		"ghcr.io/team/7daystodie":                   "7dtd",
		"ryshe/terraria:tshock":                     "terraria",
		"didstopia/rust-server":                     "rust",
		"factoriotools/factorio:stable":             "factorio",
		"joedwards32/cs2":                           "cs2",
		"mbround18/valheim:3":                       "valheim",
		"sknnr/enshrouded-dedicated-server":         "enshrouded",
		"trueosiris/vrising":                        "vrising",
		"wolveix/satisfactory-server":               "satisfactory",
		"nginx:alpine":                              "",
		"lscr.io/linuxserver/sparkle":               "", // whole words only: not "ark"
		"registry.example.com/team/minecraftserver": "",
	} {
		p := Match(image)
		got := ""
		if p != nil {
			got = p.ID
		}
		if got != want {
			t.Errorf("%s: %q, want %q", image, got, want)
		}
	}
	if !slices.Equal(Words("ghcr.io/Owner/Palworld-Server:v1.2"), []string{"palworld", "server", "v1", "2"}) {
		t.Errorf("words: %v", Words("ghcr.io/Owner/Palworld-Server:v1.2"))
	}
}

func TestPlayers(t *testing.T) {
	s := Default()
	for _, tt := range []struct {
		game, out   string
		online, max int
		names       []string
	}{
		{"palworld", "name,playeruid,steamid\r\nAlice,123,7656\r\nBob,456,7657\r\n", 2, 0, []string{"Alice", "Bob"}},
		{"palworld", "name,playeruid,steamid\n", 0, 0, nil},
		{"palworld", "NAME,playeruid\n,123\n", 1, 0, nil}, // a player without a name still counts
		{"minecraft", "There are 2 of a max of 20 players online: alex, sam", 2, 20, []string{"alex", "sam"}},
		{"minecraft", "There are 0 of a max of 20 players online:", 0, 20, nil},
		{"minecraft", "There are 1 of a max of 10 players online: alex\n", 1, 10, []string{"alex"}},
		{"ark", "0. Alice, 0002abc\n1. Bob, 0002def", 2, 0, []string{"Alice", "Bob"}},
		{"ark", "No Players Connected", 0, 0, nil},
		{"ark", "0. Mr. Smith, 0002abc", 1, 0, []string{"Mr. Smith"}}, // the first ". " ends the number; the name runs to the comma
	} {
		got, err := s.ByID(tt.game).Console.Players.Parse(tt.out)
		if err != nil || got.Online != tt.online || got.Max != tt.max || !slices.Equal(got.Names, tt.names) {
			t.Errorf("%s %q: %+v %v", tt.game, tt.out, got, err)
		}
	}
	// IDs, for actions that take them.
	for _, tt := range []struct {
		game, out string
		ids       []string
	}{
		{"palworld", "name,playeruid,steamid\nAlice,123,steam_7656\nBob,456\n", []string{"steam_7656", ""}},
		{"ark", "0. Alice, 0002abc\n1. Mr. Smith, 76561198", []string{"0002abc", "76561198"}},
		{"minecraft", "There are 1 of a max of 10 players online: alex", nil},
	} {
		p := s.ByID(tt.game).Console.Players
		got, err := p.Parse(tt.out)
		if err != nil || !slices.Equal(got.IDs, tt.ids) || p.HasIDs() != (tt.ids != nil) {
			t.Errorf("%s ids: %+v %v", tt.game, got, err)
		}
	}
	for game, garbage := range map[string]string{"palworld": "Unknown command", "minecraft": "xxxxxxxxxx"} {
		if _, err := s.ByID(game).Console.Players.Parse(garbage); err == nil {
			t.Errorf("%s: garbage accepted", game)
		}
	}
}

func TestValidation(t *testing.T) {
	good := "id: x\nname: X\nmatch: [[x]]\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, y := range map[string]string{
		"unknown field":    good + "colour: red\n",
		"bad id":           "id: X Y\nname: X\nmatch: [[x]]\n",
		"no name":          "id: x\nmatch: [[x]]\n",
		"no match":         "id: x\nname: X\n",
		"empty set":        "id: x\nname: X\nmatch: [[]]\n",
		"two words":        "id: x\nname: X\nmatch: [[two words]]\n",
		"upper case word":  "id: x\nname: X\nmatch: [[Palworld]]\n",
		"protocol":         good + "query: {protocol: gopher, port: 70}\n",
		"no port":          good + "query: {protocol: minecraft}\n",
		"stray port":       good + "query: {port: 70}\n",
		"a2s port":         good + "query: {a2s_ports: [70000]}\n",
		"rcon port":        good + "console: {rcon_port: -1}\n",
		"save before stop": good + "console: {save_before_stop: true}\n",
		"exec":             good + "console: {exec: [{image: x, command: rcon-cli}]}\n",
		"driver":           good + "console: {rest: {driver: nope}}\n",
		"format":           good + "console: {players: {command: list, format: xml}}\n",
		"csv header":       good + "console: {players: {command: list, format: csv}}\n",
		"no command":       good + "console: {players: {format: csv, header: x}}\n",
		"bad regexp":       good + "console: {players: {command: list, format: lines, pattern: '('}}\n",
		"missing group":    good + "console: {players: {command: list, format: summary, pattern: '(?P<online>\\d+)'}}\n",
		"file name":        good + "settings_files: [{names: [a/b.ini]}]\n",
		"no file names":    good + "settings_files: [{prefer: /x/}]\n",
		"unknown action":   good + "console: {actions: {smite: 'smite {player}'}}\n",
		"no player":        good + "console: {actions: {kick: kick}}\n",
		"placeholder":      good + "console: {actions: {kick: 'kick {player} {when}'}}\n",
		"two lines":        good + "console: {actions: {kick: \"kick {player}\\nstop\"}}\n",
		"id column 1":      good + "console: {players: {command: l, format: csv, header: n, id_column: 1}}\n",
		"id column lines":  good + "console: {players: {command: l, format: lines, pattern: '(?P<name>.*)', id_column: 2}}\n",
		"unknown list":     good + "player_lists: {friends: {file: f.txt, format: lines}}\n",
		"list format":      good + "player_lists: {ban: {file: f.txt, format: xml}}\n",
		"list path":        good + "player_lists: {ban: {file: a/f.txt, format: lines}}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Two profiles with one id.
	fsys := fstest.MapFS{"p/a.yaml": {Data: []byte(good)}, "p/b.yaml": {Data: []byte(good)}, "p/readme.txt": {Data: []byte("x")}}
	if _, err := Load(fsys, "p"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("duplicate id: %v", err)
	}
}

func TestMods(t *testing.T) {
	mc := Default().ByID("minecraft")
	if mc.Mods == nil || mc.Mods.Timeout() != 10*time.Minute || !mc.Ref().Mods {
		t.Fatalf("minecraft mods: %+v", mc.Mods)
	}
	for env, want := range map[string]string{"fabric": "/data/mods", "PAPER": "/data/plugins", "": ""} {
		name, l, ok := mc.Mods.Modrinth.Loader(map[string]string{"TYPE": env})
		if ok != (want != "") || l.Dir != want {
			t.Errorf("TYPE=%q: %s %+v %v", env, name, l, ok)
		}
	}
	if Default().ByID("valheim").Ref().Mods {
		t.Error("valheim takes mods?")
	}
	for image, env := range map[string]string{
		"hermsi/ark-server:latest":                       "GAME_MOD_IDS",
		"alinmear/docker-conanexiles":                    "CONANEXILES_MODS",
		"renegademaster/zomboid-dedicated-server:latest": "MOD_WORKSHOP_IDS",
	} {
		p := Match(image)
		if p == nil || p.Mods == nil {
			t.Errorf("%s: no mods", image)
			continue
		}
		w, ok := p.Mods.Workshop.For(image)
		if !ok || w.Env != env || !p.Ref().Mods {
			t.Errorf("%s: %+v", image, w)
		}
	}
	if _, ok := Default().ByID("ark").Mods.Workshop.For("thmhoag/arkserver"); ok {
		t.Error("an image hlm doesn't know the variable of")
	}
	base := "id: x\nname: X\nmatch: [[x]]\nmods:\n"
	for body, want := range map[string]string{
		"  modrinth: { loader_env: TYPE, loaders: { FABRIC: { modrinth: [fabric], dir: /data/mods } } }": "",
		"  modrinth: { loader_env: TYPE, loaders: {} }":                                                  "loaders needs",
		"  modrinth: { loader_env: 'A B', loaders: { F: { modrinth: [f], dir: /m } } }":                  "loader_env",
		"  modrinth: { loader_env: T, loaders: { F: { modrinth: [f], dir: data/mods } } }":               "clean absolute path",
		"  modrinth: { loader_env: T, loaders: { F: { modrinth: [f], dir: /data/../etc } } }":            "clean absolute path",
		"  modrinth: { loader_env: T, loaders: { F: { modrinth: ['F!'], dir: /m } } }":                   "Modrinth loader",
		"  start_timeout: 2h": "up to an hour",
	} {
		_, err := Parse([]byte(base + body))
		if (want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
}
