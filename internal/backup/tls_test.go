package backup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// writeTestCA writes server.crt and server.key, for 127.0.0.1 and signed by
// a new CA, into dir, and returns the CA's certificate as PEM.
func writeTestCA(t *testing.T, dir string) string {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Homelab CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "seaweedfs"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	// Readable by SeaweedFS in its container; it's a throwaway test key.
	os.WriteFile(filepath.Join(dir, "server.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o644)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// SeaweedFS over https, with a certificate from a private CA.
func TestSeaweedFSPrivateCA(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	caPEM := writeTestCA(t, dir)
	endpoint := startSeaweed(t, dir, caPEM)
	if !strings.HasPrefix(endpoint, "https://") {
		t.Fatalf("endpoint = %s", endpoint)
	}

	store, _ := secrets.Open(t.TempDir(), "k")
	s := New(store, &fakeServers{files: map[string]string{}}, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	in := Settings{Settings: s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret"}}

	if _, err := s.Save(ctx, in); err == nil || !strings.Contains(err.Error(), "isn't trusted") {
		t.Fatalf("without the CA: err = %v", err)
	}
	in.CACert = caPEM
	pub, err := s.Save(ctx, in)
	if err != nil {
		t.Fatalf("with the CA: %v", err)
	}
	if len(pub.CACerts) != 1 || pub.CACerts[0].Subject != "CN=Homelab CA" || pub.CACert != caPEM {
		t.Errorf("public settings = %+v", pub)
	}
	// Saved settings keep the CA: a configuration backup goes up over https.
	s.UseConfig(ConfigSource{DataDir: t.TempDir()})
	if err := s.StartConfig(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		o, err := s.ConfigOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if o.Running == nil {
			if o.Plan.Last == nil || o.Plan.Last.Error != "" || len(o.Backups) != 1 {
				t.Fatalf("over https: last = %+v, backups = %v, error = %q", o.Plan.Last, o.Backups, o.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("didn't finish")
		}
	}
}
