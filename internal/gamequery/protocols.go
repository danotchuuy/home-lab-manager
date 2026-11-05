// Package gamequery asks game servers how they're doing, in their own
// protocols: are they up, how many players are on and who, which map and
// version. hlm polls running servers with it.
package gamequery

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Protocols.
const (
	ProtoAuto      = ""          // find one
	ProtoA2S       = "a2s"       // Steam's server query (UDP)
	ProtoMinecraft = "minecraft" // Minecraft Java's server list ping (TCP)
	ProtoBedrock   = "bedrock"   // Minecraft Bedrock's RakNet ping (UDP)
	ProtoRCON      = "rcon"      // a player list command through the console
	ProtoTCP       = "tcp"       // only whether the port is open
	ProtoOff       = "off"       // don't query
)

// Result is what a server said.
type Result struct {
	Protocol    string        `json:"protocol"`
	Address     string        `json:"address,omitempty"`
	Name        string        `json:"name,omitempty"`
	Map         string        `json:"map,omitempty"`
	Game        string        `json:"game,omitempty"`
	Version     string        `json:"version,omitempty"`
	Players     int           `json:"players"`
	MaxPlayers  int           `json:"maxPlayers,omitempty"`
	PlayerNames []string      `json:"playerNames,omitempty"`
	Password    bool          `json:"password,omitempty"`
	Latency     time.Duration `json:"latency"`
	// HasPlayers is false when the protocol doesn't say (a TCP check).
	HasPlayers bool `json:"hasPlayers"`
}

const maxPlayerNames = 100

func deadline(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(t) {
		return dl
	}
	return t
}

// --- Steam A2S ---

var a2sInfoRequest = append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x54}, "Source Engine Query\x00"...)

// A2S asks a Steam game server for its info and players over UDP.
func A2S(ctx context.Context, addr string) (Result, error) {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", addr)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx, 3*time.Second))

	resp, err := a2sRequest(conn, a2sInfoRequest, 0x49, true)
	if err != nil {
		return Result{}, err
	}
	res, err := parseA2SInfo(resp)
	if err != nil {
		return Result{}, err
	}
	res.Protocol, res.Address, res.Latency, res.HasPlayers = ProtoA2S, addr, time.Since(start), true
	// Names are a bonus: some servers don't answer A2S_PLAYER.
	if res.Players > 0 {
		req := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x55, 0xFF, 0xFF, 0xFF, 0xFF}
		if p, err := a2sRequest(conn, req, 0x44, false); err == nil {
			res.PlayerNames = parseA2SPlayers(p)
		}
	}
	return res, nil
}

// a2sRequest sends req, answering a challenge if one comes back, and
// returns the payload (after the header byte) of a response of type want.
// Info requests take the challenge appended; player requests take it in
// place of their own.
func a2sRequest(conn net.Conn, req []byte, want byte, appendChallenge bool) ([]byte, error) {
	for range 3 {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		data, err := readA2S(conn)
		if err != nil {
			return nil, err
		}
		if len(data) < 1 {
			return nil, errors.New("empty A2S response")
		}
		switch data[0] {
		case want:
			return data[1:], nil
		case 0x41: // S2C_CHALLENGE
			if len(data) < 5 {
				return nil, errors.New("short A2S challenge")
			}
			if appendChallenge {
				req = append(append([]byte{}, a2sInfoRequest...), data[1:5]...)
			} else {
				req = append(append([]byte{}, req[:5]...), data[1:5]...)
			}
		default:
			return nil, fmt.Errorf("unexpected A2S response type 0x%02x", data[0])
		}
	}
	return nil, errors.New("the server kept answering with challenges")
}

// readA2S reads one response, putting split packets back together, and
// returns it without the FF FF FF FF header.
func readA2S(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 1400*2)
	parts := map[byte][]byte{}
	total := -1
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		p := buf[:n]
		if len(p) < 4 {
			continue
		}
		switch binary.LittleEndian.Uint32(p) {
		case 0xFFFFFFFF: // a single packet
			return append([]byte{}, p[4:]...), nil
		case 0xFFFFFFFE: // part of a split packet (Source format)
			if len(p) < 12 {
				return nil, errors.New("short A2S split packet")
			}
			id := binary.LittleEndian.Uint32(p[4:])
			if id&0x80000000 != 0 {
				return nil, errors.New("compressed A2S responses aren't supported")
			}
			total = int(p[8])
			parts[p[9]] = append([]byte{}, p[12:]...)
			if total > 0 && len(parts) == total {
				var all []byte
				for i := range total {
					all = append(all, parts[byte(i)]...)
				}
				if len(all) < 4 || binary.LittleEndian.Uint32(all) != 0xFFFFFFFF {
					return nil, errors.New("bad A2S split response")
				}
				return all[4:], nil
			}
		}
	}
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) byte() byte {
	if r.err != nil || len(r.b) < 1 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) str() string {
	if r.err != nil {
		return ""
	}
	i := bytes.IndexByte(r.b, 0)
	if i < 0 {
		r.err = io.ErrUnexpectedEOF
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return strings.ToValidUTF8(s, "")
}

func (r *reader) skip(n int) {
	if r.err != nil || len(r.b) < n {
		r.err = io.ErrUnexpectedEOF
		return
	}
	r.b = r.b[n:]
}

func (r *reader) u16() uint16 {
	if r.err != nil || len(r.b) < 2 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b)
	r.b = r.b[2:]
	return v
}

func parseA2SInfo(p []byte) (Result, error) {
	r := &reader{b: p}
	r.byte() // protocol version
	res := Result{Name: r.str(), Map: r.str()}
	r.str() // folder
	res.Game = r.str()
	appID := r.u16()
	res.Players, res.MaxPlayers = int(r.byte()), int(r.byte())
	bots := int(r.byte())
	r.byte() // server type
	r.byte() // environment
	res.Password = r.byte() == 1
	r.byte()           // VAC
	if appID == 2400 { // The Ship
		r.skip(3)
	}
	res.Version = r.str()
	if r.err != nil && res.Name == "" {
		return Result{}, errors.New("bad A2S_INFO response")
	}
	// Bots count as players in A2S; people care about humans.
	if bots > 0 && bots <= res.Players {
		res.Players -= bots
	}
	return res, nil
}

func parseA2SPlayers(p []byte) []string {
	r := &reader{b: p}
	n := int(r.byte())
	var names []string
	for range n {
		r.byte() // index
		name := strings.TrimSpace(r.str())
		r.skip(8) // score, duration
		if r.err != nil {
			break
		}
		if name != "" && len(names) < maxPlayerNames {
			names = append(names, name)
		}
	}
	return names
}

// --- Minecraft Java ---

func putVarInt(b []byte, v int) []byte {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7F|0x80))
		u >>= 7
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var v uint32
	for i := range 5 {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(c&0x7F) << (7 * i)
		if c&0x80 == 0 {
			return int(int32(v)), nil
		}
	}
	return 0, errors.New("VarInt too long")
}

// Minecraft asks a Minecraft Java server for its status with a server list
// ping over TCP.
func Minecraft(ctx context.Context, addr string) (Result, error) {
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx, 5*time.Second))
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)

	var hs []byte
	hs = putVarInt(hs, 0x00)
	hs = putVarInt(hs, -1) // protocol version: any
	hs = putVarInt(hs, len(host))
	hs = append(hs, host...)
	hs = binary.BigEndian.AppendUint16(hs, uint16(port))
	hs = putVarInt(hs, 1) // next state: status
	var msg []byte
	msg = putVarInt(msg, len(hs))
	msg = append(msg, hs...)
	msg = append(msg, 0x01, 0x00) // status request
	if _, err := conn.Write(msg); err != nil {
		return Result{}, err
	}
	br := bufio.NewReader(conn)
	n, err := readVarInt(br)
	if err != nil {
		return Result{}, err
	}
	if n <= 0 || n > 1<<20 {
		return Result{}, errors.New("bad Minecraft status response")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return Result{}, err
	}
	bb := bytes.NewReader(body)
	if id, err := readVarInt(bb); err != nil || id != 0 {
		return Result{}, errors.New("bad Minecraft status response")
	}
	l, err := readVarInt(bb)
	if err != nil || l < 0 || l > bb.Len() {
		return Result{}, errors.New("bad Minecraft status response")
	}
	js := make([]byte, l)
	_, _ = io.ReadFull(bb, js)
	res, err := parseMinecraftStatus(js)
	if err != nil {
		return Result{}, err
	}
	res.Protocol, res.Address, res.Latency, res.HasPlayers = ProtoMinecraft, addr, time.Since(start), true
	return res, nil
}

func parseMinecraftStatus(js []byte) (Result, error) {
	var st struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Max    int `json:"max"`
			Online int `json:"online"`
			Sample []struct {
				Name string `json:"name"`
			} `json:"sample"`
		} `json:"players"`
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal(js, &st); err != nil {
		return Result{}, fmt.Errorf("bad Minecraft status: %w", err)
	}
	res := Result{Version: st.Version.Name, Players: st.Players.Online, MaxPlayers: st.Players.Max,
		Name: chatText(st.Description), Game: "Minecraft"}
	for _, p := range st.Players.Sample {
		// Servers hide the list behind a fake entry with a zero ID or a
		// message; real names have no spaces or formatting.
		if p.Name != "" && !strings.ContainsAny(p.Name, " §") && len(res.PlayerNames) < maxPlayerNames {
			res.PlayerNames = append(res.PlayerNames, p.Name)
		}
	}
	return res, nil
}

// chatText flattens a Minecraft chat component (a string, or an object
// with text and extra) to plain text.
func chatText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return stripFormatting(s)
	}
	var c struct {
		Text  string            `json:"text"`
		Extra []json.RawMessage `json:"extra"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	out := c.Text
	for _, e := range c.Extra {
		out += chatText(e)
	}
	return stripFormatting(out)
}

func stripFormatting(s string) string {
	var b strings.Builder
	skip := false
	for _, r := range s {
		switch {
		case skip:
			skip = false
		case r == '§':
			skip = true
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

// --- Minecraft Bedrock ---

var raknetMagic = []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}

// Bedrock asks a Minecraft Bedrock server for its status with a RakNet
// unconnected ping over UDP.
func Bedrock(ctx context.Context, addr string) (Result, error) {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", addr)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx, 3*time.Second))
	ping := []byte{0x01}
	ping = binary.BigEndian.AppendUint64(ping, uint64(time.Now().UnixMilli()))
	ping = append(ping, raknetMagic...)
	guid := make([]byte, 8)
	_, _ = rand.Read(guid)
	ping = append(ping, guid...)
	if _, err := conn.Write(ping); err != nil {
		return Result{}, err
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return Result{}, err
	}
	res, err := parseBedrockPong(buf[:n])
	if err != nil {
		return Result{}, err
	}
	res.Protocol, res.Address, res.Latency, res.HasPlayers = ProtoBedrock, addr, time.Since(start), true
	return res, nil
}

func parseBedrockPong(p []byte) (Result, error) {
	// 0x1c, time (8), server GUID (8), magic (16), length (2), string.
	if len(p) < 35 || p[0] != 0x1c || !bytes.Equal(p[17:33], raknetMagic) {
		return Result{}, errors.New("bad Bedrock pong")
	}
	l := int(binary.BigEndian.Uint16(p[33:35]))
	if 35+l > len(p) {
		return Result{}, errors.New("bad Bedrock pong")
	}
	// MCPE;motd;protocol;version;players;max;server id;sub motd;game mode;…
	f := strings.Split(string(p[35:35+l]), ";")
	if len(f) < 6 {
		return Result{}, errors.New("bad Bedrock status")
	}
	res := Result{Name: stripFormatting(f[1]), Version: f[3], Game: "Minecraft Bedrock"}
	res.Players, _ = strconv.Atoi(f[4])
	res.MaxPlayers, _ = strconv.Atoi(f[5])
	if len(f) > 7 {
		res.Map = stripFormatting(f[7])
	}
	return res, nil
}

// --- TCP ---

// TCP only checks that a TCP port accepts connections.
func TCP(ctx context.Context, addr string) (Result, error) {
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{}, err
	}
	conn.Close()
	return Result{Protocol: ProtoTCP, Address: addr, Latency: time.Since(start)}, nil
}

// --- Player lists from console commands ---

// rconCommands are the console commands that list players, by game.
var rconCommands = map[string]string{
	"palworld":  "ShowPlayers",
	"minecraft": "list",
	"ark":       "ListPlayers",
}

// addPalworldMetrics reads the console's answer to Metrics (from
// Palworld's REST API): "Players: 3/32" and so on.
func addPalworldMetrics(res *Result, out string) {
	for _, l := range strings.Split(out, "\n") {
		var n, max int
		if _, err := fmt.Sscanf(strings.TrimSpace(l), "Players: %d/%d", &n, &max); err == nil {
			res.Players, res.MaxPlayers = n, max
		}
	}
}

// addPalworldInfo reads the console's answer to Info (from Palworld's REST
// API): "Server: name" and "Version: v0.1.5.0".
func addPalworldInfo(res *Result, out string) {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "Server: "); ok {
			res.Name = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "Version: "); ok {
			res.Version = strings.TrimSpace(v)
		}
	}
	res.Game = "Palworld"
}

// parsePlayerList reads a game's answer to its rconCommands command.
func parsePlayerList(game, out string) (Result, error) {
	res := Result{HasPlayers: true}
	out = strings.ReplaceAll(out, "\r\n", "\n")
	switch game {
	case "palworld":
		// name,playeruid,steamid — a header line, then one line per player.
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) == 0 || !strings.HasPrefix(strings.ToLower(lines[0]), "name,") {
			return res, errors.New("unexpected ShowPlayers output")
		}
		for _, l := range lines[1:] {
			name, _, _ := strings.Cut(strings.TrimSpace(l), ",")
			if l = strings.TrimSpace(l); l != "" {
				res.Players++
				if name != "" && len(res.PlayerNames) < maxPlayerNames {
					res.PlayerNames = append(res.PlayerNames, name)
				}
			}
		}
	case "minecraft":
		// There are 2 of a max of 20 players online: alex, sam
		var online, max int
		head, names, _ := strings.Cut(out, ":")
		if _, err := fmt.Sscanf(strings.TrimSpace(head), "There are %d of a max of %d players online", &online, &max); err != nil {
			return res, errors.New("unexpected list output")
		}
		res.Players, res.MaxPlayers = online, max
		for _, n := range strings.Split(names, ",") {
			if n = strings.TrimSpace(n); n != "" && len(res.PlayerNames) < maxPlayerNames {
				res.PlayerNames = append(res.PlayerNames, n)
			}
		}
	case "ark":
		// "0. Name, 0002…" per line, or "No Players Connected".
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.EqualFold(l, "No Players Connected") {
				continue
			}
			if _, rest, ok := strings.Cut(l, ". "); ok {
				name, _, _ := strings.Cut(rest, ",")
				res.Players++
				if len(res.PlayerNames) < maxPlayerNames {
					res.PlayerNames = append(res.PlayerNames, strings.TrimSpace(name))
				}
			}
		}
	default:
		return res, fmt.Errorf("no player list command for %q", game)
	}
	return res, nil
}
