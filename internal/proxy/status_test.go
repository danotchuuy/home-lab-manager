package proxy

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
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRenderStatus(t *testing.T) {
	if c := RenderStatus(nil, nil, "", 0, false, nil); strings.Contains(c, "server {") {
		t.Errorf("no domains:\n%s", c)
	}
	c := RenderStatus([]string{"friends.example.com", "status.example.com"}, map[string]StatusCert{"status.example.com": letsEncryptCert("status.example.com")}, "10.77.77.2", 8080, true, nil)
	friends := c[strings.Index(c, "# friends.example.com"):strings.Index(c, "# status.example.com")]
	status := c[strings.Index(c, "# status.example.com"):]
	for _, want := range []string{
		"listen 80;", "listen [::]:80;", "server_name friends.example.com;", "root /var/www/hlm-acme;",
		"proxy_pass http://10.77.77.2:8080;", "proxy_set_header Host $host;", "proxy_set_header X-Forwarded-Proto $scheme;",
		"location = / {\n        return 302 /status;", "location / {\n        return 404;",
	} {
		if !strings.Contains(friends, want) {
			t.Errorf("friends (HTTP) lacks %q:\n%s", want, friends)
		}
	}
	if strings.Contains(friends, "443") {
		t.Errorf("HTTPS without a certificate:\n%s", friends)
	}
	for _, want := range []string{
		"return 301 https://$host$request_uri;", "listen 443 ssl;", "listen [::]:443 ssl;",
		"ssl_certificate /etc/letsencrypt/live/status.example.com/fullchain.pem;",
		"ssl_certificate_key /etc/letsencrypt/live/status.example.com/privkey.pem;", "proxy_pass http://10.77.77.2:8080;",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("status (HTTPS) lacks %q:\n%s", want, status)
		}
	}
	if strings.Contains(RenderStatus([]string{"a.example.com"}, nil, "fd00::2", 8080, false, nil), "[::]") ||
		!strings.Contains(RenderStatus([]string{"a.example.com"}, nil, "fd00::2", 8080, false, nil), "proxy_pass http://[fd00::2]:8080;") {
		t.Error("IPv6 upstream or listens")
	}
	// Cloudflare: the page on port 80 as well (no redirect), with the
	// scheme Cloudflare says, and the Origin CA certificate on 443.
	cf := RenderStatus([]string{"cf.example.com"}, map[string]StatusCert{"cf.example.com": originCert("cf.example.com")}, "10.77.77.2", 8080, false, nil)
	http, https := cf[:strings.Index(cf, "listen 443")], cf[strings.Index(cf, "listen 443"):]
	for _, want := range []string{"listen 80;", "proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;", "proxy_pass http://10.77.77.2:8080;"} {
		if !strings.Contains(http, want) {
			t.Errorf("Cloudflare HTTP lacks %q:\n%s", want, cf)
		}
	}
	for _, want := range []string{"ssl_certificate /etc/nginx/hlm-origin/cf.example.com.crt;", "ssl_certificate_key /etc/nginx/hlm-origin/cf.example.com.key;", "X-Forwarded-Proto $scheme;"} {
		if !strings.Contains(https, want) {
			t.Errorf("Cloudflare HTTPS lacks %q:\n%s", want, cf)
		}
	}
	if strings.Contains(cf, "return 301") {
		t.Errorf("Cloudflare redirects:\n%s", cf)
	}
}

func TestApplyStatus(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.UseStatusSites(func(context.Context) ([]string, int, error) {
		return []string{"c.example.com", "a.example.com", "b.example.com", "not a domain", "a.example.com"}, 8080, nil
	})
	var scripts, configs []string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		scripts = append(scripts, script)
		switch {
		case strings.Contains(script, "hlm:confd"):
			if string(stdin) != "a.example.com\nb.example.com\nc.example.com\n" {
				t.Errorf("probe input %q", stdin)
			}
			return remote.Result{Stdout: "hlm:confd\nhlm:cert:a.example.com\n"}, nil
		case strings.Contains(script, StatusConfigPath):
			configs = append(configs, string(stdin))
			return remote.Result{Stdout: "hlm:applied\n"}, nil
		case strings.Contains(script, "certbot certonly"):
			if strings.Contains(script, "'c.example.com'") {
				return remote.Result{Stdout: "Challenge failed for domain c.example.com\nSome challenges have failed.", ExitCode: 1}, nil
			}
			if !strings.Contains(script, "'ops@example.com'") {
				t.Errorf("certbot without the email: %s", script)
			}
			return remote.Result{}, nil
		}
		t.Errorf("unexpected script:\n%s", script)
		return remote.Result{}, nil
	}
	ctx := context.Background()
	st := Settings{Upstream: "10.77.77.2", UpstreamSource: "wireguard", StatusPage: true, StatusTLS: TLSLetsEncrypt, StatusEmail: "ops@example.com"}
	res := s.applyStatus(ctx, st)
	if res.Error != "" || !slices.Equal(res.Domains, []string{"a.example.com", "b.example.com", "c.example.com"}) ||
		!slices.Equal(res.HTTPS, []string{"a.example.com", "b.example.com"}) || !strings.Contains(res.Failed["c.example.com"], "Some challenges have failed") {
		t.Fatalf("result = %+v", res)
	}
	// HTTP for all first (so certbot's challenges are answered), then HTTPS
	// for the ones with certificates.
	if len(configs) != 2 || strings.Contains(configs[0], "live/b.example.com") || !strings.Contains(configs[1], "live/b.example.com") ||
		strings.Contains(configs[1], "live/c.example.com") || !strings.Contains(configs[0], "live/a.example.com") {
		t.Errorf("configs:\n%s\n---\n%s", configs[0], configs[len(configs)-1])
	}

	// Only through the WireGuard tunnel: never by forwarding hlm's port.
	scripts = nil
	if res := s.applyStatus(ctx, Settings{Upstream: "203.0.113.7", UpstreamSource: "manual", StatusPage: true}); !strings.Contains(res.Error, "WireGuard") || len(scripts) != 0 {
		t.Errorf("not through the tunnel: %+v, %d scripts", res, len(scripts))
	}
	// Turned off: removed once, then left alone.
	configs = nil
	if res := s.applyStatus(ctx, Settings{}); res != nil || len(configs) != 1 || strings.Contains(configs[0], "server {") {
		t.Errorf("turned off: %+v %q", res, configs)
	}
	configs = nil
	s.applyStatus(ctx, Settings{})
	if len(configs) != 0 {
		t.Error("removed again")
	}
}

func TestStatusFirewallRule(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.Save(Settings{Target: remote.Target{Host: "vps.example.com", User: "root"}, Upstream: "203.0.113.7", StatusPage: true})
	has := func(rules []string) bool { return slices.Contains(rules, statusRuleLabel) }
	labels := func(servers []Proxied) []string {
		var out []string
		for _, r := range s.wantedRules(servers) {
			out = append(out, r.Label)
		}
		return out
	}
	if !has(labels([]Proxied{})) || has(labels(nil)) {
		t.Error("the status rule follows the applied relays")
	}
	if r := statusRule(nil); r.Ports != "80,443" || r.Protocol != "TCP" || !slices.Equal(r.Addresses.IPv4, []string{"0.0.0.0/0"}) {
		t.Errorf("rule = %+v", r)
	}
	// Behind Cloudflare: only Cloudflare's addresses, once they're known.
	st, _, _ := s.Settings()
	st.StatusTLS = TLSCloudflare
	s.Save(st)
	rule := func() linode.Rule {
		for _, r := range s.wantedRules([]Proxied{}) {
			if r.Label == statusRuleLabel {
				return r
			}
		}
		t.Fatal("no status rule")
		return linode.Rule{}
	}
	if r := rule(); !slices.Equal(r.Addresses.IPv6, []string{"::/0"}) {
		t.Errorf("before Cloudflare's addresses are known: %+v", r.Addresses)
	}
	store.Set(secretCloudflareIPs, linode.Addresses{IPv4: []string{"173.245.48.0/20"}, IPv6: []string{"2400:cb00::/32"}})
	if r := rule(); !slices.Equal(r.Addresses.IPv4, []string{"173.245.48.0/20"}) || !slices.Equal(r.Addresses.IPv6, []string{"2400:cb00::/32"}) {
		t.Errorf("Cloudflare's addresses: %+v", r.Addresses)
	}
}

// fakeCloudflare records what the status page's sites asked of Cloudflare.
type fakeCloudflare struct {
	records map[string]string // hostname → target
	certs   []string
	mode    string
	refuse  string // a hostname with someone else's record
}

func (f *fakeCloudflare) ProxyRecord(_ context.Context, hostname, target string) error {
	if hostname == f.refuse {
		return errors.New(hostname + " already has a DNS record (A 192.0.2.1) that hlm didn't make")
	}
	f.records[hostname] = target
	return nil
}

func (f *fakeCloudflare) RemoveProxyRecord(_ context.Context, hostname string) error {
	delete(f.records, hostname)
	return nil
}

func (f *fakeCloudflare) OriginCertificate(_ context.Context, hostname, csr string) (string, time.Time, error) {
	b, _ := pem.Decode([]byte(csr))
	req, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || req.CheckSignature() != nil || !slices.Equal(req.DNSNames, []string{hostname}) {
		return "", time.Time{}, fmt.Errorf("bad CSR: %v", err)
	}
	f.certs = append(f.certs, hostname)
	return "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n", time.Now().AddDate(15, 0, 0), nil
}

func (f *fakeCloudflare) SSLMode(context.Context, string) (string, error) { return f.mode, nil }

func (f *fakeCloudflare) CloudflareIPs(context.Context) ([]string, []string, error) {
	return []string{"173.245.48.0/20"}, []string{"2400:cb00::/32"}, nil
}

func TestApplyStatusCloudflare(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	domains := []string{"a.example.com", "b.example.com", "taken.example.com"}
	s.UseStatusSites(func(context.Context) ([]string, int, error) { return domains, 8080, nil })
	cf := &fakeCloudflare{records: map[string]string{}, mode: "flexible", refuse: "taken.example.com"}
	s.UseCloudflare(cf)
	var configs, installs, removed []string
	certbot := 0
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		switch {
		case strings.Contains(script, "hlm:confd"):
			return remote.Result{Stdout: "hlm:confd\nhlm:origin:b.example.com\n"}, nil
		case strings.Contains(script, StatusConfigPath):
			configs = append(configs, string(stdin))
			return remote.Result{Stdout: "hlm:applied\n"}, nil
		case strings.Contains(script, "hlm:installed"):
			lines := strings.Split(strings.TrimSpace(string(stdin)), "\n")
			key, _ := base64.StdEncoding.DecodeString(lines[1])
			if len(lines) != 2 || !strings.Contains(string(key), "PRIVATE KEY") || !strings.Contains(script, "umask 077") {
				t.Errorf("install input %q", stdin)
			}
			installs = append(installs, strings.SplitN(script, "\n", 2)[0])
			return remote.Result{Stdout: "hlm:installed\n"}, nil
		case strings.HasPrefix(script, "rm -f"):
			removed = append(removed, script)
			return remote.Result{}, nil
		case strings.Contains(script, "certbot certonly"):
			certbot++
		}
		return remote.Result{}, nil
	}
	ctx := context.Background()
	st := Settings{Target: remote.Target{Host: "203.0.113.9"}, Upstream: "10.77.77.2", UpstreamSource: "wireguard", StatusPage: true, StatusTLS: TLSCloudflare}
	res := s.applyStatus(ctx, st)
	if res.Error != "" || res.TLS != TLSCloudflare || !slices.Equal(res.HTTPS, []string{"a.example.com", "b.example.com"}) ||
		!strings.Contains(res.Failed["taken.example.com"], "hlm didn't make") || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "Full (strict)") {
		t.Fatalf("result = %+v", res)
	}
	if cf.records["a.example.com"] != "203.0.113.9" || cf.records["b.example.com"] != "203.0.113.9" || len(cf.records) != 2 {
		t.Errorf("records = %v", cf.records)
	}
	// A certificate for the domain without one; b's is already there.
	if !slices.Equal(cf.certs, []string{"a.example.com"}) || !slices.Equal(installs, []string{"d='a.example.com'"}) {
		t.Errorf("certs %v, installs %v", cf.certs, installs)
	}
	if len(configs) != 1 || !strings.Contains(configs[0], "hlm-origin/a.example.com.crt") || strings.Contains(configs[0], "hlm-origin/taken.example.com") {
		t.Errorf("config:\n%s", configs[0])
	}
	if certbot != 0 {
		t.Error("certbot behind Cloudflare")
	}
	var ips linode.Addresses
	if ok, _ := store.Get(secretCloudflareIPs, &ips); !ok || ips.IPv4[0] != "173.245.48.0/20" {
		t.Errorf("Cloudflare's addresses: %+v", ips)
	}

	// A domain leaves: its record and certificate go.
	domains = []string{"a.example.com"}
	s.applyStatus(ctx, st)
	if _, ok := cf.records["b.example.com"]; ok || len(removed) != 1 || !strings.Contains(removed[0], "b.example.com.key") {
		t.Errorf("records %v, removed %q", cf.records, removed)
	}
	// Let's Encrypt instead: Cloudflare's records go.
	st.StatusTLS = TLSLetsEncrypt
	s.applyStatus(ctx, st)
	if len(cf.records) != 0 {
		t.Errorf("records left: %v", cf.records)
	}
	if ok, _ := store.Get(secretCloudflareIPs, &ips); ok {
		t.Error("Cloudflare's addresses kept")
	}

	// Removed altogether.
	st.StatusTLS = TLSCloudflare
	s.applyStatus(ctx, st)
	if len(cf.records) != 1 {
		t.Fatalf("records = %v", cf.records)
	}
	s.applyStatus(ctx, Settings{})
	if len(cf.records) != 0 {
		t.Errorf("turned off, records left: %v", cf.records)
	}
}

// TestDockerStatusConfig checks real nginx accepts the configuration, with
// and without certificates (opt-in: HLM_DOCKER_TESTS=1).
func TestDockerStatusConfig(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to check the configuration with nginx")
	}
	dir := t.TempDir()
	live := filepath.Join(dir, "live", "status.example.com")
	os.MkdirAll(live, 0o755)
	os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "status.example.com"},
		DNSNames: []string{"status.example.com"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "status.example.com"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}, &k.PublicKey, k)
	kder, _ := x509.MarshalECPrivateKey(k)
	os.WriteFile(filepath.Join(live, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(live, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o644)
	origin := filepath.Join(dir, "origin")
	os.MkdirAll(origin, 0o700)
	os.WriteFile(filepath.Join(origin, "cf.example.com.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(origin, "cf.example.com.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	// The proxy's own address too: on port 80 beside the domains (and the
	// image's default site), and on a port of its own.
	// HTTPS there too, with a certificate in hlm-certs.
	certs := filepath.Join(dir, "certs")
	os.MkdirAll(certs, 0o700)
	os.WriteFile(filepath.Join(certs, "play.example.com.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(certs, "play.example.com.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	tlsFiles := addressCertFiles("play.example.com")
	for _, addr := range []*StatusAddress{nil, {Port: 80, Names: []string{"203.0.113.7", "[2001:db8::7]", "play.example.com"}}, {Port: 8081},
		{Port: 80, Names: []string{"203.0.113.7", "play.example.com"}, Name: "play.example.com", TLS: &tlsFiles},
		{Port: 8443, Name: "play.example.com", TLS: &tlsFiles}, {Port: 80}} {
		conf := RenderStatus([]string{"cf.example.com", "friends.example.com", "status.example.com"}, map[string]StatusCert{
			"status.example.com": letsEncryptCert("status.example.com"), "cf.example.com": originCert("cf.example.com"),
		}, "10.77.77.2", 8080, true, addr)
		os.WriteFile(filepath.Join(dir, "conf.d", "hlm-status.conf"), []byte(conf), 0o644)
		out, err := exec.Command("docker", "run", "--rm", "-v", filepath.Join(dir, "conf.d", "hlm-status.conf")+":/etc/nginx/conf.d/hlm-status.conf:ro",
			"-v", filepath.Join(dir, "live")+":/etc/letsencrypt/live:ro", "-v", origin+":"+originDir+":ro", "-v", certs+":"+certDir+":ro",
			"nginx:alpine", "nginx", "-t").CombinedOutput()
		if err != nil {
			t.Fatalf("nginx -t: %v\n%s\n%s", err, out, conf)
		}
	}
}

func TestStatusOnAddress(t *testing.T) {
	// On port 80, beside the domains: only the proxy's own names.
	c := RenderStatus([]string{"friends.example.com"}, nil, "10.77.77.2", 8080, true,
		&StatusAddress{Port: 80, Names: statusAddressNames("Play.Example.com", []string{"203.0.113.7", "2001:db8::7", "fe80::1", "127.0.0.1", "203.0.113.7"})})
	own := c[strings.Index(c, "# The proxy server's own address"):]
	for _, want := range []string{"listen 80;", "listen [::]:80;", "server_name play.example.com 203.0.113.7 [2001:db8::7];",
		"proxy_pass http://10.77.77.2:8080;", "proxy_set_header X-Hlm-Status-Only 1;", "return 302 /status;", "location / {\n        return 404;"} {
		if !strings.Contains(own, want) {
			t.Errorf("own address lacks %q:\n%s", want, own)
		}
	}
	if strings.Contains(own, "default_server") || strings.Contains(own, "fe80") || strings.Contains(own, "127.0.0.1") {
		t.Errorf("own address on 80:\n%s", own)
	}
	// On a port of its own: anything that comes, and no domains needed.
	c = RenderStatus(nil, nil, "10.77.77.2", 8080, false, &StatusAddress{Port: 8081})
	if !strings.Contains(c, "listen 8081 default_server;") || !strings.Contains(c, "server_name _;") || strings.Contains(c, "[::]") {
		t.Errorf("own port:\n%s", c)
	}
	for host, want := range map[string]string{"203.0.113.7": "http://203.0.113.7:8081/status", "2001:db8::7": "http://[2001:db8::7]:8081/status"} {
		if got := statusAddressURL(host, 8081); got != want {
			t.Errorf("URL(%s) = %s", host, got)
		}
	}
	if got := statusAddressURL("vps.example.com", 80); got != "http://vps.example.com/status" {
		t.Errorf("URL on 80 = %s", got)
	}

	// Settings: 80 by default, never 443 or the SSH port.
	st := Settings{Target: remote.Target{Host: "203.0.113.7", User: "root"}, StatusOnAddress: true}
	if err := Validate(&st); err != nil || st.StatusPort != 80 {
		t.Errorf("default port: %d %v", st.StatusPort, err)
	}
	for _, p := range []int{443, 22, 70000} {
		st.StatusPort = p
		if err := Validate(&st); err == nil {
			t.Errorf("port %d accepted", p)
		}
	}
	st.StatusOnAddress, st.StatusPort = false, 8081
	if Validate(&st); st.StatusPort != 0 {
		t.Error("a port without the option")
	}

	// A relayed game port can't be the status page's too.
	st = Settings{StatusOnAddress: true, StatusPort: 8081}
	if why := statusPortConflict(st, []Proxied{{Server: "web", Ports: []forwarding.Mapping{{Port: 8081, Proto: "udp"}}}}); why != "" {
		t.Errorf("UDP conflicts: %s", why)
	}
	if why := statusPortConflict(st, []Proxied{{Server: "web", Ports: []forwarding.Mapping{{Port: 8081, Proto: "tcp"}}}}); !strings.Contains(why, "web") {
		t.Errorf("conflict = %q", why)
	}

	// Applying: no sites needed, and the address the probe found.
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	s.UseStatusSites(func(context.Context) ([]string, int, error) { return nil, 8080, nil })
	var configs []string
	s.run = func(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
		switch {
		case strings.Contains(script, "hlm:confd"):
			return remote.Result{Stdout: "hlm:confd\nhlm:addr:198.51.100.4\n"}, nil
		case strings.Contains(script, StatusConfigPath):
			configs = append(configs, string(stdin))
			return remote.Result{Stdout: "hlm:applied\n"}, nil
		}
		return remote.Result{}, nil
	}
	set := Settings{Target: remote.Target{Host: "play.example.com", User: "root"}, Upstream: "10.77.77.2", UpstreamSource: "wireguard",
		StatusPage: true, StatusOnAddress: true, StatusPort: 80}
	res := s.applyStatusRecorded(context.Background(), set, []Proxied{{Server: "mc", Ports: []forwarding.Mapping{{Port: 25565, Proto: "tcp"}}}})
	if res.Error != "" || res.Address != "http://play.example.com/status" || len(configs) != 1 ||
		!strings.Contains(configs[0], "server_name play.example.com 198.51.100.4;") {
		t.Errorf("result = %+v\n%s", res, strings.Join(configs, "\n"))
	}
	set.StatusPort = 25565
	if res := s.applyStatusRecorded(context.Background(), set, []Proxied{{Server: "mc", Ports: []forwarding.Mapping{{Port: 25565, Proto: "tcp"}}}}); !strings.Contains(res.Error, "mc") || len(configs) != 1 {
		t.Errorf("conflict applied: %+v", res)
	}

	// The firewall: port 80 is open already; another port gets a rule.
	set.StatusPort = 8081
	if _, err := s.Save(set); err != nil {
		t.Fatal(err)
	}
	labels := func() (out []string) {
		for _, r := range s.wantedRules([]Proxied{}) {
			out = append(out, r.Label+" "+r.Ports)
		}
		return out
	}
	if got := labels(); !slices.Contains(got, statusAddressRuleLabel+" 8081") {
		t.Errorf("rules = %v", got)
	}
	set.StatusPort = 80
	if _, err := s.Save(set); err != nil {
		t.Fatal(err)
	}
	if got := labels(); slices.Contains(got, statusAddressRuleLabel+" 80") {
		t.Errorf("rules on 80 = %v", got)
	}
}

func TestNginxComplaint(t *testing.T) {
	config := "# Managed by Home Lab Manager\nserver {\n    listen 80;\n    server_name ;\n}\n"
	out := "nginx: [emerg] invalid number of arguments in \"server_name\" directive in /etc/nginx/conf.d/hlm-status.conf:4\nnginx: configuration file /etc/nginx/nginx.conf test failed"
	got := nginxComplaint(out, config)
	if !strings.Contains(got, `invalid number of arguments in "server_name" directive`) || !strings.Contains(got, "(hlm's line 4: server_name ;)") {
		t.Errorf("complaint = %q", got)
	}
	// Elsewhere than hlm's file: as nginx says it.
	out = "nginx: [emerg] a duplicate default server for 0.0.0.0:8081 in /etc/nginx/sites-enabled/mine:3"
	if got := nginxComplaint(out, config); got != "a duplicate default server for 0.0.0.0:8081 in /etc/nginx/sites-enabled/mine:3" {
		t.Errorf("complaint = %q", got)
	}
	if got := nginxComplaint("something else", config); got != "" {
		t.Errorf("no emerg: %q", got)
	}
}

func TestStatusAddressNamesEdges(t *testing.T) {
	// A host name without dots is a name too.
	if got := statusAddressNames("vps", nil); len(got) != 1 || got[0] != "vps" {
		t.Errorf("single label: %v", got)
	}
	// A site's domain has its own block.
	if got := withoutSites([]string{"play.example.com", "203.0.113.7"}, []string{"play.example.com"}); len(got) != 1 || got[0] != "203.0.113.7" {
		t.Errorf("without sites: %v", got)
	}
	// Nothing left to answer to on port 80: still valid for nginx.
	c := RenderStatus([]string{"play.example.com"}, nil, "10.77.77.2", 8080, false, &StatusAddress{Port: 80})
	if strings.Contains(c, "server_name ;") || !strings.Contains(c, "server_name _;") {
		t.Errorf("empty names:\n%s", c)
	}
}
