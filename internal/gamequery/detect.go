package gamequery

import (
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// candidate is a way a server might answer a status query.
type candidate struct {
	Protocol string
	Port     int // container port; 0 for the console
}

// candidates lists the ways a server might answer a status query, best
// first: from its game, its template's ports, and its ports.
func candidates(info docker.ContainerJSON, app *catalog.App, consoleReplies bool) []candidate {
	game := gameprofile.Match(info.Config.Image)
	env := map[string]string{}
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	envPort := func(name string, def int) int {
		if n, err := strconv.Atoi(env[name]); name != "" && err == nil && n > 0 && n < 65536 {
			return n
		}
		return def
	}
	var out []candidate
	add := func(c candidate) {
		if (c.Port > 0 || c.Protocol == ProtoRCON) && !slices.Contains(out, c) && len(out) < 16 {
			out = append(out, c)
		}
	}

	// Some games' own answers through their console (Palworld's REST API,
	// or RCON) have names and counts, where their Steam query port often
	// has nothing.
	if consoleReplies && game != nil && game.Query.ConsoleFirst {
		add(candidate{ProtoRCON, 0})
	}
	// The game's own status protocol.
	if game != nil {
		switch game.Query.Protocol {
		case "bedrock":
			add(candidate{ProtoBedrock, envPort(game.Query.PortEnv, game.Query.Port)})
		case "minecraft":
			add(candidate{ProtoMinecraft, envPort(game.Query.PortEnv, game.Query.Port)})
		}
	}

	// The template's ports, with Steam query ports first.
	type port struct {
		n     int
		proto string
		name  string
	}
	var ports []port
	if app != nil {
		for _, c := range app.Config {
			if c.Type != catalog.TypePort {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(c.Target))
			if err != nil || n <= 0 {
				continue
			}
			ports = append(ports, port{n, strings.ToLower(cmpOr(c.Mode, "tcp")), strings.ToLower(c.Name)})
		}
	}
	for key := range info.Config.ExposedPorts {
		n, proto, _ := strings.Cut(key, "/")
		if p, err := strconv.Atoi(n); err == nil {
			ports = append(ports, port{p, cmpOr(proto, "tcp"), ""})
		}
	}
	for key := range info.HostConfig.PortBindings {
		n, proto, _ := strings.Cut(key, "/")
		if p, err := strconv.Atoi(n); err == nil {
			ports = append(ports, port{p, cmpOr(proto, "tcp"), ""})
		}
	}
	slices.SortStableFunc(ports, func(a, b port) int { return a.n - b.n })
	for _, p := range ports {
		if strings.Contains(p.name, "query") || strings.Contains(p.name, "steam") {
			add(candidate{ProtoA2S, p.n})
		}
	}
	// The game's usual Steam query ports.
	if game != nil {
		for _, q := range game.Query.A2SPorts {
			add(candidate{ProtoA2S, q})
		}
	}
	// Many games answer A2S on the game port, or the one after it.
	for _, p := range ports {
		if p.proto == "udp" {
			add(candidate{ProtoA2S, p.n})
			add(candidate{ProtoA2S, p.n + 1})
		}
	}
	// Its player list, through the console.
	if consoleReplies && game != nil && game.Console.Players != nil {
		add(candidate{ProtoRCON, 0})
	}
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// consoleGame is the profile of a game whose player list can be read
// through its console, if the image's is one.
func consoleGame(image string) *gameprofile.Profile {
	if p := gameprofile.Match(image); p != nil && p.Console.Players != nil {
		return p
	}
	return nil
}
