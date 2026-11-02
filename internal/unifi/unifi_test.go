package unifi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/unifi"
	"github.com/danotchuuy/home-lab-manager/internal/unifi/unifitest"
)

// pinned returns a client for srv with its certificate pinned, checking on
// the way that an unpinned client refuses the self-signed certificate.
func pinned(t *testing.T, url string, s unifi.Settings) *unifi.Client {
	t.Helper()
	s.URL = url
	c, err := unifi.New(s)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Test(context.Background())
	var certErr *unifi.CertError
	if !errors.As(err, &certErr) || len(certErr.Fingerprint) != 64 {
		t.Fatalf("unpinned self-signed certificate: err = %v, want a unifi.CertError with a fingerprint", err)
	}
	s.Fingerprint = certErr.Fingerprint
	c, err = unifi.New(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func exercise(t *testing.T, c *unifi.Client, f *unifitest.Fake, wantOS bool) {
	t.Helper()
	ctx := context.Background()
	info, err := c.Test(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.UniFiOS != wantOS || info.PortForwards != 0 {
		t.Errorf("test = %+v", info)
	}

	pf, err := c.CreatePortForward(ctx, unifi.PortForward{
		Name: "hlm valheim 2456-2458/udp", Enabled: true, Src: "any",
		DstPort: "2456-2458", Fwd: "192.168.1.10", FwdPort: "2456-2458", Proto: "udp", Interface: "wan",
	})
	if err != nil || pf.ID == "" {
		t.Fatalf("create = %+v, %v", pf, err)
	}
	// A conflicting rule is refused, with the controller's message.
	_, err = c.CreatePortForward(ctx, unifi.PortForward{Name: "x", DstPort: "2456-2458", Proto: "tcp_udp", Fwd: "192.168.1.11", FwdPort: "1"})
	var apiErr *unifi.APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "PortForwardConflict") {
		t.Errorf("conflict: err = %v", err)
	}

	pf.Fwd = "192.168.1.20"
	if err := c.UpdatePortForward(ctx, pf); err != nil {
		t.Fatal(err)
	}
	list, err := c.PortForwards(ctx)
	if err != nil || len(list) != 1 || list[0].Fwd != "192.168.1.20" {
		t.Errorf("list = %+v, %v", list, err)
	}

	// An expired session signs in again transparently.
	f.Expire()
	if err := c.DeletePortForward(ctx, pf.ID); err != nil {
		t.Fatalf("delete after session expiry: %v", err)
	}
	if list, _ := c.PortForwards(ctx); len(list) != 0 {
		t.Errorf("after delete: %+v", list)
	}
}

func TestUniFiOSPassword(t *testing.T) {
	f, srv := unifitest.New(t, true)
	c := pinned(t, srv.URL, unifi.Settings{AuthType: "password", Username: "admin", Password: "pw"})
	exercise(t, c, f, true)
	if f.Logins() != 2 {
		t.Errorf("logins = %d, want 2 (initial + after expiry)", f.Logins())
	}
}

func TestUniFiOSAPIKey(t *testing.T) {
	f, srv := unifitest.New(t, true)
	c := pinned(t, srv.URL, unifi.Settings{AuthType: "apikey", APIKey: "key-abc"})
	exercise(t, c, f, true)
	if f.Logins() != 0 {
		t.Errorf("API key auth signed in %d times", f.Logins())
	}
}

func TestSelfHostedController(t *testing.T) {
	f, srv := unifitest.New(t, false)
	c := pinned(t, srv.URL, unifi.Settings{AuthType: "password", Username: "admin", Password: "pw"})
	exercise(t, c, f, false)
}

func TestBadCredentials(t *testing.T) {
	_, srv := unifitest.New(t, true)
	c := pinned(t, srv.URL, unifi.Settings{AuthType: "password", Username: "admin", Password: "wrong"})
	if _, err := c.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "sign-in failed") {
		t.Errorf("wrong password: %v", err)
	}
	c = pinned(t, srv.URL, unifi.Settings{AuthType: "apikey", APIKey: "nope"})
	if _, err := c.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("wrong API key: %v", err)
	}
}

func TestWrongPinIsRefused(t *testing.T) {
	_, srv := unifitest.New(t, true)
	c, _ := unifi.New(unifi.Settings{URL: srv.URL, AuthType: "apikey", APIKey: "key-abc", Fingerprint: strings.Repeat("ab", 32)})
	if _, err := c.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned fingerprint") {
		t.Errorf("wrong pin: %v", err)
	}
}

func TestSettingsValidation(t *testing.T) {
	bad := []unifi.Settings{
		{URL: "192.168.1.1", AuthType: "apikey", APIKey: "k"},
		{URL: "https://192.168.1.1/network", AuthType: "apikey", APIKey: "k"},
		{URL: "https://192.168.1.1", AuthType: "password", Username: "admin"},
		{URL: "https://192.168.1.1", AuthType: "apikey"},
		{URL: "https://192.168.1.1", AuthType: "token"},
	}
	for _, s := range bad {
		if _, err := unifi.New(s); err == nil {
			t.Errorf("%+v: want error", s)
		}
	}
}

func TestRateLimits(t *testing.T) {
	f, srv := unifitest.New(t, true)
	c := pinned(t, srv.URL, unifi.Settings{AuthType: "password", Username: "admin", Password: "pw"})
	ctx := context.Background()

	// A rate-limited API call is retried after the requested delay.
	f.Throttle(1)
	start := time.Now()
	if _, err := c.PortForwards(ctx); err != nil {
		t.Fatalf("throttled once: %v", err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Error("retried without waiting for Retry-After")
	}
	// Persistent throttling gives up with a clear message.
	f.Throttle(10)
	if _, err := c.PortForwards(ctx); err == nil || !strings.Contains(err.Error(), "rate-limiting") {
		t.Errorf("persistent throttling: %v", err)
	}
	f.Throttle(0)

	// Sign-ins that are rate limited aren't retried, and say what to do.
	f.LimitLogins(f.Logins())
	fresh := pinned(t, srv.URL, unifi.Settings{AuthType: "password", Username: "admin", Password: "pw"})
	if _, err := fresh.PortForwards(ctx); err == nil || !strings.Contains(err.Error(), "too many recently") {
		t.Errorf("rate-limited sign-in: %v", err)
	}
	// The existing session keeps working.
	if _, err := c.PortForwards(ctx); err != nil {
		t.Errorf("existing session: %v", err)
	}
}
