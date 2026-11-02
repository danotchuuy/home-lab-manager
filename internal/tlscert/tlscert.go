// Package tlscert serves hlm over HTTPS with a certificate from Let's Encrypt
// (or another ACME CA), proved with a DNS record made through Cloudflare. It's
// optional: until it's turned on and has a certificate, hlm stays on plain
// HTTP, and cookies aren't marked Secure because browsers wouldn't send them
// back over HTTP.
//
// HTTPS is served on the same port as HTTP: each connection is looked at, and
// one that starts a TLS handshake gets TLS. So it can be turned on and off
// without a restart, and hlm stays reachable while a certificate is pending.
package tlscert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretSettings = "https"
	secretCert     = "https-cert"
	secretAccounts = "https-acme"

	// LetsEncrypt and LetsEncryptStaging are the ACME directories. Staging's
	// certificates aren't trusted by browsers, but its limits are much higher,
	// for trying things out.
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	// A certificate is renewed when a third of its life is left, as Let's
	// Encrypt suggests: 30 days of a 90-day one, 2 of a 6-day one.
	renewFraction = 3
	checkEvery    = 12 * time.Hour
	retryAfter    = time.Hour
)

// Settings are what's saved.
type Settings struct {
	Enabled bool   `json:"enabled"`
	Domain  string `json:"domain"`
	Email   string `json:"email,omitempty"`
	Staging bool   `json:"staging,omitempty"`
}

// DNS makes the TXT record a DNS-01 challenge asks for, and returns how to
// remove it.
type DNS interface {
	SetTXT(ctx context.Context, name, value string) (remove func(context.Context) error, err error)
	// Connected reports whether there's an account to make records with.
	Connected() bool
}

// Options configure a Service.
type Options struct {
	Secrets *secrets.Store
	DNS     DNS
	Log     *slog.Logger
	// Off ignores the settings and serves plain HTTP (HLM_TLS=off), to get
	// back in if HTTPS goes wrong.
	Off bool
	// Directory is the ACME directory, overriding Let's Encrypt (and the
	// staging setting): for another ACME CA, like step-ca.
	Directory string
	// HTTPClient talks to the ACME CA.
	HTTPClient *http.Client
	// Resolver is the DNS server (host:port) asked whether the challenge
	// record can be seen yet.
	Resolver string
	Now      func() time.Time
}

// stored is the saved certificate.
type stored struct {
	Domain    string    `json:"domain"`
	CertPEM   string    `json:"cert"`
	KeyPEM    string    `json:"key"`
	NotAfter  time.Time `json:"notAfter"`
	NotBefore time.Time `json:"notBefore,omitzero"`
	Issued    time.Time `json:"issued"`
	Issuer    string    `json:"issuer"`
	Staging   bool      `json:"staging,omitempty"`
}

// Status is what the settings page shows.
type Status struct {
	Settings
	// Active means HTTPS is being served and plain HTTP sent to it.
	Active bool `json:"active"`
	// State is off, waiting (for the Cloudflare connection), issuing,
	// active or error.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Off is set when HLM_TLS=off overrides the settings.
	Off        bool      `json:"off,omitempty"`
	Cloudflare bool      `json:"cloudflare"`
	NotAfter   time.Time `json:"notAfter,omitzero"`
	Issued     time.Time `json:"issued,omitzero"`
	Issuer     string    `json:"issuer,omitempty"`
	CertFor    string    `json:"certFor,omitempty"`
}

// Service gets and renews the certificate and serves it.
type Service struct {
	opts Options
	wake chan struct{}

	mu      sync.Mutex
	cert    *tls.Certificate
	info    stored
	state   string
	lastErr string
}

// New makes a Service and loads the saved certificate, if any.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Resolver == "" {
		o.Resolver = "1.1.1.1:53"
	}
	s := &Service{opts: o, wake: make(chan struct{}, 1), state: "off"}
	var st stored
	if ok, err := o.Secrets.Get(secretCert, &st); err != nil {
		o.Log.Error("read the HTTPS certificate", "err", err)
	} else if ok {
		if err := s.use(st); err != nil {
			o.Log.Warn("saved HTTPS certificate can't be used", "err", err)
		}
	}
	return s
}

func (s *Service) use(st stored) error {
	c, err := tls.X509KeyPair([]byte(st.CertPEM), []byte(st.KeyPEM))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cert, s.info = &c, st
	s.mu.Unlock()
	return nil
}

// Settings returns what's saved.
func (s *Service) Settings() Settings {
	var st Settings
	if _, err := s.opts.Secrets.Get(secretSettings, &st); err != nil {
		s.opts.Log.Error("read HTTPS settings", "err", err)
	}
	return st
}

var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Save checks and saves settings, and gets a certificate if one's needed.
func (s *Service) Save(in Settings) (Status, error) {
	in.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Domain)), ".")
	in.Email = strings.TrimSpace(in.Email)
	if in.Enabled || in.Domain != "" {
		if !hostname.MatchString(in.Domain) {
			return Status{}, errors.New("give the name hlm is reached at, like hlm.home.example.com: Let's Encrypt doesn't issue certificates for IP addresses")
		}
	}
	if in.Email != "" && !strings.Contains(in.Email, "@") {
		return Status{}, errors.New("that isn't an email address")
	}
	if in.Enabled && s.opts.DNS != nil && !s.opts.DNS.Connected() {
		return Status{}, errors.New("connect Cloudflare first (Tunnel → Cloudflare connection): the certificate is proved with a DNS record made there")
	}
	if err := s.opts.Secrets.Set(secretSettings, in); err != nil {
		return Status{}, err
	}
	s.opts.Log.Info("saved HTTPS settings", "enabled", in.Enabled, "domain", in.Domain, "staging", in.Staging)
	s.Renew()
	return s.Status(), nil
}

// Renew wakes the loop to check the certificate now.
func (s *Service) Renew() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status reports the settings and the certificate.
func (s *Service) Status() Status {
	st := Status{Settings: s.Settings(), Off: s.opts.Off}
	st.Cloudflare = s.opts.DNS != nil && s.opts.DNS.Connected()
	st.Active = s.Active()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.State, st.Error = s.state, s.lastErr
	if s.cert != nil {
		st.NotAfter, st.Issued, st.Issuer, st.CertFor = s.info.NotAfter, s.info.Issued, s.info.Issuer, s.info.Domain
	}
	if st.Active {
		st.State = "active"
	}
	return st
}

// Active reports whether HTTPS is on and has a certificate for its name that
// hasn't expired. Only then is plain HTTP sent to HTTPS: until then, and if
// renewing fails until it expires, hlm stays reachable over HTTP.
func (s *Service) Active() bool {
	if s.opts.Off {
		return false
	}
	set := s.Settings()
	s.mu.Lock()
	defer s.mu.Unlock()
	return set.Enabled && s.cert != nil && s.info.Domain == set.Domain && s.opts.Now().Before(s.info.NotAfter)
}

func (s *Service) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.opts.Off || !s.Settings().Enabled {
		return nil, errors.New("HTTPS is off")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return s.cert, nil
}

// TLSConfig is the configuration HTTPS connections are served with.
func (s *Service) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: s.getCertificate,
		MinVersion:     tls.VersionTLS12,
		// hlm's server isn't set up for HTTP/2 on its own listener.
		NextProtos: []string{"http/1.1"},
	}
}

// Redirect sends plain HTTP requests to HTTPS while it's active. Requests a
// proxy got over HTTPS (X-Forwarded-Proto) are already encrypted on the way
// in, so they're left alone.
func (s *Service) Redirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || !s.Active() {
			next.ServeHTTP(w, r)
			return
		}
		host := s.Settings().Domain
		if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		// Temporary, so browsers don't remember it: HTTPS can be turned off
		// again, and a lapsed certificate falls back to HTTP.
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}

// Run keeps the certificate current until ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		wait := s.maintain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

func (s *Service) setState(state, errText string) {
	s.mu.Lock()
	s.state, s.lastErr = state, errText
	s.mu.Unlock()
}

// maintain gets a certificate if one's needed, and returns how long until
// it's worth looking again.
func (s *Service) maintain(ctx context.Context) time.Duration {
	set := s.Settings()
	if s.opts.Off || !set.Enabled {
		s.setState("off", "")
		return checkEvery
	}
	if s.opts.DNS == nil || !s.opts.DNS.Connected() {
		s.setState("waiting", "Cloudflare isn't connected, so the certificate can't be proved.")
		return retryAfter
	}
	s.mu.Lock()
	current := s.cert != nil && s.info.Domain == set.Domain && s.info.Staging == set.Staging &&
		s.opts.Now().Before(renewAt(s.info))
	s.mu.Unlock()
	if current {
		s.setState("active", "")
		return checkEvery
	}
	s.setState("issuing", "")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	st, err := s.obtain(ctx, set)
	if err != nil {
		s.opts.Log.Warn("couldn't get an HTTPS certificate", "domain", set.Domain, "err", err)
		s.setState("error", err.Error())
		return retryAfter
	}
	if err := s.opts.Secrets.Set(secretCert, st); err != nil {
		s.setState("error", "save the certificate: "+err.Error())
		return retryAfter
	}
	if err := s.use(st); err != nil {
		s.setState("error", err.Error())
		return retryAfter
	}
	s.opts.Log.Info("got an HTTPS certificate", "domain", st.Domain, "issuer", st.Issuer, "expires", st.NotAfter)
	s.setState("active", "")
	return checkEvery
}

// renewAt is when a certificate should be renewed.
func renewAt(st stored) time.Time {
	start := st.NotBefore
	if start.IsZero() {
		start = st.Issued
	}
	life := st.NotAfter.Sub(start)
	if life <= 0 {
		return st.NotAfter
	}
	return st.NotAfter.Add(-life / renewFraction)
}

// leaf reads the first certificate of a PEM chain.
func leaf(chain []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(chain)
	if block == nil {
		return nil, errors.New("no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func issuerName(c *x509.Certificate) string {
	if c.Issuer.CommonName != "" {
		return c.Issuer.CommonName
	}
	return fmt.Sprint(c.Issuer.Organization)
}
