package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// HTTPS for the status page on the proxy server's own address. The name
// players use for the game servers points straight at the proxy server
// (Cloudflare's proxy can't carry game traffic), so the certificate has to
// be one browsers trust: hlm gets it from Let's Encrypt itself, proving the
// domain with a TXT record through its Cloudflare connection (the token
// stays with hlm), installs it and its key on the proxy server, readable by
// root alone, and renews it.

// CertIssuer gets a certificate for a domain: its chain and key (PEM), and
// when it's valid.
type CertIssuer func(ctx context.Context, domain, email string) (certPEM, keyPEM string, notBefore, notAfter time.Time, err error)

// UseCertIssuer sets how certificates for the status page on the proxy's
// address are got.
func (s *Service) UseCertIssuer(f CertIssuer) { s.issueCert = f }

// certDir holds the certificates hlm gets for the proxy server.
const certDir = "/etc/nginx/hlm-certs"

// secretAddressCert keeps the certificate (and key) for the proxy's address,
// so it's renewed only when due and can be put back on the proxy.
const secretAddressCert = "proxy-status-address-cert"

// addressCert is the saved certificate.
type addressCert struct {
	Domain    string    `json:"domain"`
	CertPEM   string    `json:"cert"`
	KeyPEM    string    `json:"key"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	// LastError and LastTry are the last failed attempt to get one, so
	// renewals don't keep asking Let's Encrypt.
	LastError string    `json:"lastError,omitempty"`
	LastTry   time.Time `json:"lastTry,omitzero"`
}

// due says whether a certificate should be got (again): none for the name,
// or a third of its life left.
func (c addressCert) due(domain string, now time.Time) bool {
	if c.Domain != domain || c.CertPEM == "" {
		return true
	}
	life := c.NotAfter.Sub(c.NotBefore)
	if life <= 0 {
		life = 90 * 24 * time.Hour
	}
	return c.NotAfter.Sub(now) < life/3
}

func (c addressCert) hash() string {
	sum := sha256.Sum256([]byte(c.CertPEM))
	return hex.EncodeToString(sum[:])
}

// addressCertFiles is where a domain's certificate and key go on the proxy.
func addressCertFiles(domain string) StatusCert {
	return StatusCert{Cert: certDir + "/" + domain + ".crt", Key: certDir + "/" + domain + ".key", Redirect: true}
}

// certInstallScript installs a certificate and key, read from stdin as one
// base64 line each, readable by root alone, unless the certificate there
// already has the hash $h; then reloads nginx if it's running and its
// configuration passes.
const certInstallScript = `PATH="$PATH:/usr/sbin:/sbin"
set -e
dir=` + certDir + `
mkdir -p "$dir"; chmod 700 "$dir"
if [ -f "$dir/$d.crt" ] && [ -f "$dir/$d.key" ] && [ "$(sha256sum "$dir/$d.crt" | cut -d' ' -f1)" = "$h" ]; then
  echo "hlm:current"; exit 0
fi
umask 077
IFS= read -r crt; IFS= read -r key
printf '%s' "$crt" | base64 -d > "$dir/$d.crt.new"
printf '%s' "$key" | base64 -d > "$dir/$d.key.new"
mv "$dir/$d.crt.new" "$dir/$d.crt"; mv "$dir/$d.key.new" "$dir/$d.key"
echo "hlm:installed"
if nginx -t >/dev/null 2>&1; then
  if [ -d /run/systemd/system ] && systemctl is-active --quiet nginx; then systemctl reload nginx || true
  else nginx -s reload 2>/dev/null || true; fi
fi`

// ensureAddressCert has a current certificate for the proxy's address on
// the proxy server, getting one first if it's due (but not within an hour
// of a failure; see RetryCert).
func (s *Service) ensureAddressCert(ctx context.Context, st Settings) (StatusCert, error) {
	domain := st.StatusAddressName
	var c addressCert
	if _, err := s.secrets.Get(secretAddressCert, &c); err != nil {
		return StatusCert{}, err
	}
	old := c.Domain
	c, err := s.currentCert(ctx, c, domain, st.StatusEmail, func(c addressCert) error { return s.secrets.Set(secretAddressCert, c) })
	if err != nil {
		return StatusCert{}, err
	}
	if old != "" && old != domain {
		s.removeCertFiles(ctx, old) // the old name's files
	}
	return s.installCert(ctx, c)
}

// currentCert brings a saved certificate up to date for domain: got (again)
// if it's due, but not within an hour of a failure (see RetryCert), keeping a
// still good one when renewing fails. save keeps what changed.
func (s *Service) currentCert(ctx context.Context, c addressCert, domain, email string, save func(addressCert) error) (addressCert, error) {
	if s.issueCert == nil {
		return c, errors.New("hlm can't get certificates here")
	}
	now := time.Now()
	if !c.due(domain, now) {
		return c, nil
	}
	usable := c.Domain == domain && c.CertPEM != "" && now.Before(c.NotAfter)
	if c.LastError != "" && now.Sub(c.LastTry) < time.Hour {
		if !usable {
			return c, errors.New(c.LastError)
		}
		return c, nil
	}
	certPEM, keyPEM, nb, na, err := s.issueCert(ctx, domain, email)
	if err != nil {
		c.LastError, c.LastTry = "getting a certificate for "+domain+": "+err.Error(), now
		_ = save(c)
		if !usable {
			return c, errors.New(c.LastError)
		}
		s.log.Warn("renew a status page certificate on the proxy; the current one is still good", "domain", domain, "err", err)
		return c, nil
	}
	c = addressCert{Domain: domain, CertPEM: certPEM, KeyPEM: keyPEM, NotBefore: nb, NotAfter: na}
	if err := save(c); err != nil {
		return c, err
	}
	s.log.Info("got a certificate for the status page on the proxy", "domain", domain, "until", na)
	return c, nil
}

// installCert puts a certificate and its key on the proxy server (unless
// it's there already), reloading nginx.
func (s *Service) installCert(ctx context.Context, c addressCert) (StatusCert, error) {
	stdin := base64.StdEncoding.EncodeToString([]byte(c.CertPEM)) + "\n" + base64.StdEncoding.EncodeToString([]byte(c.KeyPEM)) + "\n"
	res, err := s.run(ctx, "d="+remote.Quote(c.Domain)+" h="+remote.Quote(c.hash())+"\n"+certInstallScript, []byte(stdin), nil)
	switch {
	case err != nil:
		return StatusCert{}, fmt.Errorf("install the certificate on the proxy server: %w", err)
	case !strings.Contains(res.Stdout, "hlm:installed") && !strings.Contains(res.Stdout, "hlm:current"):
		return StatusCert{}, fmt.Errorf("installing the certificate on the proxy server failed (exit status %d): %s", res.ExitCode, lastLine(res.Output()))
	}
	return addressCertFiles(c.Domain), nil
}

// removeCertFiles removes a domain's certificate and key from the proxy.
func (s *Service) removeCertFiles(ctx context.Context, domain string) error {
	script := "rm -f " + remote.Quote(certDir+"/"+domain+".crt") + " " + remote.Quote(certDir+"/"+domain+".key")
	_, err := s.run(ctx, script, nil, nil)
	if err != nil {
		s.log.Warn("remove a status page certificate from the proxy", "domain", domain, "err", err)
	}
	return err
}

// secretSiteCerts keeps the certificates for the sites' domains, by domain,
// with the "letsencrypt-cloudflare" HTTPS option.
const secretSiteCerts = "proxy-status-site-certs"

func (s *Service) siteCerts() map[string]addressCert {
	m := map[string]addressCert{}
	_, _ = s.secrets.Get(secretSiteCerts, &m)
	return m
}

// ensureSiteCert has a current Let's Encrypt certificate for a site's
// domain on the proxy, got through Cloudflare DNS (so the domain's record can
// point anywhere while it's got).
func (s *Service) ensureSiteCert(ctx context.Context, domain, email string) (StatusCert, error) {
	certs := s.siteCerts()
	c, err := s.currentCert(ctx, certs[domain], domain, email, func(c addressCert) error {
		m := s.siteCerts()
		m[domain] = c
		return s.secrets.Set(secretSiteCerts, m)
	})
	if err != nil {
		return StatusCert{}, err
	}
	return s.installCert(ctx, c)
}

// forgetSiteCerts removes the sites' certificates that aren't in keep, from
// the proxy and from hlm.
func (s *Service) forgetSiteCerts(ctx context.Context, keep []string) {
	m := s.siteCerts()
	changed := false
	for d := range m {
		if slices.Contains(keep, d) {
			continue
		}
		if s.removeCertFiles(ctx, d) == nil {
			delete(m, d)
			changed = true
		}
	}
	if !changed {
		return
	}
	if len(m) == 0 {
		_ = s.secrets.Delete(secretSiteCerts)
	} else {
		_ = s.secrets.Set(secretSiteCerts, m)
	}
}

// RetryCert lets the next apply ask for the certificate again straight
// away, after a failure: for an apply by hand.
func (s *Service) RetryCert() {
	var c addressCert
	if ok, _ := s.secrets.Get(secretAddressCert, &c); ok && c.LastError != "" {
		c.LastError, c.LastTry = "", time.Time{}
		_ = s.secrets.Set(secretAddressCert, c)
	}
	m, changed := s.siteCerts(), false
	for d, c := range m {
		if c.LastError != "" {
			c.LastError, c.LastTry = "", time.Time{}
			m[d], changed = c, true
		}
	}
	if changed {
		_ = s.secrets.Set(secretSiteCerts, m)
	}
}

// forgetAddressCert removes the certificate for the proxy's address, once
// it's no longer used (HTTPS turned off there, or another name).
func (s *Service) forgetAddressCert(ctx context.Context, keep string) {
	var c addressCert
	if ok, _ := s.secrets.Get(secretAddressCert, &c); !ok || c.Domain == "" || c.Domain == keep {
		return
	}
	if s.removeCertFiles(ctx, c.Domain) != nil {
		return
	}
	_ = s.secrets.Delete(secretAddressCert)
}

// RunStatusCertRenewal renews the certificate for the status page on the
// proxy's address when it's due, checking every interval until ctx ends.
func (s *Service) RunStatusCertRenewal(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, ok, err := s.Settings()
		if err != nil || !ok || !st.StatusPage {
			continue
		}
		var c addressCert
		_, _ = s.secrets.Get(secretAddressCert, &c)
		addressDue := st.StatusOnAddress && st.StatusAddressHTTPS && c.due(st.StatusAddressName, time.Now())
		var sitesDue []string
		if st.StatusTLS == TLSLetsEncryptCloudflare {
			for d, c := range s.siteCerts() {
				if c.due(d, time.Now()) {
					sitesDue = append(sitesDue, d)
				}
			}
		}
		if !addressDue && len(sitesDue) == 0 {
			continue
		}
		s.applyMu.Lock()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if addressDue {
			if _, err := s.ensureAddressCert(rctx, st); err != nil {
				s.log.Warn("renew the status page's certificate on the proxy", "err", err)
			}
		}
		for _, d := range sitesDue {
			if _, err := s.ensureSiteCert(rctx, d, st.StatusEmail); err != nil {
				s.log.Warn("renew a status page site's certificate on the proxy", "domain", d, "err", err)
			}
		}
		cancel()
		s.applyMu.Unlock()
	}
}
