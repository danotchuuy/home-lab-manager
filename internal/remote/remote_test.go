package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is a minimal SSH server that runs "exec" requests with sh and
// only accepts the given client key.
func testServer(t *testing.T, allowed ssh.PublicKey) (Target, ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(nc, cfg)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return Target{Host: "127.0.0.1", Port: port, User: "tester"}, hostSigner.PublicKey()
}

func serveConn(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		ch, reqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(req.Payload)
				cmd := exec.Command("sh", "-c", string(req.Payload[4:4+n]))
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				req.Reply(true, nil)
				code := 0
				if err := cmd.Run(); err != nil {
					var ee *exec.ExitError
					code = 1
					if errors.As(err, &ee) {
						code = ee.ExitCode()
					}
				}
				ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(code)))
				return
			}
		}()
	}
}

func clientKey(t *testing.T) (KeyPair, ssh.PublicKey) {
	t.Helper()
	kp, err := GenerateKey("hlm@test")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(kp.Public))
	if err != nil {
		t.Fatal(err)
	}
	return kp, pub
}

func TestTrustOnFirstUseAndRun(t *testing.T) {
	kp, pub := clientKey(t)
	if !strings.HasPrefix(kp.Public, "ssh-ed25519 ") || !strings.HasSuffix(kp.Public, " hlm@test") ||
		!strings.Contains(kp.Private, "OPENSSH PRIVATE KEY") || !strings.HasPrefix(kp.Fingerprint, "SHA256:") {
		t.Fatalf("key pair = %+v", kp)
	}
	target, hostKey := testServer(t, pub)
	ctx := context.Background()

	// First contact: the host key must be confirmed.
	_, err := Dial(ctx, target, kp)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Fingerprint != ssh.FingerprintSHA256(hostKey) || hk.Type != "ed25519" {
		t.Fatalf("first dial: err = %v", err)
	}

	target.HostKey = hk.Key
	c, err := Dial(ctx, target, kp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Run(ctx, "cat; echo oops >&2; exit 3", []byte("hello"))
	if err != nil || res.Stdout != "hello" || res.Stderr != "oops\n" || res.ExitCode != 3 {
		t.Errorf("run = %+v, %v", res, err)
	}
	quoted := "it's a \"test\" $HOME"
	res, _ = c.Run(ctx, "printf %s "+Quote(quoted), nil)
	if res.Stdout != quoted {
		t.Errorf("quoting: got %q", res.Stdout)
	}

	// Commands can be cancelled.
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Run(cctx, "sleep 10", nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Errorf("cancel: err = %v after %s", err, time.Since(start))
	}
}

func TestHostKeyChanged(t *testing.T) {
	kp, pub := clientKey(t)
	target, _ := testServer(t, pub)
	other, _ := clientKey(t) // any other key in authorized_keys form
	target.HostKey = other.Public
	if _, err := Dial(context.Background(), target, kp); !errors.Is(err, ErrHostKeyChanged) {
		t.Errorf("err = %v, want ErrHostKeyChanged", err)
	}
}

func TestKeyNotAuthorized(t *testing.T) {
	kp, _ := clientKey(t)
	_, otherPub := clientKey(t)
	target, hostKey := testServer(t, otherPub)
	target.HostKey = string(ssh.MarshalAuthorizedKey(hostKey))
	_, err := Dial(context.Background(), target, kp)
	if err == nil || !strings.Contains(err.Error(), "authorized_keys") {
		t.Errorf("err = %v", err)
	}
}

func TestParseKey(t *testing.T) {
	kp, _ := clientKey(t)
	again, err := ParseKey(kp.Private, "imported")
	if err != nil || again.Fingerprint != kp.Fingerprint || !strings.HasSuffix(again.Public, " imported") {
		t.Errorf("parse = %+v, %v", again, err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if _, err := ParseKey(string(pemEncode(block)), ""); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("passphrase key: %v", err)
	}
	if _, err := ParseKey("not a key", ""); err == nil {
		t.Error("garbage: want error")
	}
}

func pemEncode(b *pem.Block) []byte { return pem.EncodeToMemory(b) }

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{onLine: func(s string) { got = append(got, s) }}
	w.Write([]byte("Reading package lists... 0%\rReading package lists... Done\nSetting up wiregu"))
	w.Write([]byte("ard-tools (1.0)\n\n"))
	w.Write([]byte("no newline"))
	w.flush()
	want := []string{"Reading package lists... 0%", "Reading package lists... Done", "Setting up wireguard-tools (1.0)", "no newline"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
}
