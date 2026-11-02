package docker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newCert(t *testing.T, name string, parent *testCert, usage x509.ExtKeyUsage) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	signer, signerKey := tmpl, key
	if parent == nil {
		tmpl.IsCA = true
		tmpl.BasicConstraintsValid = true
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{usage}
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCert{cert: cert, key: key, der: der}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startTLSDaemon starts a fake daemon that, like dockerd with --tlsverify,
// only accepts clients presenting a certificate signed by ca.
func startTLSDaemon(t *testing.T, ca, server *testCert) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.45")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Write([]byte("OK"))
			return
		}
		w.Write([]byte(`{"Version":"test","ApiVersion":"1.45"}`))
	}))
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{server.der}, PrivateKey: server.key}},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String()
}

func TestTLSClientCertificates(t *testing.T) {
	ca := newCert(t, "test-ca", nil, 0)
	server := newCert(t, "daemon", ca, x509.ExtKeyUsageServerAuth)
	client := newCert(t, "client", ca, x509.ExtKeyUsageClientAuth)
	host := startTLSDaemon(t, ca, server)

	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, "ca.pem"), "CERTIFICATE", ca.der)
	writePEM(t, filepath.Join(dir, "cert.pem"), "CERTIFICATE", client.der)
	keyDER, err := x509.MarshalECPrivateKey(client.key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, "key.pem"), "EC PRIVATE KEY", keyDER)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewClient(Config{Host: host, TLSVerify: true, CertPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version over TLS: %v", err)
	}
	if v.Version != "test" {
		t.Errorf("Version = %+v", v)
	}

	// Without client certificates the daemon refuses the connection.
	noCert := t.TempDir()
	writePEM(t, filepath.Join(noCert, "ca.pem"), "CERTIFICATE", ca.der)
	c, err = NewClient(Config{Host: host, TLSVerify: true, CertPath: noCert})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err == nil {
		t.Error("Ping without a client certificate succeeded")
	}

	// Plain HTTP to a TLS daemon fails.
	c, err = NewClient(Config{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err == nil {
		t.Error("Ping without TLS succeeded")
	}
}

func TestTLSVerifyRejectsUnknownCA(t *testing.T) {
	ca := newCert(t, "test-ca", nil, 0)
	server := newCert(t, "daemon", ca, x509.ExtKeyUsageServerAuth)
	client := newCert(t, "client", ca, x509.ExtKeyUsageClientAuth)
	host := startTLSDaemon(t, ca, server)

	// The client trusts a different CA, so verification must fail.
	other := newCert(t, "other-ca", nil, 0)
	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, "ca.pem"), "CERTIFICATE", other.der)
	writePEM(t, filepath.Join(dir, "cert.pem"), "CERTIFICATE", client.der)
	keyDER, _ := x509.MarshalECPrivateKey(client.key)
	writePEM(t, filepath.Join(dir, "key.pem"), "EC PRIVATE KEY", keyDER)

	c, err := NewClient(Config{Host: host, TLSVerify: true, CertPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("Ping with untrusted daemon certificate: err = %v, want certificate error", err)
	}

	// Without DOCKER_TLS_VERIFY the certificate isn't checked, like the docker CLI.
	c, err = NewClient(Config{Host: host, CertPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping with verification off: %v", err)
	}
}

func TestNewClientHosts(t *testing.T) {
	tests := []struct {
		cfg     Config
		base    string
		wantErr bool
	}{
		{Config{}, "http://docker", false},
		{Config{Host: "unix:///run/user/1000/docker.sock"}, "http://docker", false},
		{Config{Host: "tcp://10.0.0.5:2375"}, "http://10.0.0.5:2375", false},
		{Config{Host: "tcp://10.0.0.5:2376", CertPath: t.TempDir()}, "https://10.0.0.5:2376", false},
		{Config{Host: "unix:///var/run/docker.sock", TLSVerify: true}, "http://docker", false}, // TLS ignored for sockets
		{Config{Host: "ssh://user@host"}, "", true},
		{Config{Host: "tcp://"}, "", true},
		{Config{Host: "unix://"}, "", true},
	}
	for _, tt := range tests {
		c, err := NewClient(tt.cfg)
		if tt.wantErr {
			if err == nil {
				t.Errorf("NewClient(%+v): want error", tt.cfg)
			}
			continue
		}
		if err != nil {
			t.Errorf("NewClient(%+v): %v", tt.cfg, err)
			continue
		}
		if c.base != tt.base {
			t.Errorf("NewClient(%+v).base = %q, want %q", tt.cfg, c.base, tt.base)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://remote:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("HOME", "/home/test")
	cfg := ConfigFromEnv()
	if cfg.Host != "tcp://remote:2376" || !cfg.TLSVerify || cfg.CertPath != "/home/test/.docker" {
		t.Errorf("ConfigFromEnv() = %+v", cfg)
	}
}
