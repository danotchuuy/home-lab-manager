package hostmetrics

import (
	"bufio"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Listener is a TCP port the host listens on, or a UDP port it has bound.
type Listener struct {
	Addr  netip.Addr // the unspecified address for all addresses
	Port  int
	Proto string // tcp or udp
}

// Listeners lists the host's listening TCP sockets and bound UDP sockets,
// from its network namespace (/proc/1/net).
func (c *Collector) Listeners() ([]Listener, error) {
	if !c.Available() {
		return nil, errors.New("the host can't be read")
	}
	var out []Listener
	for _, f := range []struct{ file, proto string }{{"tcp", "tcp"}, {"tcp6", "tcp"}, {"udp", "udp"}, {"udp6", "udp"}} {
		ls, err := readSockets(c.proc("1", "net", f.file), f.proto)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		out = append(out, ls...)
	}
	return out, nil
}

// tcpListen is the LISTEN state in /proc/net/tcp; UDP sockets are taken
// whatever their state, since a bound one already holds its port.
const tcpListen = "0A"

func readSockets(path, proto string) ([]Listener, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Listener
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		// "sl local_address rem_address st ...", addresses as HEXIP:HEXPORT.
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || (proto == "tcp" && fields[3] != tcpListen) {
			continue
		}
		hexIP, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		addr, ok := parseProcAddr(hexIP)
		if !ok {
			continue
		}
		out = append(out, Listener{Addr: addr, Port: int(port), Proto: proto})
	}
	return out, sc.Err()
}

// parseProcAddr decodes an address from /proc/net/{tcp,udp}{,6}: 32-bit
// words in host (little-endian) byte order.
func parseProcAddr(s string) (netip.Addr, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	a, _ := netip.AddrFromSlice(b)
	return a.Unmap(), true
}
