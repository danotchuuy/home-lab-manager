package cftunnel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
)

// The status page's sites on the nginx proxy server, with Cloudflare in
// front: a proxied ("orange cloud") DNS record for each domain, pointing at
// the proxy server, and an Origin CA certificate for nginx there, so
// Cloudflare serves the domain over HTTPS and talks HTTPS to the proxy.

// ProxyRecordComment marks the DNS records hlm makes for the proxy server;
// the tunnel's records have RecordComment.
const ProxyRecordComment = "Managed by Home Lab Manager (status page on the proxy server)"

// zoneFor is the account's zone a hostname is in.
func (s *Service) zoneFor(ctx context.Context, c *cloudflare.Client, accountID, hostname string) (cloudflare.Zone, error) {
	zones, err := c.Zones(ctx, accountID)
	if err != nil {
		return cloudflare.Zone{}, err
	}
	z, ok := zoneOf(hostname, zones)
	if !ok {
		return cloudflare.Zone{}, fmt.Errorf("%s isn't in any of your Cloudflare domains (zones)", hostname)
	}
	return z, nil
}

// proxyRecordFor is the record pointing a name at target: A or AAAA for an
// address, CNAME for a name; through Cloudflare's proxy, or DNS only.
func proxyRecordFor(hostname, target string, proxied bool) cloudflare.DNSRecord {
	r := cloudflare.DNSRecord{Name: hostname, Content: target, Type: "CNAME", Proxied: proxied, TTL: 1, Comment: ProxyRecordComment}
	if a, err := netip.ParseAddr(target); err == nil {
		r.Type, r.Content = "A", a.String()
		if a.Is6() {
			r.Type = "AAAA"
		}
	}
	return r
}

// ProxyRecord points a hostname at the proxy server (target, its address
// or name) through Cloudflare's proxy. A record someone else made for the
// name (or the tunnel's) is left alone, and reported.
func (s *Service) ProxyRecord(ctx context.Context, hostname, target string) error {
	return s.pointRecord(ctx, hostname, target, true)
}

// DNSRecord points a hostname straight at the proxy server (DNS only, not
// through Cloudflare's proxy), for nginx there to answer it with its own
// certificate. Records are left alone and reported as ProxyRecord's are.
func (s *Service) DNSRecord(ctx context.Context, hostname, target string) error {
	return s.pointRecord(ctx, hostname, target, false)
}

func (s *Service) pointRecord(ctx context.Context, hostname, target string, proxied bool) error {
	c, a, err := s.client()
	if err != nil {
		return err
	}
	z, err := s.zoneFor(ctx, c, a.AccountID, hostname)
	if err != nil {
		return err
	}
	recs, err := c.DNSRecords(ctx, z.ID, hostname)
	if err != nil {
		return err
	}
	want := proxyRecordFor(hostname, target, proxied)
	var ours *cloudflare.DNSRecord
	for i, rec := range recs {
		switch {
		case rec.Comment == ProxyRecordComment && (rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME"):
			if ours == nil {
				ours = &recs[i]
			} else if err := c.DeleteDNSRecord(ctx, z.ID, rec.ID); err != nil {
				return err
			}
		case rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME":
			what := "that hlm didn't make"
			if rec.Comment == RecordComment {
				what = "for the Cloudflare Tunnel (remove that route on the Tunnel page)"
			}
			return fmt.Errorf("%s already has a DNS record (%s %s) %s", hostname, rec.Type, rec.Content, what)
		}
	}
	switch {
	case ours == nil:
		_, err = c.CreateDNSRecord(ctx, z.ID, want)
	case ours.Type != want.Type || ours.Content != want.Content || ours.Proxied != want.Proxied:
		_, err = c.UpdateDNSRecord(ctx, z.ID, ours.ID, want)
	}
	if err == nil {
		s.log.Info("pointed a status page domain at the proxy server through Cloudflare", "hostname", hostname)
	}
	return err
}

// RemoveProxyRecord removes the record ProxyRecord made for a hostname.
func (s *Service) RemoveProxyRecord(ctx context.Context, hostname string) error {
	c, a, err := s.client()
	if err != nil {
		return err
	}
	z, err := s.zoneFor(ctx, c, a.AccountID, hostname)
	if err != nil {
		return nil // nothing hlm could have made
	}
	recs, err := c.DNSRecords(ctx, z.ID, hostname)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Comment == ProxyRecordComment {
			if err := c.DeleteDNSRecord(ctx, z.ID, rec.ID); err != nil && !cloudflare.IsNotFound(err) {
				return err
			}
			s.log.Info("removed the proxy server's DNS record", "hostname", hostname)
		}
	}
	return nil
}

// OriginCertificate has Cloudflare's Origin CA sign a certificate request
// (PEM) for a hostname, valid for 15 years, and returns it (PEM) with its
// expiry.
func (s *Service) OriginCertificate(ctx context.Context, hostname, csr string) (string, time.Time, error) {
	c, a, err := s.client()
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.zoneFor(ctx, c, a.AccountID, hostname); err != nil {
		return "", time.Time{}, err
	}
	cert, err := c.CreateOriginCertificate(ctx, csr, []string{hostname}, 5475)
	if err != nil {
		return "", time.Time{}, err
	}
	exp, err := time.Parse("2006-01-02 15:04:05 -0700 MST", cert.ExpiresOn)
	if err != nil {
		exp = time.Now().AddDate(15, 0, 0)
	}
	if !strings.Contains(cert.Certificate, "BEGIN CERTIFICATE") {
		return "", time.Time{}, errors.New("Cloudflare returned no certificate")
	}
	return cert.Certificate, exp, nil
}

// SSLMode is the SSL/TLS mode of a hostname's zone: off, flexible, full or
// strict.
func (s *Service) SSLMode(ctx context.Context, hostname string) (string, error) {
	c, a, err := s.client()
	if err != nil {
		return "", err
	}
	z, err := s.zoneFor(ctx, c, a.AccountID, hostname)
	if err != nil {
		return "", err
	}
	return c.SSLMode(ctx, z.ID)
}

// CloudflareIPs are the ranges Cloudflare connects to origin servers from.
func (s *Service) CloudflareIPs(ctx context.Context) ([]string, []string, error) {
	c, _, err := s.client()
	if err != nil {
		return nil, nil, err
	}
	r, err := c.IPs(ctx)
	return r.IPv4, r.IPv6, err
}
