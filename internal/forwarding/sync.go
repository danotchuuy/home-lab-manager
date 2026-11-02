package forwarding

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Whether a port's setting is in place, for each place it can be set up
// (UniFi, the nginx proxy, the proxy's firewall).
const (
	SyncDone     = "synced"   // wanted and in place
	SyncPending  = "pending"  // wanted, not in place (yet)
	SyncRemoving = "removing" // not wanted, but still in place
	SyncConflict = "conflict" // wanted, but something else uses the port
	SyncOff      = "off"      // not wanted, not in place
)

// SyncState is a port's state in one place, with why if it isn't in sync.
type SyncState struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// PortStates compares each of a server's ports with the gateway's rules, by
// Mapping.Key. It returns nil when UniFi isn't set up.
func (s *Service) PortStates(ctx context.Context, server string) (map[string]SyncState, error) {
	cl, cfg, err := s.client()
	if errors.Is(err, ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := s.servers.Inspect(ctx, server)
	if err != nil {
		return nil, err
	}
	st := s.status(ctx, cl, cfg, info)
	if st.Error != "" {
		return nil, errors.New(st.Error)
	}
	ports, err := s.ServerPorts(info)
	if err != nil {
		return nil, err
	}
	out := map[string]SyncState{}
	for _, m := range ports {
		port := strconv.Itoa(m.Port)
		if !m.Forward {
			out[m.Key()] = SyncState{State: SyncOff}
			for _, pf := range st.Stale {
				if rangesOverlap(pf.DstPort, port) && protosOverlap(pf.Proto, m.Proto) {
					out[m.Key()] = SyncState{SyncRemoving, "Still forwarded by " + pf.Name + "; Update rules removes it."}
				}
			}
			continue
		}
		state := SyncState{SyncPending, "Not forwarded yet; Update rules adds it."}
		if st.Note != "" {
			state.Detail = st.Note
		}
		for _, r := range st.Rules {
			if !rangesOverlap(r.DstPort, port) || !protosOverlap(r.Proto, m.Proto) {
				continue
			}
			switch {
			case r.State == "forwarded":
				state = SyncState{State: SyncDone}
			case r.State == "outdated":
				state = SyncState{SyncPending, "The rule on the gateway is out of date; Update rules fixes it."}
			case len(r.Conflicts) > 0:
				names := make([]string, len(r.Conflicts))
				for i, c := range r.Conflicts {
					names[i] = fmt.Sprintf("%q", c.Name)
				}
				state = SyncState{SyncConflict, "Internet port already forwarded by " + strings.Join(names, ", ") + "."}
			}
		}
		out[m.Key()] = state
	}
	return out, nil
}
