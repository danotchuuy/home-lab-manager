package cftunnel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
)

// Connected reports whether a Cloudflare account is saved.
func (s *Service) Connected() bool {
	_, ok, err := s.account()
	return ok && err == nil
}

// SetTXT makes a TXT record, for an ACME DNS-01 challenge, in the zone that
// name is in, and returns how to remove it.
func (s *Service) SetTXT(ctx context.Context, name, value string) (func(context.Context) error, error) {
	c, _, err := s.client()
	if err != nil {
		return nil, err
	}
	zones, err := c.Zones(ctx, "")
	if err != nil {
		return nil, err
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	var zone cloudflare.Zone
	for _, z := range zones {
		zn := strings.ToLower(z.Name)
		if (name == zn || strings.HasSuffix(name, "."+zn)) && len(zn) > len(zone.Name) {
			zone = z
		}
	}
	if zone.ID == "" {
		return nil, fmt.Errorf("none of the Cloudflare token's zones contains %s", strings.TrimPrefix(name, "_acme-challenge."))
	}
	rec, err := c.CreateDNSRecord(ctx, zone.ID, cloudflare.DNSRecord{
		Type: "TXT", Name: name, Content: value, TTL: 60, Comment: "hlm: HTTPS certificate challenge, removed after",
	})
	if err != nil {
		return nil, err
	}
	if rec.ID == "" {
		return nil, errors.New("cloudflare made the record but didn't say its ID")
	}
	return func(ctx context.Context) error { return c.DeleteDNSRecord(ctx, zone.ID, rec.ID) }, nil
}
