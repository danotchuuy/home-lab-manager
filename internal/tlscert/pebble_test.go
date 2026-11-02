package tlscert

// The whole certificate flow against Pebble, Let's Encrypt's test CA, which
// checks the DNS-01 record for real (through its companion DNS server). The
// record goes through hlm's Cloudflare code to a fake Cloudflare, which copies
// it to that DNS server. Needs Docker: HLM_DOCKER_TESTS=1.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/cftunnel"
	"github.com/danotchuuy/home-lab-manager/internal/cloudflare/cloudflaretest"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %s", strings.Join(args, " "), out)
	}
	return strings.TrimSpace(string(out))
}

func hostPort(t *testing.T, container, port string) string {
	t.Helper()
	out := docker(t, "port", container, port)
	return strings.Split(out, "\n")[0] // "127.0.0.1:49153"
}

// challDNS makes records with hlm's Cloudflare code, and copies them to
// Pebble's DNS server, which stands in for the real DNS.
type challDNS struct {
	*cftunnel.Service
	mgmt string
}

func (d challDNS) SetTXT(ctx context.Context, name, value string) (func(context.Context) error, error) {
	remove, err := d.Service.SetTXT(ctx, name, value)
	if err != nil {
		return nil, err
	}
	post := func(path string, body any) error {
		b, _ := json.Marshal(body)
		res, err := http.Post("http://"+d.mgmt+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		res.Body.Close()
		return nil
	}
	if err := post("/set-txt", map[string]string{"host": name + ".", "value": value}); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		post("/clear-txt", map[string]string{"host": name + "."})
		return remove(ctx)
	}, nil
}

func TestLetsEncryptFlowWithPebble(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	name := fmt.Sprintf("hlm-pebble-%d", time.Now().UnixNano())
	docker(t, "network", "create", name)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", name+"-ca", name+"-dns").Run()
		exec.Command("docker", "network", "rm", name).Run()
	})
	docker(t, "run", "-d", "--name", name+"-dns", "--network", name, "--network-alias", "chall",
		"-p", "127.0.0.1::8053/udp", "-p", "127.0.0.1::8053/tcp", "-p", "127.0.0.1::8055",
		"ghcr.io/letsencrypt/pebble-challtestsrv:latest", "-defaultIPv6", "", "-defaultIPv4", "127.0.0.1")
	docker(t, "run", "-d", "--name", name+"-ca", "--network", name,
		"-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_WFE_NONCEREJECT=0",
		"-p", "127.0.0.1::14000", "-p", "127.0.0.1::15000",
		"ghcr.io/letsencrypt/pebble:latest", "-dnsserver", "chall:8053")
	acmeAddr, mgmtAddr := hostPort(t, name+"-ca", "14000"), hostPort(t, name+"-ca", "15000")
	resolver, chall := hostPort(t, name+"-dns", "8053/udp"), hostPort(t, name+"-dns", "8055")

	// Pebble's API has a certificate from its own test CA, for "localhost".
	caPEM := docker(t, "cp", name+"-ca:/test/certs/pebble.minica.pem", "-")
	start := strings.Index(caPEM, "-----BEGIN")
	end := strings.LastIndex(caPEM, "-----END CERTIFICATE-----") + len("-----END CERTIFICATE-----")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM[start:end])) {
		t.Fatal("no Pebble CA certificate")
	}
	_, acmePort, _ := net.SplitHostPort(acmeAddr)
	_, mgmtPort, _ := net.SplitHostPort(mgmtAddr)
	acmeClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	for range 50 { // Pebble takes a moment to start
		if res, err := acmeClient.Get("https://localhost:" + acmePort + "/dir"); err == nil {
			res.Body.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	store, err := secrets.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	fake, cf := cloudflaretest.New()
	t.Cleanup(cf.Close)
	fake.AddZone("z1", "example.com")
	tunnel := cftunnel.New(store, nil, nil, "", log)
	tunnel.UseAPI(cf.URL)
	if err := tunnel.SaveAccount(context.Background(), cftunnel.AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	s := New(Options{
		Secrets: store, DNS: challDNS{tunnel, chall}, Log: log,
		Directory: "https://localhost:" + acmePort + "/dir", HTTPClient: acmeClient, Resolver: resolver,
	})
	const domain = "hlm.home.example.com"
	if _, err := s.Save(Settings{Enabled: true, Domain: domain, Email: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s.maintain(ctx)
	st := s.Status()
	if !st.Active || st.State != "active" || st.CertFor != domain || !strings.Contains(st.Issuer, "Pebble") {
		t.Fatalf("status = %+v", st)
	}
	// The challenge record was made through Cloudflare and taken away again.
	if recs := fake.Records("z1"); len(recs) != 0 {
		t.Errorf("records left behind: %+v", recs)
	}

	// The certificate chains to Pebble's root, and is served.
	res, err := acmeClient.Get("https://localhost:" + mgmtPort + "/roots/0")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := io.ReadAll(res.Body)
	res.Body.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(root)
	addr := serve(t, s)
	if _, body, err := get(t, client(roots, domain, addr), "https://"+domain+"/"); err != nil || body != "tls" {
		t.Fatalf("serving the certificate: %q %v", body, err)
	}

	// It isn't renewed while it's current, and it's kept across a restart.
	before := s.Status()
	s.maintain(ctx)
	if after := s.Status(); !after.Issued.Equal(before.Issued) {
		t.Errorf("renewed a current certificate: %v until %v, then %v", before.Issued, before.NotAfter, after.Issued)
	}
	again := New(Options{Secrets: store, DNS: challDNS{tunnel, chall}, Log: log})
	if !again.Active() {
		t.Error("the certificate wasn't kept")
	}
}
