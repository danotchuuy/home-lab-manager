package proxy

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
)

// The WireGuard tunnel between the proxy server and the Docker host. Its
// proxy end is set up over SSH with wg-quick; its home end runs in a
// container on the Docker host. The home end connects out to the proxy and
// keeps the tunnel open, so nothing has to be forwarded at home.
//
// The home end runs in one of two ways:
//   - On the host's network (the default): the tunnel's home address is on
//     the host, so it reaches the servers' published ports directly.
//   - On a macvlan/ipvlan LAN network ("LAN mode"), and on Docker's default
//     bridge. It then routes for the proxy: traffic to a server's own LAN
//     address goes out on the LAN, and traffic to the tunnel's home address
//     is sent on to the host's published ports through the bridge's gateway.
//     Both are masqueraded, so replies come back through the home end. This
//     lets servers that share ports each keep their own LAN address.
// Either way, nginx relays to the tunnel's home address, or to a server's
// LAN address in LAN mode.
//
// On the host's network, a mistake in the home end breaks the host's own
// networking, so it's guarded: the image's init (whose VPN kill switch
// deletes the default route when a tunnel fails) is replaced by homeScript,
// and the tunnel's network is checked against the host's routes, and the
// proxy server's, before it's brought up.

const (
	secretWireGuard = "proxy-wireguard"
	// WireGuardInterface is the tunnel's network interface on both ends. It
	// keeps the old short name, gsh: renaming it would mean taking down
	// tunnels already running on both ends.
	WireGuardInterface = "gsh0"
	wireGuardConfPath  = "/etc/wireguard/" + WireGuardInterface + ".conf"
	// WireGuardContainer runs the home end on the Docker host. Its
	// configuration, private key included, lives in a volume of the same
	// name.
	WireGuardContainer = "hlm-wireguard"
	// legacyWireGuardContainer is the home end's container, and volume,
	// from before the short name changed from gsh to hlm.
	legacyWireGuardContainer = "gsh-wireguard"
	// DefaultWireGuardImage runs the home end. Any image with sh, ip and
	// wg-quick will do: hlm runs homeScript in it, not its own entrypoint.
	DefaultWireGuardImage = "lscr.io/linuxserver/wireguard:latest"
	// LabelWireGuard marks the home end's container.
	LabelWireGuard = "io.hlm.wireguard"
	// labelHomeVersion marks a home end run by homeScript; older ones ran
	// the image's own init.
	labelHomeVersion     = "io.hlm.wireguard.version"
	homeVersion          = "2"
	wireGuardRuleLabel   = rulePrefix + "wireguard"
	defaultTunnelNetwork = "10.77.77.0/24"
	defaultWireGuardPort = 51820
)

// Containers runs the tunnel's home end; *docker.Client implements it.
type Containers interface {
	PullImage(ctx context.Context, ref string, progress func(docker.PullProgress)) error
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error)
	CopyTo(ctx context.Context, id, dir string, tarStream io.Reader) error
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout int) error
	WaitContainer(ctx context.Context, id string) (int, error)
	RemoveContainer(ctx context.Context, id string, force bool) error
	RemoveVolume(ctx context.Context, name string) error
	RenameContainer(ctx context.Context, id, name string) error
	ListNetworks(ctx context.Context) ([]docker.Network, error)
	ConnectNetwork(ctx context.Context, network, container string, endpoint any) error
	Logs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error)
}

// UseDocker sets where the tunnel's home end runs, and its image (empty for
// DefaultWireGuardImage).
func (s *Service) UseDocker(d Containers, image string) {
	if image == "" {
		image = DefaultWireGuardImage
	}
	s.docker, s.wgImage = d, image
}

// WireGuard is the stored tunnel configuration, private keys included. It
// only lives in the encrypted secrets store.
type WireGuard struct {
	// Network holds the tunnel's addresses: the first is the proxy's, the
	// second this host's.
	Network    string `json:"network"`
	ListenPort int    `json:"listenPort"`
	// Endpoint is the address the home end connects to; empty means the
	// proxy server's SSH host.
	Endpoint   string    `json:"endpoint,omitempty"`
	ProxyKey   string    `json:"proxyKey"`
	HomeKey    string    `json:"homeKey"`
	Configured time.Time `json:"configured"`
	// LANNetwork is the macvlan/ipvlan Docker network the home end joins
	// in LAN mode; empty runs it on the host's network.
	LANNetwork string `json:"lanNetwork,omitempty"`
	// LANSubnet is LANNetwork's IPv4 subnet, which the proxy routes
	// through the tunnel.
	LANSubnet string `json:"lanSubnet,omitempty"`
	// LANAddress is the home end's fixed address on LANNetwork; empty lets
	// Docker choose one.
	LANAddress string `json:"lanAddress,omitempty"`
	// BridgeGateway is the host's address on Docker's default bridge, where
	// the home end reaches published ports in LAN mode.
	BridgeGateway string `json:"bridgeGateway,omitempty"`
	// Hosts are the tunnel's ends on other Docker hosts, by host ID: each a
	// peer of the proxy's end, with its own address in the tunnel.
	Hosts map[string]WireGuardEnd `json:"hosts,omitempty"`

	// homeAddr is the home end's address when it isn't this host's (see
	// end).
	homeAddr netip.Addr
}

// WireGuardEnd is the tunnel's end on another Docker host.
type WireGuardEnd struct {
	// Address is its address in the tunnel.
	Address       string    `json:"address"`
	HomeKey       string    `json:"homeKey"`
	LANNetwork    string    `json:"lanNetwork,omitempty"`
	LANAddress    string    `json:"lanAddress,omitempty"`
	LANSubnet     string    `json:"lanSubnet,omitempty"`
	BridgeGateway string    `json:"bridgeGateway,omitempty"`
	Configured    time.Time `json:"configured"`
}

// end is the tunnel as seen from another host's end: its key, address and
// LAN mode instead of this host's.
func (wg WireGuard) end(host string) (WireGuard, bool) {
	e, ok := wg.Hosts[host]
	if !ok {
		return WireGuard{}, false
	}
	out := wg
	out.Hosts = nil
	out.HomeKey, out.LANNetwork, out.LANAddress, out.LANSubnet, out.BridgeGateway = e.HomeKey, e.LANNetwork, e.LANAddress, e.LANSubnet, e.BridgeGateway
	out.Configured = e.Configured
	out.homeAddr, _ = netip.ParseAddr(e.Address)
	return out, true
}

// freeAddress is the next tunnel address for another host's end.
func (wg WireGuard) freeAddress() (netip.Addr, error) {
	used := map[netip.Addr]bool{wg.ProxyAddress(): true, wg.ProxyAddress().Next(): true}
	for _, e := range wg.Hosts {
		if a, err := netip.ParseAddr(e.Address); err == nil {
			used[a] = true
		}
	}
	p := wg.prefix()
	for a := wg.ProxyAddress().Next(); p.Contains(a); a = a.Next() {
		if !used[a] && p.Contains(a.Next()) { // not the broadcast address
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("the tunnel network %s has no addresses left for another host", wg.Network)
}

func (wg WireGuard) prefix() netip.Prefix { return netip.MustParsePrefix(wg.Network) }

// ProxyAddress is the proxy's address in the tunnel.
func (wg WireGuard) ProxyAddress() netip.Addr { return wg.prefix().Addr().Next() }

// HomeAddress is this host's address in the tunnel, which nginx relays to
// (or, for another host's end, that host's).
func (wg WireGuard) HomeAddress() netip.Addr {
	if wg.homeAddr.IsValid() {
		return wg.homeAddr
	}
	return wg.ProxyAddress().Next()
}

// lanReach finds the address a server with its own LAN address is relayed
// to, and the tunnel end that reaches it. That's any end on its LAN, not
// only its own host's: a container with its own address is a device on the
// LAN, which the end there reaches whichever Docker host runs it (and only
// one end may reach a LAN, so with two hosts on one LAN, the other host's
// end can't). Its own host's end is tried first, then this host's, then
// the others'.
func (wg WireGuard) lanReach(info docker.ContainerJSON, host string) (netip.Addr, string, error) {
	type end struct {
		host   string
		subnet string
		net    string
	}
	var ends []end
	if e, ok := wg.end(host); ok && host != localHost {
		ends = append(ends, end{host, e.LANSubnet, e.LANNetwork})
	}
	ends = append(ends, end{localHost, wg.LANSubnet, wg.LANNetwork})
	for _, id := range slices.Sorted(maps.Keys(wg.Hosts)) {
		if id != host {
			ends = append(ends, end{id, wg.Hosts[id].LANSubnet, wg.Hosts[id].LANNetwork})
		}
	}
	// Its fixed address too while it's stopped (asleep, with its wake
	// listener there), so its relay stays.
	addrs := deploy.ContainerAddresses(info)
	var reach []string
	for _, e := range ends {
		if e.subnet == "" {
			continue
		}
		subnet := netip.MustParsePrefix(e.subnet)
		for _, a := range addrs {
			if subnet.Contains(a) {
				return a, e.host, nil
			}
		}
		where := "this host's end"
		if e.host != localHost {
			where = "the end on " + e.host
		}
		reach = append(reach, fmt.Sprintf("%s reaches %s (%s)", where, e.net, e.subnet))
	}
	switch {
	case len(reach) == 0:
		return netip.Addr{}, "", errors.New("has its own LAN address, which the WireGuard tunnel only reaches with \"Reach servers with their own LAN address\" set up on an end on that LAN; choose its network under the tunnel, or relay it to your public address instead")
	case len(addrs) == 0:
		return netip.Addr{}, "", errors.New("has no address yet (is it running?), so the tunnel can't reach it")
	}
	var have []string
	for _, a := range addrs {
		have = append(have, a.String())
	}
	return netip.Addr{}, "", fmt.Errorf("has its own LAN address (%s), which no end of the WireGuard tunnel reaches: %s", strings.Join(have, ", "), strings.Join(reach, "; "))
}

// WireGuardInput is what the user chooses when setting up the tunnel.
type WireGuardInput struct {
	Network    string `json:"network"`
	ListenPort int    `json:"listenPort"`
	Endpoint   string `json:"endpoint"`
	// UseForRelays makes the tunnel the proxy's home address, re-applying
	// nginx if the relays are applied.
	UseForRelays bool `json:"useForRelays"`
	// LANNetwork, if set, runs the home end in LAN mode on this
	// macvlan/ipvlan network, optionally with a fixed LANAddress.
	LANNetwork string `json:"lanNetwork"`
	LANAddress string `json:"lanAddress"`
}

// LANNetwork is a macvlan/ipvlan network the home end can join.
type LANNetwork = docker.LANNetwork

// WireGuardStatus describes the tunnel, without private keys.
type WireGuardStatus struct {
	Configured     bool      `json:"configured"`
	Network        string    `json:"network"`
	ListenPort     int       `json:"listenPort"`
	Endpoint       string    `json:"endpoint,omitempty"`
	ProxyAddress   string    `json:"proxyAddress,omitempty"`
	HomeAddress    string    `json:"homeAddress,omitempty"`
	ProxyPublicKey string    `json:"proxyPublicKey,omitempty"`
	HomePublicKey  string    `json:"homePublicKey,omitempty"`
	ConfiguredAt   time.Time `json:"configuredAt,omitzero"`
	// Home is the state of the container running the home end.
	Home WireGuardHome `json:"home"`
	// InUse means the proxy's home address is the tunnel.
	InUse bool   `json:"inUse"`
	Image string `json:"image"`
	// LANNetwork and LANSubnet are set in LAN mode.
	LANNetwork string `json:"lanNetwork,omitempty"`
	LANSubnet  string `json:"lanSubnet,omitempty"`
	// LANAddress is the fixed address asked for; the home end's actual one
	// is in Home.
	LANAddress string `json:"lanAddress,omitempty"`
	// LANNetworks are the networks LAN mode can use.
	LANNetworks []LANNetwork `json:"lanNetworks"`
	// Hosts are the tunnel's ends on other Docker hosts.
	Hosts []WireGuardHostStatus `json:"hosts"`
}

// WireGuardHome is the state of the home end's container.
type WireGuardHome struct {
	// State is Docker's ("running", "exited", …), or "missing".
	State    string `json:"state,omitempty"`
	Running  bool   `json:"running"`
	ExitCode int    `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
	// LANAddress is its address on the LAN network, in LAN mode.
	LANAddress string `json:"lanAddress,omitempty"`
}

// WireGuardResult is the outcome of setting up the tunnel.
type WireGuardResult struct {
	Status WireGuardStatus `json:"status"`
	Output string          `json:"output"`
	// Apply is the outcome of re-applying nginx to relay through the tunnel.
	Apply      *ApplyResult  `json:"apply,omitempty"`
	ApplyError string        `json:"applyError,omitempty"`
	Firewall   *FirewallSync `json:"firewall,omitempty"`
}

// WireGuardEvent reports progress while the tunnel is set up.
type WireGuardEvent struct {
	// Stage is "step" (Message says what's being done), "output" (Message
	// is a line printed on the proxy server or by Docker), and, from the
	// API, "done" (with Result) or "error" (with Output).
	Stage   string           `json:"stage"`
	Message string           `json:"message,omitempty"`
	Output  string           `json:"output,omitempty"`
	Result  *WireGuardResult `json:"result,omitempty"`
}

// WireGuardCheck is what the proxy end sees of the tunnel.
type WireGuardCheck struct {
	// Handshake is the last time the two ends completed a handshake; zero
	// if they never have since the proxy end started.
	Handshake time.Time `json:"handshake,omitzero"`
	// Hosts are the last handshakes with the ends on other hosts, by host.
	Hosts map[string]time.Time `json:"hosts,omitempty"`
	// Reachable means the proxy could ping this host through the tunnel;
	// nil if ping isn't installed there.
	Reachable *bool  `json:"reachable,omitempty"`
	Output    string `json:"output,omitempty"`
}

func (s *Service) tunnel() (WireGuard, bool, error) {
	var wg WireGuard
	ok, err := s.secrets.Get(secretWireGuard, &wg)
	return wg, ok, err
}

// --- keys ---

func newWireGuardKey() (string, error) {
	k := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	// Clamp, as "wg genkey" does.
	k[0] &= 248
	k[31] = k[31]&127 | 64
	return base64.StdEncoding.EncodeToString(k), nil
}

// wireGuardPublic derives the public key from a private key.
func wireGuardPublic(private string) (string, error) {
	k, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(k) != curve25519.ScalarSize {
		return "", errors.New("invalid WireGuard private key")
	}
	pub, err := curve25519.X25519(k, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// --- configuration ---

// validateWireGuard checks the input and fills in defaults.
func validateWireGuard(in *WireGuardInput) error {
	in.Network, in.Endpoint = strings.TrimSpace(in.Network), strings.TrimSpace(in.Endpoint)
	if in.Network == "" {
		in.Network = defaultTunnelNetwork
	}
	p, err := netip.ParsePrefix(in.Network)
	if err != nil || !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 30 {
		return fmt.Errorf("invalid tunnel network %q: use a private IPv4 network from /16 to /30, e.g. %s", in.Network, defaultTunnelNetwork)
	}
	if !p.Addr().IsPrivate() {
		return fmt.Errorf("the tunnel network %s isn't a private range (10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16)", in.Network)
	}
	in.Network = p.Masked().String()
	if in.ListenPort == 0 {
		in.ListenPort = defaultWireGuardPort
	}
	if in.ListenPort < 1 || in.ListenPort > 65535 {
		return fmt.Errorf("invalid WireGuard port %d", in.ListenPort)
	}
	if in.Endpoint != "" && !validHost(in.Endpoint) {
		return fmt.Errorf("invalid endpoint %q: use the proxy server's IP address or hostname, without a port", in.Endpoint)
	}
	in.LANNetwork, in.LANAddress = strings.TrimSpace(in.LANNetwork), strings.TrimSpace(in.LANAddress)
	if in.LANNetwork == "" {
		in.LANAddress = ""
	}
	if in.LANAddress != "" {
		if a, err := netip.ParseAddr(in.LANAddress); err != nil || !a.Is4() {
			return fmt.Errorf("invalid LAN address %q: use an IPv4 address", in.LANAddress)
		}
	}
	return nil
}

// resolveLAN fills in LAN mode's subnet and bridge gateway from Docker's
// networks, and checks they work with the tunnel's network.
func (s *Service) resolveLAN(ctx context.Context, wg *WireGuard) error {
	wg.LANSubnet, wg.BridgeGateway = "", ""
	if wg.LANNetwork == "" {
		return nil
	}
	nets, err := s.dk(ctx).ListNetworks(ctx)
	if err != nil {
		return err
	}
	var lan, bridge *docker.Network
	for i := range nets {
		switch nets[i].Name {
		case wg.LANNetwork:
			lan = &nets[i]
		case "bridge":
			bridge = &nets[i]
		}
	}
	if lan == nil || (lan.Driver != "macvlan" && lan.Driver != "ipvlan") {
		return fmt.Errorf("%q isn't a macvlan or ipvlan network on the Docker host", wg.LANNetwork)
	}
	subnet, _ := lan.IPv4()
	if !subnet.IsValid() {
		return fmt.Errorf("the network %s has no IPv4 subnet", wg.LANNetwork)
	}
	if bridge == nil {
		return errors.New("Docker's default bridge network is missing, so the tunnel couldn't reach published ports")
	}
	bridgeNet, gw := bridge.IPv4()
	if !gw.IsValid() {
		return errors.New("Docker's default bridge network has no IPv4 gateway")
	}
	for _, other := range []netip.Prefix{subnet, bridgeNet} {
		if other.IsValid() && other.Overlaps(wg.prefix()) {
			return fmt.Errorf("the tunnel network %s overlaps %s; choose another tunnel network", wg.Network, other)
		}
	}
	if wg.LANAddress != "" && !subnet.Contains(netip.MustParseAddr(wg.LANAddress)) {
		return fmt.Errorf("%s isn't in %s's subnet %s", wg.LANAddress, wg.LANNetwork, subnet)
	}
	wg.LANSubnet, wg.BridgeGateway = subnet.String(), gw.String()
	return nil
}

// endpoint is where the home end connects: host:port.
func (wg WireGuard) endpoint(st Settings) string {
	host := wg.Endpoint
	if host == "" {
		host = st.Host
	}
	return net.JoinHostPort(host, strconv.Itoa(wg.ListenPort))
}

// RenderWireGuard produces the configurations of the proxy's end and the
// home end.
func RenderWireGuard(wg WireGuard, st Settings) (proxyConf, homeConf string, err error) {
	proxyPub, err := wireGuardPublic(wg.ProxyKey)
	if err != nil {
		return "", "", err
	}
	homePub, err := wireGuardPublic(wg.HomeKey)
	if err != nil {
		return "", "", err
	}
	bits := strconv.Itoa(wg.prefix().Bits())
	allowed := wg.HomeAddress().String() + "/32"
	lanRules := ""
	if wg.LANSubnet != "" {
		// The proxy sends traffic for servers' LAN addresses through the
		// tunnel too. The home end passes the tunnel's home address on to
		// the host's published ports, and masquerades everything it
		// forwards so replies come back through it.
		allowed += ", " + wg.LANSubnet
		dnat := "-t nat %s PREROUTING -i %%i -d " + wg.HomeAddress().String() + " -j DNAT --to-destination " + wg.BridgeGateway
		masq := "-t nat %s POSTROUTING -s " + wg.prefix().String() + " ! -o %%i -j MASQUERADE"
		lanRules = "PostUp = iptables " + fmt.Sprintf(dnat, "-A") + "; iptables " + fmt.Sprintf(masq, "-A") + "\n" +
			"PostDown = iptables " + fmt.Sprintf(dnat, "-D") + "; iptables " + fmt.Sprintf(masq, "-D") + "\n"
	}
	proxyConf = "# Managed by Home Lab Manager; changes made here are overwritten.\n" +
		"# Tunnel to the game server host, which nginx relays to.\n" +
		"[Interface]\n" +
		"Address = " + wg.ProxyAddress().String() + "/" + bits + "\n" +
		"ListenPort = " + strconv.Itoa(wg.ListenPort) + "\n" +
		"PrivateKey = " + wg.ProxyKey + "\n\n" +
		"[Peer]\n" +
		"# The game server host\n" +
		"PublicKey = " + homePub + "\n" +
		"AllowedIPs = " + allowed + "\n"
	// The ends on other Docker hosts.
	for _, id := range slices.Sorted(maps.Keys(wg.Hosts)) {
		e := wg.Hosts[id]
		pub, err := wireGuardPublic(e.HomeKey)
		if err != nil {
			return "", "", fmt.Errorf("the tunnel's end on %s: %w", id, err)
		}
		hostAllowed := e.Address + "/32"
		if e.LANSubnet != "" {
			hostAllowed += ", " + e.LANSubnet
		}
		proxyConf += "\n[Peer]\n# Docker host " + id + "\nPublicKey = " + pub + "\nAllowedIPs = " + hostAllowed + "\n"
	}
	homeConf = "# Managed by Home Lab Manager; changes made here are overwritten.\n" +
		"# Tunnel to the proxy server, which relays game traffic here.\n" +
		"[Interface]\n" +
		"Address = " + wg.HomeAddress().String() + "/" + bits + "\n" +
		"PrivateKey = " + wg.HomeKey + "\n" +
		lanRules + "\n" +
		"[Peer]\n" +
		"# The proxy server\n" +
		"PublicKey = " + proxyPub + "\n" +
		"Endpoint = " + wg.endpoint(st) + "\n" +
		"AllowedIPs = " + wg.ProxyAddress().String() + "/32\n" +
		// Home is usually behind NAT: keep the mapping open so the proxy
		// can always reach it.
		"PersistentKeepalive = 25\n"
	return proxyConf, homeConf, nil
}

// wireGuardRule opens the tunnel's port on the proxy's firewall. The home
// end's address isn't known (and may change), so it's open to all.
func wireGuardRule(port int) linode.Rule {
	return linode.Rule{
		Action: "ACCEPT", Protocol: "UDP", Ports: strconv.Itoa(port),
		Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}},
		Label:     wireGuardRuleLabel, Description: "Home Lab Manager: WireGuard tunnel to the game server host",
	}
}

// --- status ---

// WireGuardStatus describes the tunnel and the home end's container.
func (s *Service) WireGuardStatus(ctx context.Context) (WireGuardStatus, error) {
	out := WireGuardStatus{Network: defaultTunnelNetwork, ListenPort: defaultWireGuardPort, Image: s.wgImage, LANNetworks: []LANNetwork{},
		Hosts: []WireGuardHostStatus{}}
	if s.docker != nil {
		if nets, err := s.dk(ctx).ListNetworks(ctx); err == nil {
			out.LANNetworks = docker.LANNetworks(nets)
		}
	}
	wg, ok, err := s.tunnel()
	if err != nil || !ok {
		return out, err
	}
	st, _, err := s.Settings()
	if err != nil {
		return out, err
	}
	out.Configured, out.Network, out.ListenPort, out.Endpoint = true, wg.Network, wg.ListenPort, wg.endpoint(st)
	out.ProxyAddress, out.HomeAddress = wg.ProxyAddress().String(), wg.HomeAddress().String()
	out.ProxyPublicKey, _ = wireGuardPublic(wg.ProxyKey)
	out.HomePublicKey, _ = wireGuardPublic(wg.HomeKey)
	out.ConfiguredAt = wg.Configured
	out.InUse = st.UpstreamSource == "wireguard"
	out.LANNetwork, out.LANSubnet, out.LANAddress = wg.LANNetwork, wg.LANSubnet, wg.LANAddress
	out.Home = s.homeState(ctx, wg)
	out.Hosts = s.hostStatuses(ctx, wg)
	return out, nil
}

func (s *Service) homeState(ctx context.Context, wg WireGuard) WireGuardHome {
	if s.dk(ctx) == nil {
		return WireGuardHome{Error: "Docker isn't available"}
	}
	info, err := s.dk(ctx).InspectContainer(ctx, WireGuardContainer)
	if docker.IsNotFound(err) {
		// Not renamed from gsh-wireguard yet.
		info, err = s.dk(ctx).InspectContainer(ctx, legacyWireGuardContainer)
	}
	if docker.IsNotFound(err) {
		return WireGuardHome{State: "missing", Error: "The " + WireGuardContainer + " container is gone; set the tunnel up again."}
	}
	if err != nil {
		return WireGuardHome{Error: err.Error()}
	}
	h := WireGuardHome{State: info.State.Status, Running: info.State.Running && !info.State.Restarting, ExitCode: info.State.ExitCode, Error: info.State.Error}
	if wg.LANNetwork != "" {
		h.LANAddress = info.NetworkSettings.Networks[wg.LANNetwork].IPAddress
	}
	return h
}

// --- setup ---

// wireGuardScript installs wireguard-tools if needed, writes the proxy end's
// configuration from stdin and (re)starts the interface.
const wireGuardScript = `
PATH="$PATH:/usr/sbin:/sbin"
if ! command -v wg-quick >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q && apt-get install -y -q --no-install-recommends wireguard-tools || exit 1
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y wireguard-tools || exit 1
  else
    echo "hlm:nopkg"
    exit 2
  fi
fi
umask 077
mkdir -p /etc/wireguard
tmp=$(mktemp) || exit 1
cat > "$tmp"
install -m 600 "$tmp" ` + wireGuardConfPath + `; rm -f "$tmp"
if [ -d /run/systemd/system ]; then
  systemctl enable wg-quick@` + WireGuardInterface + ` >/dev/null 2>&1
  if ! systemctl restart wg-quick@` + WireGuardInterface + `; then
    journalctl -u wg-quick@` + WireGuardInterface + ` -n 20 --no-pager 2>/dev/null
    exit 5
  fi
else
  wg-quick down ` + WireGuardInterface + ` >/dev/null 2>&1
  wg-quick up ` + WireGuardInterface + ` || exit 5
fi
wg show ` + WireGuardInterface + ` >/dev/null && echo hlm:wgup
`

// SetupWireGuard sets up (or updates) the tunnel: the proxy end over SSH,
// the home end as a container on the Docker host. Keys are made the first
// time and kept after that. With UseForRelays, the tunnel becomes the
// proxy's home address, and applied relays are re-applied to use it.
// Progress goes to emit, if non-nil, as it happens.
func (s *Service) SetupWireGuard(ctx context.Context, in WireGuardInput, emit func(WireGuardEvent)) (WireGuardResult, error) {
	var res WireGuardResult
	if emit == nil {
		emit = func(WireGuardEvent) {}
	}
	step := func(msg string) { emit(WireGuardEvent{Stage: "step", Message: msg}) }
	output := func(line string) {
		if !strings.HasPrefix(line, "hlm:") { // the script's own markers
			emit(WireGuardEvent{Stage: "output", Message: line})
		}
	}
	if err := validateWireGuard(&in); err != nil {
		return res, err
	}
	if s.docker == nil {
		return res, errors.New("Docker isn't available, so the tunnel's end on this host can't run")
	}
	st, err := s.saved()
	if err != nil {
		return res, err
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()

	wg, _, err := s.tunnel()
	if err != nil {
		return res, err
	}
	if len(wg.Hosts) > 0 && wg.Network != "" && wg.Network != in.Network {
		return res, errors.New("other Docker hosts have ends of this tunnel, with addresses in its network: remove them before changing the tunnel network")
	}
	wg.Network, wg.ListenPort, wg.Endpoint = in.Network, in.ListenPort, in.Endpoint
	wg.LANNetwork, wg.LANAddress = in.LANNetwork, in.LANAddress
	if err := s.resolveLAN(ctx, &wg); err != nil {
		return res, err
	}
	if wg.LANSubnet != "" {
		for id, e := range wg.Hosts {
			if e.LANSubnet != "" && netip.MustParsePrefix(e.LANSubnet).Overlaps(netip.MustParsePrefix(wg.LANSubnet)) {
				return res, fmt.Errorf("the tunnel's end on %s already reaches %s; one end per LAN can reach servers' own addresses", id, e.LANSubnet)
			}
		}
	}
	for _, k := range []*string{&wg.ProxyKey, &wg.HomeKey} {
		if *k == "" {
			if *k, err = newWireGuardKey(); err != nil {
				return res, err
			}
		}
	}
	if host, _, _ := net.SplitHostPort(wg.endpoint(st)); host != "" {
		if a, err := netip.ParseAddr(host); err == nil && wg.prefix().Contains(a.Unmap()) {
			return res, fmt.Errorf("the proxy server's address %s is in the tunnel network %s; choose another tunnel network", a, wg.Network)
		}
	}
	proxyConf, homeConf, err := RenderWireGuard(wg, st)
	if err != nil {
		return res, err
	}

	// The proxy end first: if it can't be set up, nothing changes at home.
	// Routing the tunnel there mustn't take over the proxy server's own
	// networks, or it would cut itself off.
	step("Checking the proxy server's networks")
	proxyNets := []netip.Prefix{wg.prefix()}
	if wg.LANSubnet != "" {
		proxyNets = append(proxyNets, netip.MustParsePrefix(wg.LANSubnet))
	}
	r, err := s.run(ctx, routesScript, nil, nil)
	if err != nil {
		return res, err
	}
	if err := routeConflict(r.Stdout, proxyNets); err != nil {
		return res, fmt.Errorf("on the proxy server, %w", err)
	}
	step("Setting up WireGuard on the proxy server " + st.Host + " (installing wireguard-tools if it's missing)")
	r, err = s.run(ctx, wireGuardScript, []byte(proxyConf), output)
	res.Output = r.Output()
	if err != nil {
		return res, err
	}
	switch {
	case strings.Contains(r.Stdout, "hlm:wgup"):
		res.Output = strings.TrimSpace(strings.ReplaceAll(res.Output, "hlm:wgup", ""))
	case strings.Contains(r.Stdout, "hlm:nopkg"):
		return res, errors.New("only apt (Debian, Ubuntu) and dnf (Fedora, RHEL) systems can be set up automatically; install wireguard-tools on the proxy server yourself, then try again")
	case r.ExitCode == 1:
		return res, errors.New("installing wireguard-tools on the proxy server failed")
	default:
		return res, fmt.Errorf("starting WireGuard on the proxy server failed (exit status %d); its kernel may lack WireGuard support", r.ExitCode)
	}
	// The keys are in use on the proxy now, so keep them even if the home
	// end fails: trying again then only has to fix that end.
	wg.Configured = time.Now().UTC()
	if err := s.secrets.Set(secretWireGuard, wg); err != nil {
		return res, err
	}
	s.log.Info("set up WireGuard on the proxy server", "network", wg.Network, "port", wg.ListenPort)

	if err := s.startHome(ctx, wg, homeConf, step, output); err != nil {
		res.Status, _ = s.WireGuardStatus(ctx)
		return res, fmt.Errorf("WireGuard is running on the proxy server, but starting this end failed: %w", err)
	}
	s.log.Info("started the WireGuard tunnel's home end", "container", WireGuardContainer, "address", wg.HomeAddress())

	if in.UseForRelays && st.UpstreamSource != "wireguard" {
		st.UpstreamSource = "wireguard"
		if _, err := s.Save(st); err != nil {
			return res, err
		}
	}
	// Relay through the tunnel's (possibly new) address straight away;
	// applying also opens the tunnel's port in the firewall.
	state, _ := s.State()
	if st.UpstreamSource == "wireguard" && state.Applied != "" {
		step("Applying nginx on the proxy server to relay through the tunnel")
		a, err := s.Apply(ctx)
		res.Apply, res.Firewall = &a, a.Firewall
		for _, line := range strings.Split(a.Output, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				output(line)
			}
		}
		if err != nil {
			res.ApplyError = err.Error()
		}
	} else {
		if s.FirewallChosen() {
			step(fmt.Sprintf("Opening port %d/udp in the proxy server's firewall", wg.ListenPort))
		}
		s.applyMu.Lock()
		res.Firewall = s.syncFirewallAfter(ctx, s.wantedPorts(ctx))
		s.applyMu.Unlock()
	}
	res.Status, err = s.WireGuardStatus(ctx)
	return res, err
}

// startHome replaces the home end's container with one running conf,
// reporting steps and output as it goes.
func (s *Service) startHome(ctx context.Context, wg WireGuard, conf string, step, output func(string)) error {
	step("Stopping the old " + WireGuardContainer + " container")
	if err := s.removeHome(ctx); err != nil {
		return err
	}
	step("Pulling " + s.wgImage)
	pullErr := s.dk(ctx).PullImage(ctx, s.wgImage, func(p docker.PullProgress) {
		// Leave out the flood of byte counts; layer status changes are enough.
		if p.Status == "" || p.Status == "Downloading" || p.Status == "Extracting" || p.Status == "Waiting" {
			return
		}
		if p.ID != "" {
			output(p.ID + ": " + p.Status)
		} else {
			output(p.Status)
		}
	})
	if pullErr != nil {
		output("Pulling failed (" + pullErr.Error() + "); trying the image already on the host")
	}
	req := docker.CreateRequest{
		Image: s.wgImage,
		// homeScript, not the image's init: see homeScript.
		Entrypoint: []string{"/bin/sh", "-c", homeScript},
		Env:        []string{"TZ=Etc/UTC"},
		Labels:     map[string]string{LabelWireGuard: "home", labelHomeVersion: homeVersion},
		HostConfig: docker.HostConfig{
			// The tunnel's interface goes on the host, so its address
			// reaches the servers' published ports.
			NetworkMode:   "host",
			CapAdd:        []string{"NET_ADMIN"},
			Binds:         []string{WireGuardContainer + ":/config"},
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		},
	}
	if wg.LANNetwork != "" {
		// Its own address on the LAN, from which it forwards.
		req.HostConfig.NetworkMode = wg.LANNetwork
		req.HostConfig.Sysctls = map[string]string{"net.ipv4.ip_forward": "1"}
		if wg.LANAddress != "" {
			req.NetworkingConfig = &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{
				wg.LANNetwork: {IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: wg.LANAddress}},
			}}
		}
	}
	step("Creating the " + WireGuardContainer + " container")
	id, _, err := s.dk(ctx).CreateContainer(ctx, WireGuardContainer, req)
	if err != nil {
		if pullErr != nil {
			return fmt.Errorf("pull %s: %w", s.wgImage, pullErr)
		}
		return err
	}
	if wg.LANNetwork != "" {
		// And on the default bridge, to reach the host's published ports.
		if err := s.dk(ctx).ConnectNetwork(ctx, "bridge", id, nil); err != nil {
			return fmt.Errorf("connect to the bridge network: %w", err)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "wg_confs/", Mode: 0o700, ModTime: now})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "wg_confs/" + WireGuardInterface + ".conf", Mode: 0o600, Size: int64(len(conf)), ModTime: now})
	_, _ = tw.Write([]byte(conf))
	if err := tw.Close(); err != nil {
		return err
	}
	if err := s.dk(ctx).CopyTo(ctx, id, "/config", &buf); err != nil {
		return fmt.Errorf("write the configuration: %w", err)
	}
	if wg.LANNetwork == "" {
		step("Checking this host's networks")
		if err := s.prepareHostNetwork(ctx, wg, output); err != nil {
			_ = s.dk(ctx).RemoveContainer(context.WithoutCancel(ctx), id, true)
			return err
		}
	}
	step("Starting it")
	if err := s.dk(ctx).StartContainer(ctx, id); err != nil {
		return err
	}
	return s.followStartup(ctx, id, output)
}

// homeScript runs the home end in place of the image's init, which would
// run the tunnel as a VPN client: when a tunnel fails it deletes the default
// route, as a kill switch, and on the host's network that's the host's own.
// This brings gsh0 up and keeps it up until the container is stopped. If
// that cut off the default gateway anyway, it takes gsh0 down again and
// stays down, rather than being restarted into the same mistake. It logs
// what followStartup looks for.
const homeScript = `conf=/config/wg_confs/` + WireGuardInterface + `.conf
trap 'wg-quick down "$conf"; exit 0' TERM INT
gateway() { ip -4 route show default | awk '$2 == "via" { print $3; exit }'; }
idle() { while :; do sleep 3600 & wait $!; done; }
gw=$(gateway)
if ! wg-quick up "$conf"; then
  echo "**** Tunnel $conf ` + tunnelFailed + `! ****"
  exit 1
fi
if [ -n "$gw" ] && { [ -z "$(gateway)" ] || ip -4 route get "$gw" | grep -q "dev ` + WireGuardInterface + `"; }; then
  wg-quick down "$conf"
  echo "` + WireGuardInterface + ` took over the route to the default gateway $gw, so it was taken down again; choose a tunnel network that doesn't overlap this host's networks"
  echo "**** Tunnel $conf ` + tunnelFailed + `! ****"
  idle
fi
echo "**** ` + tunnelsUp + ` ****"
idle
`

// routesScript prints a machine's IPv4 routes, for routeConflict.
const routesScript = `PATH="$PATH:/usr/sbin:/sbin"
ip -4 -o route show 2>/dev/null
exit 0`

// prepareHostNetwork runs on the host's network before the home end starts
// there. It deletes a gsh0 left by a home end that didn't shut down cleanly
// (killed, or crashed): the interface outlives the container, and wg-quick
// refuses to bring up one that already exists. Then it checks the tunnel's
// network against the host's routes: overlapping them would send the
// host's own traffic, perhaps to its default gateway, into the tunnel. hlm
// may itself run in a container without the host's network, so a
// short-lived container on the host's network does this.
func (s *Service) prepareHostNetwork(ctx context.Context, wg WireGuard, output func(string)) error {
	const name = WireGuardContainer + "-cleanup"
	warn := func(err error) error {
		// homeScript still takes gsh0 down if it cuts off the gateway.
		output("Couldn't check this host's networks: " + err.Error())
		return nil
	}
	_ = s.dk(ctx).RemoveContainer(ctx, name, true)
	id, _, err := s.dk(ctx).CreateContainer(ctx, name, docker.CreateRequest{
		Image:      s.wgImage,
		Entrypoint: []string{"/bin/sh", "-c", "ip link del " + WireGuardInterface + " 2>/dev/null && echo hlm:removed\n" + routesScript},
		HostConfig: docker.HostConfig{NetworkMode: "host", CapAdd: []string{"NET_ADMIN"}},
	})
	if err != nil {
		return warn(err)
	}
	defer s.dk(ctx).RemoveContainer(context.WithoutCancel(ctx), id, true)
	if err := s.dk(ctx).StartContainer(ctx, id); err != nil {
		return warn(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.dk(ctx).WaitContainer(wctx, id); err != nil {
		return warn(err)
	}
	logs, err := s.dk(ctx).Logs(wctx, id, docker.LogsOptions{Tail: "all"})
	if err != nil {
		return warn(err)
	}
	defer logs.Close()
	b, err := io.ReadAll(logs)
	if err != nil {
		return warn(err)
	}
	if strings.Contains(string(b), "hlm:removed") {
		output("Removed a leftover " + WireGuardInterface + " interface from the host")
	}
	if err := routeConflict(string(b), []netip.Prefix{wg.prefix()}); err != nil {
		return fmt.Errorf("on this host, %w", err)
	}
	return nil
}

// routeConflict checks routes, as "ip -4 -o route show" prints them, for
// any that the tunnel's networks would take over: overlapping networks, or
// a default gateway inside them. Routes through gsh0 are the tunnel's own.
func routeConflict(routes string, nets []netip.Prefix) error {
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "unicast":
			f = f[1:]
		case "local", "broadcast", "multicast", "unreachable", "blackhole", "prohibit", "throw", "nat":
			continue // not a route out of an interface
		}
		if len(f) == 0 {
			continue
		}
		var dev, via string
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "dev":
				dev = f[i+1]
			case "via":
				via = f[i+1]
			}
		}
		if dev == WireGuardInterface {
			continue
		}
		for _, n := range nets {
			if f[0] == "default" {
				if gw, err := netip.ParseAddr(via); err == nil && n.Contains(gw) {
					return fmt.Errorf("the default gateway %s is in %s; choose another tunnel network", gw, n)
				}
				continue
			}
			dst, err := netip.ParsePrefix(f[0])
			if err != nil {
				a, err := netip.ParseAddr(f[0])
				if err != nil {
					continue
				}
				dst = netip.PrefixFrom(a, a.BitLen())
			}
			if dst.Overlaps(n) {
				return fmt.Errorf("%s overlaps the network %s on %s; choose another tunnel network", n, dst, dev)
			}
		}
	}
	return nil
}

// RecreateWireGuardHome replaces the home end's container, e.g. to run it on
// a newer image, with the tunnel's current configuration. The proxy's end
// is left as it is.
func (s *Service) RecreateWireGuardHome(ctx context.Context, step, output func(string)) error {
	if s.docker == nil {
		return errors.New("Docker isn't available")
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	wg, ok, err := s.tunnel()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the WireGuard tunnel isn't set up")
	}
	st, err := s.saved()
	if err != nil {
		return err
	}
	_, homeConf, err := RenderWireGuard(wg, st)
	if err != nil {
		return err
	}
	if err := s.startHome(ctx, wg, homeConf, step, output); err != nil {
		return err
	}
	s.log.Info("recreated the WireGuard tunnel's home end", "container", WireGuardContainer)
	return nil
}

// UpgradeWireGuardHome recreates a home end that still runs the image's own
// init, which can delete the host's default route, so it runs homeScript.
// It also gives a home end from before the rename from gsh its new name.
// It's meant to run once when hlm starts.
func (s *Service) UpgradeWireGuardHome(ctx context.Context) {
	if s.docker == nil {
		return
	}
	if _, ok, err := s.tunnel(); err != nil || !ok {
		return
	}
	s.renameLegacyHome(ctx)
	info, err := s.dk(ctx).InspectContainer(ctx, WireGuardContainer)
	if err != nil || info.Config.Labels[LabelWireGuard] == "" || info.Config.Labels[labelHomeVersion] == homeVersion {
		return
	}
	s.log.Info("recreating the WireGuard tunnel's home end so it can't break the host's networking", "container", WireGuardContainer)
	noop := func(string) {}
	if err := s.RecreateWireGuardHome(ctx, noop, noop); err != nil {
		s.log.Error("recreate the WireGuard tunnel's home end", "err", err)
	}
}

// renameLegacyHome renames a gsh-wireguard container to WireGuardContainer.
// Renaming leaves it running, so the tunnel stays up; its volume keeps the
// old name until the home end is next recreated.
func (s *Service) renameLegacyHome(ctx context.Context) {
	if _, err := s.dk(ctx).InspectContainer(ctx, WireGuardContainer); !docker.IsNotFound(err) {
		return
	}
	if _, err := s.dk(ctx).InspectContainer(ctx, legacyWireGuardContainer); err != nil {
		return
	}
	if err := s.dk(ctx).RenameContainer(ctx, legacyWireGuardContainer, WireGuardContainer); err != nil {
		s.log.Warn("rename the WireGuard tunnel's home end", "from", legacyWireGuardContainer, "to", WireGuardContainer, "err", err)
		return
	}
	s.log.Info("renamed the WireGuard tunnel's home end", "from", legacyWireGuardContainer, "to", WireGuardContainer)
}

// tunnelsUp is what the default image logs once its tunnels are up.
const tunnelsUp = "All tunnels are now active"

// tunnelFailed is what the default image logs when wg-quick fails.
const tunnelFailed = "failed, will stop all others"

// followStartup passes on the home end's log while it brings the tunnel up:
// until the image says it's done or failed, the container stops, or a few
// seconds pass. It returns an error if the image says the tunnel failed.
func (s *Service) followStartup(ctx context.Context, id string, output func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	logs, err := s.dk(ctx).Logs(ctx, id, docker.LogsOptions{Follow: true, Tail: "all"})
	if err != nil {
		output("Couldn't read the container's log: " + err.Error())
		return nil
	}
	defer logs.Close()
	sc := bufio.NewScanner(logs)
	var last string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r ")
		if line == "" {
			continue
		}
		output(line)
		switch {
		case strings.Contains(line, tunnelsUp):
			return nil
		case strings.Contains(line, tunnelFailed):
			if last != "" {
				return fmt.Errorf("WireGuard didn't start in the %s container: %s", WireGuardContainer, last)
			}
			return fmt.Errorf("WireGuard didn't start in the %s container; see its log", WireGuardContainer)
		}
		last = strings.Trim(line, "* ")
	}
	return nil
}

// removeHome removes the home end's container and the volume holding its
// key. The container is stopped first, so wg-quick takes the tunnel down
// (and undoes its iptables rules): killed, it would leave gsh0 behind on
// the host's network, and tearing a live WireGuard interface out of a
// network namespace can stall the host's networking.
// A gsh-wireguard container or volume from before the rename goes too.
func (s *Service) removeHome(ctx context.Context) error {
	names := []string{WireGuardContainer, legacyWireGuardContainer}
	for _, name := range names {
		if err := s.dk(ctx).StopContainer(ctx, name, 15); err != nil && !docker.IsNotFound(err) {
			s.log.Warn("stop the WireGuard home end gracefully", "err", err)
		}
		if err := s.dk(ctx).RemoveContainer(ctx, name, true); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("remove the old %s container: %w", name, err)
		}
	}
	for _, name := range names {
		if err := s.dk(ctx).RemoveVolume(ctx, name); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("remove the old %s volume: %w", name, err)
		}
	}
	return nil
}

// CheckWireGuard asks the proxy end when it last heard from this host, and
// pings this host through the tunnel.
func (s *Service) CheckWireGuard(ctx context.Context) (WireGuardCheck, error) {
	var out WireGuardCheck
	wg, ok, err := s.tunnel()
	if err != nil {
		return out, err
	}
	if !ok {
		return out, errors.New("the WireGuard tunnel isn't set up")
	}
	script := `PATH="$PATH:/usr/sbin:/sbin"
wg show ` + WireGuardInterface + ` latest-handshakes || exit 3
if command -v ping >/dev/null 2>&1; then
  if ping -c 2 -W 2 ` + wg.HomeAddress().String() + ` >/dev/null 2>&1; then echo hlm:ping=yes; else echo hlm:ping=no; fi
fi`
	r, err := s.run(ctx, script, nil, nil)
	if err != nil {
		return out, err
	}
	if r.ExitCode != 0 {
		out.Output = r.Output()
		return out, errors.New("WireGuard isn't running on the proxy server; set the tunnel up again")
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "hlm:ping=yes", line == "hlm:ping=no":
			reachable := line == "hlm:ping=yes"
			out.Reachable = &reachable
		default:
			// "<public key>\t<unix time>"; 0 means never.
			key, ts, found := strings.Cut(line, "\t")
			n, err := strconv.ParseInt(ts, 10, 64)
			if !found || err != nil || n <= 0 {
				continue
			}
			at := time.Unix(n, 0).UTC()
			if pub, _ := wireGuardPublic(wg.HomeKey); key == pub {
				out.Handshake = at
			}
			for id, e := range wg.Hosts {
				if pub, _ := wireGuardPublic(e.HomeKey); key == pub {
					if out.Hosts == nil {
						out.Hosts = map[string]time.Time{}
					}
					out.Hosts[id] = at
				}
			}
		}
	}
	return out, nil
}

// RemoveWireGuard takes the tunnel down on both ends and forgets it. It
// refuses while applied relays go through it.
func (s *Service) RemoveWireGuard(ctx context.Context) (WireGuardResult, error) {
	var res WireGuardResult
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	st, ok, err := s.Settings()
	if err != nil {
		return res, err
	}
	if ok && st.UpstreamSource == "wireguard" {
		if state, _ := s.State(); state.Applied != "" {
			return res, errors.New("the relays send traffic through the tunnel; choose another home address and apply, or remove the relays, first")
		}
	}
	if ok {
		script := `PATH="$PATH:/usr/sbin:/sbin"
if [ -d /run/systemd/system ]; then systemctl disable --now wg-quick@` + WireGuardInterface + ` 2>/dev/null; fi
command -v wg-quick >/dev/null 2>&1 && ip link show ` + WireGuardInterface + ` >/dev/null 2>&1 && wg-quick down ` + WireGuardInterface + `
rm -f ` + wireGuardConfPath + `
exit 0`
		r, err := s.run(ctx, script, nil, nil)
		res.Output = r.Output()
		if err != nil {
			return res, fmt.Errorf("take WireGuard down on the proxy server: %w", err)
		}
	}
	if s.docker != nil {
		if err := s.removeHome(ctx); err != nil {
			return res, err
		}
	}
	// The ends on other hosts go too.
	if wg, ok, _ := s.tunnel(); ok {
		for id := range wg.Hosts {
			if hctx, err := s.onHost(ctx, id); err == nil {
				if err := s.removeHome(hctx); err != nil {
					s.log.Warn("remove the tunnel's end on another host", "host", id, "err", err)
				}
			}
		}
	}
	if err := s.secrets.Delete(secretWireGuard); err != nil {
		return res, err
	}
	if ok && st.UpstreamSource == "wireguard" {
		st.UpstreamSource = "manual"
		if _, err := s.Save(st); err != nil {
			return res, err
		}
	}
	s.log.Info("removed the WireGuard tunnel")
	s.applyMu.Lock()
	res.Firewall = s.syncFirewallAfter(ctx, s.wantedPorts(ctx))
	s.applyMu.Unlock()
	res.Status, err = s.WireGuardStatus(ctx)
	return res, err
}

// The home end of a host other than this one runs on that host's Docker:
// its client travels in the context.
type dockerKey struct{}

func withDocker(ctx context.Context, d Containers) context.Context {
	return context.WithValue(ctx, dockerKey{}, d)
}

// dk is the Docker the context's home end runs on: this host's, unless
// another host's was put in the context.
func (s *Service) dk(ctx context.Context) Containers {
	if d, ok := ctx.Value(dockerKey{}).(Containers); ok {
		return d
	}
	return s.docker
}
