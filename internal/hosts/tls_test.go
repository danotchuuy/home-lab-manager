package hosts

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

func parseCert(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		t.Fatal("not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewCerts(t *testing.T) {
	cs, err := newCerts([]string{"192.168.1.20", "nas", "nas.lan", "localhost", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parseCert(t, cs.CA))
	server, client := parseCert(t, cs.ServerCert), parseCert(t, cs.ClientCert)
	for _, name := range []string{"192.168.1.20", "nas.lan", "127.0.0.1"} {
		if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("server certificate for %s: %v", name, err)
		}
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.lan"}); err == nil {
		t.Error("server certificate valid for a name it wasn't made for")
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate: %v", err)
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("client certificate usable as a server's")
	}
	// Another host's CA doesn't trust this client certificate.
	other, _ := newCerts([]string{"other"})
	otherPool := x509.NewCertPool()
	otherPool.AddCert(parseCert(t, other.CA))
	if _, err := client.Verify(x509.VerifyOptions{Roots: otherPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Error("one host's client certificate works on another")
	}
	if strings.Contains(cs.CA+cs.ServerCert+cs.ClientCert, "PRIVATE") {
		t.Error("a private key in a certificate")
	}
}

func TestCertNames(t *testing.T) {
	got := certNames("192.168.1.20", Inspection{Hostname: "nas.lan", IPs: "192.168.1.20 172.17.0.1 fe80::1"})
	want := "192.168.1.20 nas.lan nas 172.17.0.1 fe80::1 localhost 127.0.0.1"
	if strings.Join(got, " ") != want {
		t.Errorf("names = %q", got)
	}
}

func TestTLSScriptSyntax(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(tlsScript(2376))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}

// TestSetUpTLS goes through the whole setup with a fake host: the SSH
// server answers the probe, takes the daemon's certificates from the
// script's input and starts a TLS "daemon" with them that wants hlm's
// client certificate, and hlm saves the host once it can connect to it.
func TestSetUpTLS(t *testing.T) {
	kp, _ := remote.GenerateKey("hlm@test")
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(kp.Public))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	var commands []string
	probeOut := "id=ubuntu\nuid=1000\nuser=hlm\nsudo=yes\ndocker=yes\nsocket=yes\nsystemd=yes\nhostname=nas\nips=127.0.0.1\n"
	exec := func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		mu.Lock()
		commands = append(commands, cmd)
		mu.Unlock()
		switch {
		case strings.Contains(cmd, "os-release"):
			io.WriteString(stdout, probeOut)
			return 0
		case strings.HasPrefix(cmd, "sudo -n sh -c ") && strings.Contains(cmd, "hlm-tls"):
			r := bufio.NewReader(stdin)
			var pems []string
			for range 3 {
				line, _ := r.ReadString('\n')
				b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
				if err != nil {
					fmt.Fprintln(stderr, "bad input:", err)
					return 1
				}
				pems = append(pems, string(b))
			}
			pair, err := tls.X509KeyPair([]byte(pems[1]), []byte(pems[2]))
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM([]byte(pems[0]))
			ds := httptest.NewUnstartedServer(&fakeDocker{})
			ds.Listener.Close()
			ds.Listener = ln
			ds.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
			ds.Config.ErrorLog = log.New(io.Discard, "", 0)
			ds.StartTLS()
			t.Cleanup(ds.Close)
			io.WriteString(stdout, "active\n")
			return 0
		}
		fmt.Fprintln(stderr, "unexpected command:", cmd)
		return 127
	}
	target, hostKey, _ := sshServer(t, pub, sshOptions{exec: exec})
	target.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	r, store, got := registry(t, kp)
	in := TLSInput{Input: Input{Host: Host{ID: "nas", Kind: KindTCP, SSH: &target, AppdataRoot: "/srv/appdata", Address: "127.0.0.1"}}, Port: port}

	var lines []string
	h, err := r.SetUpTLS(context.Background(), in, true, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(lines, "\n"))
	}
	if h.Kind != KindTCP || h.URL != "tcp://127.0.0.1:"+strconv.Itoa(port) || !h.HasKey || h.SSH != nil ||
		!strings.Contains(h.CA, "CERTIFICATE") || !strings.Contains(h.Cert, "CERTIFICATE") {
		t.Errorf("saved = %+v", h)
	}
	if len(*got) != 1 {
		t.Fatalf("manager got %+v", *got)
	}
	if v, err := (*got)[0].Docker.Version(context.Background()); err != nil || v.Version != "27.1.0" {
		t.Errorf("version over TLS = %+v, %v", v, err)
	}
	var key string
	if ok, _ := store.Get(secretKeyPrefix+"nas", &key); !ok || !strings.Contains(key, "PRIVATE KEY") {
		t.Error("hlm's client key wasn't kept")
	}
	// The server's key went over stdin, never on a command line.
	for _, c := range commands {
		if strings.Contains(c, "PRIVATE KEY") || strings.Contains(c, base64.StdEncoding.EncodeToString([]byte("-----BEGIN PRIVATE KEY"))[:20]) {
			t.Errorf("a key on the command line: %.80s", c)
		}
	}

	// A host without Docker yet: nothing is changed.
	probeOut = "id=ubuntu\nuid=0\nuser=root\nsystemd=yes\n"
	commands = nil
	in.ID = "other"
	if _, err := r.SetUpTLS(context.Background(), in, true, func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("without Docker: %v", err)
	}
	if len(commands) != 1 {
		t.Errorf("commands run without Docker: %q", commands)
	}
	// A bad host (here, its data folder) is refused before signing in.
	in.AppdataRoot = "relative"
	commands = nil
	if _, err := r.SetUpTLS(context.Background(), in, true, func(string) {}); err == nil || len(commands) != 0 {
		t.Errorf("bad host: %v, commands %q", err, commands)
	}
}

// TestDockerTLSScript runs the script in a throwaway container with a stub
// systemctl, checking the files it writes, the drop-in's command line, and
// that it puts things back when Docker won't start. Opt-in:
// HLM_DOCKER_TESTS=1.
func TestDockerTLSScript(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run the TLS script in a busybox container")
	}
	cs, err := newCerts([]string{"nas"})
	if err != nil {
		t.Fatal(err)
	}
	stdin := base64.StdEncoding.EncodeToString([]byte(cs.CA)) + "\n" +
		base64.StdEncoding.EncodeToString([]byte(cs.ServerCert)) + "\n" +
		base64.StdEncoding.EncodeToString([]byte(cs.ServerKey)) + "\n"
	// systemctl: "show" answers as docker-ce's unit does (with the drop-in
	// removed); "restart" fails when /fail exists.
	stub := `mkdir -p /usr/local/bin /etc/docker
cat > /usr/local/bin/systemctl <<'EOS'
#!/bin/sh
echo "systemctl $*" >> /calls
case "$1" in
show) echo '{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }' ;;
restart) [ ! -e /fail ] || { [ -e /etc/systemd/system/docker.service.d/hlm-tls.conf ] && exit 1; exit 0; } ;;
is-active) echo active ;;
esac
EOS
printf '#!/bin/sh\necho journal\n' > /usr/local/bin/journalctl
chmod +x /usr/local/bin/systemctl /usr/local/bin/journalctl
`
	run := func(pre, after string) (string, error) {
		script := stub + pre + "\nsh -c " + remote.Quote(tlsScript(2376)) + " < /in; echo exit=$?\n" + after
		cmd := exec.Command("docker", "run", "--rm", "-i", "busybox:1.37", "sh", "-c",
			"cat > /in; "+script)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("", `cat /etc/systemd/system/docker.service.d/hlm-tls.conf
ls -l /etc/docker/hlm-tls | awk '{print $1, $NF}'
cmp -s /etc/docker/hlm-tls/server-key.pem /dev/null || head -c 27 /etc/docker/hlm-tls/server-key.pem; echo
stat -c '%a' /etc/docker/hlm-tls`)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"exit=0",
		"ExecStart=\nExecStart=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock -H tcp://0.0.0.0:2376 --tlsverify --tlscacert=/etc/docker/hlm-tls/ca.pem --tlscert=/etc/docker/hlm-tls/server-cert.pem --tlskey=/etc/docker/hlm-tls/server-key.pem",
		"-r--r--r-- ca.pem", "-r--r--r-- server-cert.pem", "-r-------- server-key.pem",
		"-----BEGIN PRIVATE KEY-----", "\n700\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Docker won't start with TLS: the drop-in goes, and Docker restarts.
	out, _ = run("touch /fail", `[ -e /etc/systemd/system/docker.service.d/hlm-tls.conf ] && echo "drop-in kept"; grep -c restart /calls`)
	if !strings.Contains(out, "exit=1") || strings.Contains(out, "drop-in kept") || !strings.Contains(out, "putting it back") ||
		!strings.HasSuffix(strings.TrimSpace(out), "2") {
		t.Errorf("failed restart:\n%s", out)
	}

	// daemon.json already sets hosts: nothing is touched.
	out, _ = run(`echo '{"hosts": ["unix:///var/run/docker.sock"]}' > /etc/docker/daemon.json`, `ls /etc/docker`)
	if !strings.Contains(out, "exit=1") || !strings.Contains(out, "already sets hosts") || strings.Contains(out, "hlm-tls") {
		t.Errorf("daemon.json with hosts:\n%s", out)
	}
}
