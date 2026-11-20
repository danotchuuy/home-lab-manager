package hosts

import (
	"context"
	"errors"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/nut"
)

// UPS reads a UPS for a host: a host with hlm's agent asks the NUT server
// itself (it may be on a network hlm can't reach); for this host and SSH
// or TLS hosts, hlm asks.
func (r *Registry) UPS(ctx context.Context, id string, q nut.Query) (nut.Status, error) {
	agent, err := r.viaAgent(id)
	if err != nil {
		return nut.Status{}, err
	}
	if agent {
		return r.opts.Agents.UPS(ctx, id, q)
	}
	return nut.Fetch(ctx, q)
}

// UPSList lists a NUT server's UPSes, as seen from a host (as UPS asks).
func (r *Registry) UPSList(ctx context.Context, id string, q nut.Query) ([]nut.UPS, error) {
	agent, err := r.viaAgent(id)
	if err != nil {
		return nil, err
	}
	if agent {
		return r.opts.Agents.UPSList(ctx, id, q)
	}
	return nut.List(ctx, q)
}

// viaAgent reports whether a host's UPS is read by its agent.
func (r *Registry) viaAgent(id string) (bool, error) {
	if id == "" || id == manager.LocalHost {
		return false, nil
	}
	h, err := r.Get(id)
	if err != nil {
		return false, err
	}
	if h.Kind != KindAgent {
		return false, nil
	}
	if r.opts.Agents == nil {
		return false, agentUnavailable
	}
	return true, nil
}

// ErrNoShutdown is the answer for a host hlm can't shut down.
var ErrNoShutdown = errors.New("only hosts with hlm's agent can be shut down")

// CanShutdown reports whether hlm can shut a host down: one with its agent.
func (r *Registry) CanShutdown(id string) bool {
	agent, err := r.viaAgent(id)
	return err == nil && agent
}

// Shutdown tells a host's agent to shut the host down.
func (r *Registry) Shutdown(ctx context.Context, id string) error {
	if !r.CanShutdown(id) {
		return ErrNoShutdown
	}
	return r.opts.Agents.Shutdown(ctx, id)
}
