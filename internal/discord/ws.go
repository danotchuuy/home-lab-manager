package discord

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The gateway is a WebSocket of JSON text messages. Only what it needs is
// implemented (RFC 6455): text messages, fragmentation, ping, pong and
// close, as a client, and as a server for the tests' fake gateway.

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xa

	// maxMessage bounds a message, so a peer can't make hlm allocate much.
	maxMessage = 4 << 20
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// CloseError is the close frame the peer sent.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("closed (%d: %s)", e.Code, e.Reason)
	}
	return fmt.Sprintf("closed (%d)", e.Code)
}

// Conn is a WebSocket carrying text messages.
type Conn struct {
	conn   net.Conn
	r      *bufio.Reader
	client bool // clients mask what they send

	wmu    sync.Mutex
	closed bool
}

// Dial connects to a ws:// or wss:// URL.
func Dial(ctx context.Context, rawURL string, tlsConfig *tls.Config) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var d net.Dialer
	var nc net.Conn
	switch u.Scheme {
	case "wss":
		cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
		if tlsConfig != nil {
			cfg = tlsConfig.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName = u.Hostname()
			}
		}
		td := tls.Dialer{NetDialer: &d, Config: cfg}
		nc, err = td.DialContext(ctx, "tcp", host)
	case "ws":
		nc, err = d.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("%s: not a WebSocket URL", rawURL)
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nUser-Agent: " + userAgent + "\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		nc.Close()
		return nil, err
	}
	r := bufio.NewReader(nc)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil {
		nc.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		nc.Close()
		return nil, fmt.Errorf("the gateway refused the WebSocket (HTTP %d)", resp.StatusCode)
	}
	nc.SetDeadline(time.Time{})
	return &Conn{conn: nc, r: r, client: true}, nil
}

// Accept takes a request over as the server end of a WebSocket (for the
// fake gateway in tests).
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "not a WebSocket", http.StatusBadRequest)
		return nil, errors.New("not a WebSocket")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("can't take over the connection")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := io.WriteString(nc, resp); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{conn: nc, r: rw.Reader}, nil
}

// SetReadDeadline bounds the next reads.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage reads the next text message, answering pings on the way. A
// close from the peer is a *CloseError.
func (c *Conn) ReadMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			ce := &CloseError{Code: 1005}
			if len(payload) >= 2 {
				ce.Code = int(binary.BigEndian.Uint16(payload))
				ce.Reason = string(payload[2:])
			}
			c.writeFrame(opClose, payload[:min(len(payload), 2)])
			c.conn.Close()
			return nil, ce
		case opText, opBinary:
			if started {
				return nil, errors.New("websocket: a new message inside another")
			}
			started = true
		case opCont:
			if !started {
				return nil, errors.New("websocket: continuation without a message")
			}
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
		if len(msg)+len(payload) > maxMessage {
			return nil, errors.New("websocket: message too large")
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > maxMessage {
		err = errors.New("websocket: frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteText sends a text message.
func (c *Conn) WriteText(p []byte) error { return c.writeFrame(opText, p) }

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	hdr := []byte{0x80 | op, 0}
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	data := payload
	if c.client {
		hdr[1] |= 0x80
		var mask [4]byte
		rand.Read(mask[:])
		hdr = append(hdr, mask[:]...)
		data = make([]byte, n)
		for i := range payload {
			data[i] = payload[i] ^ mask[i%4]
		}
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(append(hdr, data...))
	return err
}

// Close sends a close frame with code and closes the connection.
func (c *Conn) Close(code int) error {
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(code))
	c.writeFrame(opClose, p[:])
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
	return c.conn.Close()
}
