package console

import (
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// passwordVars are environment variables that commonly hold a game's RCON
// password, best first.
var passwordVars = []string{
	"RCON_PASSWORD", "RCON_PASS", "RCONPASSWORD", "RCON_PWD", "RCONPASS",
	"SERVER_ADMIN_PASSWORD", "ADMIN_PASSWORD", "ADMINPASSWORD", "SERVER_ADMINPASSWORD",
}

// portVars are environment variables that commonly hold the RCON port.
var portVars = []string{"RCON_PORT", "RCONPORT", "RCON_PORT_NUMBER"}

// knownRCONPorts are games' usual RCON ports: Minecraft and Palworld 25575,
// Source games 27015, ARK 27020, Rust 28016.
var knownRCONPorts = []int{25575, 27020, 28016, 27015}

// execImages are images that come with a command that sends commands to
// the server for you, knowing the RCON password itself.
var execImages = []struct {
	match, exec, because string
	off                  func(env map[string]string) bool
}{
	{"itzg/minecraft-bedrock-server", "send-command {command}", "This image has send-command, which types commands into the server's console.", nil},
	{"itzg/minecraft-server", "rcon-cli {command}", "This image has rcon-cli, which already knows the server's RCON password.",
		func(env map[string]string) bool { return strings.EqualFold(env["ENABLE_RCON"], "false") }},
	{"palworld-server-docker", "rcon-cli {command}", "This image has rcon-cli, which already knows the server's RCON password.",
		func(env map[string]string) bool { return strings.EqualFold(env["RCON_ENABLED"], "false") }},
}

// restQuickCommands are the shortcuts for Palworld's REST API.
var restQuickCommands = []string{"ShowPlayers", "Metrics", "Info", "Save", "Broadcast ", "KickPlayer ", "BanPlayer "}

// Detect works out how a server probably takes commands, and says why. It
// returns Settings with ModeNone if it can't tell.
func Detect(info docker.ContainerJSON) (Settings, string) {
	env := envMap(info.Config.Env)
	image := strings.ToLower(info.Config.Image)
	// Palworld's REST API replaces its RCON.
	if gameOf(image) == "palworld" && restAvailable(info) {
		set := Settings{Mode: ModeREST}
		for _, v := range restPasswordVars {
			if env[v] != "" {
				set.PasswordVar = v
				return set, "This Palworld server has its REST API (port " + strconv.Itoa(restPort(info, set)) +
					") and an admin password in " + v + "."
			}
		}
		return set, "This Palworld server has its REST API on port " + strconv.Itoa(restPort(info, set)) +
			"; enter its admin password."
	}
	for _, e := range execImages {
		if strings.Contains(image, e.match) && (e.off == nil || !e.off(env)) {
			return Settings{Mode: ModeExec, Exec: e.exec}, e.because
		}
	}
	for _, v := range passwordVars {
		if env[v] == "" {
			continue
		}
		if port := rconPort(info); port != 0 {
			return Settings{Mode: ModeRCON, PasswordVar: v},
				"The server has an RCON password in " + v + " and RCON on port " + strconv.Itoa(port) + "."
		}
	}
	// hlm opens every server's input, so that alone says nothing; only
	// suggest it for games known to read commands there.
	if info.Config.OpenStdin && stdinGames[gameOf(info.Config.Image)] {
		return Settings{Mode: ModeStdin}, "This game reads commands from its console, so they can be typed into it."
	}
	return Settings{}, ""
}

// restAvailable says whether a Palworld container looks like it runs the
// REST API: a variable turns it on, or it exposes or publishes its port.
func restAvailable(info docker.ContainerJSON) bool {
	env := envMap(info.Config.Env)
	for _, v := range restEnabledVars {
		switch strings.ToLower(strings.TrimSpace(env[v])) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	key := strconv.Itoa(restPort(info, Settings{})) + "/tcp"
	_, exposed := info.Config.ExposedPorts[key]
	_, published := info.HostConfig.PortBindings[key]
	return exposed || published
}

// rconPort finds a container's RCON port: from a variable that sets it, a
// usual RCON port that it exposes or publishes, or its game's usual one.
func rconPort(info docker.ContainerJSON) int {
	env := envMap(info.Config.Env)
	for _, v := range portVars {
		if n, err := strconv.Atoi(strings.TrimSpace(env[v])); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	has := func(port int) bool {
		key := strconv.Itoa(port) + "/tcp"
		_, exposed := info.Config.ExposedPorts[key]
		_, published := info.HostConfig.PortBindings[key]
		return exposed || published
	}
	for _, p := range knownRCONPorts {
		if has(p) {
			return p
		}
	}
	// Images don't always declare the RCON port (itzg/minecraft-server
	// only exposes the game's), so fall back to the game's usual one.
	return gameRCONPorts[gameOf(info.Config.Image)]
}

// stdinGames are games whose servers read commands from their console.
var stdinGames = map[string]bool{"terraria": true}

// gameRCONPorts are games' default RCON ports.
var gameRCONPorts = map[string]int{
	"minecraft": 25575, "palworld": 25575, "ark": 27020, "rust": 28016, "cs2": 27015, "factorio": 27015,
}

// games maps the parts of image names to the games they run; every part
// listed has to be in the name.
var games = []struct {
	parts []string
	game  string
}{
	{[]string{"minecraft", "bedrock"}, "minecraft-bedrock"},
	{[]string{"minecraft"}, "minecraft"},
	{[]string{"palworld"}, "palworld"},
	{[]string{"ark"}, "ark"},
	{[]string{"arkse"}, "ark"},
	{[]string{"rust"}, "rust"},
	{[]string{"terraria"}, "terraria"},
	{[]string{"tshock"}, "terraria"},
	{[]string{"factorio"}, "factorio"},
	{[]string{"cs2"}, "cs2"},
	{[]string{"7dtd"}, "7dtd"},
	{[]string{"7daystodie"}, "7dtd"},
}

// gameOf names the game an image runs, from the parts of its name (split
// at - _ and .), or "" if it isn't known.
func gameOf(image string) string {
	image = strings.ToLower(image)
	// The repository name, without registry, owner and tag.
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	image, _, _ = strings.Cut(image, ":")
	parts := strings.FieldsFunc(image, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for _, g := range games {
		if !slices.ContainsFunc(g.parts, func(p string) bool { return !slices.Contains(parts, p) }) {
			return g.game
		}
	}
	return ""
}

// gameCommands are common commands for each game, offered as shortcuts. A
// trailing space means something goes after it.
var gameCommands = map[string][]string{
	"minecraft":         {"list", "save-all", "say ", "whitelist add ", "op ", "kick ", "time set day", "weather clear"},
	"minecraft-bedrock": {"list", "say ", "allowlist add ", "op ", "kick ", "time set day"},
	"palworld":          {"ShowPlayers", "Info", "Save", "Broadcast ", "KickPlayer ", "BanPlayer "},
	"ark":               {"ListPlayers", "SaveWorld", "Broadcast ", "ServerChat ", "KickPlayer ", "DestroyWildDinos"},
	"rust":              {"status", "players", "server.save", "say ", "kick ", "ban "},
	"terraria":          {"playing", "save", "say ", "kick ", "time", "dawn"},
	"factorio":          {"/players online", "/server-save", "/kick ", "/ban ", "/time"},
	"cs2":               {"status", "say ", "changelevel ", "mp_restartgame 1", "kick "},
	"7dtd":              {"listplayers", "saveworld", "say ", "kick ", "gettime"},
}
