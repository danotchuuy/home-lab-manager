package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeDNS struct{ connected bool }

func (f fakeDNS) Connected() bool { return f.connected }
func (f fakeDNS) SetTXT(context.Context, string, string) (func(context.Context) error, error) {
	return func(context.Context) error { return nil }, nil
}

// selfSigned makes a certificate for domain that's valid until notAfter.
func selfSigned(t *testing.T, domain string, notAfter time.Time) (stored, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return stored{
		Domain: domain, CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})), NotAfter: notAfter,
	}, pool
}

func newService(t *testing.T, set Settings, cert *stored, o Options) *Service {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(secretSettings, set); err != nil {
		t.Fatal(err)
	}
	if cert != nil {
		if err := store.Set(secretCert, *cert); err != nil {
			t.Fatal(err)
		}
	}
	o.Secrets, o.Log = store, slog.New(slog.DiscardHandler)
	if o.DNS == nil {
		o.DNS = fakeDNS{true}
	}
	return New(o)
}

// serve runs the service's listener with a handler that says whether the
// request came over TLS and sets a cookie the way hlm's sign-in does.
func serve(t *testing.T, s *Service) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "hlm_session", Value: "x", HttpOnly: true, Secure: r.TLS != nil})
		if r.TLS != nil {
			io.WriteString(w, "tls")
		} else {
			io.WriteString(w, "plain")
		}
	})
	srv := &http.Server{Handler: s.Redirect(h)}
	go srv.Serve(s.Listener(ln))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func client(pool *x509.CertPool, domain, addr string) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: domain},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string, error) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b), nil
}

func TestHTTPSAndHTTPOnOnePort(t *testing.T) {
	const domain = "hlm.home.example.com"
	cert, pool := selfSigned(t, domain, time.Now().Add(60*24*time.Hour))
	s := newService(t, Settings{Enabled: true, Domain: domain}, &cert, Options{})
	addr := serve(t, s)
	_, port, _ := net.SplitHostPort(addr)
	c := client(pool, domain, addr)

	res, body, err := get(t, c, "https://"+domain+":"+port+"/api/x?y=1")
	if err != nil || body != "tls" {
		t.Fatalf("https = %q, %v", body, err)
	}
	if ck := res.Header.Get("Set-Cookie"); !strings.Contains(ck, "Secure") {
		t.Errorf("cookie over HTTPS isn't Secure: %s", ck)
	}
	// Plain HTTP is sent to HTTPS on the same port, keeping the path.
	res, _, err = get(t, c, "http://192.168.11.101:"+port+"/api/x?y=1")
	if err != nil || res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != "https://"+domain+":"+port+"/api/x?y=1" {
		t.Fatalf("http = %v %v", res, err)
	}
	// A proxy that took HTTPS itself isn't redirected.
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if res, err := c.Do(req); err != nil || res.StatusCode != 200 {
		t.Errorf("behind an HTTPS proxy = %v %v", res, err)
	}
}

func TestPlainUntilThereIsACertificate(t *testing.T) {
	const domain = "hlm.home.example.com"
	expired, pool := selfSigned(t, domain, time.Now().Add(-time.Minute))
	other, _ := selfSigned(t, "old.example.com", time.Now().Add(60*24*time.Hour))
	good, _ := selfSigned(t, domain, time.Now().Add(60*24*time.Hour))
	for name, try := range map[string]struct {
		set  Settings
		cert *stored
		opts Options
	}{
		"off":                {Settings{Domain: domain}, &good, Options{}},
		"no certificate yet": {Settings{Enabled: true, Domain: domain}, nil, Options{}},
		"expired":            {Settings{Enabled: true, Domain: domain}, &expired, Options{}},
		"for another name":   {Settings{Enabled: true, Domain: domain}, &other, Options{}},
		"HLM_TLS=off":        {Settings{Enabled: true, Domain: domain}, &good, Options{Off: true}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(t, try.set, try.cert, try.opts)
			addr := serve(t, s)
			res, body, err := get(t, client(pool, domain, addr), "http://"+addr+"/")
			if err != nil || body != "plain" || res.StatusCode != 200 {
				t.Fatalf("http = %v %q %v", res, body, err)
			}
			if ck := res.Header.Get("Set-Cookie"); strings.Contains(ck, "Secure") {
				t.Errorf("cookie over plain HTTP is Secure, so browsers wouldn't send it back: %s", ck)
			}
		})
	}
	// Turned off, or with HLM_TLS=off, HTTPS isn't served at all.
	for name, s := range map[string]*Service{
		"HLM_TLS=off": newService(t, Settings{Enabled: true, Domain: domain}, &good, Options{Off: true}),
		"turned off":  newService(t, Settings{Domain: domain}, &good, Options{}),
	} {
		addr := serve(t, s)
		if _, _, err := get(t, client(pool, domain, addr), "https://"+domain+"/"); err == nil {
			t.Errorf("%s: HTTPS answered", name)
		}
	}
}

// A connection that sends nothing doesn't hold up the others.
func TestSlowClientDoesntBlock(t *testing.T) {
	s := newService(t, Settings{}, nil, Options{})
	addr := serve(t, s)
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	done := make(chan error, 1)
	go func() {
		_, _, err := get(t, &http.Client{}, "http://"+addr+"/")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a silent connection held up another")
	}
}

func TestSaveChecks(t *testing.T) {
	s := newService(t, Settings{}, nil, Options{})
	for _, bad := range []Settings{
		{Enabled: true, Domain: "192.168.11.101"},
		{Enabled: true, Domain: "*.example.com"},
		{Enabled: true, Domain: "localhost"},
		{Enabled: true, Domain: "hlm.example.com", Email: "nope"},
	} {
		if _, err := s.Save(bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	st, err := s.Save(Settings{Enabled: true, Domain: " HLM.Example.com. "})
	if err != nil || st.Domain != "hlm.example.com" {
		t.Errorf("save = %+v, %v", st, err)
	}
	// Turning it off needs nothing else.
	if _, err := s.Save(Settings{}); err != nil {
		t.Error(err)
	}
	noCF := newService(t, Settings{}, nil, Options{DNS: fakeDNS{false}})
	if _, err := noCF.Save(Settings{Enabled: true, Domain: "hlm.example.com"}); err == nil || !strings.Contains(err.Error(), "Cloudflare") {
		t.Errorf("without Cloudflare: %v", err)
	}
}

func TestDirectory(t *testing.T) {
	s := newService(t, Settings{}, nil, Options{})
	if s.directory(Settings{}) != LetsEncrypt || s.directory(Settings{Staging: true}) != LetsEncryptStaging {
		t.Error("wrong Let's Encrypt directory")
	}
	s.opts.Directory = "https://ca.internal/acme/directory"
	if s.directory(Settings{Staging: true}) != s.opts.Directory {
		t.Error("HLM_ACME_DIRECTORY isn't used")
	}
}

func TestRenewAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for life, want := range map[time.Duration]time.Duration{
		90 * 24 * time.Hour: 60 * 24 * time.Hour,
		6 * 24 * time.Hour:  4 * 24 * time.Hour,
		45 * 24 * time.Hour: 30 * 24 * time.Hour,
	} {
		if got := renewAt(stored{NotBefore: now, NotAfter: now.Add(life)}); !got.Equal(now.Add(want)) {
			t.Errorf("%v certificate: renew at %v, want %v", life, got, now.Add(want))
		}
	}
	// Certificates saved before NotBefore was kept use when they were issued.
	if got := renewAt(stored{Issued: now, NotAfter: now.Add(90 * 24 * time.Hour)}); !got.Equal(now.Add(60 * 24 * time.Hour)) {
		t.Errorf("without NotBefore: %v", got)
	}
}
