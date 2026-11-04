// Package rcon is a client for the Source RCON protocol, which Minecraft,
// Rust, ARK, Palworld, Counter-Strike, Conan Exiles and many other game
// servers use for remote admin commands.
//
// See https://developer.valvesoftware.com/wiki/Source_RCON_Protocol.
package rcon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Packet types.
const (
	typeResponse     = 0 // SERVERDATA_RESPONSE_VALUE
	typeExec         = 2 // SERVERDATA_EXECCOMMAND
	typeAuthResponse = 2 // SERVERDATA_AUTH_RESPONSE
	typeAuth         = 3 // SERVERDATA_AUTH
)

// MaxCommand is the longest command sent; servers refuse longer packets.
const MaxCommand = 1400

// maxPacket is the largest packet accepted from a server.
const maxPacket = 64 << 10

// fullPacket is the size of reply packet after which more may follow; the
// protocol splits replies into packets of up to 4096 bytes.
const fullPacket = 4000

// maxReply is how much of a reply is kept.
const maxReply = 256 << 10

// ErrAuth means the server refused the password.
var ErrAuth = errors.New("the RCON password was refused")

// Client is a connection to an RCON server. It is safe for concurrent use;
// commands are sent one at a time.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	nextID int32
	// settle is how long to wait for more of a reply once some has come,
	// for servers that don't answer the end-of-reply marker.
	settle time.Duration
	// broken is set when the connection can't be used for another command.
	broken bool
}

// Broken reports whether the connection can't be used for more commands,
// and a new one is needed.
func (c *Client) Broken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broken
}

// Dial connects to addr and signs in with password.
func Dial(ctx context.Context, addr, password string) (*Client, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, nextID: 1, settle: 400 * time.Millisecond}
	if err := c.auth(ctx, password); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) deadline(ctx context.Context) {
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Now().Add(10 * time.Second)
	}
	_ = c.conn.SetDeadline(d)
}

func (c *Client) id() int32 {
	id := c.nextID
	c.nextID++
	if c.nextID > 1<<30 {
		c.nextID = 1
	}
	return id
}

func (c *Client) auth(ctx context.Context, password string) error {
	c.deadline(ctx)
	id := c.id()
	if err := writePacket(c.conn, id, typeAuth, password); err != nil {
		return err
	}
	// Some servers send an empty response value first; the auth response
	// carries the request's ID, or -1 if the password was wrong.
	for range 3 {
		p, err := readPacket(c.conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w (the server closed the connection)", ErrAuth)
			}
			return err
		}
		if p.id == -1 {
			return ErrAuth
		}
		if p.typ == typeAuthResponse && p.id == id {
			return nil
		}
	}
	return errors.New("the server didn't answer the RCON sign-in")
}

// Exec runs a command and returns the server's reply.
func (c *Client) Exec(ctx context.Context, command string) (string, error) {
	if len(command) > MaxCommand {
		return "", fmt.Errorf("the command is too long (at most %d bytes)", MaxCommand)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline(ctx)
	id := c.id()
	if err := writePacket(c.conn, id, typeExec, command); err != nil {
		c.broken = true
		return "", err
	}
	// A reply longer than a packet comes in several. Once a full-size one
	// arrives, an empty response-value packet is sent: servers answer it
	// after the rest of the reply, marking its end, and servers that don't
	// answer it are given a moment instead. It isn't sent with the command,
	// because Minecraft drops clients that send two packets at once.
	var out bytes.Buffer
	end := int32(0)
	for {
		if end != 0 {
			settle := time.Now().Add(c.settle)
			if d, ok := ctx.Deadline(); ok && d.Before(settle) {
				settle = d
			}
			_ = c.conn.SetReadDeadline(settle)
		}
		p, err := readPacket(c.conn)
		if err != nil {
			c.broken = true
			var ne net.Error
			if end != 0 && errors.As(err, &ne) && ne.Timeout() {
				// The whole reply has come, but the connection may be
				// partway through another packet, so it isn't used again.
				break
			}
			return out.String(), err
		}
		if end != 0 && p.id == end {
			// Source servers answer the marker with a second packet too;
			// it's skipped, as an earlier ID, by the next command.
			break
		}
		if p.id != id {
			continue // a late answer to something earlier
		}
		if out.Len() < maxReply {
			out.WriteString(p.body)
		}
		if len(p.body) < fullPacket {
			if end == 0 {
				break // a short reply is all there is
			}
			continue
		}
		if end == 0 {
			end = c.id()
			if err := writePacket(c.conn, end, typeResponse, ""); err != nil {
				c.broken = true
				return out.String(), err
			}
		}
	}
	c.deadline(ctx)
	return out.String(), nil
}

type packet struct {
	id   int32
	typ  int32
	body string
}

func writePacket(w io.Writer, id, typ int32, body string) error {
	if strings.ContainsRune(body, 0) {
		return errors.New("the command can't contain NUL characters")
	}
	buf := make([]byte, 12, 14+len(body))
	binary.LittleEndian.PutUint32(buf[0:], uint32(10+len(body)))
	binary.LittleEndian.PutUint32(buf[4:], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:], uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}

func readPacket(r io.Reader) (packet, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return packet{}, err
	}
	size := int32(binary.LittleEndian.Uint32(hdr[:4]))
	if size < 10 || size > maxPacket {
		return packet{}, fmt.Errorf("bad RCON packet size %d; is this an RCON port?", size)
	}
	if _, err := io.ReadFull(r, hdr[4:12]); err != nil {
		return packet{}, err
	}
	body := make([]byte, size-8)
	if _, err := io.ReadFull(r, body); err != nil {
		return packet{}, err
	}
	// The body ends with a NUL, then an empty string's NUL.
	body = bytes.TrimRight(body, "\x00")
	return packet{
		id:   int32(binary.LittleEndian.Uint32(hdr[4:8])),
		typ:  int32(binary.LittleEndian.Uint32(hdr[8:12])),
		body: string(body),
	}, nil
}
