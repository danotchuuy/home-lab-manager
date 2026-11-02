package forwarding

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// IPCheck is what UniFi knows about an address on the LAN.
type IPCheck struct {
	IP string `json:"ip"`
	// Checked is false when UniFi isn't set up (nothing was checked).
	Checked bool `json:"checked"`
	// InUse is set when another device is using the address or it is
	// reserved for one; Conflict says which.
	InUse    bool   `json:"inUse"`
	Conflict string `json:"conflict,omitempty"`
	// Warning flags a risk that doesn't block using the address, e.g. that
	// it's inside the DHCP range.
	Warning string `json:"warning,omitempty"`
	// Network is the UniFi network the address belongs to, if any.
	Network string `json:"network,omitempty"`
	Error   string `json:"error,omitempty"`
}

func normMAC(m string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(m, "-", ":"), ".", ""))
}

// CheckIP asks UniFi whether ip is free: not used by a connected device, not
// reserved as another device's fixed IP, and not the gateway's address.
// Devices whose MAC is in ignore (the server's own container, when it is
// being reconfigured) don't count.
func (s *Service) CheckIP(ctx context.Context, ip string, ignore []string) (IPCheck, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return IPCheck{}, fmt.Errorf("invalid IP address %q", ip)
	}
	res := IPCheck{IP: addr.String()}
	cl, _, err := s.client()
	if errors.Is(err, ErrNotConfigured) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	skip := map[string]bool{}
	for _, m := range ignore {
		if m != "" {
			skip[normMAC(m)] = true
		}
	}

	// The network it's on: the gateway's own address, and the DHCP pool.
	if nets, err := cl.Networks(ctx); err == nil {
		for _, n := range nets {
			gw, err := netip.ParsePrefix(n.IPSubnet)
			if err != nil || !gw.Masked().Contains(addr) {
				continue
			}
			res.Network = n.Name
			if gw.Addr() == addr {
				res.InUse, res.Conflict = true, fmt.Sprintf("%s is the gateway's address on %s", addr, n.Name)
			}
			start, err1 := netip.ParseAddr(n.DHCPStart)
			stop, err2 := netip.ParseAddr(n.DHCPStop)
			if n.DHCPEnabled && err1 == nil && err2 == nil && start.Compare(addr) <= 0 && addr.Compare(stop) <= 0 {
				res.Warning = fmt.Sprintf("%s is inside %s's DHCP range (%s–%s), so the router could hand it to another device later; an address outside that range is safer", addr, n.Name, start, stop)
			}
			break
		}
	}

	active, err := cl.ActiveClients(ctx)
	if err != nil {
		return res, err
	}
	res.Checked = true
	for _, d := range active {
		if d.IP == res.IP && !skip[normMAC(d.MAC)] && !res.InUse {
			res.InUse, res.Conflict = true, fmt.Sprintf("%s is in use by %s (%s)", addr, d.Label(), d.MAC)
		}
	}
	// Fixed-IP reservations count even while the device is offline.
	if known, err := cl.KnownClients(ctx); err == nil {
		for _, d := range known {
			if d.UseFixedIP && d.FixedIP == res.IP && !skip[normMAC(d.MAC)] && !res.InUse {
				res.InUse, res.Conflict = true, fmt.Sprintf("%s is reserved as the fixed IP of %s (%s)", addr, d.Label(), d.MAC)
			}
		}
	}
	return res, nil
}
