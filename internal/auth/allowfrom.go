package auth

// Allowed source addresses for API keys and SSH keys, like OpenSSH's from=.
// They're checked on every request, against the connection's own address:
// X-Forwarded-For is ignored, since anyone can send it.

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

const maxAllowFrom = 32

// parseAllowFrom checks a list of addresses and ranges (10.0.3.115,
// 10.0.3.0/24, fd00::/8) and returns it as ranges. An empty list allows any
// address.
func parseAllowFrom(in []string) ([]string, error) {
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(s, "/") {
			var err error
			if p, err = netip.ParsePrefix(s); err != nil {
				return nil, fmt.Errorf("%q isn't an address or range like 10.0.3.0/24", s)
			}
			p = p.Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("%q isn't an address or range like 10.0.3.0/24", s)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if !slices.Contains(out, p.String()) {
			out = append(out, p.String())
		}
	}
	if len(out) > maxAllowFrom {
		return nil, fmt.Errorf("give up to %d addresses or ranges", maxAllowFrom)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// remoteAddr is the address a request came from, from http.Request's
// RemoteAddr ("ip:port").
func remoteAddr(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, errors.New("no client address")
	}
	return a.Unmap(), nil
}

// allowedFrom reports whether remote may use something limited to list.
func allowedFrom(list []string, remote string) bool {
	if len(list) == 0 {
		return true
	}
	a, err := remoteAddr(remote)
	if err != nil {
		return false
	}
	for _, s := range list {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseAllowFrom checks a list of addresses and ranges, as for an API key's
// "allowed from" list.
func ParseAllowFrom(in []string) ([]string, error) { return parseAllowFrom(in) }

// AllowedFrom reports whether remote (a RemoteAddr) is in list. An empty
// list allows nobody here, unlike a key's list.
func AllowedFrom(list []string, remote string) bool {
	return len(list) > 0 && allowedFrom(list, remote)
}
