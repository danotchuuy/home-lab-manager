package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/linode"
)

// The proxy server's cloud firewall. hlm adds inbound rules for the relayed
// ports, labelled with rulePrefix, after the firewall's other rules, and
// only ever changes or removes its own; other rules and the default
// policies are left as they are.

const (
	secretFirewall = "proxy-firewall"
	rulePrefix     = "hlm-"
	// legacyRulePrefix labelled hlm's rules before the short name changed
	// from gsh to hlm. Such rules are still hlm's, and get rulePrefix when
	// the firewall is next synced.
	legacyRulePrefix = "gsh-"
	ruleNote         = "Home Lab Manager: relayed game server ports"
)

// FirewallConfig is the stored firewall configuration, token included. It
// only lives in the encrypted secrets store.
type FirewallConfig struct {
	Provider string `json:"provider"` // "linode"
	Token    string `json:"token,omitempty"`
	// FirewallID is the firewall hlm manages; 0 until one is chosen.
	FirewallID int `json:"firewallId"`
}

// FirewallStatus describes the Linode connection and the firewall, without
// the token.
type FirewallStatus struct {
	// Configured means a token is saved; FirewallID is 0 until a firewall
	// is chosen.
	Configured    bool   `json:"configured"`
	Provider      string `json:"provider,omitempty"`
	HasToken      bool   `json:"hasToken"`
	FirewallID    int    `json:"firewallId,omitempty"`
	FirewallLabel string `json:"firewallLabel,omitempty"`
	FirewallState string `json:"firewallState,omitempty"` // enabled, disabled
	InboundPolicy string `json:"inboundPolicy,omitempty"`
	// Rules are hlm's rules on the firewall now; Wanted what they should be.
	Rules  []linode.Rule `json:"rules"`
	Wanted []linode.Rule `json:"wanted"`
	InSync bool          `json:"inSync"`
	// OtherRules counts the firewall's rules hlm doesn't manage.
	OtherRules int    `json:"otherRules"`
	Error      string `json:"error,omitempty"`
}

// FirewallSync is the outcome of updating the firewall.
type FirewallSync struct {
	Changed bool          `json:"changed"`
	Rules   []linode.Rule `json:"rules"`
	Error   string        `json:"error,omitempty"`
}

// FirewallChoice is a firewall the token can manage.
type FirewallChoice struct {
	linode.Firewall
	// Linodes are the labels of the Linodes it's assigned to.
	Linodes []string `json:"linodes,omitempty"`
}

// FirewallTest is what a token can see.
type FirewallTest struct {
	Firewalls []FirewallChoice `json:"firewalls"`
	// Suggested is the firewall of the Linode the proxy server runs on, if
	// it could be found by its address.
	Suggested int    `json:"suggested,omitempty"`
	Linode    string `json:"linode,omitempty"`
	Note      string `json:"note,omitempty"`
}

// UseLinodeAPI points the Linode client somewhere else (for tests).
func (s *Service) UseLinodeAPI(baseURL string) { s.linodeBase = baseURL }

func (s *Service) firewallConfig() (FirewallConfig, bool, error) {
	var c FirewallConfig
	ok, err := s.secrets.Get(secretFirewall, &c)
	return c, ok, err
}

func (s *Service) linodeClient(token string) (*linode.Client, error) {
	return linode.New(token, s.linodeBase)
}

// TestFirewall lists the firewalls a token (or, if empty, the saved one)
// can manage, and suggests the one assigned to the proxy server.
func (s *Service) TestFirewall(ctx context.Context, token string) (FirewallTest, error) {
	if token == "" {
		c, _, err := s.firewallConfig()
		if err != nil {
			return FirewallTest{}, err
		}
		token = c.Token
	}
	cl, err := s.linodeClient(token)
	if err != nil {
		return FirewallTest{}, err
	}
	fws, err := cl.Firewalls(ctx)
	if err != nil {
		return FirewallTest{}, err
	}
	out := FirewallTest{Firewalls: []FirewallChoice{}}
	byID := map[int]int{}
	for _, f := range fws {
		byID[f.ID] = len(out.Firewalls)
		out.Firewalls = append(out.Firewalls, FirewallChoice{Firewall: f})
	}
	instances, err := cl.Instances(ctx)
	if err != nil {
		// Firewalls can still be chosen by hand.
		out.Note = "Couldn't list Linodes (" + err.Error() + "), so the proxy server's firewall couldn't be found automatically."
		return out, nil
	}
	var addrs []netip.Addr
	if st, ok, _ := s.Settings(); ok && st.Host != "" {
		addrs = resolve(ctx, st.Host)
	}
	for _, in := range instances {
		ifws, err := cl.InstanceFirewalls(ctx, in.ID)
		if err != nil {
			continue
		}
		mine := hasAddr(in, addrs)
		for _, f := range ifws {
			if i, ok := byID[f.ID]; ok {
				out.Firewalls[i].Linodes = append(out.Firewalls[i].Linodes, in.Label)
			}
			if mine && out.Suggested == 0 {
				out.Suggested, out.Linode = f.ID, in.Label
			}
		}
		if mine && len(ifws) == 0 {
			out.Linode = in.Label
			out.Note = "The proxy server (" + in.Label + ") has no Cloud Firewall. Create one in Linode's Cloud Manager and assign it to the Linode, then test again."
		}
	}
	if len(addrs) > 0 && out.Linode == "" && out.Note == "" {
		out.Note = "None of this token's Linodes has the proxy server's address; choose the firewall yourself."
	}
	return out, nil
}

func resolve(ctx context.Context, host string) []netip.Addr {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}
	}
	addrs, _ := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs
}

func hasAddr(in linode.Instance, addrs []netip.Addr) bool {
	ips := append([]string(nil), in.IPv4...)
	if v6, _, _ := strings.Cut(in.IPv6, "/"); v6 != "" {
		ips = append(ips, v6)
	}
	for _, ip := range ips {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		for _, b := range addrs {
			if a == b {
				return true
			}
		}
	}
	return false
}

// SaveFirewall stores the Linode token and the firewall to manage, after
// checking that the token works. An empty token keeps the saved one; a
// FirewallID of 0 saves the connection without choosing a firewall.
func (s *Service) SaveFirewall(ctx context.Context, in FirewallConfig) (FirewallStatus, error) {
	saved, _, err := s.firewallConfig()
	if err != nil {
		return FirewallStatus{}, err
	}
	if in.Provider == "" {
		in.Provider = "linode"
	}
	if in.Provider != "linode" {
		return FirewallStatus{}, fmt.Errorf("unknown firewall provider %q", in.Provider)
	}
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" {
		in.Token = saved.Token
	}
	if in.FirewallID < 0 {
		return FirewallStatus{}, errors.New("invalid firewall")
	}
	cl, err := s.linodeClient(in.Token)
	if err != nil {
		return FirewallStatus{}, err
	}
	if _, err := cl.Firewalls(ctx); err != nil {
		return FirewallStatus{}, err
	}
	if in.FirewallID != 0 {
		if _, err := cl.FirewallRules(ctx, in.FirewallID); err != nil {
			return FirewallStatus{}, err
		}
	}
	// Moving to another firewall: take hlm's rules off the old one first.
	if saved.FirewallID != 0 && saved.FirewallID != in.FirewallID {
		s.applyMu.Lock()
		_, err := s.syncFirewall(ctx, nil)
		s.applyMu.Unlock()
		if err != nil {
			return FirewallStatus{}, fmt.Errorf("remove hlm's rules from the previous firewall: %w", err)
		}
	}
	if err := s.secrets.Set(secretFirewall, in); err != nil {
		return FirewallStatus{}, err
	}
	s.log.Info("saved Linode settings", "firewall", in.FirewallID)
	if in.FirewallID == 0 {
		return s.FirewallStatus(ctx), nil
	}
	// Open the relayed ports on it straight away.
	_, syncErr := s.SyncFirewall(ctx)
	st := s.FirewallStatus(ctx)
	if syncErr != nil && st.Error == "" {
		st.Error = "Saved, but updating the firewall failed: " + syncErr.Error()
	}
	return st, nil
}

// DeleteFirewall forgets the Linode connection. Unless keepRules is
// set, hlm's rules are removed first, so no ports are left open.
func (s *Service) DeleteFirewall(ctx context.Context, keepRules bool) error {
	if !keepRules {
		s.applyMu.Lock()
		_, err := s.syncFirewall(ctx, nil)
		s.applyMu.Unlock()
		if err != nil {
			return fmt.Errorf("remove hlm's rules from the firewall: %w", err)
		}
	}
	return s.secrets.Delete(secretFirewall)
}

// wantedPorts are the ports the firewall should open: those of the applied
// configuration, or none if it isn't applied.
func (s *Service) wantedPorts(ctx context.Context) []Proxied {
	state, err := s.State()
	if err != nil || state.Applied == "" {
		return nil
	}
	st, ok, err := s.Settings()
	if err != nil || !ok {
		return nil
	}
	return s.relayed(ctx, st)
}

// FirewallChosen reports whether hlm manages a firewall for the proxy.
func (s *Service) FirewallChosen() bool {
	c, ok, err := s.firewallConfig()
	return err == nil && ok && c.FirewallID != 0
}

// FirewallStatus compares the firewall with the relayed ports.
func (s *Service) FirewallStatus(ctx context.Context) FirewallStatus {
	c, ok, err := s.firewallConfig()
	st := FirewallStatus{Rules: []linode.Rule{}, Wanted: []linode.Rule{}}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if !ok {
		return st
	}
	st.Configured, st.Provider, st.HasToken, st.FirewallID = true, c.Provider, c.Token != "", c.FirewallID
	if c.FirewallID == 0 {
		return st
	}
	cl, err := s.linodeClient(c.Token)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if f, err := cl.Firewall(ctx, c.FirewallID); err == nil {
		st.FirewallLabel, st.FirewallState = f.Label, f.Status
	}
	rules, err := cl.FirewallRules(ctx, c.FirewallID)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.InboundPolicy = rules.InboundPolicy
	others, ours := splitRules(rules.Inbound)
	st.OtherRules = len(others) + len(rules.Outbound)
	st.Rules = ours
	st.Wanted = s.wantedRules(s.wantedPorts(ctx))
	st.InSync = sameRules(currentLabels(ours), st.Wanted)
	return st
}

// SyncFirewall brings the firewall in line with the applied configuration.
func (s *Service) SyncFirewall(ctx context.Context) (FirewallSync, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if c, ok, err := s.firewallConfig(); err != nil || !ok || c.FirewallID == 0 {
		if err == nil {
			err = errors.New("choose the firewall to manage under System → Linode first")
		}
		return FirewallSync{}, err
	}
	return s.syncFirewall(ctx, s.wantedRules(s.wantedPorts(ctx)))
}

// syncFirewallAfter updates the firewall, if one is set up, after the
// relays changed. A failure is reported, not returned: nginx already changed.
func (s *Service) syncFirewallAfter(ctx context.Context, servers []Proxied) *FirewallSync {
	if c, ok, err := s.firewallConfig(); err != nil || !ok || c.FirewallID == 0 {
		return nil
	}
	res, err := s.syncFirewall(ctx, s.wantedRules(servers))
	if err != nil {
		res.Error = err.Error()
		s.log.Error("update proxy firewall", "err", err)
	}
	return &res
}

// syncFirewall makes hlm's rules on the firewall exactly wanted (nil removes
// them all). The caller holds applyMu.
func (s *Service) syncFirewall(ctx context.Context, wanted []linode.Rule) (FirewallSync, error) {
	res := FirewallSync{Rules: []linode.Rule{}}
	c, ok, err := s.firewallConfig()
	if err != nil || !ok || c.FirewallID == 0 {
		return res, err
	}
	cl, err := s.linodeClient(c.Token)
	if err != nil {
		return res, err
	}
	rules, err := cl.FirewallRules(ctx, c.FirewallID)
	if err != nil {
		return res, err
	}
	others, ours := splitRules(rules.Inbound)
	if wanted == nil {
		wanted = []linode.Rule{}
	}
	res.Rules = wanted
	if sameRules(ours, wanted) {
		return res, nil
	}
	if n := len(others) + len(wanted) + len(rules.Outbound); n > linode.MaxRules {
		return res, fmt.Errorf("the firewall would need %d rules, but Linode allows %d; remove some of its other rules or relay fewer ports", n, linode.MaxRules)
	}
	rules.Inbound = append(others, wanted...)
	if _, err := cl.SetFirewallRules(ctx, c.FirewallID, rules); err != nil {
		return res, err
	}
	res.Changed = true
	s.log.Info("updated proxy firewall", "firewall", c.FirewallID, "rules", len(wanted))
	return res, nil
}

func splitRules(in []linode.Rule) (others, ours []linode.Rule) {
	others, ours = []linode.Rule{}, []linode.Rule{}
	for _, r := range in {
		if strings.HasPrefix(r.Label, rulePrefix) || strings.HasPrefix(r.Label, legacyRulePrefix) {
			ours = append(ours, r)
		} else {
			others = append(others, r)
		}
	}
	return others, ours
}

// currentLabels returns rules with legacyRulePrefix labels as they'll be
// relabelled.
func currentLabels(rules []linode.Rule) []linode.Rule {
	out := make([]linode.Rule, len(rules))
	for i, r := range rules {
		if rest, ok := strings.CutPrefix(r.Label, legacyRulePrefix); ok {
			r.Label = rulePrefix + rest
		}
		out[i] = r
	}
	return out
}

func sameRules(a, b []linode.Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Label != y.Label || x.Action != y.Action || x.Protocol != y.Protocol ||
			strings.ReplaceAll(x.Ports, " ", "") != strings.ReplaceAll(y.Ports, " ", "") ||
			!reflect.DeepEqual(x.Addresses, y.Addresses) {
			return false
		}
	}
	return true
}

// wantedRules are hlm's rules for servers' relayed ports, plus the
// WireGuard tunnel's port while the tunnel is set up: it stays open when the
// relays are removed, since the tunnel doesn't depend on them.
func (s *Service) wantedRules(servers []Proxied) []linode.Rule {
	rules := WantedRules(servers)
	if wg, ok, err := s.tunnel(); err == nil && ok {
		rules = append(rules, wireGuardRule(wg.ListenPort))
	}
	// HTTP and HTTPS for the status page's sites, while relays are applied.
	if st, ok, err := s.Settings(); err == nil && ok && st.StatusPage && servers != nil {
		var from *linode.Addresses
		if st.StatusTLS == TLSCloudflare {
			var cf linode.Addresses
			if ok, err := s.secrets.Get(secretCloudflareIPs, &cf); err == nil && ok {
				from = &cf
			}
		}
		rules = append(rules, statusRule(from))
		// Port 80 is already open to everyone, unless only Cloudflare may
		// reach it.
		if st.StatusOnAddress && (st.StatusPort != 80 || from != nil) {
			rules = append(rules, statusAddressRule(st.StatusPort, st.StatusAddressHTTPS))
		}
	}
	return rules
}

// WantedRules are the inbound rules that open the relayed ports: one per
// protocol, split to stay within Linode's limit on ports per rule.
func WantedRules(servers []Proxied) []linode.Rule {
	byProto := map[string][]int{}
	for _, p := range servers {
		for _, m := range p.Ports {
			if m.Firewall {
				byProto[m.Proto] = append(byProto[m.Proto], m.Port)
			}
		}
	}
	out := []linode.Rule{}
	for _, proto := range []string{"tcp", "udp"} {
		ports := byProto[proto]
		if len(ports) == 0 {
			continue
		}
		var chunk []string
		pieces, n := 0, 0
		flush := func() {
			if len(chunk) == 0 {
				return
			}
			n++
			out = append(out, linode.Rule{
				Action: "ACCEPT", Protocol: strings.ToUpper(proto), Ports: strings.Join(chunk, ","),
				Addresses: &linode.Addresses{IPv4: []string{"0.0.0.0/0"}},
				Label:     rulePrefix + proto + "-" + strconv.Itoa(n), Description: ruleNote,
			})
			chunk, pieces = nil, 0
		}
		for _, r := range portRanges(ports) {
			cost := 1
			if strings.Contains(r, "-") {
				cost = 2
			}
			if pieces+cost > linode.MaxPortPieces {
				flush()
			}
			chunk = append(chunk, r)
			pieces += cost
		}
		flush()
	}
	return out
}

// portRanges sorts ports and merges consecutive ones: [2456 2457 2458 27015]
// becomes ["2456-2458" "27015"].
func portRanges(ports []int) []string {
	ports = append([]int(nil), ports...)
	sort.Ints(ports)
	var out []string
	for i := 0; i < len(ports); {
		j := i
		for j+1 < len(ports) && ports[j+1] <= ports[j]+1 {
			j++
		}
		if ports[i] == ports[j] {
			out = append(out, strconv.Itoa(ports[i]))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", ports[i], ports[j]))
		}
		i = j + 1
	}
	return out
}
