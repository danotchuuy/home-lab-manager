package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
)

// Item is one of a server's settings: a template item or one the user
// added, with the value Build uses for it.
type Item struct {
	catalog.Config
	// Effective is the value: the request's, or else the template's.
	Effective string
	Disabled  bool
}

// Items lists a server's template items and its extra ones, as Build sees
// them. Values are keyed by Item.ID in Request.Values, for extra items too.
func Items(app *catalog.App, req Request) ([]Item, error) {
	extra, err := extraConfigs(req.Extra)
	if err != nil {
		return nil, err
	}
	disabled := map[string]bool{}
	for _, id := range req.Disabled {
		disabled[id] = true
	}
	var out []Item
	for _, cfg := range append(app.Config[:len(app.Config):len(app.Config)], extra...) {
		v, ok := req.Values[cfg.ID]
		if !ok {
			v = cfg.Value
		}
		out = append(out, Item{Config: cfg, Effective: strings.TrimSpace(v), Disabled: disabled[cfg.ID]})
	}
	return out, nil
}

// HostPortRange is the host ports a port item's value publishes: start to
// end, on ip if it names one.
type HostPortRange struct {
	IP         string
	Start, End int
}

// ParseHostPorts reads a port item's value: "27015", "27015-27020" or
// "ip:port".
func ParseHostPorts(value string) (HostPortRange, error) {
	ip, r, err := parseHostPorts(value)
	if err != nil {
		return HostPortRange{}, err
	}
	return HostPortRange{IP: ip, Start: r.start, End: r.end}, nil
}

// Shift returns the value with its ports moved by n.
func (r HostPortRange) Shift(n int) (string, error) {
	start, end := r.Start+n, r.End+n
	if start < 1 || end > 65535 {
		return "", fmt.Errorf("ports %d-%d moved by %d leave the valid range", r.Start, r.End, n)
	}
	v := strconv.Itoa(start)
	if end != start {
		v += "-" + strconv.Itoa(end)
	}
	if r.IP != "" {
		if strings.Contains(r.IP, ":") {
			return "[" + r.IP + "]:" + v, nil
		}
		return r.IP + ":" + v, nil
	}
	return v, nil
}
