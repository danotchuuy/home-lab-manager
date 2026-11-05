package proxy

// The WireGuard tunnel's ends on other Docker hosts. Each is another peer of
// the proxy server's end, with its own key and address in the tunnel, and
// runs in a container on its host, as this host's end does here. nginx
// relays a server on that host to that address, so servers on several
// machines can all be relayed through the tunnel, wherever those machines
// are.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// UseDockerHosts gives the Docker of other hosts, by host ID, for the
// tunnel's ends there; *docker.Client implements Containers.
func (s *Service) UseDockerHosts(f func(host string) (Containers, error)) { s.dockerFor = f }

// localHost is this host's ID, as servers' IDs name it.
const localHost = "local"

// hostOfID is the Docker host of a server's ID (name@host), or localHost.
func hostOfID(id string) string {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[i+1:]
	}
	return localHost
}

// onHost is ctx with another host's Docker, for its end.
func (s *Service) onHost(ctx context.Context, host string) (context.Context, error) {
	if s.dockerFor == nil {
		return nil, errors.New("hlm can't reach other Docker hosts here")
	}
	d, err := s.dockerFor(host)
	if err != nil {
		return nil, err
	}
	return withDocker(ctx, d), nil
}

// HostWireGuardInput is how another host's end runs: on its network, or in
// LAN mode on one of its macvlan/ipvlan networks.
type HostWireGuardInput struct {
	LANNetwork string `json:"lanNetwork"`
	LANAddress string `json:"lanAddress"`
}

// WireGuardHostStatus is the tunnel's end on another host.
type WireGuardHostStatus struct {
	Host          string        `json:"host"`
	HomeAddress   string        `json:"homeAddress"`
	HomePublicKey string        `json:"homePublicKey"`
	ConfiguredAt  time.Time     `json:"configuredAt,omitzero"`
	LANNetwork    string        `json:"lanNetwork,omitempty"`
	LANSubnet     string        `json:"lanSubnet,omitempty"`
	LANAddress    string        `json:"lanAddress,omitempty"`
	Home          WireGuardHome `json:"home"`
}

// hostStatuses describes the ends on other hosts.
func (s *Service) hostStatuses(ctx context.Context, wg WireGuard) []WireGuardHostStatus {
	out := []WireGuardHostStatus{}
	for _, id := range slices.Sorted(maps.Keys(wg.Hosts)) {
		e := wg.Hosts[id]
		end, _ := wg.end(id)
		hs := WireGuardHostStatus{Host: id, HomeAddress: e.Address, ConfiguredAt: e.Configured,
			LANNetwork: e.LANNetwork, LANSubnet: e.LANSubnet, LANAddress: e.LANAddress}
		hs.HomePublicKey, _ = wireGuardPublic(e.HomeKey)
		if hctx, err := s.onHost(ctx, id); err != nil {
			hs.Home = WireGuardHome{Error: err.Error()}
		} else {
			hs.Home = s.homeState(hctx, end)
		}
		out = append(out, hs)
	}
	return out
}

// pushProxyEnd writes the proxy end's configuration (every peer) and
// restarts it.
func (s *Service) pushProxyEnd(ctx context.Context, wg WireGuard, extraNets []netip.Prefix, step, output func(string)) (string, error) {
	st, err := s.saved()
	if err != nil {
		return "", err
	}
	proxyConf, _, err := RenderWireGuard(wg, st)
	if err != nil {
		return "", err
	}
	if len(extraNets) > 0 {
		step("Checking the proxy server's networks")
		r, err := s.run(ctx, routesScript, nil, nil)
		if err != nil {
			return "", err
		}
		if err := routeConflict(r.Stdout, extraNets); err != nil {
			return "", fmt.Errorf("on the proxy server, %w", err)
		}
	}
	step("Updating WireGuard on the proxy server " + st.Host)
	r, err := s.run(ctx, wireGuardScript, []byte(proxyConf), output)
	out := strings.TrimSpace(strings.ReplaceAll(r.Output(), "hlm:wgup", ""))
	if err != nil {
		return out, err
	}
	if !strings.Contains(r.Stdout, "hlm:wgup") {
		return out, fmt.Errorf("restarting WireGuard on the proxy server failed (exit status %d)", r.ExitCode)
	}
	return out, nil
}

// SetupHostWireGuard sets up (or updates) the tunnel's end on another
// Docker host: a peer on the proxy server, and a container there. The
// tunnel itself (this host's end) has to be set up first.
func (s *Service) SetupHostWireGuard(ctx context.Context, host string, in HostWireGuardInput, emit func(WireGuardEvent)) (WireGuardResult, error) {
	var res WireGuardResult
	if emit == nil {
		emit = func(WireGuardEvent) {}
	}
	step := func(msg string) { emit(WireGuardEvent{Stage: "step", Message: msg}) }
	output := func(line string) {
		if !strings.HasPrefix(line, "hlm:") {
			emit(WireGuardEvent{Stage: "output", Message: line})
		}
	}
	if host == "" || host == localHost {
		return res, errors.New("this host's end is the tunnel itself: set it up above")
	}
	in.LANNetwork, in.LANAddress = strings.TrimSpace(in.LANNetwork), strings.TrimSpace(in.LANAddress)
	if in.LANNetwork == "" {
		in.LANAddress = ""
	}
	if in.LANAddress != "" {
		if a, err := netip.ParseAddr(in.LANAddress); err != nil || !a.Is4() {
			return res, fmt.Errorf("invalid LAN address %q: use an IPv4 address", in.LANAddress)
		}
	}
	hctx, err := s.onHost(ctx, host)
	if err != nil {
		return res, err
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	wg, ok, err := s.tunnel()
	if err != nil {
		return res, err
	}
	if !ok || wg.ProxyKey == "" {
		return res, errors.New("set up the WireGuard tunnel (this host's end) first")
	}
	e := wg.Hosts[host]
	if e.Address == "" {
		a, err := wg.freeAddress()
		if err != nil {
			return res, err
		}
		e.Address = a.String()
	}
	if e.HomeKey == "" {
		if e.HomeKey, err = newWireGuardKey(); err != nil {
			return res, err
		}
	}
	// LAN mode, resolved on that host's Docker.
	probe := wg
	probe.LANNetwork, probe.LANAddress = in.LANNetwork, in.LANAddress
	if err := s.resolveLAN(hctx, &probe); err != nil {
		return res, fmt.Errorf("on %s: %w", host, err)
	}
	e.LANNetwork, e.LANAddress, e.LANSubnet, e.BridgeGateway = in.LANNetwork, in.LANAddress, probe.LANSubnet, probe.BridgeGateway
	// The proxy routes each LAN subnet to one end only.
	if e.LANSubnet != "" {
		subnet := netip.MustParsePrefix(e.LANSubnet)
		if wg.LANSubnet != "" && netip.MustParsePrefix(wg.LANSubnet).Overlaps(subnet) {
			return res, fmt.Errorf("this host's end already reaches %s; one end per LAN can reach servers' own addresses", wg.LANSubnet)
		}
		for id, other := range wg.Hosts {
			if id != host && other.LANSubnet != "" && netip.MustParsePrefix(other.LANSubnet).Overlaps(subnet) {
				return res, fmt.Errorf("the end on %s already reaches %s; one end per LAN can reach servers' own addresses", id, other.LANSubnet)
			}
		}
	}
	if wg.Hosts == nil {
		wg.Hosts = map[string]WireGuardEnd{}
	}
	e.Configured = time.Now().UTC()
	wg.Hosts[host] = e
	var extra []netip.Prefix
	if e.LANSubnet != "" {
		extra = append(extra, netip.MustParsePrefix(e.LANSubnet))
	}
	if res.Output, err = s.pushProxyEnd(ctx, wg, extra, step, output); err != nil {
		return res, err
	}
	// The proxy knows the peer now: keep it, so trying again only has to
	// fix the host's end.
	if err := s.secrets.Set(secretWireGuard, wg); err != nil {
		return res, err
	}
	end, _ := wg.end(host)
	st, _ := s.saved()
	_, homeConf, err := RenderWireGuard(end, st)
	if err != nil {
		return res, err
	}
	if err := s.startHome(hctx, end, homeConf, step, output); err != nil {
		res.Status, _ = s.WireGuardStatus(ctx)
		return res, fmt.Errorf("the proxy server knows %s's end now, but starting it there failed: %w", host, err)
	}
	s.log.Info("started the WireGuard tunnel's end on another host", "host", host, "address", e.Address)
	s.reapplyThroughTunnel(ctx, &res, step, output)
	res.Status, err = s.WireGuardStatus(ctx)
	return res, err
}

// RemoveHostWireGuard takes the tunnel's end on another host away: its
// container there, and its peer on the proxy server.
func (s *Service) RemoveHostWireGuard(ctx context.Context, host string) (WireGuardResult, error) {
	var res WireGuardResult
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	wg, ok, err := s.tunnel()
	if err != nil {
		return res, err
	}
	if _, has := wg.Hosts[host]; !ok || !has {
		return res, fmt.Errorf("%s has no end of the tunnel", host)
	}
	if hctx, err := s.onHost(ctx, host); err == nil {
		if err := s.removeHome(hctx); err != nil {
			s.log.Warn("remove the tunnel's end on another host", "host", host, "err", err)
		}
	}
	delete(wg.Hosts, host)
	nop := func(string) {}
	if res.Output, err = s.pushProxyEnd(ctx, wg, nil, nop, nop); err != nil {
		return res, err
	}
	if err := s.secrets.Set(secretWireGuard, wg); err != nil {
		return res, err
	}
	s.log.Info("removed the WireGuard tunnel's end on another host", "host", host)
	s.reapplyThroughTunnel(ctx, &res, nop, nop)
	res.Status, err = s.WireGuardStatus(ctx)
	return res, err
}

// RecreateWireGuardHomeOn replaces a host's end's container (this host's,
// or another's), e.g. to run it on a newer image.
func (s *Service) RecreateWireGuardHomeOn(ctx context.Context, host string, step, output func(string)) error {
	if host == "" || host == localHost {
		return s.RecreateWireGuardHome(ctx, step, output)
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	wg, ok, err := s.tunnel()
	if err != nil {
		return err
	}
	end, has := wg.end(host)
	if !ok || !has {
		return fmt.Errorf("%s has no end of the WireGuard tunnel", host)
	}
	hctx, err := s.onHost(ctx, host)
	if err != nil {
		return err
	}
	st, err := s.saved()
	if err != nil {
		return err
	}
	_, homeConf, err := RenderWireGuard(end, st)
	if err != nil {
		return err
	}
	return s.startHome(hctx, end, homeConf, step, output)
}

// reapplyThroughTunnel re-applies applied relays that go through the
// tunnel, so they follow its ends.
func (s *Service) reapplyThroughTunnel(ctx context.Context, res *WireGuardResult, step, output func(string)) {
	st, ok, _ := s.Settings()
	state, _ := s.State()
	if !ok || st.UpstreamSource != "wireguard" || state.Applied == "" {
		return
	}
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
}
