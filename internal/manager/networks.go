package manager

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
)

// NetworkRequest describes a network to create for game servers.
type NetworkRequest struct {
	Name string `json:"name"`
	// Driver is "bridge" for a private network on this host, or "macvlan" /
	// "ipvlan" to give servers their own addresses on the LAN.
	Driver  string `json:"driver"`
	Parent  string `json:"parent,omitempty"`  // host interface, for macvlan/ipvlan
	Subnet  string `json:"subnet"`            // e.g. 192.168.1.0/24
	Gateway string `json:"gateway,omitempty"` // e.g. 192.168.1.1
	// IPRange limits the addresses Docker hands out automatically, e.g. a
	// block outside the router's DHCP pool. Fixed addresses can be anywhere
	// in the subnet.
	IPRange string `json:"ipRange,omitempty"`
}

var networkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// CreateNetwork creates a network servers can join with fixed addresses.
func (m *Manager) CreateNetwork(ctx context.Context, req NetworkRequest) (NetworkInfo, error) {
	if err := validateNetwork(&req); err != nil {
		return NetworkInfo{}, err
	}
	cfg := docker.NetworkIPAMRange{Subnet: req.Subnet, Gateway: req.Gateway, IPRange: req.IPRange}
	create := docker.CreateNetworkRequest{
		Name:   req.Name,
		Driver: req.Driver,
		IPAM:   &docker.NetworkIPAM{Driver: "default", Config: []docker.NetworkIPAMRange{cfg}},
		Labels: map[string]string{deploy.LabelManaged: "true"},
	}
	if req.Parent != "" {
		create.Options = map[string]string{"parent": req.Parent}
	}
	if _, err := m.docker.CreateNetwork(ctx, create); err != nil {
		return NetworkInfo{}, err
	}
	m.log.Info("created network", "name", req.Name, "driver", req.Driver, "subnet", req.Subnet, "parent", req.Parent)
	nets, err := m.Networks(ctx)
	if err != nil {
		return NetworkInfo{}, err
	}
	for _, n := range nets {
		if n.Name == req.Name {
			return n, nil
		}
	}
	return NetworkInfo{}, errors.New("network was created but can't be found")
}

func validateNetwork(req *NetworkRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	if !networkName.MatchString(req.Name) {
		return fmt.Errorf("invalid network name %q: use letters, digits, '_', '.' and '-'", req.Name)
	}
	switch req.Name {
	case "bridge", "host", "none", "default":
		return fmt.Errorf("%q is reserved by Docker", req.Name)
	}
	switch req.Driver {
	case "bridge":
		req.Parent = ""
	case "macvlan", "ipvlan":
		if strings.TrimSpace(req.Parent) == "" {
			return errors.New("choose the host network interface the servers should appear on")
		}
	default:
		return fmt.Errorf("unsupported network type %q", req.Driver)
	}
	subnet, err := netip.ParsePrefix(strings.TrimSpace(req.Subnet))
	if err != nil {
		return fmt.Errorf("invalid subnet %q: use CIDR form like 192.168.1.0/24", req.Subnet)
	}
	if subnet != subnet.Masked() {
		return fmt.Errorf("subnet %s has host bits set; did you mean %s?", subnet, subnet.Masked())
	}
	req.Subnet = subnet.String()
	if g := strings.TrimSpace(req.Gateway); g != "" {
		gw, err := netip.ParseAddr(g)
		if err != nil || !subnet.Contains(gw) {
			return fmt.Errorf("gateway %q must be an address inside %s", g, subnet)
		}
		req.Gateway = gw.String()
	} else if req.Driver != "bridge" {
		return errors.New("a LAN network needs your router's address as its gateway")
	}
	if r := strings.TrimSpace(req.IPRange); r != "" {
		rng, err := netip.ParsePrefix(r)
		if err != nil || !subnet.Contains(rng.Masked().Addr()) || rng.Bits() < subnet.Bits() {
			return fmt.Errorf("IP range %q must be a CIDR block inside %s", r, subnet)
		}
		req.IPRange = rng.Masked().String()
	}
	return nil
}

// RemoveNetwork deletes a network hlm created, if no container uses it.
func (m *Manager) RemoveNetwork(ctx context.Context, name string) error {
	n, err := m.docker.InspectNetwork(ctx, name)
	if docker.IsNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if n.Labels[deploy.LabelManaged] != "true" {
		return errors.New("only networks created in Home Lab Manager can be deleted here")
	}
	if len(n.Containers) > 0 {
		var names []string
		for _, c := range n.Containers {
			names = append(names, c.Name)
		}
		return fmt.Errorf("network %s is still used by %s", name, strings.Join(names, ", "))
	}
	return m.docker.RemoveNetwork(ctx, n.ID)
}

// HostInterfaces lists the Docker host's network interfaces and their
// subnets, for suggesting LAN network settings. It returns nil when the host
// can't be read (e.g. a remote Docker host).
func (m *Manager) HostInterfaces() []hostmetrics.HostInterface {
	if m.host == nil || !m.host.Available() {
		return nil
	}
	ifaces, err := m.host.Interfaces()
	if err != nil {
		m.log.Debug("read host interfaces", "err", err)
		return nil
	}
	return ifaces
}
