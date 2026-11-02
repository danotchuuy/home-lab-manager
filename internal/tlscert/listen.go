package tlscert

import (
	"bufio"
	"crypto/tls"
	"net"
	"sync"
	"time"
)

// Listener serves HTTPS and plain HTTP on one port: a connection that starts
// with a TLS handshake record gets TLS, others are left as they are. Looking
// happens off the accept loop, so a slow client can't hold up others.
func (s *Service) Listener(ln net.Listener) net.Listener {
	l := &sniffing{Listener: ln, cfg: s.TLSConfig(), conns: make(chan net.Conn), done: make(chan struct{}), errc: make(chan error, 1)}
	go l.accept()
	return l
}

type sniffing struct {
	net.Listener
	cfg   *tls.Config
	conns chan net.Conn
	errc  chan error
	done  chan struct{}
	once  sync.Once
}

func (l *sniffing) accept() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.errc <- err:
			case <-l.done:
			}
			return
		}
		go l.classify(c)
	}
}

// tlsHandshake is the first byte of a TLS handshake record.
const tlsHandshake = 0x16

func (l *sniffing) classify(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var out net.Conn = &peeked{Conn: c, r: br}
	if first[0] == tlsHandshake {
		out = tls.Server(out, l.cfg)
	}
	select {
	case l.conns <- out:
	case <-l.done:
		c.Close()
	}
}

func (l *sniffing) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *sniffing) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// peeked is a connection whose first bytes were read to look at them.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }
