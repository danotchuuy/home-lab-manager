package forwarding

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Each server's ports have options: the internet port players use, and
// whether the port is forwarded by UniFi, relayed by the nginx proxy and
// opened in the proxy's cloud firewall. They aren't secret, but the secrets
// store is where hlm keeps its integration settings.
const (
	secretPortOptions = "port-options"
	// secretPorts held only internet ports, before the other options
	// existed; it's migrated on first use.
	secretPorts = "internet-ports"
)

// Mapping is one of a server's ports as players on the internet reach it
// (Port) and as it's reached in the LAN (Local: the port published on the
// host, or the server's own port for servers with a LAN IP). They're the same
// unless the internet port was changed, e.g. so two servers of the same game
// can both be reachable from one public address.
type Mapping struct {
	Port  int    `json:"port"`
	Local int    `json:"local"`
	Proto string `json:"proto"` // tcp or udp
	// Forward, Proxy and Firewall say whether UniFi forwards the port, the
	// nginx proxy relays it, and the proxy's firewall opens it.
	Forward  bool `json:"forward"`
	Proxy    bool `json:"proxy"`
	Firewall bool `json:"firewall"`
}

// Key identifies the port among the server's ports, e.g. "2456/udp".
func (m Mapping) Key() string { return Port{m.Local, m.Proto}.String() }

// PortOptions are a port's stored options. The zero value is the default:
// the same port on the internet, included everywhere.
type PortOptions struct {
	Internet   int  `json:"internet,omitempty"`
	NoForward  bool `json:"noForward,omitempty"`
	NoProxy    bool `json:"noProxy,omitempty"`
	NoFirewall bool `json:"noFirewall,omitempty"`
}

// ServerOptions are a server's port options, by Mapping.Key.
type ServerOptions map[string]PortOptions

// Mappings applies a server's options to its ports.
func Mappings(info docker.ContainerJSON, opts ServerOptions) []Mapping {
	out := []Mapping{}
	for _, p := range localPorts(info) {
		o := opts[p.String()]
		m := Mapping{Port: p.Number, Local: p.Number, Proto: p.Proto, Forward: !o.NoForward, Proxy: !o.NoProxy, Firewall: !o.NoFirewall}
		if o.Internet != 0 {
			m.Port = o.Internet
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return portLess(Port{out[i].Port, out[i].Proto}, Port{out[j].Port, out[j].Proto})
	})
	return out
}

func (s *Service) allOptions() (map[string]ServerOptions, error) {
	all := map[string]ServerOptions{}
	ok, err := s.secrets.Get(secretPortOptions, &all)
	if err != nil || ok {
		return all, err
	}
	var legacy map[string]map[string]int
	if ok, err := s.secrets.Get(secretPorts, &legacy); err != nil || !ok {
		return all, err
	}
	for server, ports := range legacy {
		all[server] = ServerOptions{}
		for key, n := range ports {
			all[server][key] = PortOptions{Internet: n}
		}
	}
	return all, nil
}

func (s *Service) saveOptions(all map[string]ServerOptions) error {
	if err := s.secrets.Set(secretPortOptions, all); err != nil {
		return err
	}
	return s.secrets.Delete(secretPorts)
}

// Options returns a server's stored port options.
func (s *Service) Options(server string) (ServerOptions, error) {
	all, err := s.allOptions()
	if err != nil {
		return nil, err
	}
	opts := all[server]
	if opts == nil {
		opts = ServerOptions{}
	}
	return opts, nil
}

// ServerPorts returns a server's ports with their options.
func (s *Service) ServerPorts(info docker.ContainerJSON) ([]Mapping, error) {
	opts, err := s.Options(strings.TrimPrefix(info.Name, "/"))
	if err != nil {
		return nil, err
	}
	return Mappings(info, opts), nil
}

// PortSetting is what SetOptions changes for a port.
type PortSetting struct {
	Port     int  `json:"port"` // internet port; 0 keeps the LAN port
	Forward  bool `json:"forward"`
	Proxy    bool `json:"proxy"`
	Firewall bool `json:"firewall"`
}

// SetOptions changes a server's port options, by Mapping.Key. Ports left out
// get the defaults.
func (s *Service) SetOptions(ctx context.Context, server string, in map[string]PortSetting) ([]Mapping, error) {
	s.portsMu.Lock()
	defer s.portsMu.Unlock()
	info, err := s.servers.Inspect(ctx, server)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	local := map[string]Port{}
	for _, p := range localPorts(info) {
		local[p.String()] = p
	}
	opts := ServerOptions{}
	for key, ps := range in {
		p, ok := local[key]
		if !ok {
			return nil, fmt.Errorf("%s doesn't use port %s", name, key)
		}
		if ps.Port < 0 || ps.Port > 65535 {
			return nil, fmt.Errorf("invalid internet port %d for %s", ps.Port, key)
		}
		o := PortOptions{NoForward: !ps.Forward, NoProxy: !ps.Proxy, NoFirewall: !ps.Firewall}
		if ps.Port != 0 && ps.Port != p.Number {
			o.Internet = ps.Port
		}
		if o != (PortOptions{}) {
			opts[key] = o
		}
	}
	// Two of the server's ports can't share an internet port.
	used := map[Port]string{}
	for _, m := range Mappings(info, opts) {
		k := Port{m.Port, m.Proto}
		if other, dup := used[k]; dup {
			return nil, fmt.Errorf("internet port %s is set for both %s and %s", k, other, m.Key())
		}
		used[k] = m.Key()
	}

	all, err := s.allOptions()
	if err != nil {
		return nil, err
	}
	if len(opts) == 0 {
		delete(all, name)
	} else {
		all[name] = opts
	}
	if err := s.saveOptions(all); err != nil {
		return nil, err
	}
	s.log.Info("set port options", "server", name, "changed", len(opts))
	return Mappings(info, opts), nil
}

// ForgetServer drops a deleted server's port options, so a new server with
// the same name starts with its own.
func (s *Service) ForgetServer(name string) error {
	s.portsMu.Lock()
	defer s.portsMu.Unlock()
	all, err := s.allOptions()
	if err != nil {
		return err
	}
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	return s.saveOptions(all)
}
