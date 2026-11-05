package gamequery

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// candidate is a way a server might answer a status query.
type candidate struct {
	Protocol string
	Port     int // container port; 0 for the console
}

// knownQueryPorts are games' usual Steam query ports (UDP).
var knownQueryPorts = map[string][]int{
	"valheim":    {2457},
	"enshrouded": {15637},
	"vrising":    {9877},
	"ark":        {27015},
	"arkse":      {27015},
	"cs2":        {27015},
	"conan":      {27015},
	"rust":       {28015},
	"7dtd":       {26900},
	"7daystodie": {26900},
	"zomboid":    {16261},
	"palworld":   {27015},
	"satisfactory": {
		15777,
	},
}

// consoleGames are games whose player list can be read through the
// console (see rconCommands).
var consoleGames = []string{"palworld", "minecraft", "ark"}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

// words splits an image reference, tag included (ich777's images name the
// game in the tag, as in steamcmd:valheim), into lower-case words.
func words(image string) []string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return wordRE.FindAllString(strings.ToLower(image), -1)
}

func hasWord(ws []string, w ...string) bool {
	for _, x := range w {
		if !slices.Contains(ws, x) {
			return false
		}
	}
	return true
}

// candidates lists the ways a server might answer a status query, best
// first: from its game, its template's ports, and its ports.
func candidates(info docker.ContainerJSON, app *catalog.App, consoleReplies bool) []candidate {
	ws := words(info.Config.Image)
	env := map[string]string{}
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	envPort := func(def int) int {
		if n, err := strconv.Atoi(env["SERVER_PORT"]); err == nil && n > 0 && n < 65536 {
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

	// Palworld's own answers through its console (its REST API, or RCON)
	// have names and counts; its Steam query port often has nothing.
	if consoleReplies && hasWord(ws, "palworld") {
		add(candidate{ProtoRCON, 0})
	}
	switch {
	case hasWord(ws, "minecraft", "bedrock"):
		add(candidate{ProtoBedrock, envPort(19132)})
	case hasWord(ws, "minecraft"):
		add(candidate{ProtoMinecraft, envPort(25565)})
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
	for game, qs := range knownQueryPorts {
		if hasWord(ws, game) {
			for _, q := range qs {
				add(candidate{ProtoA2S, q})
			}
		}
	}
	// Many games answer A2S on the game port, or the one after it.
	for _, p := range ports {
		if p.proto == "udp" {
			add(candidate{ProtoA2S, p.n})
			add(candidate{ProtoA2S, p.n + 1})
		}
	}
	if consoleReplies {
		for _, g := range consoleGames {
			if hasWord(ws, g) {
				add(candidate{ProtoRCON, 0})
			}
		}
	}
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// consoleGame names the game whose player list command to use.
func consoleGame(image string) string {
	ws := words(image)
	for _, g := range consoleGames {
		if hasWord(ws, g) {
			return g
		}
	}
	return ""
}
