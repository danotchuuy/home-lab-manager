package rcon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// style is how a fake server behaves, after real games.
type style int

const (
	source    style = iota // empty response before the auth response; answers the end marker twice
	minecraft              // no empty response; answers the end marker with "Unknown request"
	silent                 // ignores the end marker (as Palworld does)
)

type fakeServer struct {
	ln       net.Listener
	style    style
	password string
	// replies maps commands to replies; long ones are split into packets.
	replies map[string]string
}

func newFake(t *testing.T, st style) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, style: st, password: "hunter2", replies: map[string]string{
		"list":  "There are 2 of a max of 20 players online: alex, sam",
		"long":  strings.Repeat("x", 9000),
		"exact": strings.Repeat("y", 4096),
		"empty": "",
	}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

// readOne reads a packet as Minecraft does: with one read of up to 1460
// bytes, which must hold exactly one packet, or the client is dropped.
func readOne(conn net.Conn) (packet, error) {
	buf := make([]byte, 1460)
	n, err := conn.Read(buf)
	if err != nil {
		return packet{}, err
	}
	if n < 4 || int(int32(binary.LittleEndian.Uint32(buf))) != n-4 {
		return packet{}, errors.New("not exactly one packet in the read")
	}
	return readPacket(bytes.NewReader(buf[:n]))
}

func (f *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	authed := false
	read := readPacket
	if f.style == minecraft {
		read = func(r io.Reader) (packet, error) { return readOne(conn) }
	}
	for {
		p, err := read(conn)
		if err != nil {
			return
		}
		switch {
		case p.typ == typeAuth:
			if f.style == source {
				writePacket(conn, p.id, typeResponse, "")
			}
			if p.body != f.password {
				writePacket(conn, -1, typeAuthResponse, "")
				if f.style == minecraft {
					return
				}
				continue
			}
			authed = true
			writePacket(conn, p.id, typeAuthResponse, "")
		case !authed:
			return
		case p.typ == typeExec:
			reply, ok := f.replies[p.body]
			if !ok {
				reply = "Unknown command: " + p.body
			}
			for len(reply) > 4096 {
				writePacket(conn, p.id, typeResponse, reply[:4096])
				reply = reply[4096:]
			}
			writePacket(conn, p.id, typeResponse, reply)
		case p.typ == typeResponse:
			switch f.style {
			case source:
				writePacket(conn, p.id, typeResponse, "")
				writePacket(conn, p.id, typeResponse, "\x00\x01\x00\x00")
			case minecraft:
				writePacket(conn, p.id, typeResponse, "Unknown request 0")
			}
		}
	}
}

func TestExec(t *testing.T) {
	for _, st := range []style{source, minecraft, silent} {
		f := newFake(t, st)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dial := func() *Client {
			c, err := Dial(ctx, f.ln.Addr().String(), "hunter2")
			if err != nil {
				t.Fatalf("style %d: %v", st, err)
			}
			c.settle = 100 * time.Millisecond
			return c
		}
		c := dial()
		// Short replies end at once, and the connection stays usable.
		for _, cmd := range []string{"list", "empty", "nosuch", "list"} {
			want, ok := f.replies[cmd]
			if !ok {
				want = "Unknown command: " + cmd
			}
			out, err := c.Exec(ctx, cmd)
			if err != nil || out != want || c.Broken() {
				t.Errorf("style %d, %s: %q, %v, broken %v", st, cmd, out, err, c.Broken())
			}
		}
		// Long replies come in several packets, all of which are kept.
		for _, cmd := range []string{"long", "exact"} {
			if c.Broken() {
				c.Close()
				c = dial()
			}
			out, err := c.Exec(ctx, cmd)
			if err != nil || out != f.replies[cmd] {
				t.Errorf("style %d, %s: %d bytes, %v", st, cmd, len(out), err)
			}
			// Servers that answer the end marker leave the connection
			// usable; one that doesn't is waited for, and then replaced.
			if broken := c.Broken(); broken != (st == silent) {
				t.Errorf("style %d, %s: broken = %v", st, cmd, broken)
			}
		}
		c.Close()
		cancel()
	}
}

func TestAuthFailure(t *testing.T) {
	for _, st := range []style{source, minecraft} {
		f := newFake(t, st)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := Dial(ctx, f.ln.Addr().String(), "wrong")
		cancel()
		if !errors.Is(err, ErrAuth) {
			t.Errorf("style %d: err = %v, want ErrAuth", st, err)
		}
	}
}

func TestNotRCON(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		conn.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, ln.Addr().String(), "x"); err == nil || !strings.Contains(err.Error(), "RCON port") {
		t.Errorf("talking to something else: %v", err)
	}
}

func TestLimits(t *testing.T) {
	f := newFake(t, source)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.ln.Addr().String(), "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Exec(ctx, strings.Repeat("a", MaxCommand+1)); err == nil {
		t.Error("an over-long command was sent")
	}
	if _, err := c.Exec(ctx, "a\x00b"); err == nil {
		t.Error("a command with a NUL was sent")
	}
}
