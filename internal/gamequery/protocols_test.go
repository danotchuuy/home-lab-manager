package gamequery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeA2S is a Steam game server that wants a challenge and splits its
// player list across two packets.
func fakeA2S(t *testing.T, split bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	challenge := []byte{1, 2, 3, 4}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			hdr := []byte{0xFF, 0xFF, 0xFF, 0xFF}
			switch {
			case len(req) >= 5 && req[4] == 0x54:
				if !bytes.HasSuffix(req, challenge) {
					pc.WriteTo(append(append(hdr, 0x41), challenge...), from)
					continue
				}
				info := append(hdr, 0x49, 17)
				info = append(info, "My Valheim\x00Meadows\x00valheim\x00Valheim\x00"...)
				info = append(info, 0x00, 0x00) // app id
				info = append(info, 3, 10, 1)   // players (incl. 1 bot), max, bots
				info = append(info, 'd', 'l', 1, 0)
				info = append(info, "0.219.16\x00"...)
				pc.WriteTo(info, from)
			case len(req) >= 5 && req[4] == 0x55:
				if !bytes.Equal(req[5:9], challenge) {
					pc.WriteTo(append(append(hdr, 0x41), challenge...), from)
					continue
				}
				pl := append(hdr, 0x44, 3)
				for i, name := range []string{"alex", "", "sam"} {
					pl = append(pl, byte(i))
					pl = append(pl, name+"\x00"...)
					pl = append(pl, make([]byte, 8)...)
				}
				if !split {
					pc.WriteTo(pl, from)
					continue
				}
				half := len(pl) / 2
				for i, part := range [][]byte{pl[:half], pl[half:]} {
					p := []byte{0xFE, 0xFF, 0xFF, 0xFF, 7, 0, 0, 0, 2, byte(i), 0xE0, 0x04} // -2, little-endian
					pc.WriteTo(append(p, part...), from)
				}
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestA2S(t *testing.T) {
	for _, split := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := A2S(ctx, fakeA2S(t, split))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if res.Name != "My Valheim" || res.Map != "Meadows" || res.Game != "Valheim" || res.Version != "0.219.16" ||
			res.Players != 2 || res.MaxPlayers != 10 || !res.Password || !res.HasPlayers || res.Protocol != ProtoA2S {
			t.Errorf("split=%v: %+v", split, res)
		}
		if !slices.Equal(res.PlayerNames, []string{"alex", "sam"}) {
			t.Errorf("split=%v: names %q", split, res.PlayerNames)
		}
	}
	// Nothing listening: an error, quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	addr := pc.LocalAddr().String()
	pc.Close()
	if _, err := A2S(ctx, addr); err == nil {
		t.Error("no error from a closed port")
	}
}

func fakeMinecraft(t *testing.T, status string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		conn.Read(buf) // handshake and status request
		var body []byte
		body = putVarInt(body, 0)
		body = putVarInt(body, len(status))
		body = append(body, status...)
		var msg []byte
		msg = putVarInt(msg, len(body))
		conn.Write(append(msg, body...))
	}()
	return ln.Addr().String()
}

func TestMinecraft(t *testing.T) {
	status := `{"version":{"name":"1.21.4","protocol":769},"players":{"max":20,"online":2,"sample":[{"name":"alex","id":"1"},{"name":"§7and more","id":"0"}]},"description":{"text":"§aA ","extra":[{"text":"Minecraft Server"}]}}`
	res, err := Minecraft(context.Background(), fakeMinecraft(t, status))
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "A Minecraft Server" || res.Version != "1.21.4" || res.Players != 2 || res.MaxPlayers != 20 ||
		!slices.Equal(res.PlayerNames, []string{"alex"}) {
		t.Errorf("%+v", res)
	}
	res, err = Minecraft(context.Background(), fakeMinecraft(t, `{"version":{"name":"1.8"},"players":{"max":5,"online":0},"description":"Plain §lMOTD"}`))
	if err != nil || res.Name != "Plain MOTD" {
		t.Errorf("plain description: %+v, %v", res, err)
	}
}

func TestBedrock(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 100)
		n, from, err := pc.ReadFrom(buf)
		if err != nil || n < 33 || buf[0] != 0x01 {
			return
		}
		s := "MCPE;Dedicated Server;748;1.21.44;1;10;123;Bedrock level;Survival;1;19132;19133;"
		p := append([]byte{0x1c}, buf[1:9]...)
		p = append(p, make([]byte, 8)...)
		p = append(p, raknetMagic...)
		p = binary.BigEndian.AppendUint16(p, uint16(len(s)))
		pc.WriteTo(append(p, s...), from)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := Bedrock(ctx, pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "Dedicated Server" || res.Version != "1.21.44" || res.Players != 1 || res.MaxPlayers != 10 || res.Map != "Bedrock level" {
		t.Errorf("%+v", res)
	}
}

func TestTCP(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	res, err := TCP(context.Background(), ln.Addr().String())
	if err != nil || res.HasPlayers || res.Protocol != ProtoTCP {
		t.Errorf("%+v, %v", res, err)
	}
}

func TestParsePlayerList(t *testing.T) {
	tests := []struct {
		game, out string
		players   int
		names     []string
	}{
		{"palworld", "name,playeruid,steamid\r\nAlice,123,7656\r\nBob,456,7657\r\n", 2, []string{"Alice", "Bob"}},
		{"palworld", "name,playeruid,steamid\n", 0, nil},
		{"minecraft", "There are 2 of a max of 20 players online: alex, sam", 2, []string{"alex", "sam"}},
		{"minecraft", "There are 0 of a max of 20 players online:", 0, nil},
		{"ark", "0. Alice, 0002abc\n1. Bob, 0002def", 2, []string{"Alice", "Bob"}},
		{"ark", "No Players Connected", 0, nil},
	}
	for _, tt := range tests {
		res, err := parsePlayerList(tt.game, tt.out)
		if err != nil || res.Players != tt.players || !slices.Equal(res.PlayerNames, tt.names) {
			t.Errorf("%s %q: %+v, %v", tt.game, tt.out, res, err)
		}
	}
	if _, err := parsePlayerList("palworld", "Unknown command"); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := parsePlayerList("minecraft", strings.Repeat("x", 10)); err == nil {
		t.Error("garbage accepted")
	}
}

func TestShortError(t *testing.T) {
	tests := map[string]string{
		"read udp 10.0.14.42:59528->192.168.11.16:15636: read: connection refused": "nothing is listening on that port",
		"read udp 1.2.3.4:5->6.7.8.9:10: i/o timeout":                              "no answer",
		"dial tcp 192.168.11.14:25575: connect: no route to host":                  "no route to host (is the server on a macvlan network hlm isn't connected to?)",
		"bad A2S_INFO response":                                                    "bad A2S_INFO response",
	}
	for in, want := range tests {
		if got := shortError(errors.New(in)); got != want {
			t.Errorf("shortError(%q) = %q", in, got)
		}
	}
}
