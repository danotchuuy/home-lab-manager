package hosts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// Setting up Docker's API over TCP with TLS on a host, over SSH, as
// Docker's "Protect the Docker daemon socket" page describes: a CA, a
// server certificate for the daemon and a client certificate for hlm, and
// dockerd started with --tlsverify. Each host gets its own CA, whose key is
// thrown away once the two certificates are signed, so a certificate for
// one host is no use on another; setting TLS up again replaces them all.

// DefaultTLSPort is Docker's port for its API with TLS.
const DefaultTLSPort = 2376

// certValidity is how long the certificates last.
const certValidity = 10 * 365 * 24 * time.Hour

// tlsDir is where the daemon's certificates go on the host.
const tlsDir = "/etc/docker/hlm-tls"

// dropIn is the systemd drop-in that starts dockerd with TLS.
const dropIn = "/etc/systemd/system/docker.service.d/hlm-tls.conf"

// certs are a host's CA and the daemon's and hlm's certificates, in PEM.
type certs struct {
	CA, ServerCert, ServerKey, ClientCert, ClientKey string
}

// newCerts makes a CA and signs a server certificate for names (host names
// and IP addresses) and a client certificate with it.
func newCerts(names []string) (certs, error) {
	now := time.Now().Add(-time.Hour)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certs{}, err
	}
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "Home Lab Manager Docker CA", Organization: []string{"Home Lab Manager"}},
		NotBefore: now, NotAfter: now.Add(certValidity), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return certs{}, err
	}
	if ca, err = x509.ParseCertificate(caDER); err != nil {
		return certs{}, err
	}
	issue := func(cn string, usage x509.ExtKeyUsage, names []string) (string, string, error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		t := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: cn}, NotBefore: now, NotAfter: now.Add(certValidity),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		for _, n := range names {
			if ip := net.ParseIP(n); ip != nil {
				t.IPAddresses = append(t.IPAddresses, ip)
			} else {
				t.DNSNames = append(t.DNSNames, n)
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, t, ca, &k.PublicKey, caKey)
		if err != nil {
			return "", "", err
		}
		kder, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return "", "", err
		}
		return pemOf("CERTIFICATE", der), pemOf("PRIVATE KEY", kder), nil
	}
	var c certs
	c.CA = pemOf("CERTIFICATE", caDER)
	if c.ServerCert, c.ServerKey, err = issue(names[0], x509.ExtKeyUsageServerAuth, names); err != nil {
		return certs{}, err
	}
	if c.ClientCert, c.ClientKey, err = issue("home-lab-manager", x509.ExtKeyUsageClientAuth, nil); err != nil {
		return certs{}, err
	}
	return c, nil // caKey goes out of scope here: nothing else is ever signed with it
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func pemOf(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

// certNames are the names the daemon's certificate is for: the address hlm
// connects to first, then the host's own names and addresses, so another
// of them can be used later.
func certNames(address string, insp Inspection) []string {
	var out []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n != "" && !containsFold(out, n) {
			out = append(out, n)
		}
	}
	add(address)
	add(insp.Hostname)
	if short, _, ok := strings.Cut(insp.Hostname, "."); ok {
		add(short)
	}
	for _, ip := range strings.Fields(insp.IPs) {
		add(ip)
	}
	add("localhost")
	add("127.0.0.1")
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// tlsScript installs the daemon's certificates (read from stdin, one base64
// line each: CA, certificate, key, so the key is never on a command line)
// and starts dockerd with TLS on port: a systemd drop-in that keeps the
// unit's own command and adds the TCP socket and TLS flags. If Docker
// doesn't start with it, the drop-in is removed and Docker restarted as it
// was.
func tlsScript(port int) string {
	p := strconv.Itoa(port)
	return `set -e
d=` + tlsDir + `
dropin=` + dropIn + `

# Docker refuses to start when daemon.json and the command line both set
# these.
if [ -f /etc/docker/daemon.json ] && grep -Eq '"(hosts|tls|tlsverify|tlscacert|tlscert|tlskey)"' /etc/docker/daemon.json; then
  echo "/etc/docker/daemon.json already sets hosts or TLS options: remove them, or set TLS up yourself" >&2
  exit 1
fi

echo "Installing the daemon's certificates in $d"
install -d -m 0700 "$d"
IFS= read -r ca
IFS= read -r cert
IFS= read -r key
printf '%s' "$ca" | base64 -d > "$d/ca.pem"
printf '%s' "$cert" | base64 -d > "$d/server-cert.pem"
(umask 077; printf '%s' "$key" | base64 -d > "$d/server-key.pem")
chmod 0444 "$d/ca.pem" "$d/server-cert.pem"
chmod 0400 "$d/server-key.pem"

# How docker.service starts dockerd, without hlm's drop-in.
rm -f "$dropin"
systemctl daemon-reload
cur=$(systemctl show -p ExecStart --value docker.service | sed -n 's/^.*argv\[\]=\([^;]*\) ;.*$/\1/p' | head -n 1)
cur=$(printf '%s' "$cur" | sed 's/ *$//')
if [ -z "$cur" ]; then
  echo "couldn't read how docker.service starts dockerd" >&2
  exit 1
fi

echo "Starting dockerd with its API on TCP port ` + p + `, with TLS and client certificates"
install -d -m 0755 "$(dirname "$dropin")"
cat > "$dropin" <<EOF
# Written by Home Lab Manager: Docker's API on TCP port ` + p + `, for clients
# with a certificate signed by $d/ca.pem.
[Service]
ExecStart=
ExecStart=$cur -H tcp://0.0.0.0:` + p + ` --tlsverify --tlscacert=$d/ca.pem --tlscert=$d/server-cert.pem --tlskey=$d/server-key.pem
EOF
systemctl daemon-reload
if ! systemctl restart docker.service; then
  echo "Docker didn't start with TLS; putting it back as it was" >&2
  journalctl -u docker.service -n 15 --no-pager >&2 || true
  rm -f "$dropin"
  systemctl daemon-reload
  systemctl restart docker.service || true
  exit 1
fi
systemctl is-active docker.service
`
}

// TLSInput is a host to set TLS up on: the host as it'll be saved (with
// Kind "tcp"; URL, CA and certificates are filled in), and the SSH
// sign-in used to set it up.
type TLSInput struct {
	Input
	// Port is the TCP port for Docker's API; DefaultTLSPort if 0.
	Port int `json:"port,omitempty"`
}

// SetUpTLS signs in to a host over SSH, makes its certificates, installs
// the daemon's and starts dockerd with TLS, then saves the host (Kind
// "tcp") with the CA and hlm's client certificate, after connecting to it
// that way. create says whether the host is new. Lines of output go to
// onLine. Restarting Docker restarts its containers.
func (r *Registry) SetUpTLS(ctx context.Context, in TLSInput, create bool, onLine func(string)) (Host, error) {
	t, err := sshTarget(in.Input)
	if err != nil {
		return Host{}, err
	}
	port := in.Port
	if port == 0 {
		port = DefaultTLSPort
	}
	if port < 1 || port > 65535 {
		return Host{}, errors.New("the port is 1–65535")
	}
	address := strings.TrimSpace(in.Address)
	if address == "" {
		address = t.Host
	}
	// Check the rest of the host before touching anything.
	h := in.Host
	h.Kind, h.SSH, h.Socket = KindTCP, nil, ""
	h.URL = "tcp://" + net.JoinHostPort(address, strconv.Itoa(port))
	h.CA, h.Cert = "", ""
	if err := validate(&h, ""); err != nil {
		return Host{}, err
	}

	c, err := r.dialSSH(ctx, t)
	if err != nil {
		return Host{}, err
	}
	defer c.Close()
	insp, err := inspect(ctx, c)
	if err != nil {
		return Host{}, err
	}
	switch {
	case !insp.Docker || !insp.Running:
		return Host{}, errors.New("Docker isn't installed and running there yet: check for Docker and set it up first")
	case !insp.Systemd:
		return Host{}, errors.New("the host doesn't use systemd, so hlm can't change how Docker starts: set TLS up yourself")
	case !insp.Root && !insp.Sudo:
		return Host{}, fmt.Errorf("setting TLS up needs root, and %s can't run sudo without a password", insp.User)
	}

	onLine("Making a CA, and certificates for the daemon (" + strings.Join(certNames(address, insp), ", ") + ") and for hlm")
	cs, err := newCerts(certNames(address, insp))
	if err != nil {
		return Host{}, err
	}
	stdin := []byte(base64.StdEncoding.EncodeToString([]byte(cs.CA)) + "\n" +
		base64.StdEncoding.EncodeToString([]byte(cs.ServerCert)) + "\n" +
		base64.StdEncoding.EncodeToString([]byte(cs.ServerKey)) + "\n")
	cmd := "sh -c " + remote.Quote(tlsScript(port))
	if !insp.Root {
		cmd = "sudo -n " + cmd
	}
	r.opts.Log.Info("setting up Docker TLS on a host", "host", t.Host, "port", port)
	res, err := c.RunLive(ctx, cmd, stdin, onLine)
	if err != nil {
		return Host{}, err
	}
	if res.ExitCode != 0 {
		return Host{}, fmt.Errorf("setting TLS up failed (exit status %d); the output above says why", res.ExitCode)
	}

	// Docker takes a moment to listen; Save connects with the new client
	// certificate before saving anything.
	h.CA, h.Cert = cs.CA, cs.ClientCert
	save := Input{Host: h, Key: cs.ClientKey}
	onLine("Connecting to " + h.URL + " with hlm's client certificate")
	var saved Host
	for attempt := 0; ; attempt++ {
		saved, err = r.Save(ctx, save, create)
		if err == nil || attempt == 5 || ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Host{}, fmt.Errorf("Docker restarted with TLS, but hlm can't connect to %s (is port %d open in the host's firewall?): %w", h.URL, port, err)
	}
	return saved, nil
}
