package forwarding

import (
	"context"
	"fmt"
	"net/netip"
)

// cgnat is the shared address space ISPs use for carrier-grade NAT.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// WANAddress returns the gateway's public IP address on a WAN ("wan", the
// default, or "wan2"), as UniFi reports it. Addresses that can't be reached
// from the internet (private or carrier-grade NAT, when the gateway is
// behind another router or the ISP shares addresses) are an error, since
// nothing outside could connect to them.
func (s *Service) WANAddress(ctx context.Context, wan string) (string, error) {
	if wan == "" {
		wan = "wan"
	}
	cl, _, err := s.client()
	if err != nil {
		return "", err
	}
	addrs, err := cl.WANAddresses(ctx)
	if err != nil {
		return "", err
	}
	ip, ok := addrs[wan]
	if !ok {
		return "", fmt.Errorf("UniFi doesn't report an internet address for %s; is it connected?", wan)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("UniFi reported an invalid address %q for %s", ip, wan)
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || cgnat.Contains(addr) {
		return "", fmt.Errorf("UniFi reports %s for %s, which isn't reachable from the internet: the gateway is behind another router or your ISP uses carrier-grade NAT. Use a VPN address (e.g. WireGuard) as the home address instead", ip, wan)
	}
	return addr.String(), nil
}
