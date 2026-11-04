package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// UpstreamState records what the relay was last configured with and, when
// the home address comes from UniFi, what UniFi last reported.
type UpstreamState struct {
	// Applied is the home address in the configuration on the relay, empty
	// if hlm's configuration isn't applied.
	Applied   string    `json:"applied,omitempty"`
	AppliedAt time.Time `json:"appliedAt,omitzero"`
	// Current is the address UniFi reported at CheckedAt.
	Current   string    `json:"current,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	// Error is why the last check or automatic update failed.
	Error string `json:"error,omitempty"`
}

// UseWANLookup sets how the "unifi" home address source finds the
// gateway's public address.
func (s *Service) UseWANLookup(fn func(ctx context.Context, wan string) (string, error)) {
	s.wanLookup = fn
}

func (s *Service) lookupWAN(ctx context.Context, wan string) (string, error) {
	if s.wanLookup == nil {
		return "", errors.New("UniFi isn't available")
	}
	return s.wanLookup(ctx, wan)
}

// State returns the saved upstream state.
func (s *Service) State() (UpstreamState, error) {
	var st UpstreamState
	_, err := s.secrets.Get(secretState, &st)
	return st, err
}

func (s *Service) updateState(fn func(*UpstreamState)) error {
	st, err := s.State()
	if err != nil {
		return err
	}
	fn(&st)
	return s.secrets.Set(secretState, st)
}

func (s *Service) recordApplied(upstream string, servers []Proxied) error {
	applied := map[string][]forwarding.Mapping{}
	for _, p := range servers {
		if len(p.Ports) > 0 {
			applied[p.Server] = p.Ports
		}
	}
	if err := s.secrets.Set(secretApplied, applied); err != nil {
		return err
	}
	return s.updateState(func(st *UpstreamState) {
		st.Applied = upstream
		st.AppliedAt = time.Now().UTC()
		if upstream != "" {
			st.Error = ""
		}
	})
}

// CheckWAN looks up the home address from UniFi and, if it changed since
// the configuration was applied, applies it again. It does nothing unless
// the home address comes from UniFi, and never applies a configuration
// that wasn't applied before.
func (s *Service) CheckWAN(ctx context.Context) (UpstreamState, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	settings, err := s.saved()
	if err != nil {
		return UpstreamState{}, err
	}
	if settings.UpstreamSource != "unifi" {
		return s.State()
	}

	ip, lookupErr := s.lookupWAN(ctx, settings.UnifiWAN)
	if err := s.updateState(func(st *UpstreamState) {
		st.CheckedAt = time.Now().UTC()
		if lookupErr != nil {
			st.Error = "Looking up the home address from UniFi failed: " + lookupErr.Error()
			return
		}
		st.Current, st.Error = ip, ""
	}); err != nil {
		return UpstreamState{}, err
	}
	state, err := s.State()
	if err != nil || lookupErr != nil {
		return state, errors.Join(lookupErr, err)
	}
	if state.Applied == "" || state.Applied == ip {
		return state, nil
	}

	s.log.Info("home address changed; updating the proxy", "from", state.Applied, "to", ip)
	from := state.Applied
	if _, applyErr := s.Apply(ctx); applyErr != nil {
		msg := fmt.Sprintf("The home address changed to %s, but updating the proxy failed: %v", ip, applyErr)
		s.log.Error("update proxy for new home address", "err", applyErr)
		s.notify(notify.Event{
			Kind: notify.KindProxyUpdateFailed, Title: "Updating the proxy for a new home address failed",
			Message: msg, Link: "/proxy", Fields: map[string]string{"Old address": from, "New address": ip},
		})
		if err := s.updateState(func(st *UpstreamState) { st.Error = msg }); err != nil {
			return state, err
		}
		state, _ = s.State()
		return state, errors.New(msg)
	}
	if s.notifier != nil {
		s.notifier.Clear(notify.DedupeKey(notify.KindProxyUpdateFailed, ""))
	}
	s.notify(notify.Event{
		Kind: notify.KindWANChanged, Title: "Home address changed to " + ip,
		Message: fmt.Sprintf("The home address changed from %s to %s, and the proxy was updated.", from, ip),
		Link:    "/proxy", Fields: map[string]string{"Old address": from, "New address": ip},
	})
	return s.State()
}

// RunWANWatcher checks the home address every interval until ctx is done.
func (s *Service) RunWANWatcher(ctx context.Context, interval time.Duration) {
	check := func() {
		st, ok, err := s.Settings()
		if err != nil || !ok || st.UpstreamSource != "unifi" {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if _, err := s.CheckWAN(cctx); err != nil && ctx.Err() == nil {
			s.log.Warn("check home address", "err", err)
		}
	}
	check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// UseNotifier sets where home address changes are reported.
func (s *Service) UseNotifier(n notify.Notifier) { s.notifier = n }

func (s *Service) notify(e notify.Event) {
	if s.notifier != nil {
		s.notifier.Notify(e)
	}
}
