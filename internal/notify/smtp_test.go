package notify

import (
	"bufio"
	"context"
	"errors"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"
)

// smtpServer is a minimal SMTP server that accepts one message.
type smtpServer struct {
	ln   net.Listener
	from string
	to   []string
	data string
	done chan struct{}
}

func newSMTPServer(t *testing.T) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpServer{ln: ln, done: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *smtpServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	defer close(s.done)
	r := bufio.NewReader(conn)
	reply := func(line string) { conn.Write([]byte(line + "\r\n")) }
	reply("220 test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250 test")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.from = strings.Trim(cmd[len("MAIL FROM:"):], "<> ")
			reply("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.to = append(s.to, strings.Trim(cmd[len("RCPT TO:"):], "<> "))
			reply("250 ok")
		case upper == "DATA":
			reply("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.data = b.String()
			reply("250 queued")
		case upper == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

func TestEmail(t *testing.T) {
	srv := newSMTPServer(t)
	_, port, _ := net.SplitHostPort(srv.ln.Addr().String())
	tgt, err := Target{Kind: KindEmail, SMTPHost: "127.0.0.1", Security: "none", From: "Home Lab <hlm@example.com>",
		To: []string{"me@example.com", "you@example.com"}}.merge(nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt.SMTPPort = mustAtoi(t, port)

	m := message{Event: Event{Kind: KindBackupFailed, Severity: Error, Title: "Backup of välheim failed",
		Message: "upload <refused>", Server: "valheim", Time: time.Now()}, URL: "https://hlm.example.com/servers/valheim"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := (emailSender{}).send(ctx, tgt, m); err != nil {
		t.Fatal(err)
	}
	<-srv.done
	if srv.from != "hlm@example.com" || len(srv.to) != 2 {
		t.Errorf("envelope from %q to %v", srv.from, srv.to)
	}
	msg, err := mail.ReadMessage(strings.NewReader(srv.data))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if !strings.Contains(subject, "Backup of välheim failed") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(srv.data, "upload &lt;refused&gt;") || !strings.Contains(srv.data, "https://hlm.example.com/servers/valheim") {
		t.Errorf("body doesn't have the escaped message and link:\n%s", srv.data)
	}
}

func TestEmailNeedsSTARTTLS(t *testing.T) {
	srv := newSMTPServer(t)
	_, port, _ := net.SplitHostPort(srv.ln.Addr().String())
	tgt, err := Target{Kind: KindEmail, SMTPHost: "127.0.0.1", From: "hlm@example.com", To: []string{"me@example.com"}}.merge(nil)
	if err != nil {
		t.Fatal(err)
	}
	tgt.SMTPPort = mustAtoi(t, port)
	err = (emailSender{}).send(context.Background(), tgt, message{Event: Event{Title: "x", Time: time.Now()}})
	var perm *permanentError
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") || !errors.As(err, &perm) {
		t.Errorf("sending without STARTTLS on offer: %v", err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
