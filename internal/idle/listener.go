package idle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ListenerCommand is the hlm subcommand that runs in a wake listener
// container: it listens on a sleeping server's ports and exits when someone
// really tries to play, printing a Contact.
const ListenerCommand = "wake-listener"

// Contact is what woke a server, printed by the listener as it exits.
type Contact struct {
	Port  string `json:"port"` // "2456/udp"
	From  string `json:"from"` // the address it came from
	About string `json:"about,omitempty"`
}

// listenerConfig is the listener's command line.
type listenerConfig struct {
	tcp, udp  []int
	minecraft int // a TCP port speaking Minecraft (Java)
	bedrock   int // a UDP port speaking Minecraft Bedrock
	motd      string
	max       int
}

func parseListenerArgs(args []string) (listenerConfig, error) {
	var c listenerConfig
	fs := flag.NewFlagSet(ListenerCommand, flag.ContinueOnError)
	tcp := fs.String("tcp", "", "TCP ports, comma-separated")
	udp := fs.String("udp", "", "UDP ports, comma-separated")
	fs.IntVar(&c.minecraft, "minecraft", 0, "the TCP port that speaks Minecraft (Java)")
	fs.IntVar(&c.bedrock, "bedrock", 0, "the UDP port that speaks Minecraft Bedrock")
	fs.StringVar(&c.motd, "motd", "Asleep: join to wake it", "what Minecraft's server list shows")
	fs.IntVar(&c.max, "max", 0, "the player limit Minecraft's server list shows")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	var err error
	if c.tcp, err = ports(*tcp); err != nil {
		return c, err
	}
	if c.udp, err = ports(*udp); err != nil {
		return c, err
	}
	if len(c.tcp)+len(c.udp) == 0 {
		return c, errors.New("no ports to listen on")
	}
	return c, nil
}

func ports(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("port %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

// RunListener listens until something wakes the server, then prints what
// did to out and returns nil. It never answers anything but Minecraft's
// server list pings.
func RunListener(ctx context.Context, args []string, out io.Writer) error {
	c, err := parseListenerArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woke := make(chan Contact, 1)
	wake := func(ct Contact) {
		select {
		case woke <- ct:
		default:
		}
	}
	for _, p := range c.tcp {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err != nil {
			return err
		}
		defer ln.Close()
		go serveTCP(ctx, ln, p, c, wake)
	}
	for _, p := range c.udp {
		pc, err := net.ListenPacket("udp", ":"+strconv.Itoa(p))
		if err != nil {
			return err
		}
		defer pc.Close()
		go serveUDP(ctx, pc, p, c, wake)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ct := <-woke:
		return json.NewEncoder(out).Encode(ct)
	}
}

func serveTCP(ctx context.Context, ln net.Listener, port int, c listenerConfig, wake func(Contact)) {
	for ctx.Err() == nil {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			ct := Contact{Port: strconv.Itoa(port) + "/tcp", From: conn.RemoteAddr().String()}
			if port == c.minecraft {
				if about, ok := minecraftJava(conn, c); ok {
					ct.About = about
					wake(ct)
				}
				return
			}
			// A port scan connects and leaves; a client says something.
			var b [1]byte
			if n, _ := conn.Read(b[:]); n > 0 {
				wake(ct)
			}
		}()
	}
}

// a2sPrefix starts Steam's server queries, which server browsers send
// constantly; they don't wake a server.
var a2sPrefix = []byte{0xff, 0xff, 0xff, 0xff}

func serveUDP(ctx context.Context, pc net.PacketConn, port int, c listenerConfig, wake func(Contact)) {
	buf := make([]byte, 2048)
	for ctx.Err() == nil {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		pkt := buf[:n]
		ct := Contact{Port: strconv.Itoa(port) + "/udp", From: from.String()}
		switch {
		case n == 0 || bytes.HasPrefix(pkt, a2sPrefix):
		case port == c.bedrock:
			if reply, joining := bedrock(pkt, port, c); reply != nil {
				pc.WriteTo(reply, from)
			} else if joining {
				ct.About = "a Minecraft Bedrock player joining"
				wake(ct)
			}
		default:
			wake(ct)
		}
	}
}

// --- Minecraft (Java): answer the server list, wake on a login ---

func readVarInt(r io.ByteReader) (int, error) {
	var n uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		n |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int(int32(n)), nil
		}
	}
	return 0, errors.New("varint too long")
}

func appendVarInt(b []byte, v int) []byte {
	u := uint32(v)
	for u >= 0x80 {
		b = append(b, byte(u)|0x80)
		u >>= 7
	}
	return append(b, byte(u))
}

func appendString(b []byte, s string) []byte {
	return append(appendVarInt(b, len(s)), s...)
}

// packet frames a packet: its length, its id, its data.
func packet(id int, data []byte) []byte {
	body := append(appendVarInt(nil, id), data...)
	return append(appendVarInt(nil, len(body)), body...)
}

// readPacket reads one packet (at most 32 KiB) and returns its id and data.
func readPacket(r *bufio.Reader) (int, *bytes.Reader, error) {
	n, err := readVarInt(r)
	if err != nil {
		return 0, nil, err
	}
	if n <= 0 || n > 32<<10 {
		return 0, nil, errors.New("bad packet length")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	br := bytes.NewReader(body)
	id, err := readVarInt(br)
	return id, br, err
}

// minecraftJava answers a server list ping (status) and reports whether the
// connection was a player logging in.
func minecraftJava(conn net.Conn, c listenerConfig) (string, bool) {
	r := bufio.NewReader(conn)
	id, hs, err := readPacket(r)
	if err != nil || id != 0 {
		return "", false
	}
	protocol, err := readVarInt(hs)
	if err != nil {
		return "", false
	}
	addrLen, err := readVarInt(hs)
	if err != nil || addrLen < 0 || addrLen > 255 {
		return "", false
	}
	if _, err := hs.Seek(int64(addrLen)+2, io.SeekCurrent); err != nil { // address and port
		return "", false
	}
	next, err := readVarInt(hs)
	if err != nil {
		return "", false
	}
	switch next {
	case 1: // status: the server list
		status, _ := json.Marshal(map[string]any{
			"version":     map[string]any{"name": "Asleep", "protocol": protocol},
			"players":     map[string]any{"max": c.max, "online": 0},
			"description": map[string]any{"text": c.motd},
		})
		for {
			id, body, err := readPacket(r)
			if err != nil {
				return "", false
			}
			switch id {
			case 0:
				conn.Write(packet(0, appendString(nil, string(status))))
			case 1: // ping: echo it
				rest, _ := io.ReadAll(body)
				conn.Write(packet(1, rest))
				return "", false
			default:
				return "", false
			}
		}
	case 2, 3: // login (or a transfer): tell them, and wake the server
		msg, _ := json.Marshal(map[string]any{"text": "The server is starting: join again in a minute or two."})
		conn.Write(packet(0, appendString(nil, string(msg))))
		return "a Minecraft player joining", true
	}
	return "", false
}

// --- Minecraft Bedrock: answer pings, wake on a connection request ---

var raknetMagic = []byte{0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe, 0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78}

// bedrock answers an unconnected ping with a pong (reply), or reports an
// open connection request (joining).
func bedrock(pkt []byte, port int, c listenerConfig) (reply []byte, joining bool) {
	switch pkt[0] {
	case 0x01, 0x02: // unconnected ping: id, time (8), magic, client GUID
		if len(pkt) < 1+8+16 || !bytes.Equal(pkt[9:25], raknetMagic) {
			return nil, false
		}
		motd := strings.NewReplacer(";", ",").Replace(c.motd)
		id := fmt.Sprintf("MCPE;%s;1;asleep;0;%d;1;hlm;Survival;1;%d;%d;", motd, c.max, port, port)
		out := []byte{0x1c}
		out = append(out, pkt[1:9]...)                       // the ping's time
		out = binary.BigEndian.AppendUint64(out, 0x686c6d00) // server GUID
		out = append(out, raknetMagic...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(id)))
		return append(out, id...), false
	case 0x05: // open connection request 1
		return nil, len(pkt) >= 17 && bytes.Equal(pkt[1:17], raknetMagic)
	}
	return nil, false
}
