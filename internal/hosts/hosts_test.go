package hosts

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeDocker answers /_ping and /version, and counts requests.
type fakeDocker struct {
	mu       sync.Mutex
	requests int
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/_ping"):
		w.Header().Set("API-Version", "1.45")
	case strings.HasSuffix(r.URL.Path, "/version"):
		json.NewEncoder(w).Encode(map[string]string{"Version": "27.1.0", "ApiVersion": "1.45", "Os": "linux", "Arch": "amd64"})
	default:
		http.NotFound(w, r)
	}
}

// sshOptions says how the test SSH server behaves.
type sshOptions struct {
	// noForwarding refuses direct-streamlocal channels, as OpenSSH does with
	// AllowTcpForwarding no.
	noForwarding bool
	// dockerCLI is the socket "docker system dial-stdio" connects to; ""
	// means there's no docker command.
	dockerCLI string
	// dockerDenied makes the docker command fail as for a user outside
	// the docker group.
	dockerDenied bool
	// shell runs other commands with sh on this machine.
	shell bool
}

// sshServer accepts hlm's key and forwards direct-streamlocal channels
// (what ssh -L does for unix sockets) to sockets on this machine.
func sshServer(t *testing.T, allowed ssh.PublicKey, o ...sshOptions) (remote.Target, ssh.PublicKey, *atomic.Int32) {
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
	logins := new(atomic.Int32)
	var opts sshOptions
	if len(o) > 0 {
		opts = o[0]
	}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				logins.Add(1)
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() == "session" {
						go serveSession(nch, opts)
						continue
					}
					if nch.ChannelType() != "direct-streamlocal@openssh.com" {
						nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					if opts.noForwarding {
						nch.Reject(ssh.Prohibited, "open failed")
						continue
					}
					var msg struct {
						Path      string
						Reserved0 string
						Reserved1 uint32
					}
					if err := ssh.Unmarshal(nch.ExtraData(), &msg); err != nil {
						nch.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					uc, err := net.Dial("unix", msg.Path)
					if err != nil {
						nch.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, chReqs, err := nch.Accept()
					if err != nil {
						uc.Close()
						continue
					}
					go ssh.DiscardRequests(chReqs)
					go func() { io.Copy(uc, ch); uc.Close() }()
					go func() { io.Copy(ch, uc); ch.Close() }()
				}
			}()
		}
	}()
	return remote.Target{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, User: "docker"}, hostSigner.PublicKey(), logins
}

// serveSession runs "docker system dial-stdio" as a pipe to the socket.
func serveSession(nch ssh.NewChannel, o sshOptions) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var cmd struct{ Command string }
		ssh.Unmarshal(req.Payload, &cmd)
		req.Reply(true, nil)
		if o.shell && !strings.HasPrefix(cmd.Command, "docker") {
			c := exec.Command("sh", "-c", cmd.Command)
			c.Stdout, c.Stderr = ch, ch.Stderr()
			code := uint32(0)
			if err := c.Run(); err != nil {
				code = 1
			}
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
			return
		}
		if o.dockerDenied {
			io.WriteString(ch.Stderr(), "permission denied while trying to connect to the Docker daemon socket\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{1}))
			return
		}
		if o.dockerCLI != "" && strings.HasPrefix(cmd.Command, "docker version") {
			io.WriteString(ch, "27.1.0\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
			return
		}
		if o.dockerCLI == "" || !strings.HasSuffix(cmd.Command, "system dial-stdio") {
			io.WriteString(ch.Stderr(), "sh: docker: not found\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{127}))
			return
		}
		uc, err := net.Dial("unix", o.dockerCLI)
		if err != nil {
			return
		}
		go func() { io.Copy(uc, ch); uc.Close() }()
		io.Copy(ch, uc)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		return
	}
}

func registry(t *testing.T, key remote.KeyPair) (*Registry, *secrets.Store, *[]manager.RemoteHost) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := new([]manager.RemoteHost)
	r := New(Options{
		Secrets:  store,
		SSHKey:   func() (remote.KeyPair, bool, error) { return key, key.Private != "", nil },
		OnChange: func(h []manager.RemoteHost) { *got = h },
		Log:      slog.New(slog.DiscardHandler),
	})
	return r, store, got
}

func TestSSHHost(t *testing.T) {
	kp, err := remote.GenerateKey("hlm@test")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	target, hostKey, logins := sshServer(t, pub)

	// Docker's socket on the "other host".
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{}
	ds := httptest.NewUnstartedServer(fd)
	ds.Listener.Close()
	ds.Listener = ln
	ds.Start()
	defer ds.Close()

	r, _, got := registry(t, kp)
	ctx := context.Background()
	in := Input{Host: Host{ID: "nas", Name: "NAS", Kind: KindSSH, SSH: &target, Socket: sock, AppdataRoot: "/mnt/user/appdata/"}}

	// The host key has to be trusted first.
	_, err = r.Test(ctx, in)
	var hk *remote.HostKeyError
	if !errors.As(err, &hk) || hk.Key != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))) {
		t.Fatalf("untrusted host key: %v", err)
	}
	if _, err := r.Save(ctx, in, true); err == nil {
		t.Fatal("saved a host whose key isn't trusted")
	}
	target.HostKey = hk.Key
	res, err := r.Test(ctx, in)
	if err != nil || res.Version != "27.1.0" || res.OS != "linux" {
		t.Fatalf("test = %+v, %v", res, err)
	}
	h, err := r.Save(ctx, in, true)
	if err != nil {
		t.Fatal(err)
	}
	if h.AppdataRoot != "/mnt/user/appdata" || h.Address != "127.0.0.1" || h.SSH.Port != target.Port || h.Created.IsZero() {
		t.Errorf("saved = %+v", h)
	}
	if _, err := r.Save(ctx, in, true); err == nil {
		t.Error("added the same host twice")
	}
	if len(*got) != 1 || (*got)[0].ID != "nas" || (*got)[0].AppdataRoot != "/mnt/user/appdata" {
		t.Fatalf("manager got %+v", *got)
	}

	// The manager's client goes over the SSH connection Save checked the
	// host on, kept open.
	c := (*got)[0].Docker
	before := logins.Load()
	for range 3 {
		if v, err := c.Version(ctx); err != nil || v.Version != "27.1.0" {
			t.Fatalf("version over SSH = %+v, %v", v, err)
		}
	}
	if n := logins.Load() - before; n != 0 {
		t.Errorf("signed in %d times for three requests", n)
	}
	if _, err := r.Check(ctx, "nas"); err != nil {
		t.Errorf("check: %v", err)
	}

	// Loaded again (as at startup), it reconnects.
	r2 := New(Options{Secrets: r.opts.Secrets, SSHKey: r.opts.SSHKey, OnChange: func(h []manager.RemoteHost) { *got = h }})
	if err := r2.Load(); err != nil || len(*got) != 1 {
		t.Fatalf("load: %v, %+v", err, *got)
	}
	if _, err := (*got)[0].Docker.Version(ctx); err != nil {
		t.Errorf("after loading: %v", err)
	}
	r2.Close()

	if err := r.Delete("nas"); err != nil || len(*got) != 0 {
		t.Errorf("delete: %v, %+v", err, *got)
	}
	if _, err := r.Get("nas"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted host: %v", err)
	}
	if err := r.Delete("nas"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: %v", err)
	}
}

// Where the SSH server doesn't forward sockets, hlm goes through the docker
// command on the host, as the docker CLI does for ssh:// hosts.
func TestSSHHostWithoutForwarding(t *testing.T) {
	kp, _ := remote.GenerateKey("hlm@test")
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ds := httptest.NewUnstartedServer(&fakeDocker{})
	ds.Listener.Close()
	ds.Listener = ln
	ds.Start()
	defer ds.Close()
	ctx := context.Background()
	r, _, got := registry(t, kp)

	// No docker command either: the error says what to change.
	target, hostKey, _ := sshServer(t, pub, sshOptions{noForwarding: true})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	in := Input{Host: Host{ID: "nas", Kind: KindSSH, SSH: &target, AppdataRoot: "/srv"}}
	if _, err := r.Test(ctx, in); !errors.Is(err, ErrNoDocker) {
		t.Errorf("without forwarding or docker: %v", err)
	}

	// A docker command that can't reach Docker: say what to change.
	target, hostKey, _ = sshServer(t, pub, sshOptions{noForwarding: true, dockerDenied: true})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	in.SSH = &target
	if _, err := r.Test(ctx, in); err == nil || !strings.Contains(err.Error(), "AllowTcpForwarding") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("docker without permission: %v", err)
	}

	target, hostKey, _ = sshServer(t, pub, sshOptions{noForwarding: true, dockerCLI: sock})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	in.SSH = &target
	if _, err := r.Save(ctx, in, true); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if v, err := (*got)[0].Docker.Version(ctx); err != nil || v.Version != "27.1.0" {
			t.Fatalf("version through the docker command = %+v, %v", v, err)
		}
	}
}

// Inspect signs in and reads what's on the host: here, this machine.
func TestInspectOverSSH(t *testing.T) {
	kp, _ := remote.GenerateKey("hlm@test")
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	target, hostKey, _ := sshServer(t, pub, sshOptions{shell: true})
	r, _, _ := registry(t, kp)
	in := Input{Host: Host{Kind: KindSSH, SSH: &target}}
	var hk *remote.HostKeyError
	if _, err := r.Inspect(context.Background(), in); !errors.As(err, &hk) {
		t.Fatalf("untrusted host key: %v", err)
	}
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	got, err := r.Inspect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	me, _ := user.Current()
	if got.User != me.Username || got.Arch == "" || got.Root != (me.Uid == "0") {
		t.Errorf("inspection = %+v", got)
	}
	if !got.Ready && got.Script == "" && got.Reason == "" {
		t.Errorf("not ready, with no way forward: %+v", got)
	}
	if _, err := r.Inspect(context.Background(), Input{Host: Host{Kind: KindTCP}}); err == nil {
		t.Error("inspected a TCP host")
	}
}

// certs makes a CA, a server certificate for 127.0.0.1 and a client
// certificate, in PEM.
func certs(t *testing.T) (caPEM string, server tls.Certificate, clientCert, clientKey string, pool *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	ca, _ = x509.ParseCertificate(caDER)
	pool = x509.NewCertPool()
	pool.AddCert(ca)
	issue := func(serial int64, usage x509.ExtKeyUsage, ip bool) (string, string) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "x"},
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		if ip {
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		kder, _ := x509.MarshalECPrivateKey(k)
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
	}
	sc, sk := issue(2, x509.ExtKeyUsageServerAuth, true)
	server, _ = tls.X509KeyPair([]byte(sc), []byte(sk))
	clientCert, clientKey = issue(3, x509.ExtKeyUsageClientAuth, false)
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return
}

func TestTLSHost(t *testing.T) {
	caPEM, serverCert, clientCert, clientKey, pool := certs(t)
	ds := httptest.NewUnstartedServer(&fakeDocker{})
	ds.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	ds.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are expected
	ds.StartTLS()
	defer ds.Close()
	addr := strings.TrimPrefix(ds.URL, "https://")

	r, store, got := registry(t, remote.KeyPair{})
	ctx := context.Background()
	in := Input{Host: Host{ID: "gpu", Kind: KindTCP, URL: "tcp://" + addr, CA: caPEM, AppdataRoot: "/srv/appdata"}}

	// Without a client certificate the daemon refuses hlm.
	if _, err := r.Test(ctx, in); err == nil {
		t.Error("connected without a client certificate")
	}
	in.Cert, in.Key = clientCert, clientKey
	h, err := r.Save(ctx, in, true)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasKey || h.Name != "gpu" || h.Address != "127.0.0.1" {
		t.Errorf("saved = %+v", h)
	}
	// The key is kept, encrypted, apart from the host, and never listed.
	list, _ := r.List()
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Error("the client key is in the list")
	}
	var key string
	if ok, _ := store.Get(secretKeyPrefix+"gpu", &key); !ok || !strings.Contains(key, "PRIVATE KEY") {
		t.Error("the client key wasn't kept")
	}
	// Changing the host without sending the key again keeps it.
	in.Name, in.Key = "GPU box", ""
	if h, err = r.Save(ctx, in, false); err != nil || !h.HasKey || h.Name != "GPU box" {
		t.Fatalf("update = %+v, %v", h, err)
	}
	if _, err := (*got)[0].Docker.Version(ctx); err != nil {
		t.Errorf("version after the update: %v", err)
	}

	// Some other daemon's certificate isn't trusted.
	other := httptest.NewUnstartedServer(&fakeDocker{})
	other.Config.ErrorLog = log.New(io.Discard, "", 0)
	other.StartTLS()
	defer other.Close()
	in2 := Input{Host: Host{ID: "other", Kind: KindTCP, URL: "tcp://" + strings.TrimPrefix(other.URL, "https://"), CA: caPEM, AppdataRoot: "/srv"}}
	if _, err := r.Test(ctx, in2); err == nil {
		t.Error("trusted a certificate the CA didn't sign")
	}

	if err := r.Delete("gpu"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.Get(secretKeyPrefix+"gpu", &key); ok {
		t.Error("the client key outlived its host")
	}
}

func TestValidate(t *testing.T) {
	ssh := &remote.Target{Host: "nas", User: "me"}
	for _, tt := range []struct {
		name string
		h    Host
		key  string
		want string
	}{
		{"bad id", Host{ID: "My NAS", Kind: KindSSH, SSH: ssh, AppdataRoot: "/a"}, "", "lower-case"},
		{"local", Host{ID: "local", Kind: KindSSH, SSH: ssh, AppdataRoot: "/a"}, "", "local"},
		{"relative data folder", Host{ID: "nas", Kind: KindSSH, SSH: ssh, AppdataRoot: "appdata"}, "", "absolute"},
		{"root data folder", Host{ID: "nas", Kind: KindSSH, SSH: ssh, AppdataRoot: "/"}, "", "can't be /"},
		{"plain http", Host{ID: "nas", Kind: KindTCP, URL: "http://nas:2375", AppdataRoot: "/a"}, "", "tcp://"},
		{"cert without key", Host{ID: "nas", Kind: KindTCP, URL: "tcp://nas", Cert: "x", AppdataRoot: "/a"}, "", "needs its key"},
		{"bad CA", Host{ID: "nas", Kind: KindTCP, URL: "tcp://nas", CA: "nope", AppdataRoot: "/a"}, "", "PEM"},
		{"no ssh user", Host{ID: "nas", Kind: KindSSH, SSH: &remote.Target{Host: "nas"}, AppdataRoot: "/a"}, "", "user"},
		{"relative socket", Host{ID: "nas", Kind: KindSSH, SSH: ssh, Socket: "docker.sock", AppdataRoot: "/a"}, "", "absolute"},
		{"unknown kind", Host{ID: "nas", Kind: "agent", AppdataRoot: "/a"}, "", "connect over"},
	} {
		h := tt.h
		if err := validate(&h, tt.key); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	h := Host{ID: "nas", Kind: KindTCP, URL: "tcp://nas.lan", AppdataRoot: "/a"}
	if err := validate(&h, ""); err != nil || h.URL != "tcp://nas.lan:2376" || h.Address != "nas.lan" || h.Name != "nas" {
		t.Errorf("defaults: %+v, %v", h, err)
	}
	h = Host{ID: "nas", Kind: KindSSH, SSH: &remote.Target{Host: "nas", User: "me"}, AppdataRoot: "/a", Address: "192.168.1.5"}
	if err := validate(&h, ""); err != nil || h.SSH.Port != 22 || h.Socket != DefaultSocket || h.Address != "192.168.1.5" {
		t.Errorf("ssh defaults: %+v, %v", h, err)
	}
}
