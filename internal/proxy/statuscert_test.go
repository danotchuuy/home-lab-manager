package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRenderStatusAddressHTTPS(t *testing.T) {
	cert := addressCertFiles("play.example.com")
	c := RenderStatus([]string{"friends.example.com"}, nil, "10.77.77.2", 8080, false,
		&StatusAddress{Port: 80, Names: []string{"play.example.com", "203.0.113.7"}, Name: "play.example.com", TLS: &cert})
	own := c[strings.Index(c, "# The proxy server's own address"):]
	for _, want := range []string{
		"listen 80;\n    server_name play.example.com 203.0.113.7;", "return 301 https://play.example.com$request_uri;",
		"listen 443 ssl;\n    server_name play.example.com;", "ssl_certificate /etc/nginx/hlm-certs/play.example.com.crt;",
		"ssl_certificate_key /etc/nginx/hlm-certs/play.example.com.key;", "proxy_set_header X-Hlm-Status-Only 1;",
	} {
		if !strings.Contains(own, want) {
			t.Errorf("HTTPS on 80/443 lacks %q:\n%s", want, own)
		}
	}
	c = RenderStatus(nil, nil, "10.77.77.2", 8080, true, &StatusAddress{Port: 8443, Name: "play.example.com", TLS: &cert})
	for _, want := range []string{"listen 8443 ssl default_server;", "listen [::]:8443 ssl default_server;",
		"error_page 497 =301 https://play.example.com:8443$request_uri;", "ssl_certificate /etc/nginx/hlm-certs/play.example.com.crt;"} {
		if !strings.Contains(c, want) {
			t.Errorf("HTTPS on its own port lacks %q:\n%s", want, c)
		}
	}
	if got := statusAddressHTTPSURL("play.example.com", 80); got != "https://play.example.com/status" {
		t.Error(got)
	}
	if got := statusAddressHTTPSURL("play.example.com", 8443); got != "https://play.example.com:8443/status" {
		t.Error(got)
	}
	if r := statusAddressRule(80, true); r.Ports != "80,443" {
		t.Errorf("rule = %+v", r)
	}
	if r := statusAddressRule(8443, true); r.Ports != "8443" {
		t.Errorf("rule = %+v", r)
	}
}

func TestValidateStatusHTTPS(t *testing.T) {
	st := Settings{Target: remote.Target{Host: "Play.Example.com", User: "root"}, StatusOnAddress: true, StatusAddressHTTPS: true}
	if err := Validate(&st); err != nil || st.StatusAddressName != "play.example.com" || st.StatusPort != 80 {
		t.Errorf("from the host: %+v %v", st, err)
	}
	st = Settings{Target: remote.Target{Host: "203.0.113.7", User: "root"}, StatusOnAddress: true, StatusAddressHTTPS: true}
	if err := Validate(&st); err == nil || !strings.Contains(err.Error(), "domain") {
		t.Errorf("an IP address: %v", err)
	}
	st.StatusAddressName = "Play.Example.com."
	if err := Validate(&st); err != nil || st.StatusAddressName != "play.example.com" {
		t.Errorf("named: %+v %v", st, err)
	}
	st.StatusPort = 443
	if err := Validate(&st); err == nil || !strings.Contains(err.Error(), "choose port 80") {
		t.Errorf("443: %v", err)
	}
	st.StatusPort, st.StatusAddressHTTPS = 80, false
	if Validate(&st); st.StatusAddressName != "" {
		t.Error("a name without HTTPS")
	}
}

// certRig is a proxy with a fake issuer and a fake proxy server that keeps
// the files hlm installs.
type certRig struct {
	s       *Service
	issued  []string
	fail    error
	files   map[string]string // domain → certificate on the proxy
	removed []string
	configs []string
	now     time.Time
}

func newCertRig(t *testing.T) *certRig {
	store, _ := secrets.Open(t.TempDir(), "")
	r := &certRig{s: New(store, fakeServers{}, slog.New(slog.DiscardHandler)), files: map[string]string{}, now: time.Now()}
	r.s.UseStatusSites(func(context.Context) ([]string, int, error) { return nil, 8080, nil })
	r.s.UseCertIssuer(func(_ context.Context, domain, email string) (string, string, time.Time, time.Time, error) {
		if r.fail != nil {
			return "", "", time.Time{}, time.Time{}, r.fail
		}
		r.issued = append(r.issued, domain)
		return "CERT " + domain + " " + string(rune('0'+len(r.issued))), "KEY", r.now.Add(-time.Hour), r.now.Add(90 * 24 * time.Hour), nil
	})
	r.s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		switch {
		case strings.Contains(script, "hlm:confd"):
			return remote.Result{Stdout: "hlm:confd\nhlm:addr:203.0.113.7\n"}, nil
		case strings.Contains(script, StatusConfigPath):
			r.configs = append(r.configs, string(stdin))
			return remote.Result{Stdout: "hlm:applied\n"}, nil
		case strings.Contains(script, "hlm:installed"):
			d := strings.Trim(strings.Fields(script)[0][2:], "'")
			lines := strings.Split(string(stdin), "\n")
			crt, _ := base64.StdEncoding.DecodeString(lines[0])
			if r.files[d] == string(crt) {
				return remote.Result{Stdout: "hlm:current\n"}, nil
			}
			r.files[d] = string(crt)
			return remote.Result{Stdout: "hlm:installed\n"}, nil
		case strings.HasPrefix(script, "rm -f"):
			r.removed = append(r.removed, script)
			return remote.Result{}, nil
		}
		return remote.Result{}, nil
	}
	return r
}

func TestStatusAddressCert(t *testing.T) {
	r := newCertRig(t)
	ctx := context.Background()
	st := Settings{Target: remote.Target{Host: "203.0.113.7", User: "root"}, Upstream: "10.77.77.2", UpstreamSource: "wireguard",
		StatusPage: true, StatusOnAddress: true, StatusPort: 80, StatusAddressHTTPS: true, StatusAddressName: "play.example.com"}
	res := r.s.applyStatus(ctx, st)
	if res.Error != "" || res.AddressError != "" || res.Address != "https://play.example.com/status" {
		t.Fatalf("result = %+v", res)
	}
	if len(r.issued) != 1 || r.files["play.example.com"] != "CERT play.example.com 1" {
		t.Errorf("issued %v, files %v", r.issued, r.files)
	}
	if last := r.configs[len(r.configs)-1]; !strings.Contains(last, "ssl_certificate /etc/nginx/hlm-certs/play.example.com.crt;") ||
		!strings.Contains(last, "server_name 203.0.113.7 play.example.com;") {
		t.Errorf("config:\n%s", last)
	}
	// Applied again: the same certificate, not reinstalled.
	r.s.applyStatus(ctx, st)
	if len(r.issued) != 1 {
		t.Errorf("issued again: %v", r.issued)
	}
	// A third of its life left: renewed.
	var c addressCert
	r.s.secrets.Get(secretAddressCert, &c)
	c.NotBefore, c.NotAfter = time.Now().Add(-70*24*time.Hour), time.Now().Add(20*24*time.Hour)
	r.s.secrets.Set(secretAddressCert, c)
	r.s.applyStatus(ctx, st)
	if len(r.issued) != 2 || r.files["play.example.com"] != "CERT play.example.com 2" {
		t.Errorf("not renewed: %v %v", r.issued, r.files)
	}
	// Renewal failing while the certificate is still good: kept, and not
	// asked again within the hour.
	r.s.secrets.Get(secretAddressCert, &c)
	c.NotBefore, c.NotAfter = time.Now().Add(-70*24*time.Hour), time.Now().Add(20*24*time.Hour)
	r.s.secrets.Set(secretAddressCert, c)
	r.fail = errors.New("rate limited")
	if res := r.s.applyStatus(ctx, st); res.AddressError != "" || !strings.HasPrefix(res.Address, "https://") {
		t.Errorf("good certificate dropped: %+v", res)
	}
	r.fail = nil
	r.s.applyStatus(ctx, st)
	if len(r.issued) != 2 {
		t.Errorf("asked again within the hour: %v", r.issued)
	}
	r.s.RetryCert()
	r.s.applyStatus(ctx, st)
	if len(r.issued) != 3 {
		t.Errorf("not asked after RetryCert: %v", r.issued)
	}

	// Another name: a new certificate, the old one's files removed.
	st.StatusAddressName = "games.example.com"
	r.s.applyStatus(ctx, st)
	if r.issued[len(r.issued)-1] != "games.example.com" || len(r.removed) != 1 || !strings.Contains(r.removed[0], "play.example.com.crt") {
		t.Errorf("issued %v, removed %v", r.issued, r.removed)
	}
	// HTTPS off there: removed.
	st.StatusAddressHTTPS = false
	if res := r.s.applyStatus(ctx, st); res.Address != "http://203.0.113.7/status" || len(r.removed) != 2 {
		t.Errorf("HTTPS off: %+v %v", res, r.removed)
	}
}

func TestStatusAddressCertFails(t *testing.T) {
	r := newCertRig(t)
	r.fail = errors.New("the DNS record didn't show up")
	st := Settings{Target: remote.Target{Host: "203.0.113.7", User: "root"}, Upstream: "10.77.77.2", UpstreamSource: "wireguard",
		StatusPage: true, StatusOnAddress: true, StatusPort: 80, StatusAddressHTTPS: true, StatusAddressName: "play.example.com"}
	res := r.s.applyStatus(context.Background(), st)
	// Served over HTTP meanwhile, saying why.
	if res.Error != "" || res.Address != "http://203.0.113.7/status" || !strings.Contains(res.AddressError, "the DNS record didn't show up") ||
		strings.Contains(r.configs[len(r.configs)-1], "ssl_certificate") {
		t.Errorf("result = %+v", res)
	}
	// A site's domain has its own HTTPS.
	r = newCertRig(t)
	r.s.UseStatusSites(func(context.Context) ([]string, int, error) { return []string{"play.example.com"}, 8080, nil })
	if res := r.s.applyStatus(context.Background(), st); !strings.Contains(res.AddressError, "also a status page site") || len(r.issued) != 0 {
		t.Errorf("site's domain: %+v", res)
	}
}

// "Let's Encrypt, DNS on Cloudflare": each site's domain points straight at
// the proxy (DNS only), with a certificate got through Cloudflare DNS.
func TestStatusSitesLetsEncryptCloudflare(t *testing.T) {
	r := newCertRig(t)
	cf := &fakeCloudflare{records: map[string]string{}, refuse: "tunnel.example.com"}
	r.s.UseCloudflare(cf)
	r.s.UseStatusSites(func(context.Context) ([]string, int, error) {
		return []string{"friends.example.com", "tunnel.example.com"}, 8080, nil
	})
	st := Settings{Target: remote.Target{Host: "203.0.113.7", User: "root"}, Upstream: "10.77.77.2", UpstreamSource: "wireguard",
		StatusPage: true, StatusTLS: TLSLetsEncryptCloudflare}
	res := r.s.applyStatus(context.Background(), st)
	if res.Error != "" || cf.records["friends.example.com"] != "dns-only 203.0.113.7" {
		t.Fatalf("result = %+v, records %v", res, cf.records)
	}
	if len(res.HTTPS) != 1 || res.HTTPS[0] != "friends.example.com" || r.files["friends.example.com"] == "" {
		t.Errorf("https %v, files %v", res.HTTPS, r.files)
	}
	// One whose name has the tunnel's record: said, and served over HTTP.
	if !strings.Contains(res.Failed["tunnel.example.com"], "Cloudflare Tunnel") || len(r.issued) != 1 {
		t.Errorf("failed %v, issued %v", res.Failed, r.issued)
	}
	last := r.configs[len(r.configs)-1]
	if !strings.Contains(last, "ssl_certificate /etc/nginx/hlm-certs/friends.example.com.crt;") ||
		!strings.Contains(last, "return 301 https://$host$request_uri;") {
		t.Errorf("config:\n%s", last)
	}
	// Applied again: the certificate kept, not got again.
	r.s.applyStatus(context.Background(), st)
	if len(r.issued) != 1 {
		t.Errorf("issued again: %v", r.issued)
	}
	// Another HTTPS option: the records and certificates go.
	st.StatusTLS = ""
	r.s.applyStatus(context.Background(), st)
	if _, ok := cf.records["friends.example.com"]; ok || len(r.s.siteCerts()) != 0 {
		t.Errorf("left behind: records %v, certs %v", cf.records, r.s.siteCerts())
	}
}
