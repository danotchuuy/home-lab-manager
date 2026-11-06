package console

import (
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
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

// Detect works out how a server probably takes commands, and says why. It
// returns Settings with ModeNone if it can't tell.
func Detect(info docker.ContainerJSON) (Settings, string) {
	env := envMap(info.Config.Env)
	image := strings.ToLower(info.Config.Image)
	game := gameprofile.Match(image)
	// Palworld's REST API replaces its RCON.
	if game != nil && game.Console.REST != nil && game.Console.REST.Driver == "palworld" && restAvailable(info) {
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
	// Images that bring their own command sender.
	if game != nil {
		for _, e := range game.Console.Exec {
			if strings.Contains(image, strings.ToLower(e.Image)) && !e.Off(env) {
				return Settings{Mode: ModeExec, Exec: e.Command}, e.Why
			}
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
	if info.Config.OpenStdin && game != nil && game.Console.Stdin {
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
	if game := gameprofile.Match(info.Config.Image); game != nil {
		return game.Console.RCONPort
	}
	return 0
}

// gameOf names the game an image runs (its profile's id), or "" if it
// isn't known.
func gameOf(image string) string {
	if p := gameprofile.Match(image); p != nil {
		return p.ID
	}
	return ""
}
