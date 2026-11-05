package agent

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The agent's connection to hlm is a WebSocket carrying binary frames, so
// it passes through the reverse proxies and tunnels (Cloudflare's, nginx,
// Caddy…) that hlm is often behind: they pass WebSockets on, but not other
// upgrades. Only what's needed is implemented: binary messages, ping and
// close (RFC 6455).

// Protocol is the WebSocket subprotocol the agent speaks.
const Protocol = "hlm-agent.v1"

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxFrame bounds a frame's length, so a peer can't make hlm allocate much.
const maxFrame = 1 << 20

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// newKey is a Sec-WebSocket-Key.
func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// isUpgrade says whether a request asks for the agent's WebSocket.
func isUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHas(r.Header, "Connection", "upgrade") &&
		headerHas(r.Header, "Upgrade", "websocket") && r.Header.Get("Sec-WebSocket-Version") == "13" &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

// accept takes over a request's connection as the server end of a
// WebSocket. The caller has checked isUpgrade.
func accept(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	// The server's read and write timeouts don't apply any more.
	_ = c.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n" +
		"Sec-WebSocket-Protocol: " + Protocol + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		c.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		c.Close()
		return nil, err
	}
	return newWSConn(rw.Reader, c, c, false), nil
}

// wsConn is a stream of bytes over a WebSocket's binary messages.
type wsConn struct {
	r      *bufio.Reader
	w      io.Writer
	c      io.Closer
	client bool // clients mask what they send

	readMu sync.Mutex
	left   int64 // bytes left in the current frame
	mask   [4]byte
	masked bool
	pos    int

	writeMu   sync.Mutex
	closeOnce sync.Once
}

func newWSConn(r *bufio.Reader, w io.Writer, c io.Closer, client bool) *wsConn {
	return &wsConn{r: r, w: w, c: c, client: client}
}

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

func (c *wsConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.left == 0 {
		op, n, err := c.readHeader()
		if err != nil {
			return 0, err
		}
		switch op {
		case opBinary, opContinuation, opText:
			c.left = n
		case opClose:
			_, _ = io.CopyN(io.Discard, c.r, n)
			c.writeFrame(opClose, nil)
			return 0, io.EOF
		case opPing:
			payload := make([]byte, n)
			if _, err := io.ReadFull(c.r, payload); err != nil {
				return 0, err
			}
			c.unmask(payload)
			if err := c.writeFrame(opPong, payload); err != nil {
				return 0, err
			}
		case opPong:
			if _, err := io.CopyN(io.Discard, c.r, n); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("websocket: unexpected opcode %d", op)
		}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.unmask(p[:n])
	c.left -= int64(n)
	return n, err
}

func (c *wsConn) readHeader() (op byte, n int64, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return 0, 0, err
	}
	op = h[0] & 0x0F
	if h[0]&0x70 != 0 {
		return 0, 0, errors.New("websocket: extensions aren't supported")
	}
	c.masked = h[1]&0x80 != 0
	if !c.client && !c.masked {
		return 0, 0, errors.New("websocket: the client's frames must be masked")
	}
	n = int64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return 0, 0, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return 0, 0, err
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > maxFrame {
		return 0, 0, errors.New("websocket: frame too large")
	}
	if op >= opClose && n > 125 {
		return 0, 0, errors.New("websocket: control frame too large")
	}
	if c.masked {
		if _, err = io.ReadFull(c.r, c.mask[:]); err != nil {
			return 0, 0, err
		}
	}
	c.pos = 0
	return op, n, nil
}

func (c *wsConn) unmask(p []byte) {
	if !c.masked {
		return
	}
	for i := range p {
		p[i] ^= c.mask[(c.pos+i)%4]
	}
	c.pos += len(p)
}

// Write sends p as binary frames.
func (c *wsConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFrame {
			chunk = chunk[:maxFrame]
		}
		if err := c.writeFrame(opBinary, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	buf := make([]byte, 0, 14+len(payload))
	buf = append(buf, 0x80|op)
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		buf = append(buf, maskBit|byte(n))
	case n <= 0xFFFF:
		buf = append(buf, maskBit|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		buf = append(buf, maskBit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if c.client {
		var key [4]byte
		_, _ = rand.Read(key[:])
		buf = append(buf, key[:]...)
		start := len(buf)
		buf = append(buf, payload...)
		for i := range payload {
			buf[start+i] ^= key[i%4]
		}
	} else {
		buf = append(buf, payload...)
	}
	_, err := c.w.Write(buf)
	return err
}

// Close says goodbye and closes the connection.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeFrame(opClose, []byte{0x03, 0xE8}) // 1000: normal closure
		err = c.c.Close()
	})
	return err
}

var _ io.ReadWriteCloser = (*wsConn)(nil)
