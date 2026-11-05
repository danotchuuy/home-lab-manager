// Package remote runs commands on another machine over SSH, authenticating
// with a key pair that hlm generates (or that the user imports).
package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// KeyPair is an SSH key pair. Private holds the private key in OpenSSH PEM
// form and must only be stored encrypted.
type KeyPair struct {
	Private     string    `json:"private"`
	Public      string    `json:"public"` // authorized_keys line
	Fingerprint string    `json:"fingerprint"`
	Created     time.Time `json:"created"`
}

// GenerateKey makes a new Ed25519 key pair.
func GenerateKey(comment string) (KeyPair, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, err
	}
	return keyPair(string(pem.EncodeToMemory(block)), comment)
}

// ParseKey loads an existing private key (OpenSSH, PKCS#1/8 or SEC 1 PEM).
// Passphrase-protected keys aren't supported.
func ParseKey(private, comment string) (KeyPair, error) {
	return keyPair(strings.TrimSpace(private)+"\n", comment)
}

func keyPair(private, comment string) (KeyPair, error) {
	signer, err := ssh.ParsePrivateKey([]byte(private))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return KeyPair{}, errors.New("this key is protected by a passphrase; use a key without one")
		}
		return KeyPair{}, fmt.Errorf("not a valid private key: %w", err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		pub += " " + comment
	}
	return KeyPair{
		Private:     private,
		Public:      pub,
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		Created:     time.Now().UTC(),
	}, nil
}

// Target is a machine to connect to.
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// HostKey pins the server's host key, in authorized_keys form. Empty
	// means "not trusted yet": connecting fails with a HostKeyError that
	// carries the key for the user to confirm.
	HostKey string `json:"hostKey,omitempty"`
}

// HostKeyError is returned when the server's host key hasn't been trusted
// yet. Key is in authorized_keys form, for pinning in Target.HostKey.
type HostKeyError struct {
	Key         string
	Fingerprint string
	Type        string // e.g. "ed25519", as in /etc/ssh/ssh_host_<type>_key.pub
}

func (e *HostKeyError) Error() string {
	return "the server's host key isn't trusted yet; its fingerprint is " + e.Fingerprint
}

// ErrHostKeyChanged means the server presented a different key than the
// pinned one: it was reinstalled, or something is intercepting the
// connection.
var ErrHostKeyChanged = errors.New("the server's host key doesn't match the trusted one; if the server was reinstalled, trust its new key, otherwise someone may be intercepting the connection")

// Conn is an open SSH connection.
type Conn struct {
	client *ssh.Client
}

// Dial connects and authenticates.
func Dial(ctx context.Context, t Target, key KeyPair) (*Conn, error) {
	if t.Host == "" || t.User == "" {
		return nil, errors.New("a host and user are required")
	}
	if t.Port == 0 {
		t.Port = 22
	}
	signer, err := ssh.ParsePrivateKey([]byte(key.Private))
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}

	var pinned ssh.PublicKey
	if t.HostKey != "" {
		pinned, _, _, _, err = ssh.ParseAuthorizedKey([]byte(t.HostKey))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted host key: %w", err)
		}
	}
	var hostKeyErr error
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			switch {
			case pinned == nil:
				hostKeyErr = &HostKeyError{
					Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))),
					Fingerprint: ssh.FingerprintSHA256(k),
					Type:        keyFileType(k.Type()),
				}
			case !bytes.Equal(k.Marshal(), pinned.Marshal()):
				hostKeyErr = ErrHostKeyChanged
			default:
				return nil
			}
			return hostKeyErr
		},
		Timeout:           15 * time.Second,
		HostKeyAlgorithms: hostKeyAlgorithms(pinned),
	}

	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		nc.SetDeadline(deadline)
	} else {
		nc.SetDeadline(time.Now().Add(30 * time.Second))
	}
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if err != nil {
		nc.Close()
		if hostKeyErr != nil {
			return nil, hostKeyErr
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("the server refused the key: add hlm's public key to ~%s/.ssh/authorized_keys on %s", t.User, t.Host)
		}
		return nil, fmt.Errorf("SSH handshake with %s: %w", addr, err)
	}
	nc.SetDeadline(time.Time{}) // the handshake is done; commands set their own limits
	return &Conn{client: ssh.NewClient(sc, chans, reqs)}, nil
}

// hostKeyAlgorithms asks for the pinned key's type, so the server presents
// the key that was trusted. Before anything is pinned, Ed25519 comes first:
// it's the key people usually check, with "ssh-keygen -lf
// /etc/ssh/ssh_host_ed25519_key.pub".
func hostKeyAlgorithms(pinned ssh.PublicKey) []string {
	rsa := []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	if pinned != nil {
		if pinned.Type() == ssh.KeyAlgoRSA {
			return rsa
		}
		return []string{pinned.Type()}
	}
	return append([]string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521}, rsa...)
}

// keyFileType names a key type the way OpenSSH's host key files do.
func keyFileType(t string) string {
	switch {
	case t == ssh.KeyAlgoED25519:
		return "ed25519"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ecdsa"
	case t == ssh.KeyAlgoRSA:
		return "rsa"
	}
	return t
}

// Close closes the connection.
func (c *Conn) Close() error {
	return c.client.Close()
}

// Dial opens a connection from the server to addr: a "tcp" address or a
// "unix" socket path there, such as Docker's socket.
func (c *Conn) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.client.DialContext(ctx, network, addr)
}

// DialCommand runs a command whose input and output are a connection,
// such as "docker system dial-stdio".
func (c *Conn) DialCommand(ctx context.Context, cmd string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, err
	}
	return &cmdConn{sess: sess, stdin: stdin, stdout: stdout}, nil
}

// cmdConn is a command's input and output as a connection.
type cmdConn struct {
	sess   *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *cmdConn) Close() error {
	c.stdin.Close()
	return c.sess.Close()
}
func (c *cmdConn) LocalAddr() net.Addr              { return cmdAddr{} }
func (c *cmdConn) RemoteAddr() net.Addr             { return cmdAddr{} }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }

type cmdAddr struct{}

func (cmdAddr) Network() string { return "ssh" }
func (cmdAddr) String() string  { return "ssh-command" }

// Wait returns when the connection has ended.
func (c *Conn) Wait() error {
	return c.client.Wait()
}

// KeepAlive asks the server for a reply, to check the connection still
// works.
func (c *Conn) KeepAlive() error {
	_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
	return err
}

// Result is the outcome of a command.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// Output returns stdout and stderr together, for showing to a person.
func (r Result) Output() string {
	return strings.TrimSpace(strings.TrimSpace(r.Stdout) + "\n" + strings.TrimSpace(r.Stderr))
}

// Run runs a shell command, feeding it stdin, and waits for it. A non-zero
// exit status is reported in Result, not as an error.
func (c *Conn) Run(ctx context.Context, cmd string, stdin []byte) (Result, error) {
	return c.RunLive(ctx, cmd, stdin, nil)
}

// RunLive is Run that also passes each line of output, stdout and stderr
// alike, to onLine (if non-nil) as the command prints it.
func (c *Conn) RunLive(ctx context.Context, cmd string, stdin []byte, onLine func(string)) (Result, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	if onLine != nil {
		lines := &lineWriter{onLine: onLine}
		defer lines.flush()
		sess.Stdout, sess.Stderr = io.MultiWriter(&out, lines), io.MultiWriter(&errb, lines)
	}
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	if err := sess.Start(cmd); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		<-done // the output copiers are finished once Wait returns
		return Result{Stdout: out.String(), Stderr: errb.String(), ExitCode: -1}, ctx.Err()
	case err := <-done:
		res := Result{Stdout: out.String(), Stderr: errb.String()}
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			res.ExitCode = exit.ExitStatus()
			return res, nil
		}
		return res, err
	}
}

// lineWriter calls onLine for each complete line written to it, from
// stdout and stderr at once.
type lineWriter struct {
	mu     sync.Mutex
	buf    []byte
	onLine func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		// Progress bars (e.g. apt's) redraw with \r; treat it as a line end.
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			return len(p), nil
		}
		if line := strings.TrimRight(string(w.buf[:i]), " "); line != "" {
			w.onLine(line)
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if line := strings.TrimSpace(string(w.buf)); line != "" {
		w.onLine(line)
	}
	w.buf = nil
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
