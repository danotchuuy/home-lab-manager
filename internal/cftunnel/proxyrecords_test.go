package cftunnel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
	"github.com/danotchuuy/home-lab-manager/internal/cloudflare/cloudflaretest"
)

// The status page's sites on the proxy server, behind Cloudflare: proxied
// records to the proxy, never touching anyone else's (or the tunnel's).
func TestProxyRecords(t *testing.T) {
	s, _, fake := newService(t, fakeServers{})
	ctx := context.Background()
	if err := s.ProxyRecord(ctx, "status.example.com", "203.0.113.9"); err == nil {
		t.Error("made a record without an account")
	}
	if err := s.SaveAccount(ctx, AccountInput{Token: cloudflaretest.Token}); err != nil {
		t.Fatal(err)
	}
	if err := s.ProxyRecord(ctx, "status.example.com", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	recs := fake.Records("z1")
	if len(recs) != 1 || recs[0].Type != "A" || recs[0].Content != "203.0.113.9" || !recs[0].Proxied || recs[0].Comment != ProxyRecordComment {
		t.Fatalf("records: %+v", recs)
	}
	// The proxy moves (to a name, then IPv6): the same record, updated.
	for _, tt := range []struct{ target, typ string }{{"vps.example.net", "CNAME"}, {"2001:db8::9", "AAAA"}} {
		if err := s.ProxyRecord(ctx, "status.example.com", tt.target); err != nil {
			t.Fatal(err)
		}
		if recs := fake.Records("z1"); len(recs) != 1 || recs[0].Type != tt.typ || recs[0].Content != tt.target {
			t.Errorf("to %s: %+v", tt.target, recs)
		}
	}
	// Someone else's record, and the tunnel's, are left alone.
	fake.AddRecord("z1", cloudflare.DNSRecord{ID: "theirs", Name: "mine.example.com", Type: "A", Content: "192.0.2.1"})
	fake.AddRecord("z1", cloudflare.DNSRecord{ID: "tun", Name: "tunnel.example.com", Type: "CNAME", Content: "x.cfargotunnel.com", Comment: RecordComment})
	if err := s.ProxyRecord(ctx, "mine.example.com", "203.0.113.9"); err == nil || !strings.Contains(err.Error(), "didn't make") {
		t.Errorf("someone else's: %v", err)
	}
	if err := s.ProxyRecord(ctx, "tunnel.example.com", "203.0.113.9"); err == nil || !strings.Contains(err.Error(), "Tunnel") {
		t.Errorf("the tunnel's: %v", err)
	}
	if err := s.ProxyRecord(ctx, "status.other.net", "203.0.113.9"); err == nil || !strings.Contains(err.Error(), "isn't in any") {
		t.Errorf("not in a zone: %v", err)
	}
	// Removing takes only hlm's record.
	for _, h := range []string{"status.example.com", "mine.example.com", "tunnel.example.com", "status.other.net"} {
		if err := s.RemoveProxyRecord(ctx, h); err != nil {
			t.Errorf("remove %s: %v", h, err)
		}
	}
	if recs := fake.Records("z1"); len(recs) != 2 {
		t.Errorf("after removing: %+v", recs)
	}

	cert, exp, err := s.OriginCertificate(ctx, "status.example.com", "-----BEGIN CERTIFICATE REQUEST-----\nx\n-----END CERTIFICATE REQUEST-----\n")
	if err != nil || !strings.Contains(cert, "BEGIN CERTIFICATE") || exp.Before(time.Now().AddDate(14, 0, 0)) {
		t.Errorf("origin certificate: %q %v %v", cert, exp, err)
	}
	if len(fake.Certificates) != 1 || fake.Certificates[0].Hostnames[0] != "status.example.com" {
		t.Errorf("certificates: %+v", fake.Certificates)
	}
	if mode, err := s.SSLMode(ctx, "status.example.com"); err != nil || mode != "full" {
		t.Errorf("SSL mode %q %v", mode, err)
	}
	if v4, v6, err := s.CloudflareIPs(ctx); err != nil || len(v4) == 0 || len(v6) == 0 {
		t.Errorf("IPs %v %v %v", v4, v6, err)
	}
}
