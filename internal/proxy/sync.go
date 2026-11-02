package proxy

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
)

// PortSync is whether a server's ports are in place on the proxy and its
// firewall, by forwarding.Mapping.Key. A nil map means that part isn't set
// up; FirewallError says why the firewall couldn't be checked.
type PortSync struct {
	Proxy         map[string]forwarding.SyncState `json:"proxy"`
	Firewall      map[string]forwarding.SyncState `json:"firewall"`
	FirewallError string                          `json:"firewallError,omitempty"`
}

func (s *Service) appliedPorts() map[string][]forwarding.Mapping {
	applied := map[string][]forwarding.Mapping{}
	_, _ = s.secrets.Get(secretApplied, &applied)
	return applied
}

// PortStates compares a server's ports with the configuration last applied
// to the proxy (hlm is the only one writing it) and with the firewall's
// rules.
func (s *Service) PortStates(ctx context.Context, server string) (PortSync, error) {
	var out PortSync
	st, ok, err := s.Settings()
	if err != nil || !ok {
		return out, err
	}
	info, err := s.servers.Inspect(ctx, server)
	if err != nil {
		return out, err
	}
	server = strings.TrimPrefix(info.Name, "/") // the proxy knows servers by name
	ports, err := s.ports(info)
	if err != nil {
		return out, err
	}

	// What applying now would relay for the server, and what's relayed.
	wanted, skipped := map[string]bool{}, map[string]string{}
	if s.Relayed(server) {
		all := s.relayed(ctx, st)
		owner := map[forwarding.Port]string{}
		for _, p := range all {
			for _, m := range p.Ports {
				owner[forwarding.Port{Number: m.Port, Proto: m.Proto}] = p.Server
			}
		}
		for _, p := range all {
			if p.Server != server {
				continue
			}
			for _, m := range p.Ports {
				wanted[m.Key()] = true
			}
			for _, m := range p.Skipped {
				skipped[m.Key()] = owner[forwarding.Port{Number: m.Port, Proto: m.Proto}]
			}
		}
	}
	applied := s.appliedPorts()
	appliedOwner := map[forwarding.Port]string{} // other servers' relayed internet ports
	for name, ms := range applied {
		if name == server {
			continue
		}
		for _, m := range ms {
			appliedOwner[forwarding.Port{Number: m.Port, Proto: m.Proto}] = name
		}
	}
	mine := map[string]forwarding.Mapping{}
	for _, m := range applied[server] {
		mine[m.Key()] = m
	}

	out.Proxy = map[string]forwarding.SyncState{}
	for _, m := range ports {
		k := m.Key()
		got, isApplied := mine[k]
		switch {
		case skipped[k] != "":
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncConflict, Detail: fmt.Sprintf("Internet port %d/%s is already relayed for %s.", m.Port, m.Proto, skipped[k])}
		case wanted[k] && isApplied && got.Port == m.Port:
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncDone}
		case wanted[k] && isApplied:
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncPending, Detail: fmt.Sprintf("Relayed on %d; Apply proxy moves it to %d.", got.Port, m.Port)}
		case wanted[k]:
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncPending, Detail: "Not relayed yet; Apply proxy adds it."}
		case isApplied:
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncRemoving, Detail: fmt.Sprintf("Still relayed on %d; Apply proxy removes it.", got.Port)}
		default:
			out.Proxy[k] = forwarding.SyncState{State: forwarding.SyncOff}
		}
	}

	c, ok, err := s.firewallConfig()
	if err != nil || !ok || c.FirewallID == 0 {
		return out, err
	}
	out.Firewall = map[string]forwarding.SyncState{}
	open, err := s.openPorts(ctx, c)
	if err != nil {
		out.Firewall = nil
		out.FirewallError = err.Error()
		return out, nil
	}
	for _, m := range ports {
		k := m.Key()
		want := wanted[k] && m.Firewall
		isOpen := open(m.Proto, m.Port)
		other := appliedOwner[forwarding.Port{Number: m.Port, Proto: m.Proto}]
		switch {
		case want && isOpen:
			out.Firewall[k] = forwarding.SyncState{State: forwarding.SyncDone}
		case want:
			out.Firewall[k] = forwarding.SyncState{State: forwarding.SyncPending, Detail: "Not open yet; Apply proxy opens it."}
		case isOpen && other != "":
			out.Firewall[k] = forwarding.SyncState{State: forwarding.SyncOff, Detail: fmt.Sprintf("Open for %s, which uses the same internet port.", other)}
		case isOpen:
			out.Firewall[k] = forwarding.SyncState{State: forwarding.SyncRemoving, Detail: "Still open; Apply proxy closes it."}
		default:
			out.Firewall[k] = forwarding.SyncState{State: forwarding.SyncOff}
		}
	}
	return out, nil
}

// openPorts reads hlm's rules on the firewall and returns whether they open
// a port.
func (s *Service) openPorts(ctx context.Context, c FirewallConfig) (func(proto string, port int) bool, error) {
	cl, err := s.linodeClient(c.Token)
	if err != nil {
		return nil, err
	}
	rules, err := cl.FirewallRules(ctx, c.FirewallID)
	if err != nil {
		return nil, err
	}
	_, ours := splitRules(rules.Inbound)
	return func(proto string, port int) bool {
		for _, r := range ours {
			if r.Action == "ACCEPT" && strings.EqualFold(r.Protocol, proto) && portsInclude(r, port) {
				return true
			}
		}
		return false
	}, nil
}

// portsInclude reports whether a rule's ports ("22, 2456-2458") include
// port. A rule without ports applies to all of them.
func portsInclude(r linode.Rule, port int) bool {
	if strings.TrimSpace(r.Ports) == "" {
		return true
	}
	for _, p := range strings.Split(r.Ports, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(p), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}
