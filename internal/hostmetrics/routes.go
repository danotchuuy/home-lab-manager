package hostmetrics

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// HostInterface is a network interface of the Docker host with the IPv4
// subnets routed directly on it, for suggesting LAN network settings.
type HostInterface struct {
	Name    string   `json:"name"`
	Subnets []string `json:"subnets"`
	Gateway string   `json:"gateway,omitempty"` // the default gateway, if it's on this interface
	Default bool     `json:"default"`           // carries the default route
}

// Interfaces reads the host's IPv4 routing table (PID 1's view, so it is the
// host's even when hlm runs in a container with the host's /proc mounted).
func (c *Collector) Interfaces() ([]HostInterface, error) {
	f, err := os.Open(c.proc("1", "net", "route"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseRoutes(f)
}

// ParseRoutes reads a routing table in /proc/net/route's format: the
// interfaces with the subnets directly on them, the one with the default
// route first.
func ParseRoutes(r io.Reader) ([]HostInterface, error) {
	byName := map[string]*HostInterface{}
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || virtualIface.MatchString(fields[0]) {
			continue
		}
		dest, ok1 := hexIPv4(fields[1])
		gw, ok2 := hexIPv4(fields[2])
		mask, ok3 := hexIPv4(fields[7])
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		iface := byName[fields[0]]
		if iface == nil {
			iface = &HostInterface{Name: fields[0], Subnets: []string{}}
			byName[fields[0]] = iface
		}
		bits := maskBits(mask)
		switch {
		case dest.IsUnspecified() && bits == 0:
			iface.Default, iface.Gateway = true, gw.String()
		case gw.IsUnspecified() && bits > 0 && bits < 32:
			// A directly connected network.
			iface.Subnets = append(iface.Subnets, netip.PrefixFrom(dest, bits).Masked().String())
		}
	}
	out := make([]HostInterface, 0, len(byName))
	for _, i := range byName {
		if len(i.Subnets) > 0 {
			out = append(out, *i)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Default != out[b].Default {
			return out[a].Default
		}
		return out[a].Name < out[b].Name
	})
	return out, sc.Err()
}

// hexIPv4 decodes an address from /proc/net/route, which prints it as
// little-endian hex.
func hexIPv4(s string) (netip.Addr, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return netip.Addr{}, false
	}
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(b))
	return netip.AddrFrom4(a), true
}

func maskBits(m netip.Addr) int {
	v := binary.BigEndian.Uint32(m.AsSlice())
	n := 0
	for v&0x80000000 != 0 {
		n++
		v <<= 1
	}
	return n
}

// LANAddress returns the host's IPv4 address on the interface that carries
// the default route (e.g. 192.168.1.20), or "" if it can't be found. It is
// read from PID 1's view of the kernel's address table, so it is the host's
// address even when hlm runs in a container with the host's /proc mounted.
func (c *Collector) LANAddress() string {
	ifaces, err := c.Interfaces()
	if err != nil {
		return ""
	}
	var subnets []netip.Prefix
	for _, i := range ifaces {
		if !i.Default {
			continue
		}
		for _, s := range i.Subnets {
			if p, err := netip.ParsePrefix(s); err == nil {
				subnets = append(subnets, p)
			}
		}
	}
	data, err := os.ReadFile(c.proc("1", "net", "fib_trie"))
	if err != nil {
		return ""
	}
	return lanAddress(string(data), subnets)
}

// lanAddress picks the local address (a "/32 host LOCAL" entry of fib_trie)
// that lies in one of subnets.
func lanAddress(fibTrie string, subnets []netip.Prefix) string {
	lines := strings.Split(fibTrie, "\n")
	for i := 1; i < len(lines); i++ {
		if !strings.Contains(lines[i], "/32 host LOCAL") {
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i-1]), "|--")))
		if err != nil {
			continue
		}
		for _, s := range subnets {
			if s.Contains(ip) {
				return ip.String()
			}
		}
	}
	return ""
}
