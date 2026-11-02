package s3

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testCA makes a CA and a certificate it signed for 127.0.0.1, returning the
// CA as PEM and the server's certificate.
func testCA(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Homelab CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
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
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return caPEM, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestPrivateCA(t *testing.T) {
	caPEM, cert := testCA(t)
	f := &fakeS3{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}, meta: map[string]string{}}
	srv := httptest.NewUnstartedServer(f)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	ctx := context.Background()
	settings := Settings{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}

	// Without the CA, the server's certificate isn't trusted.
	c, err := New(settings)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.List(ctx, "")
	var certErr *CertError
	if !errors.As(err, &certErr) || !strings.Contains(err.Error(), "isn't trusted") {
		t.Errorf("without the CA: err = %v", err)
	}

	// With it, everything works.
	settings.CACert = caPEM
	if c, err = New(settings); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "k", []byte("v"), ""); err != nil {
		t.Fatalf("with the CA: %v", err)
	}
	if _, err := c.List(ctx, ""); err != nil {
		t.Errorf("with the CA: %v", err)
	}

	// The system's CAs stay trusted alongside it, and the summary describes it.
	info := DescribeCACert(caPEM)
	if len(info) != 1 || info[0].Subject != "CN=Homelab CA" || !info[0].IsCA {
		t.Errorf("DescribeCACert = %+v", info)
	}
}

func TestParseCACert(t *testing.T) {
	caPEM, cert := testCA(t)
	if certs, err := ParseCACert(caPEM + "\n" + caPEM); err != nil || len(certs) != 2 {
		t.Errorf("two certificates: %d, %v", len(certs), err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if _, err := ParseCACert(caPEM + keyPEM); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Errorf("with a private key: err = %v", err)
	}
	for _, bad := range []string{"", "hello", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"} {
		if _, err := ParseCACert(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if _, err := New(Settings{Endpoint: "http://h:8333", Bucket: "b", AccessKey: "a", SecretKey: "s", CACert: caPEM}); err == nil {
		t.Error("a CA was accepted for an http:// endpoint")
	}
}
