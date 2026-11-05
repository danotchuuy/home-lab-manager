package docker

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
)

// Network is a Docker network as returned by the network list and inspect
// endpoints.
type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Scope    string            `json:"Scope"`
	Internal bool              `json:"Internal"`
	IPAM     NetworkIPAM       `json:"IPAM"`
	Options  map[string]string `json:"Options"`
	Labels   map[string]string `json:"Labels"`
	// Containers lists the running containers attached to the network. It is
	// only filled in by InspectNetwork.
	Containers map[string]NetworkContainer `json:"Containers"`
}

// NetworkIPAM is a network's address management configuration.
type NetworkIPAM struct {
	Driver string             `json:"Driver"`
	Config []NetworkIPAMRange `json:"Config"`
}

// NetworkIPAMRange is one subnet of a network.
type NetworkIPAMRange struct {
	Subnet  string `json:"Subnet,omitempty"`
	IPRange string `json:"IPRange,omitempty"`
	Gateway string `json:"Gateway,omitempty"`
}

// NetworkContainer is a container's endpoint on a network.
type NetworkContainer struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"` // CIDR form, e.g. 172.20.0.2/16
	IPv6Address string `json:"IPv6Address"`
}

// NetworkingConfig is the network part of a container create request.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings `json:"EndpointsConfig,omitempty"`
}

// EndpointSettings configures a container's endpoint on one network.
type EndpointSettings struct {
	IPAMConfig *EndpointIPAMConfig `json:"IPAMConfig,omitempty"`
	Aliases    []string            `json:"Aliases,omitempty"`
	IPAddress  string              `json:"IPAddress,omitempty"`  // only set in inspect output
	MacAddress string              `json:"MacAddress,omitempty"` // only set in inspect output
}

// EndpointIPAMConfig requests specific addresses for an endpoint.
type EndpointIPAMConfig struct {
	IPv4Address string `json:"IPv4Address,omitempty"`
	IPv6Address string `json:"IPv6Address,omitempty"`
}

// ListNetworks returns all networks.
func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.doJSON(ctx, http.MethodGet, "/networks", nil, nil, &out)
	for _, n := range out {
		addCurrentLabels(n.Labels)
	}
	return out, err
}

// InspectNetwork returns a network, including its attached containers.
func (c *Client) InspectNetwork(ctx context.Context, id string) (Network, error) {
	var out Network
	err := c.doJSON(ctx, http.MethodGet, "/networks/"+url.PathEscape(id), nil, nil, &out)
	addCurrentLabels(out.Labels)
	return out, err
}

// CreateNetworkRequest is the body of a network create call.
type CreateNetworkRequest struct {
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver"`
	Options  map[string]string `json:"Options,omitempty"`
	IPAM     *NetworkIPAM      `json:"IPAM,omitempty"`
	Labels   map[string]string `json:"Labels,omitempty"`
	Internal bool              `json:"Internal,omitempty"`
}

// CreateNetwork creates a network and returns its ID.
func (c *Client) CreateNetwork(ctx context.Context, req CreateNetworkRequest) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/networks/create", nil, req, &out)
	return out.ID, err
}

// RemoveNetwork deletes a network. Docker refuses while containers use it.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil, nil, nil)
}

// IPv4 returns the network's first IPv4 subnet and its gateway; both are
// invalid if it has none.
func (n Network) IPv4() (subnet netip.Prefix, gateway netip.Addr) {
	for _, c := range n.IPAM.Config {
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
			gw, _ := netip.ParseAddr(c.Gateway)
			return p.Masked(), gw
		}
	}
	return netip.Prefix{}, netip.Addr{}
}

// LANNetwork is a macvlan/ipvlan network, on which containers get their own
// address on the LAN.
type LANNetwork struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Subnet string `json:"subnet"`
	Parent string `json:"parent,omitempty"`
}

// LANNetworks lists the macvlan/ipvlan networks with an IPv4 subnet, by
// name.
func LANNetworks(nets []Network) []LANNetwork {
	out := []LANNetwork{}
	for _, n := range nets {
		if n.Driver != "macvlan" && n.Driver != "ipvlan" {
			continue
		}
		if subnet, _ := n.IPv4(); subnet.IsValid() {
			out = append(out, LANNetwork{Name: n.Name, Driver: n.Driver, Subnet: subnet.String(), Parent: n.Options["parent"]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
