// Package forwarding keeps port forwarding rules on a UniFi gateway in line
// with game servers' ports, so players can reach them from the internet.
//
// Rules created here are named "hlm:<server> <ports>/<proto>" ("gsh:" before
// the short name changed from gsh to hlm); only those are ever changed or
// removed, so rules made by hand are left alone.
package forwarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/unifi"
)

const secretName = "unifi"

// Config is the stored UniFi configuration, credentials included. It only
// ever lives in the encrypted secrets store.
type Config struct {
	unifi.Settings
	// Interface is the WAN the rules apply to: "wan", "wan2" or "both".
	Interface string `json:"interface,omitempty"`
	// ForwardTo overrides the LAN address forwarded to for servers that
	// publish ports on the host. Empty means the host's detected address.
	ForwardTo string `json:"forwardTo,omitempty"`
}

// PublicConfig is Config without the credentials, for showing in the UI.
type PublicConfig struct {
	Configured  bool   `json:"configured"`
	URL         string `json:"url,omitempty"`
	Site        string `json:"site,omitempty"`
	AuthType    string `json:"authType,omitempty"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
	HasAPIKey   bool   `json:"hasApiKey"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Interface   string `json:"interface,omitempty"`
	ForwardTo   string `json:"forwardTo,omitempty"`
	// DetectedHostIP is the host's own LAN address, used when ForwardTo is empty.
	DetectedHostIP string `json:"detectedHostIp,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Inspector looks up managed servers.
type Inspector interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
}

// Service manages the UniFi configuration and servers' rules.
type Service struct {
	secrets *secrets.Store
	servers Inspector
	hostIP  func() string

	// The UniFi client is kept between requests so its sign-in session is
	// reused: UniFi OS answers 429 when a client signs in too often.
	clientMu  sync.Mutex
	cached    *unifi.Client
	cachedFor string     // the settings the cached client was made with
	portsMu   sync.Mutex // serializes changes to internet ports
	log       *slog.Logger
}

// New creates the service. hostIP returns the host's LAN address, or "".
func New(store *secrets.Store, servers Inspector, hostIP func() string, log *slog.Logger) *Service {
	return &Service{secrets: store, servers: servers, hostIP: hostIP, log: log}
}

// ErrNotConfigured is returned when UniFi hasn't been set up.
var ErrNotConfigured = errors.New("UniFi isn't set up yet; add it under System → Integrations")

func (s *Service) load() (Config, bool, error) {
	var c Config
	ok, err := s.secrets.Get(secretName, &c)
	return c, ok, err
}

// client returns the shared client for the saved settings, making a new one
// only when the settings change.
func (s *Service) client() (*unifi.Client, Config, error) {
	c, ok, err := s.load()
	if err != nil {
		return nil, c, err
	}
	if !ok {
		return nil, c, ErrNotConfigured
	}
	key, _ := json.Marshal(c.Settings)
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.cached != nil && s.cachedFor == string(key) {
		return s.cached, c, nil
	}
	cl, err := unifi.New(c.Settings)
	if err != nil {
		return nil, c, err
	}
	s.cached, s.cachedFor = cl, string(key)
	return cl, c, nil
}

// forgetClient drops the shared client, e.g. after the settings change.
func (s *Service) forgetClient() {
	s.clientMu.Lock()
	s.cached, s.cachedFor = nil, ""
	s.clientMu.Unlock()
}

// PublicConfig returns the configuration without credentials.
func (s *Service) PublicConfig() PublicConfig {
	pc := PublicConfig{DetectedHostIP: s.hostIP()}
	c, ok, err := s.load()
	if err != nil {
		pc.Error = err.Error()
		return pc
	}
	if !ok {
		return pc
	}
	pc.Configured = true
	pc.URL, pc.Site, pc.AuthType, pc.Username = c.URL, c.Site, c.AuthType, c.Username
	pc.HasPassword, pc.HasAPIKey = c.Password != "", c.APIKey != ""
	pc.Fingerprint, pc.Interface, pc.ForwardTo = c.Fingerprint, c.Interface, c.ForwardTo
	return pc
}

// merge fills in credentials left empty in in (meaning "keep the saved one")
// from the stored configuration, and checks the rest.
func (s *Service) merge(in Config) (Config, error) {
	saved, _, err := s.load()
	if err != nil {
		return in, err
	}
	in.URL = strings.TrimRight(strings.TrimSpace(in.URL), "/")
	if in.Password == "" && in.Username == saved.Username && in.URL == saved.URL {
		in.Password = saved.Password
	}
	if in.APIKey == "" && in.URL == saved.URL {
		in.APIKey = saved.APIKey
	}
	// Only keep fields for the chosen sign-in method.
	if in.AuthType == "apikey" {
		in.Username, in.Password = "", ""
	} else {
		in.APIKey = ""
	}
	switch in.Interface {
	case "":
		in.Interface = "wan"
	case "wan", "wan2", "both":
	default:
		return in, fmt.Errorf("unknown WAN interface %q", in.Interface)
	}
	if in.ForwardTo != "" {
		if _, err := netip.ParseAddr(in.ForwardTo); err != nil {
			return in, fmt.Errorf("invalid forward-to address %q", in.ForwardTo)
		}
	}
	_, err = unifi.New(in.Settings)
	return in, err
}

// Test checks the given (possibly unsaved) settings by signing in and
// reading the port forwarding rules.
func (s *Service) Test(ctx context.Context, in Config) (unifi.Controller, error) {
	c, err := s.merge(in)
	if err != nil {
		return unifi.Controller{}, err
	}
	cl, err := unifi.New(c.Settings)
	if err != nil {
		return unifi.Controller{}, err
	}
	return cl.Test(ctx)
}

// Save stores the configuration after checking that it works.
func (s *Service) Save(ctx context.Context, in Config) (PublicConfig, error) {
	c, err := s.merge(in)
	if err != nil {
		return PublicConfig{}, err
	}
	cl, err := unifi.New(c.Settings)
	if err != nil {
		return PublicConfig{}, err
	}
	if _, err := cl.Test(ctx); err != nil {
		return PublicConfig{}, err
	}
	if err := s.secrets.Set(secretName, c); err != nil {
		return PublicConfig{}, err
	}
	s.forgetClient()
	s.log.Info("saved UniFi settings", "url", c.URL, "auth", c.AuthType)
	return s.PublicConfig(), nil
}

// Delete forgets the configuration. Rules already on the gateway stay.
func (s *Service) Delete() error {
	s.forgetClient()
	return s.secrets.Delete(secretName)
}

// --- rules for a server ---

// Want is a rule a server needs.
type Want struct {
	Key     string `json:"key"`     // e.g. "2456-2458/udp"
	DstPort string `json:"dstPort"` // WAN port(s)
	FwdPort string `json:"fwdPort"` // LAN port(s)
	Proto   string `json:"proto"`   // tcp, udp, tcp_udp
	Fwd     string `json:"fwd"`     // LAN address
}

// RuleStatus compares a wanted rule with the gateway.
type RuleStatus struct {
	Want
	// State is "forwarded" (a matching hlm rule exists), "outdated" (a hlm
	// rule exists but points elsewhere or is disabled) or "missing".
	State    string             `json:"state"`
	Existing *unifi.PortForward `json:"existing,omitempty"`
	// Conflicts are other rules (not this server's) using the same WAN ports.
	Conflicts []unifi.PortForward `json:"conflicts,omitempty"`
}

// Status describes a server's forwarding on the gateway.
type Status struct {
	Configured bool         `json:"configured"`
	Target     string       `json:"target,omitempty"` // LAN address forwarded to
	Note       string       `json:"note,omitempty"`   // why some or all ports can't be forwarded
	Rules      []RuleStatus `json:"rules"`
	// Stale are this server's rules that match none of its current ports.
	Stale []unifi.PortForward `json:"stale"`
	Error string              `json:"error,omitempty"`
}

// ruleNamePrefix starts the names of rules created here; legacyRuleNamePrefix
// did before the short name changed from gsh to hlm. Such rules are still
// hlm's, and are renamed when the server's rules are next synced.
const (
	ruleNamePrefix       = "hlm:"
	legacyRuleNamePrefix = "gsh:"
)

func ruleName(server, key string) string {
	return ruleNamePrefix + server + " " + key
}

// isRule reports whether pf is the server's rule for key, by its current or
// legacy name.
func isRule(pf unifi.PortForward, server, key string) bool {
	return pf.Name == ruleName(server, key) || pf.Name == legacyRuleNamePrefix+server+" "+key
}

func ownedBy(pf unifi.PortForward, server string) bool {
	return strings.HasPrefix(pf.Name, ruleNamePrefix+server+" ") || strings.HasPrefix(pf.Name, legacyRuleNamePrefix+server+" ")
}

// Port is a port a server is reached on from outside the host: a host port
// it publishes, or, for servers on the host's or a LAN network, a port it
// listens on directly.
type Port struct {
	Number int    `json:"port"`
	Proto  string `json:"proto"` // tcp or udp
}

func (p Port) String() string { return strconv.Itoa(p.Number) + "/" + p.Proto }

// localPorts lists the ports a server is reached on in the LAN, sorted and
// without duplicates.
func localPorts(info docker.ContainerJSON) []Port {
	seen := map[Port]bool{}
	out := []Port{}
	add := func(n int, proto string) {
		if proto == "" {
			proto = "tcp"
		}
		if p := (Port{n, proto}); !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	switch network := info.HostConfig.NetworkMode; {
	case info.Config.Labels[deploy.LabelDirectIP] == "true" || network == "host":
		// The server listens on its own (or the host's) ports directly.
		for key := range info.Config.ExposedPorts {
			port, proto, _ := strings.Cut(key, "/")
			if n, err := strconv.Atoi(port); err == nil {
				add(n, proto)
			}
		}
	case network == "none":
	default:
		for key, bindings := range info.HostConfig.PortBindings {
			_, proto, _ := strings.Cut(key, "/")
			for _, b := range bindings {
				if ip, err := netip.ParseAddr(b.HostIP); err == nil && ip.IsLoopback() {
					continue // only reachable from the host itself
				}
				if hp, err := strconv.Atoi(b.HostPort); err == nil {
					add(hp, proto)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return portLess(out[i], out[j]) })
	return out
}

func portLess(a, b Port) bool {
	if a.Number != b.Number {
		return a.Number < b.Number
	}
	return a.Proto < b.Proto
}

// Wants works out the rules a server needs from its container and its
// internet ports.
func (s *Service) Wants(info docker.ContainerJSON, cfg Config, ports []Mapping) ([]Want, string, string) {
	direct := info.Config.Labels[deploy.LabelDirectIP] == "true"
	network := info.HostConfig.NetworkMode
	var target, note string
	if direct {
		if req, ok := deploy.RequestFromLabels(info.Config.Labels); ok && req.IPAddress != "" {
			target = req.IPAddress
		} else if ep, ok := info.NetworkSettings.Networks[network]; ok && ep.IPAddress != "" {
			target = ep.IPAddress
			note = "This server has no fixed IP, so its address may change when it restarts; set a fixed IP in its settings."
		} else {
			note = "This server is on a LAN network without a fixed IP and isn't running, so its address is unknown. Set a fixed IP in its settings."
		}
	}
	if network == "none" {
		note = "This server has no network."
	}
	if !direct {
		target = cfg.ForwardTo
		if target == "" {
			target = s.hostIP()
		}
		if target == "" {
			note = "The host's LAN address couldn't be detected; set \"Forward to\" in the UniFi settings."
		}
	}
	if len(ports) == 0 && note == "" {
		note = "This server doesn't publish any ports."
	}
	if target == "" {
		return nil, "", note
	}
	return groupPorts(ports, target), target, note
}

// groupPorts merges consecutive ports into ranges, and TCP and UDP rules
// with the same ports into one tcp_udp rule.
func groupPorts(ports []Mapping, target string) []Want {
	ports = append([]Mapping(nil), ports...)
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Proto != ports[j].Proto {
			return ports[i].Proto < ports[j].Proto
		}
		return ports[i].Port < ports[j].Port
	})
	// A run is consecutive internet ports forwarded to consecutive LAN ports.
	type run struct {
		proto                      string
		netLo, netHi, lanLo, lanHi int
	}
	var runs []run
	for _, p := range ports {
		if n := len(runs); n > 0 {
			r := &runs[n-1]
			if r.proto == p.Proto && p.Port == r.netHi+1 && p.Local == r.lanHi+1 {
				r.netHi, r.lanHi = p.Port, p.Local
				continue
			}
			if r.proto == p.Proto && p.Port == r.netHi && p.Local == r.lanHi {
				continue // duplicate
			}
		}
		runs = append(runs, run{p.Proto, p.Port, p.Port, p.Local, p.Local})
	}
	span := func(lo, hi int) string {
		if lo == hi {
			return strconv.Itoa(lo)
		}
		return fmt.Sprintf("%d-%d", lo, hi)
	}
	byPorts := map[string]*Want{}
	var order []string
	for _, r := range runs {
		dst, fwd := span(r.netLo, r.netHi), span(r.lanLo, r.lanHi)
		id := dst + ">" + fwd
		if w, ok := byPorts[id]; ok && w.Proto != r.proto {
			w.Proto = "tcp_udp"
			continue
		}
		byPorts[id] = &Want{DstPort: dst, FwdPort: fwd, Proto: r.proto, Fwd: target}
		order = append(order, id)
	}
	out := make([]Want, 0, len(order))
	for _, id := range order {
		w := *byPorts[id]
		w.Key = w.DstPort + "/" + strings.ReplaceAll(w.Proto, "tcp_udp", "tcp+udp")
		out = append(out, w)
	}
	return out
}

// Status compares a server's wanted rules with the gateway.
func (s *Service) Status(ctx context.Context, server string) (Status, error) {
	cl, cfg, err := s.client()
	if errors.Is(err, ErrNotConfigured) {
		return Status{Rules: []RuleStatus{}, Stale: []unifi.PortForward{}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	info, err := s.servers.Inspect(ctx, server)
	if err != nil {
		return Status{}, err
	}
	return s.status(ctx, cl, cfg, info), nil
}

func (s *Service) status(ctx context.Context, cl *unifi.Client, cfg Config, info docker.ContainerJSON) Status {
	name := strings.TrimPrefix(info.Name, "/")
	ports, err := s.ServerPorts(info)
	if err != nil {
		return Status{Configured: true, Rules: []RuleStatus{}, Stale: []unifi.PortForward{}, Error: err.Error()}
	}
	// Only ports set to be forwarded get rules; rules for the others are
	// stale and removed by Sync.
	forwarded := []Mapping{}
	for _, m := range ports {
		if m.Forward {
			forwarded = append(forwarded, m)
		}
	}
	wants, target, note := s.Wants(info, cfg, forwarded)
	if len(forwarded) == 0 && len(ports) > 0 && note == "This server doesn't publish any ports." {
		note = "None of this server's ports are set to be forwarded by UniFi."
	}
	st := Status{Configured: true, Target: target, Note: note, Rules: []RuleStatus{}, Stale: []unifi.PortForward{}}
	existing, err := cl.PortForwards(ctx)
	if err != nil {
		st.Error = err.Error()
		for _, w := range wants {
			st.Rules = append(st.Rules, RuleStatus{Want: w, State: "unknown"})
		}
		return st
	}
	used := map[string]bool{}
	for _, w := range wants {
		rs := RuleStatus{Want: w, State: "missing"}
		for i, pf := range existing {
			if isRule(pf, name, w.Key) {
				rs.Existing = &existing[i]
				used[pf.ID] = true
				if pf.Enabled && pf.Fwd == w.Fwd && pf.FwdPort == w.FwdPort && pf.DstPort == w.DstPort && pf.Proto == w.Proto &&
					(pf.Interface == "" || pf.Interface == cfg.Interface) {
					rs.State = "forwarded"
				} else {
					rs.State = "outdated"
				}
			} else if !ownedBy(pf, name) && rangesOverlap(pf.DstPort, w.DstPort) && protosOverlap(pf.Proto, w.Proto) {
				rs.Conflicts = append(rs.Conflicts, pf)
			}
		}
		st.Rules = append(st.Rules, rs)
	}
	for _, pf := range existing {
		if ownedBy(pf, name) && !used[pf.ID] {
			st.Stale = append(st.Stale, pf)
		}
	}
	return st
}

// Sync creates or updates the server's rules to match its ports and removes
// its rules for ports it no longer has. Rules that would clash with another
// rule are skipped and reported by Status afterwards.
func (s *Service) Sync(ctx context.Context, server string) (Status, error) {
	cl, cfg, err := s.client()
	if err != nil {
		return Status{}, err
	}
	info, err := s.servers.Inspect(ctx, server)
	if err != nil {
		return Status{}, err
	}
	st := s.status(ctx, cl, cfg, info)
	if st.Error != "" {
		return st, errors.New(st.Error)
	}
	name := strings.TrimPrefix(info.Name, "/")
	var errs []error
	for _, r := range st.Rules {
		pf := unifi.PortForward{
			Name: ruleName(name, r.Key), Enabled: true, Src: "any", Log: false, Interface: cfg.Interface,
			DstPort: r.DstPort, FwdPort: r.FwdPort, Fwd: r.Fwd, Proto: r.Proto,
		}
		// A rule still under its legacy name is renamed.
		rename := r.State == "forwarded" && r.Existing.Name != pf.Name
		switch {
		case r.State == "missing":
			if len(r.Conflicts) > 0 {
				errs = append(errs, fmt.Errorf("%s: WAN port already used by %q", r.Key, r.Conflicts[0].Name))
				continue
			}
			if _, err := cl.CreatePortForward(ctx, pf); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Key, err))
				continue
			}
			s.log.Info("added port forward", "server", name, "ports", r.Key, "to", r.Fwd)
		case r.State == "outdated" || rename:
			// Keep the user's source restriction and logging choices.
			pf.ID, pf.Src, pf.Log = r.Existing.ID, r.Existing.Src, r.Existing.Log
			if pf.Src == "" {
				pf.Src = "any"
			}
			if err := cl.UpdatePortForward(ctx, pf); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Key, err))
				continue
			}
			s.log.Info("updated port forward", "server", name, "ports", r.Key, "to", r.Fwd)
		}
	}
	for _, pf := range st.Stale {
		if err := cl.DeletePortForward(ctx, pf.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", pf.Name, err))
			continue
		}
		s.log.Info("removed stale port forward", "server", name, "rule", pf.Name)
	}
	return s.status(ctx, cl, cfg, info), errors.Join(errs...)
}

// Remove deletes every rule created for the server. It needs only the
// server's name, so it also works after the container is gone.
func (s *Service) Remove(ctx context.Context, name string) (int, error) {
	cl, _, err := s.client()
	if err != nil {
		return 0, err
	}
	existing, err := cl.PortForwards(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, pf := range existing {
		if !ownedBy(pf, name) {
			continue
		}
		if err := cl.DeletePortForward(ctx, pf.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if n > 0 {
		s.log.Info("removed port forwards", "server", name, "rules", n)
	}
	return n, errors.Join(errs...)
}

// Configured reports whether UniFi has been set up.
func (s *Service) Configured() bool {
	_, ok, err := s.load()
	return ok && err == nil
}

func parseRange(s string) (int, int, bool) {
	lo, hi, isRange := strings.Cut(strings.TrimSpace(s), "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return 0, 0, false
	}
	if !isRange {
		return a, a, true
	}
	b, err := strconv.Atoi(hi)
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}

func rangesOverlap(a, b string) bool {
	// UniFi also allows comma-separated lists; check each part.
	for _, pa := range strings.Split(a, ",") {
		alo, ahi, ok1 := parseRange(pa)
		for _, pb := range strings.Split(b, ",") {
			blo, bhi, ok2 := parseRange(pb)
			if ok1 && ok2 && alo <= bhi && blo <= ahi {
				return true
			}
		}
	}
	return false
}

func protosOverlap(a, b string) bool {
	return a == b || a == "tcp_udp" || b == "tcp_udp" || a == "" || b == ""
}
