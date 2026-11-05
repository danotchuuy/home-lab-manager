package cftunnel

// Tunnels on other Docker hosts. A tunnel's connectors share its traffic,
// so a connector can't be pinned to one host's servers within a tunnel:
// instead, a host can have a tunnel of its own (hlm-<host>), with cloudflared
// running there. A server's routes go through its host's tunnel when it has
// one, reaching it as a local connector would (its published ports on
// localhost, or its LAN address in LAN mode); otherwise through this host's
// tunnel, over the LAN, as before.

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

const (
	// secretHostTunnels are the other hosts' tunnels, by host ID.
	secretHostTunnels = "cloudflare-host-tunnels"
	// secretHostApplied is what was last applied to each of them.
	secretHostApplied = "cloudflare-host-applied"
)

// UseDockerHosts gives other Docker hosts' Docker, by host ID, for
// cloudflared there; *docker.Client implements Containers.
func (s *Service) UseDockerHosts(f func(host string) (Containers, error)) { s.dockerFor = f }

type dockerKey struct{}

// dk is the Docker the context's connector runs on: this host's, unless
// another host's was put in the context.
func (s *Service) dk(ctx context.Context) Containers {
	if d, ok := ctx.Value(dockerKey{}).(Containers); ok {
		return d
	}
	return s.docker
}

// onHost is ctx with a host's Docker (this host's for "" or local).
func (s *Service) onHost(ctx context.Context, host string) (context.Context, error) {
	if host == "" || host == manager.LocalHost {
		return ctx, nil
	}
	if s.dockerFor == nil {
		return nil, errors.New("hlm can't reach other Docker hosts here")
	}
	d, err := s.dockerFor(host)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, dockerKey{}, d), nil
}

func isLocal(host string) bool { return host == "" || host == manager.LocalHost }

func (s *Service) hostTunnels() (map[string]Tunnel, error) {
	m := map[string]Tunnel{}
	_, err := s.secrets.Get(secretHostTunnels, &m)
	return m, err
}

// tunnelOn is a host's tunnel.
func (s *Service) tunnelOn(host string) (Tunnel, bool, error) {
	if isLocal(host) {
		return s.tunnel()
	}
	m, err := s.hostTunnels()
	if err != nil {
		return Tunnel{}, false, err
	}
	t, ok := m[host]
	return t, ok, nil
}

func (s *Service) saveTunnelOn(host string, t Tunnel) error {
	if isLocal(host) {
		return s.secrets.Set(secretTunnel, t)
	}
	m, err := s.hostTunnels()
	if err != nil {
		return err
	}
	m[host] = t
	return s.secrets.Set(secretHostTunnels, m)
}

func (s *Service) deleteTunnelOn(host string) error {
	if isLocal(host) {
		return s.secrets.Delete(secretTunnel)
	}
	m, err := s.hostTunnels()
	if err != nil {
		return err
	}
	delete(m, host)
	return s.secrets.Set(secretHostTunnels, m)
}

// tunnels are every tunnel, by host (this one's as manager.LocalHost).
func (s *Service) tunnels() (map[string]Tunnel, error) {
	m, err := s.hostTunnels()
	if err != nil {
		return nil, err
	}
	if t, ok, err := s.tunnel(); err != nil {
		return nil, err
	} else if ok {
		m[manager.LocalHost] = t
	}
	return m, nil
}

func (s *Service) hostApplied() (map[string]appliedState, error) {
	m := map[string]appliedState{}
	_, err := s.secrets.Get(secretHostApplied, &m)
	return m, err
}

// appliedAll is what was last applied to each tunnel, by host; a tunnel
// that was made again since starts empty.
func (s *Service) appliedAll(tunnels map[string]Tunnel) (map[string]appliedState, error) {
	m, err := s.hostApplied()
	if err != nil {
		return nil, err
	}
	if ap, ok, err := s.applied(); err != nil {
		return nil, err
	} else if ok {
		m[manager.LocalHost] = ap
	}
	for host, ap := range m {
		if t, ok := tunnels[host]; ok && ap.TunnelID != t.ID {
			m[host] = appliedState{}
		}
	}
	return m, nil
}

func (s *Service) saveAppliedOn(host string, ap appliedState) error {
	if isLocal(host) {
		return s.secrets.Set(secretApplied, ap)
	}
	m, err := s.hostApplied()
	if err != nil {
		return err
	}
	m[host] = ap
	return s.secrets.Set(secretHostApplied, m)
}

func (s *Service) deleteAppliedOn(host string) error {
	if isLocal(host) {
		return s.secrets.Delete(secretApplied)
	}
	m, err := s.hostApplied()
	if err != nil {
		return err
	}
	delete(m, host)
	return s.secrets.Set(secretHostApplied, m)
}

// via is the host whose tunnel carries a route: the server's host, if it
// has a tunnel, otherwise this one.
func via(r Route, tunnels map[string]Tunnel) string {
	if r.Server != "" && !r.Hlm {
		if host, _ := manager.SplitID(r.Server); !isLocal(host) {
			if _, ok := tunnels[host]; ok {
				return host
			}
		}
	}
	return manager.LocalHost
}

func sortedHosts(m map[string]Tunnel) []string {
	return slices.Sorted(maps.Keys(m))
}
