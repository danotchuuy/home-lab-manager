package proxy

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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRenderStatus(t *testing.T) {
	if c := RenderStatus(nil, nil, "", 0, false); strings.Contains(c, "server {") {
		t.Errorf("no domains:\n%s", c)
	}
	c := RenderStatus([]string{"friends.example.com", "status.example.com"}, map[string]bool{"status.example.com": true}, "10.77.77.2", 8080, true)
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
	if strings.Contains(RenderStatus([]string{"a.example.com"}, nil, "fd00::2", 8080, false), "[::]") ||
		!strings.Contains(RenderStatus([]string{"a.example.com"}, nil, "fd00::2", 8080, false), "proxy_pass http://[fd00::2]:8080;") {
		t.Error("IPv6 upstream or listens")
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
	st := Settings{Upstream: "10.77.77.2", UpstreamSource: "wireguard", StatusPage: true, StatusHTTPS: true, StatusEmail: "ops@example.com"}
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
	if r := statusRule(); r.Ports != "80,443" || r.Protocol != "TCP" {
		t.Errorf("rule = %+v", r)
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
	conf := RenderStatus([]string{"friends.example.com", "status.example.com"}, map[string]bool{"status.example.com": true}, "10.77.77.2", 8080, true)
	os.WriteFile(filepath.Join(dir, "conf.d", "hlm-status.conf"), []byte(conf), 0o644)
	out, err := exec.Command("docker", "run", "--rm", "-v", filepath.Join(dir, "conf.d")+":/etc/nginx/conf.d:ro",
		"-v", filepath.Join(dir, "live")+":/etc/letsencrypt/live:ro", "nginx:alpine", "nginx", "-t").CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -t: %v\n%s\n%s", err, out, conf)
	}
}
