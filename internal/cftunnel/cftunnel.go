// Package cftunnel publishes servers' web interfaces on hostnames of the
// user's own domain through a Cloudflare Tunnel.
//
// hlm creates a remotely managed tunnel with the Cloudflare API and runs its
// connector, cloudflared, in a container on the Docker host. cloudflared
// connects out to Cloudflare, so nothing has to be forwarded at home. Each
// route sends one hostname (e.g. panel.example.com) to one of a server's TCP
// ports: hlm puts the route in the tunnel's ingress rules and points the
// hostname at the tunnel with a proxied CNAME record. Cloudflare serves the
// hostname over HTTPS with its own certificate.
//
// The connector runs in one of two ways, like the WireGuard tunnel's home
// end:
//   - On the host's network (the default): it reaches the servers' published
//     ports on localhost.
//   - On a macvlan/ipvlan LAN network, and on Docker's default bridge: it
//     reaches servers with their own LAN address directly, and published
//     ports through the bridge's gateway. A host can't talk to its own
//     macvlan containers, which is why this mode exists.
package cftunnel

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretAccount = "cloudflare"
	secretTunnel  = "cloudflare-tunnel"
	secretRoutes  = "cloudflare-routes"
	// secretApplied records what was last put in Cloudflare: the ingress
	// rules and the DNS records hlm made.
	secretApplied = "cloudflare-applied"

	// Container runs cloudflared on the Docker host.
	Container = "hlm-cloudflared"
	// legacyContainer is Container's name from before the short name
	// changed from gsh to hlm.
	legacyContainer = "gsh-cloudflared"
	// DefaultImage is cloudflared's official image.
	DefaultImage = "cloudflare/cloudflared:latest"
	// LabelConnector marks the cloudflared container.
	LabelConnector = "io.hlm.cloudflared"
	// DefaultTunnelName is the tunnel's name in the Cloudflare dashboard.
	DefaultTunnelName = "hlm"
	// RecordComment marks the DNS records hlm made, so it only ever changes
	// or removes those.
	RecordComment = "Managed by Home Lab Manager"
	// legacyRecordComment marked hlm's records before the project was
	// renamed. Such records are still hlm's, and get RecordComment when next
	// applied.
	legacyRecordComment = "Managed by Game Server Hoster"
	// connectedLine is what cloudflared logs once it reaches Cloudflare.
	connectedLine = "Registered tunnel connection"
)

// ErrNotConfigured means no Cloudflare API token is saved.
var ErrNotConfigured = errors.New("Cloudflare isn't connected; save an API token first")

// ErrNoTunnel means the tunnel isn't set up.
var ErrNoTunnel = errors.New("the Cloudflare Tunnel isn't set up")

// Account is the stored Cloudflare connection. It only lives in the
// encrypted secrets store.
type Account struct {
	Token       string `json:"token"`
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
}

// Tunnel is the stored tunnel, its connector token included.
type Tunnel struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Token   string    `json:"token"`
	Created time.Time `json:"created"`
	// LANNetwork is the macvlan/ipvlan network the connector joins; empty
	// runs it on the host's network.
	LANNetwork string `json:"lanNetwork,omitempty"`
	// LANAddress is the connector's fixed address there; empty lets Docker
	// choose.
	LANAddress string `json:"lanAddress,omitempty"`
	// LANSubnet is LANNetwork's IPv4 subnet.
	LANSubnet string `json:"lanSubnet,omitempty"`
	// BridgeGateway is the host's address on Docker's default bridge, where
	// the connector reaches published ports in LAN mode.
	BridgeGateway string `json:"bridgeGateway,omitempty"`
}

// Route publishes a web server on a hostname: one of a game server's TCP
// ports, or any address cloudflared can reach.
type Route struct {
	Hostname string `json:"hostname"`
	// ZoneID and Zone are the Cloudflare zone the hostname is in.
	ZoneID string `json:"zoneId"`
	Zone   string `json:"zone"`
	// Server is the game server it goes to; empty with Address.
	Server string `json:"server,omitempty"`
	// Address is the IP address or host name of another web server, e.g. a
	// NAS on the LAN, used instead of Server. It's resolved by cloudflared,
	// so "localhost" is the Docker host in the default host networking.
	Address string `json:"address,omitempty"`
	// Port is the port the server is reached on at home: for a game server,
	// the port published on the host, or the server's own port for servers
	// on the host's network or with their own LAN address; for an Address,
	// its port.
	Port int `json:"port"`
	// Scheme is how cloudflared talks to the server: "http" or "https".
	Scheme string `json:"scheme"`
	// NoTLSVerify accepts an https server's certificate without checking
	// it, for the usual self-signed ones.
	NoTLSVerify bool `json:"noTLSVerify,omitempty"`
	// Hlm sends the hostname to Home Lab Manager itself, such as a status
	// page site's domain; hlm works out its port (and Server, Address,
	// Port and Scheme are left empty).
	Hlm bool `json:"hlm,omitempty"`
}

// SelfOrigin is how hlm itself is reached: the port it listens on, and its
// container, if it runs in one.
type SelfOrigin struct {
	Port      int
	Container *docker.ContainerJSON
}

// UseSelf says how to find hlm itself, for routes to it (see Route.Hlm).
func (s *Service) UseSelf(f func(ctx context.Context) (SelfOrigin, error)) { s.self = f }

// selfOrigin is where cloudflared reaches hlm itself. hlm serves HTTPS and
// HTTP on one port and doesn't redirect requests cloudflared marks as
// HTTPS (X-Forwarded-Proto), so plain HTTP always works.
func (s *Service) selfOrigin(ctx context.Context, t Tunnel) (string, error) {
	if s.self == nil {
		return "", errors.New("hlm can't tell how it's reached here")
	}
	so, err := s.self(ctx)
	if err != nil {
		return "", err
	}
	if so.Port == 0 {
		return "", errors.New("hlm doesn't listen on a TCP port it can tell")
	}
	if info := so.Container; info != nil && info.HostConfig.NetworkMode != "host" {
		// In a container: through the port it publishes on the host (or its
		// own LAN address).
		r := Route{Server: "Home Lab Manager", Scheme: "http"}
		for _, p := range webPorts(*info) {
			if p.Container == so.Port || (p.Container == 0 && p.Port == so.Port) {
				r.Port = p.Port
			}
		}
		if r.Port == 0 {
			return "", fmt.Errorf("hlm's container doesn't publish its port %d on the host, so cloudflared can't reach it: publish it (-p %d:%d)", so.Port, so.Port, so.Port)
		}
		return origin(*info, r, t, "")
	}
	host := "localhost"
	if t.LANNetwork != "" {
		host = t.BridgeGateway
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(so.Port)), nil
}

type appliedRecord struct {
	Hostname string `json:"hostname"`
	ZoneID   string `json:"zoneId"`
	RecordID string `json:"recordId"`
}

type appliedState struct {
	TunnelID  string                   `json:"tunnelId"`
	Ingress   []cloudflare.IngressRule `json:"ingress"`
	Records   []appliedRecord          `json:"records"`
	Conflicts map[string]string        `json:"conflicts,omitempty"` // hostname -> what's in the way
	At        time.Time                `json:"at"`
}

// Containers runs the connector; *docker.Client implements it.
type Containers interface {
	PullImage(ctx context.Context, ref string, progress func(docker.PullProgress)) error
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout int) error
	RestartContainer(ctx context.Context, id string, timeout int) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	RenameContainer(ctx context.Context, id, name string) error
	ListNetworks(ctx context.Context) ([]docker.Network, error)
	ConnectNetwork(ctx context.Context, network, container string, endpoint any) error
	Logs(ctx context.Context, id string, opts docker.LogsOptions) (io.ReadCloser, error)
}

// Servers finds game servers; *manager.Manager implements it.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
}

// Service manages the Cloudflare connection, the tunnel and its routes.
type Service struct {
	secrets *secrets.Store
	servers Servers
	docker  Containers
	image   string
	apiBase string // overrides Cloudflare's API address (for tests)
	log     *slog.Logger
	mu      sync.Mutex // one change at a time
	// hostAddress is where a server on another Docker host publishes its
	// ports; "" for the local host. See UseHostAddress.
	hostAddress func(id string) string
	// self finds hlm itself; see UseSelf.
	self func(ctx context.Context) (SelfOrigin, error)
	// dockerFor is other Docker hosts' Docker; see UseDockerHosts.
	dockerFor func(host string) (Containers, error)
}

// UseHostAddress says where servers on other Docker hosts publish their
// ports (by server ID; "" for local servers), for routes to them.
func (s *Service) UseHostAddress(f func(id string) string) { s.hostAddress = f }

// New creates the service. image is cloudflared's image; empty for
// DefaultImage.
func New(store *secrets.Store, servers Servers, d Containers, image string, log *slog.Logger) *Service {
	if image == "" {
		image = DefaultImage
	}
	return &Service{secrets: store, servers: servers, docker: d, image: image, log: log}
}

// UseAPI sets the Cloudflare API's address.
func (s *Service) UseAPI(base string) { s.apiBase = base }

func (s *Service) account() (Account, bool, error) {
	var a Account
	ok, err := s.secrets.Get(secretAccount, &a)
	return a, ok, err
}

func (s *Service) tunnel() (Tunnel, bool, error) {
	var t Tunnel
	ok, err := s.secrets.Get(secretTunnel, &t)
	return t, ok, err
}

func (s *Service) routes() ([]Route, error) {
	rs := []Route{}
	_, err := s.secrets.Get(secretRoutes, &rs)
	return rs, err
}

func (s *Service) applied() (appliedState, bool, error) {
	var a appliedState
	ok, err := s.secrets.Get(secretApplied, &a)
	return a, ok, err
}

// client returns an API client and the saved account.
func (s *Service) client() (*cloudflare.Client, Account, error) {
	a, ok, err := s.account()
	if err != nil {
		return nil, a, err
	}
	if !ok {
		return nil, a, ErrNotConfigured
	}
	c, err := cloudflare.New(a.Token, s.apiBase)
	return c, a, err
}

// --- the Cloudflare connection ---

// TestResult is what a token can reach.
type TestResult struct {
	Accounts []cloudflare.Account `json:"accounts"`
	Zones    []cloudflare.Zone    `json:"zones"`
}

// Test lists the accounts and zones a token can use; an empty token tests
// the saved one.
func (s *Service) Test(ctx context.Context, token string) (TestResult, error) {
	var res TestResult
	if strings.TrimSpace(token) == "" {
		a, ok, err := s.account()
		if err != nil {
			return res, err
		}
		if !ok {
			return res, ErrNotConfigured
		}
		token = a.Token
	}
	c, err := cloudflare.New(token, s.apiBase)
	if err != nil {
		return res, err
	}
	// Cloudflare only lists accounts for tokens with Account Settings: Read,
	// which the tunnel doesn't need, so the zones' accounts count too.
	accounts, err := c.Accounts(ctx)
	if err != nil && !cloudflare.IsForbidden(err) {
		return res, err
	}
	if res.Zones, err = c.Zones(ctx, ""); err != nil {
		return res, err
	}
	for _, z := range res.Zones {
		if z.Account.ID != "" {
			accounts = append(accounts, z.Account)
		}
	}
	if len(accounts) == 0 {
		return res, errors.New("the token can't see any of your domains, so hlm can't tell which Cloudflare account to use; give it Zone → Zone: Read and Zone → DNS: Edit for the domains you want to use")
	}
	// Only accounts where it can manage tunnels will do.
	seen := map[string]bool{}
	var denied []string
	for _, a := range accounts {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		_, err := c.Tunnels(ctx, a.ID, "")
		switch {
		case cloudflare.IsForbidden(err):
			denied = append(denied, cmp.Or(a.Name, a.ID))
		case err != nil:
			return res, err
		default:
			res.Accounts = append(res.Accounts, a)
		}
	}
	if len(res.Accounts) == 0 {
		return res, fmt.Errorf("the token can't manage Cloudflare Tunnels in %s; give it Account → Cloudflare Tunnel: Edit, with that account included under Account Resources", strings.Join(denied, ", "))
	}
	return res, nil
}

// AccountInput saves the Cloudflare connection.
type AccountInput struct {
	// Token is the API token; empty keeps the saved one.
	Token string `json:"token"`
	// AccountID is the account the tunnel goes in; empty picks the token's
	// only account.
	AccountID string `json:"accountId"`
}

// SaveAccount checks and saves the token and account.
func (s *Service) SaveAccount(ctx context.Context, in AccountInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had, err := s.account()
	if err != nil {
		return err
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		if !had {
			return errors.New("enter a Cloudflare API token")
		}
		token = old.Token
	}
	res, err := s.Test(ctx, token)
	if err != nil {
		return err
	}
	a := Account{Token: token}
	for _, acc := range res.Accounts {
		if acc.ID == in.AccountID || (in.AccountID == "" && len(res.Accounts) == 1) {
			a.AccountID, a.AccountName = acc.ID, acc.Name
		}
	}
	if a.AccountID == "" {
		if in.AccountID == "" {
			return errors.New("the token can use several accounts; choose one")
		}
		return fmt.Errorf("the token can't use the account %s", in.AccountID)
	}
	if _, hasTunnel, err := s.tunnel(); err != nil {
		return err
	} else if hasTunnel && had && old.AccountID != a.AccountID {
		return errors.New("the tunnel is in another account; remove it before switching accounts")
	}
	if err := s.secrets.Set(secretAccount, a); err != nil {
		return err
	}
	s.log.Info("saved the Cloudflare connection", "account", a.AccountName)
	return nil
}

// DeleteAccount forgets the token. It refuses while the tunnel is set up,
// since hlm couldn't remove it from Cloudflare afterwards.
func (s *Service) DeleteAccount() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok, err := s.tunnel(); err != nil {
		return err
	} else if ok {
		return errors.New("remove the tunnel first, so its DNS records and the tunnel itself are removed from Cloudflare")
	}
	return s.secrets.Delete(secretAccount)
}

// --- status ---

// Status describes the Cloudflare connection and the tunnel.
type Status struct {
	Configured  bool              `json:"configured"`
	AccountID   string            `json:"accountId,omitempty"`
	AccountName string            `json:"accountName,omitempty"`
	Zones       []cloudflare.Zone `json:"zones"`
	Tunnel      *TunnelStatus     `json:"tunnel,omitempty"`
	// Hosts are the tunnels on other Docker hosts.
	Hosts []HostTunnel `json:"hosts"`
	Image string       `json:"image"`
	// LANNetworks are the networks the connector can join.
	LANNetworks []docker.LANNetwork `json:"lanNetworks"`
	// Error is set when Cloudflare couldn't be asked.
	Error string `json:"error,omitempty"`
}

// TunnelStatus describes the tunnel, without its token.
type TunnelStatus struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	// Health is Cloudflare's view: "healthy", "degraded", "down" or
	// "inactive"; empty if it couldn't be asked, "deleted" if the tunnel is
	// gone there.
	Health      string                  `json:"health,omitempty"`
	Connections []cloudflare.Connection `json:"connections"`
	Connector   Connector               `json:"connector"`
	LANNetwork  string                  `json:"lanNetwork,omitempty"`
	LANSubnet   string                  `json:"lanSubnet,omitempty"`
	LANAddress  string                  `json:"lanAddress,omitempty"`
}

// Connector is the state of the cloudflared container.
type Connector struct {
	// State is Docker's ("running", "exited", …), or "missing".
	State    string `json:"state,omitempty"`
	Running  bool   `json:"running"`
	ExitCode int    `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
	// LANAddress is its address on the LAN network, in LAN mode.
	LANAddress string `json:"lanAddress,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
}

// Status asks Cloudflare and Docker about the tunnel.
func (s *Service) Status(ctx context.Context) (Status, error) {
	out := Status{Image: s.image, Zones: []cloudflare.Zone{}, LANNetworks: []docker.LANNetwork{}, Hosts: []HostTunnel{}}
	if nets, err := s.dk(ctx).ListNetworks(ctx); err == nil {
		out.LANNetworks = docker.LANNetworks(nets)
	}
	c, a, err := s.client()
	if errors.Is(err, ErrNotConfigured) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured, out.AccountID, out.AccountName = true, a.AccountID, a.AccountName
	if zones, err := c.Zones(ctx, a.AccountID); err != nil {
		out.Error = err.Error()
	} else {
		out.Zones = zones
	}
	hosts, err := s.hostTunnels()
	if err != nil {
		return out, err
	}
	for _, host := range sortedHosts(hosts) {
		ts := s.tunnelStatus(ctx, c, a, hosts[host], host, &out)
		out.Hosts = append(out.Hosts, HostTunnel{Host: host, TunnelStatus: *ts})
	}
	t, ok, err := s.tunnel()
	if err != nil || !ok {
		return out, err
	}
	out.Tunnel = s.tunnelStatus(ctx, c, a, t, manager.LocalHost, &out)
	return out, nil
}

// HostTunnel is the tunnel on another Docker host.
type HostTunnel struct {
	Host string `json:"host"`
	TunnelStatus
}

// tunnelStatus asks Cloudflare and the host's Docker about a tunnel.
func (s *Service) tunnelStatus(ctx context.Context, c *cloudflare.Client, a Account, t Tunnel, host string, out *Status) *TunnelStatus {
	ts := &TunnelStatus{ID: t.ID, Name: t.Name, Created: t.Created, Connections: []cloudflare.Connection{},
		LANNetwork: t.LANNetwork, LANSubnet: t.LANSubnet, LANAddress: t.LANAddress}
	if out.Error == "" {
		switch ct, err := c.Tunnel(ctx, a.AccountID, t.ID); {
		case cloudflare.IsNotFound(err), err == nil && ct.DeletedAt != nil:
			ts.Health = "deleted"
		case err != nil:
			out.Error = err.Error()
		default:
			ts.Health = ct.Status
			if ct.Connections != nil {
				ts.Connections = ct.Connections
			}
		}
	}
	if hctx, err := s.onHost(ctx, host); err != nil {
		ts.Connector = Connector{Error: err.Error()}
	} else {
		ts.Connector = s.connector(hctx, t)
	}
	return ts
}

func (s *Service) connector(ctx context.Context, t Tunnel) Connector {
	info, err := s.dk(ctx).InspectContainer(ctx, Container)
	if docker.IsNotFound(err) {
		return Connector{State: "missing", Error: "The " + Container + " container is gone; set the tunnel up again."}
	}
	if err != nil {
		return Connector{Error: err.Error()}
	}
	c := Connector{State: info.State.Status, Running: info.State.Running && !info.State.Restarting, ExitCode: info.State.ExitCode, Error: info.State.Error, StartedAt: info.State.StartedAt}
	if t.LANNetwork != "" {
		c.LANAddress = info.NetworkSettings.Networks[t.LANNetwork].IPAddress
	}
	return c
}

// --- setting up the tunnel ---

// SetupInput is what the user chooses when setting up the tunnel.
type SetupInput struct {
	// Host is the Docker host cloudflared runs on: "" for this one, or
	// another, which gets a tunnel of its own.
	Host string `json:"host,omitempty"`
	// Name is the tunnel's name in Cloudflare; only used when it's created.
	Name string `json:"name"`
	// LANNetwork, if set, runs the connector on this macvlan/ipvlan network,
	// optionally with a fixed LANAddress.
	LANNetwork string `json:"lanNetwork"`
	LANAddress string `json:"lanAddress"`
}

// Event reports progress while the tunnel is set up.
type Event struct {
	// Stage is "step" (Message says what's being done), "output" (Message
	// is a line from Docker or cloudflared), and, from the API, "done" (with
	// Status) or "error".
	Stage   string  `json:"stage"`
	Message string  `json:"message,omitempty"`
	Status  *Status `json:"status,omitempty"`
	Apply   *Apply  `json:"apply,omitempty"`
}

var tunnelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func validateSetup(in *SetupInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = DefaultTunnelName
	}
	if !tunnelName.MatchString(in.Name) {
		return fmt.Errorf("invalid tunnel name %q: use letters, digits, dots, dashes and underscores", in.Name)
	}
	in.LANNetwork, in.LANAddress = strings.TrimSpace(in.LANNetwork), strings.TrimSpace(in.LANAddress)
	if in.LANNetwork == "" {
		in.LANAddress = ""
	}
	if in.LANAddress != "" {
		if a, err := netip.ParseAddr(in.LANAddress); err != nil || !a.Is4() {
			return fmt.Errorf("invalid LAN address %q: use an IPv4 address", in.LANAddress)
		}
	}
	return nil
}

// resolveLAN fills in LAN mode's subnet and bridge gateway from Docker's
// networks.
func (s *Service) resolveLAN(ctx context.Context, t *Tunnel) error {
	t.LANSubnet, t.BridgeGateway = "", ""
	if t.LANNetwork == "" {
		return nil
	}
	nets, err := s.dk(ctx).ListNetworks(ctx)
	if err != nil {
		return err
	}
	var lan, bridge *docker.Network
	for i := range nets {
		switch nets[i].Name {
		case t.LANNetwork:
			lan = &nets[i]
		case "bridge":
			bridge = &nets[i]
		}
	}
	if lan == nil || (lan.Driver != "macvlan" && lan.Driver != "ipvlan") {
		return fmt.Errorf("%q isn't a macvlan or ipvlan network on the Docker host", t.LANNetwork)
	}
	subnet, _ := lan.IPv4()
	if !subnet.IsValid() {
		return fmt.Errorf("the network %s has no IPv4 subnet", t.LANNetwork)
	}
	if bridge == nil {
		return errors.New("Docker's default bridge network is missing, so the connector couldn't reach published ports")
	}
	_, gw := bridge.IPv4()
	if !gw.IsValid() {
		return errors.New("Docker's default bridge network has no IPv4 gateway")
	}
	if t.LANAddress != "" && !subnet.Contains(netip.MustParseAddr(t.LANAddress)) {
		return fmt.Errorf("%s isn't in %s's subnet %s", t.LANAddress, t.LANNetwork, subnet)
	}
	t.LANSubnet, t.BridgeGateway = subnet.String(), gw.String()
	return nil
}

// Setup creates the tunnel in Cloudflare (the first time), runs cloudflared
// on the Docker host and applies the routes. Setting it up again keeps the
// tunnel and replaces the container. Progress goes to emit as it happens.
func (s *Service) Setup(ctx context.Context, in SetupInput, emit func(Event)) (Status, *Apply, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	step := func(msg string) { emit(Event{Stage: "step", Message: msg}) }
	output := func(line string) { emit(Event{Stage: "output", Message: line}) }
	host := in.Host
	if isLocal(host) {
		host = manager.LocalHost
	} else if strings.TrimSpace(in.Name) == "" {
		in.Name = DefaultTunnelName + "-" + host
	}
	if err := validateSetup(&in); err != nil {
		return Status{}, nil, err
	}
	base := ctx
	ctx, err := s.onHost(ctx, host)
	if err != nil {
		return Status{}, nil, err
	}
	c, a, err := s.client()
	if err != nil {
		return Status{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check the LAN settings before anything is made in Cloudflare.
	lan := Tunnel{LANNetwork: in.LANNetwork, LANAddress: in.LANAddress}
	if err := s.resolveLAN(ctx, &lan); err != nil {
		return Status{}, nil, err
	}
	t, ok, err := s.tunnelOn(host)
	if err != nil {
		return Status{}, nil, err
	}
	if ok {
		// Make sure it still exists; it may have been deleted in the
		// dashboard.
		switch ct, err := c.Tunnel(ctx, a.AccountID, t.ID); {
		case cloudflare.IsNotFound(err), err == nil && ct.DeletedAt != nil:
			output("The tunnel " + t.Name + " is gone from Cloudflare; making a new one")
			ok = false
		case err != nil:
			return Status{}, nil, err
		}
	}
	if !ok {
		step("Creating the tunnel " + in.Name + " in Cloudflare (" + a.AccountName + ")")
		existing, err := c.Tunnels(ctx, a.AccountID, in.Name)
		if err != nil {
			return Status{}, nil, err
		}
		if len(existing) > 0 {
			return Status{}, nil, fmt.Errorf("your Cloudflare account already has a tunnel called %q; choose another name, or delete that tunnel in the Cloudflare dashboard (Zero Trust → Networks → Tunnels)", in.Name)
		}
		ct, err := c.CreateTunnel(ctx, a.AccountID, in.Name)
		if err != nil {
			return Status{}, nil, err
		}
		t = Tunnel{ID: ct.ID, Name: ct.Name, Created: time.Now().UTC()}
		output("Created tunnel " + ct.ID)
		// Remember it straight away, so whatever fails next, it can be set
		// up again or removed rather than left behind in Cloudflare.
		if err := s.saveTunnelOn(host, t); err != nil {
			return Status{}, nil, err
		}
		// Routes applied to an old tunnel pointed elsewhere.
		if err := s.deleteAppliedOn(host); err != nil {
			return Status{}, nil, err
		}
	}
	t.LANNetwork, t.LANAddress, t.LANSubnet, t.BridgeGateway = lan.LANNetwork, lan.LANAddress, lan.LANSubnet, lan.BridgeGateway
	step("Fetching the connector's token")
	if t.Token, err = c.TunnelToken(ctx, a.AccountID, t.ID); err != nil {
		return Status{}, nil, err
	}
	// The tunnel exists in Cloudflare now, so keep it even if the container
	// fails: setting up again then only has to fix that.
	if err := s.saveTunnelOn(host, t); err != nil {
		return Status{}, nil, err
	}
	s.log.Info("set up the Cloudflare Tunnel", "tunnel", t.Name, "id", t.ID, "host", host)

	if err := s.startConnector(ctx, t, step, output); err != nil {
		st, _ := s.Status(base)
		return st, nil, fmt.Errorf("the tunnel exists in Cloudflare, but starting cloudflared failed: %w", err)
	}
	s.log.Info("started cloudflared", "container", Container, "host", host)

	var applied *Apply
	if rs, err := s.routes(); err == nil && len(rs) > 0 {
		step("Publishing the hostnames")
		res, err := s.apply(base, c, a)
		if err != nil {
			st, _ := s.Status(base)
			return st, nil, fmt.Errorf("the tunnel is up, but publishing the hostnames failed: %w", err)
		}
		for _, r := range res.Routes {
			if r.Error != "" {
				output(r.Hostname + ": " + r.Error)
			} else {
				output(r.Hostname + " → " + r.Origin)
			}
		}
		applied = &res
	}
	st, err := s.Status(base)
	return st, applied, err
}

// startConnector replaces the cloudflared container, reporting steps and
// output as it goes.
func (s *Service) startConnector(ctx context.Context, t Tunnel, step, output func(string)) error {
	if err := s.removeConnector(ctx); err != nil {
		return err
	}
	step("Pulling " + s.image)
	pullErr := s.dk(ctx).PullImage(ctx, s.image, func(p docker.PullProgress) {
		// Leave out the flood of byte counts; layer status changes are enough.
		if p.Status == "" || p.Status == "Downloading" || p.Status == "Extracting" || p.Status == "Waiting" {
			return
		}
		if p.ID != "" {
			output(p.ID + ": " + p.Status)
		} else {
			output(p.Status)
		}
	})
	if pullErr != nil {
		output("Pulling failed (" + pullErr.Error() + "); trying the image already on the host")
	}
	req := docker.CreateRequest{
		Image: s.image,
		// The token in the environment rather than the command line keeps it
		// out of the process list.
		Env:    []string{"TUNNEL_TOKEN=" + t.Token},
		Cmd:    []string{"tunnel", "--no-autoupdate", "run"},
		Labels: map[string]string{LabelConnector: t.ID},
		HostConfig: docker.HostConfig{
			NetworkMode:   "host",
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		},
	}
	if t.LANNetwork != "" {
		req.HostConfig.NetworkMode = t.LANNetwork
		if t.LANAddress != "" {
			req.NetworkingConfig = &docker.NetworkingConfig{EndpointsConfig: map[string]docker.EndpointSettings{
				t.LANNetwork: {IPAMConfig: &docker.EndpointIPAMConfig{IPv4Address: t.LANAddress}},
			}}
		}
	}
	step("Creating the " + Container + " container")
	id, _, err := s.dk(ctx).CreateContainer(ctx, Container, req)
	if err != nil {
		if pullErr != nil {
			return fmt.Errorf("pull %s: %w", s.image, pullErr)
		}
		return err
	}
	if t.LANNetwork != "" {
		// And on the default bridge, to reach the host's published ports.
		if err := s.dk(ctx).ConnectNetwork(ctx, "bridge", id, nil); err != nil {
			return fmt.Errorf("connect to the bridge network: %w", err)
		}
	}
	step("Starting it")
	if err := s.dk(ctx).StartContainer(ctx, id); err != nil {
		return err
	}
	if !s.followStartup(ctx, id, output) {
		output("cloudflared hasn't reported a connection yet; check its log below")
	}
	return nil
}

// removeConnector removes the cloudflared container, and one named
// gsh-cloudflared from before the rename. It stops cloudflared gracefully
// first, so it unregisters from Cloudflare and finishes requests in flight
// instead of being killed.
func (s *Service) removeConnector(ctx context.Context) error {
	for _, name := range []string{Container, legacyContainer} {
		if err := s.dk(ctx).StopContainer(ctx, name, 15); err != nil && !docker.IsNotFound(err) {
			s.log.Warn("stop cloudflared gracefully", "err", err)
		}
		if err := s.dk(ctx).RemoveContainer(ctx, name, true); err != nil && !docker.IsNotFound(err) {
			return fmt.Errorf("remove the %s container: %w", name, err)
		}
	}
	return nil
}

// RenameLegacyConnector renames a gsh-cloudflared container, from before
// the short name changed from gsh to hlm, to Container. Renaming leaves it
// running, so the tunnel stays up. It's meant to run once when hlm starts.
func (s *Service) RenameLegacyConnector(ctx context.Context) {
	if s.docker == nil {
		return
	}
	if _, err := s.dk(ctx).InspectContainer(ctx, Container); !docker.IsNotFound(err) {
		return
	}
	if _, err := s.dk(ctx).InspectContainer(ctx, legacyContainer); err != nil {
		return
	}
	if err := s.dk(ctx).RenameContainer(ctx, legacyContainer, Container); err != nil {
		s.log.Warn("rename cloudflared's container", "from", legacyContainer, "to", Container, "err", err)
		return
	}
	s.log.Info("renamed cloudflared's container", "from", legacyContainer, "to", Container)
}

// RecreateConnector replaces the cloudflared container, e.g. to run it on a
// newer image, keeping the tunnel and its settings.
func (s *Service) RecreateConnector(ctx context.Context, step, output func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok, err := s.tunnel()
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoTunnel
	}
	if err := s.startConnector(ctx, t, step, output); err != nil {
		return err
	}
	s.log.Info("recreated cloudflared", "container", Container)
	return nil
}

// RecreateConnectorOn replaces the cloudflared container on a host (this
// one, or another with a tunnel of its own).
func (s *Service) RecreateConnectorOn(ctx context.Context, host string, step, output func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok, err := s.tunnelOn(host)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoTunnel
	}
	hctx, err := s.onHost(ctx, host)
	if err != nil {
		return err
	}
	if err := s.startConnector(hctx, t, step, output); err != nil {
		return err
	}
	s.log.Info("recreated cloudflared", "container", Container, "host", host)
	return nil
}

// followStartup passes on cloudflared's log until it has connected, the
// container stops, or a few seconds pass. It reports whether it connected.
func (s *Service) followStartup(ctx context.Context, id string, output func(string)) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	logs, err := s.dk(ctx).Logs(ctx, id, docker.LogsOptions{Follow: true, Tail: "all"})
	if err != nil {
		output("Couldn't read the container's log: " + err.Error())
		return false
	}
	defer logs.Close()
	sc := bufio.NewScanner(logs)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r ")
		if line == "" {
			continue
		}
		output(line)
		if strings.Contains(line, connectedLine) {
			return true
		}
	}
	return false
}

// Control starts, stops or restarts the cloudflared container.
func (s *Service) Control(ctx context.Context, action string) (Status, error) {
	return s.ControlOn(ctx, manager.LocalHost, action)
}

// ControlOn starts, stops or restarts the cloudflared container on a host.
func (s *Service) ControlOn(ctx context.Context, host, action string) (Status, error) {
	if _, ok, err := s.tunnelOn(host); err != nil {
		return Status{}, err
	} else if !ok {
		return Status{}, ErrNoTunnel
	}
	base := ctx
	ctx, err := s.onHost(ctx, host)
	if err != nil {
		return Status{}, err
	}
	switch action {
	case "start":
		err = s.dk(ctx).StartContainer(ctx, Container)
	case "stop":
		err = s.dk(ctx).StopContainer(ctx, Container, 10)
	case "restart":
		err = s.dk(ctx).RestartContainer(ctx, Container, 10)
	default:
		return Status{}, fmt.Errorf("unknown action %q", action)
	}
	if docker.IsNotFound(err) {
		return Status{}, errors.New("the " + Container + " container is gone; set the tunnel up again")
	}
	if err != nil {
		return Status{}, err
	}
	s.log.Info("cloudflared "+action, "container", Container, "host", host)
	return s.Status(base)
}

// Logs returns cloudflared's log.
func (s *Service) Logs(ctx context.Context, opts docker.LogsOptions) (io.ReadCloser, error) {
	return s.LogsOn(ctx, manager.LocalHost, opts)
}

// LogsOn returns cloudflared's log on a host.
func (s *Service) LogsOn(ctx context.Context, host string, opts docker.LogsOptions) (io.ReadCloser, error) {
	if _, ok, err := s.tunnelOn(host); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoTunnel
	}
	ctx, err := s.onHost(ctx, host)
	if err != nil {
		return nil, err
	}
	return s.dk(ctx).Logs(ctx, Container, opts)
}

// Remove takes the tunnel down: it removes the container, the DNS records
// hlm made, and the tunnel in Cloudflare. The routes are kept, so setting a
// tunnel up again publishes them again.
func (s *Service) Remove(ctx context.Context) (Status, error) {
	return s.RemoveOn(ctx, manager.LocalHost)
}

// RemoveOn takes a host's tunnel down. Routes it carried go through this
// host's tunnel again, if there is one.
func (s *Service) RemoveOn(ctx context.Context, host string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok, err := s.tunnelOn(host)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, ErrNoTunnel
	}
	base := ctx
	hctx, err := s.onHost(ctx, host)
	if err != nil {
		return Status{}, err
	}
	if err := s.removeConnector(hctx); err != nil {
		return Status{}, err
	}
	c, a, err := s.client()
	if err != nil {
		return Status{}, err
	}
	applied, err := s.appliedAll(map[string]Tunnel{host: t})
	if err != nil {
		return Status{}, err
	}
	if ap, ok := applied[host]; ok {
		for _, r := range ap.Records {
			if err := s.deleteRecord(ctx, c, r, t.ID); err != nil {
				return Status{}, fmt.Errorf("remove the DNS record for %s: %w", r.Hostname, err)
			}
		}
	}
	// Cloudflare refuses to delete a tunnel with connections; cloudflared
	// is gone, so these are stale.
	if err := c.CleanupConnections(ctx, a.AccountID, t.ID); err != nil && !cloudflare.IsNotFound(err) {
		return Status{}, fmt.Errorf("drop the tunnel's connections: %w", err)
	}
	if err := c.DeleteTunnel(ctx, a.AccountID, t.ID); err != nil && !cloudflare.IsNotFound(err) {
		return Status{}, fmt.Errorf("delete the tunnel: %w", err)
	}
	if err := s.deleteAppliedOn(host); err != nil {
		return Status{}, err
	}
	if err := s.deleteTunnelOn(host); err != nil {
		return Status{}, err
	}
	s.log.Info("removed the Cloudflare Tunnel", "tunnel", t.Name, "host", host)
	// The routes it carried move to another tunnel, if one is left.
	if tunnels, err := s.tunnels(); err == nil && len(tunnels) > 0 {
		if _, err := s.apply(base, c, a); err != nil {
			s.log.Warn("re-apply Cloudflare routes", "err", err)
		}
	}
	return s.Status(base)
}

// deleteRecord removes a record hlm made, if it still points at the
// tunnel; one changed since is left alone.
func (s *Service) deleteRecord(ctx context.Context, c *cloudflare.Client, r appliedRecord, tunnelID string) error {
	recs, err := c.DNSRecords(ctx, r.ZoneID, r.Hostname)
	if cloudflare.IsNotFound(err) {
		return nil // the zone is gone
	}
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.ID == r.RecordID && rec.Type == "CNAME" && rec.Content == cloudflare.TunnelTarget(tunnelID) {
			if err := c.DeleteDNSRecord(ctx, r.ZoneID, rec.ID); err != nil && !cloudflare.IsNotFound(err) {
				return err
			}
			s.log.Info("removed DNS record", "hostname", r.Hostname)
		}
	}
	return nil
}

// --- routes ---

// WebPort is a TCP port a route can send traffic to.
type WebPort struct {
	Port int `json:"port"`
	// Container is the container's own port, for published ports.
	Container int `json:"container,omitempty"`
	// HostIP is the address it's published on, if not all of them.
	HostIP string `json:"hostIp,omitempty"`
}

// webPorts lists the TCP ports a server is reached on from the host,
// sorted and without duplicates.
func webPorts(info docker.ContainerJSON) []WebPort {
	seen := map[int]bool{}
	out := []WebPort{}
	add := func(p WebPort) {
		if !seen[p.Port] {
			seen[p.Port] = true
			out = append(out, p)
		}
	}
	switch network := info.HostConfig.NetworkMode; {
	case info.Config.Labels[deploy.LabelDirectIP] == "true" || network == "host":
		for key := range info.Config.ExposedPorts {
			port, proto, _ := strings.Cut(key, "/")
			if n, err := strconv.Atoi(port); err == nil && (proto == "" || proto == "tcp") {
				add(WebPort{Port: n})
			}
		}
	case network == "none":
	default:
		for key, bindings := range info.HostConfig.PortBindings {
			port, proto, _ := strings.Cut(key, "/")
			if proto != "" && proto != "tcp" {
				continue
			}
			cp, _ := strconv.Atoi(port)
			for _, b := range bindings {
				if hp, err := strconv.Atoi(b.HostPort); err == nil {
					p := WebPort{Port: hp, Container: cp}
					if ip, err := netip.ParseAddr(b.HostIP); err == nil && !ip.IsUnspecified() {
						p.HostIP = ip.String()
					}
					add(p)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// origin is where cloudflared sends a route's requests.
// remoteHost is where the server's Docker host publishes its ports, if
// that's not the local one.
func origin(info docker.ContainerJSON, r Route, t Tunnel, remoteHost string) (string, error) {
	var port *WebPort
	ports := webPorts(info)
	for i := range ports {
		if ports[i].Port == r.Port {
			port = &ports[i]
		}
	}
	if port == nil {
		return "", fmt.Errorf("%s doesn't use TCP port %d (any more)", r.Server, r.Port)
	}
	var host string
	switch {
	case info.Config.Labels[deploy.LabelDirectIP] == "true":
		if t.LANSubnet == "" {
			return "", errors.New("the server has its own LAN address, which cloudflared only reaches with \"Reach servers with their own LAN address\" set up for the tunnel")
		}
		subnet := netip.MustParsePrefix(t.LANSubnet)
		// Its fixed address too while it's stopped (asleep, say).
		for _, a := range deploy.ContainerAddresses(info) {
			if subnet.Contains(a) && host == "" {
				host = a.String()
			}
		}
		if host == "" {
			return "", fmt.Errorf("the server has no address on %s (%s), the LAN network cloudflared reaches; is it running?", t.LANNetwork, t.LANSubnet)
		}
	case remoteHost != "":
		if port.HostIP != "" && netip.MustParseAddr(port.HostIP).IsLoopback() {
			return "", fmt.Errorf("port %d is only published on %s of its host, which cloudflared here can't reach", r.Port, port.HostIP)
		}
		host = remoteHost
	case port.HostIP != "":
		if ip := netip.MustParseAddr(port.HostIP); ip.IsLoopback() && t.LANNetwork != "" {
			return "", fmt.Errorf("port %d is only published on %s, which cloudflared on a LAN network can't reach", r.Port, port.HostIP)
		}
		host = port.HostIP
	case t.LANNetwork != "":
		host = t.BridgeGateway
	default:
		host = "localhost"
	}
	return r.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(r.Port)), nil
}

// resolve finds where cloudflared sends a route's requests, from the
// tunnel on the host via.
func (s *Service) resolve(ctx context.Context, r Route, t Tunnel, via string) (string, error) {
	if r.Hlm {
		return s.selfOrigin(ctx, t)
	}
	if r.Address != "" {
		return r.Scheme + "://" + net.JoinHostPort(r.Address, strconv.Itoa(r.Port)), nil
	}
	info, err := s.servers.Inspect(ctx, r.Server)
	if err != nil {
		return "", err
	}
	// The tunnel on the server's own host reaches it as a local connector
	// would; another host's, over the LAN.
	remoteHost := ""
	if s.hostAddress != nil && manager.HostOf(info.ID) != via {
		remoteHost = s.hostAddress(info.ID)
		if remoteHost == "" && manager.HostOf(info.ID) != manager.LocalHost {
			return "", fmt.Errorf("%s's Docker host has no LAN address for the tunnel here to reach it; give it one, or set a tunnel up on that host", r.Server)
		}
	}
	return origin(info, r, t, remoteHost)
}

// normalizeAddress turns what the user typed as another server's address
// into an IP address or host name: without a scheme, port or path, and IPv6
// addresses without brackets.
func normalizeAddress(a string) string {
	a = strings.TrimSpace(a)
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	if i := strings.IndexAny(a, "/?#"); i >= 0 {
		a = a[:i]
	}
	if ip, err := netip.ParseAddr(strings.Trim(a, "[]")); err == nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(a); err == nil {
		a = host
	}
	return strings.TrimSuffix(strings.ToLower(a), ".")
}

// validAddress accepts IP addresses and host names, including single-label
// ones like "nas" that the LAN's DNS resolves.
func validAddress(a string) bool {
	if _, err := netip.ParseAddr(a); err == nil {
		return true
	}
	if a == "" || len(a) > 253 {
		return false
	}
	for _, l := range strings.Split(a, ".") {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// normalizeHostname turns what the user typed into a hostname: lower case,
// without a scheme, path or trailing dot.
func normalizeHostname(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validHostname(h string) bool {
	if len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// zoneOf finds the zone a hostname is in: the longest zone name it ends
// with.
func zoneOf(h string, zones []cloudflare.Zone) (cloudflare.Zone, bool) {
	var best cloudflare.Zone
	for _, z := range zones {
		if (h == z.Name || strings.HasSuffix(h, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best, best.ID != ""
}

// validateRoutes checks routes against the account's zones and the
// servers, and fills in their zones and defaults.
func (s *Service) validateRoutes(ctx context.Context, routes []Route, zones []cloudflare.Zone) ([]Route, error) {
	out := []Route{}
	seen := map[string]bool{}
	for _, r := range routes {
		r.Hostname = normalizeHostname(r.Hostname)
		if !validHostname(r.Hostname) {
			return nil, fmt.Errorf("invalid hostname %q: use a name like panel.example.com", r.Hostname)
		}
		if seen[r.Hostname] {
			return nil, fmt.Errorf("%s is used twice", r.Hostname)
		}
		seen[r.Hostname] = true
		z, ok := zoneOf(r.Hostname, zones)
		if !ok {
			return nil, fmt.Errorf("%s isn't in any of your Cloudflare domains (zones)", r.Hostname)
		}
		r.ZoneID, r.Zone = z.ID, z.Name
		if r.Hlm {
			r.Server, r.Address, r.Port, r.Scheme, r.NoTLSVerify = "", "", 0, "http", false
			out = append(out, r)
			continue
		}
		switch r.Scheme {
		case "":
			r.Scheme = "http"
		case "http", "https":
		default:
			return nil, fmt.Errorf("%s: the server's protocol must be http or https", r.Hostname)
		}
		if r.Scheme == "http" {
			r.NoTLSVerify = false
		}
		if r.Address != "" || r.Server == "" {
			r.Server, r.Address = "", normalizeAddress(r.Address)
			if !validAddress(r.Address) {
				return nil, fmt.Errorf("%s: invalid server address %q: use an IP address or host name, like 192.168.1.20 or nas.lan", r.Hostname, r.Address)
			}
			if r.Port < 1 || r.Port > 65535 {
				return nil, fmt.Errorf("%s: invalid port %d", r.Hostname, r.Port)
			}
			out = append(out, r)
			continue
		}
		info, err := s.servers.Inspect(ctx, r.Server)
		if err != nil {
			return nil, fmt.Errorf("%s: server %q: %w", r.Hostname, r.Server, err)
		}
		r.Server = strings.TrimPrefix(info.Name, "/")
		found := false
		for _, p := range webPorts(info) {
			found = found || p.Port == r.Port
		}
		if !found {
			return nil, fmt.Errorf("%s: %s doesn't use TCP port %d", r.Hostname, r.Server, r.Port)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// RouteState is a route and whether it's in place.
type RouteState struct {
	Route
	URL string `json:"url"`
	// Via is the Docker host whose tunnel carries it.
	Via string `json:"via"`
	// Origin is where cloudflared sends its requests; empty with Error.
	Origin string `json:"origin,omitempty"`
	Error  string `json:"error,omitempty"`
	// State is "published" (in Cloudflare as it should be), "pending"
	// (waiting to be applied), "conflict" (another DNS record has the
	// hostname) or "error".
	State string `json:"state"`
}

// ServerOption is a server routes can go to.
type ServerOption struct {
	Name    string    `json:"name"`
	Running bool      `json:"running"`
	Ports   []WebPort `json:"ports"`
	// WebUI is the template's web interface: its port and scheme, if it has one.
	WebUIPort   int    `json:"webuiPort,omitempty"`
	WebUIScheme string `json:"webuiScheme,omitempty"`
}

// Routes is the routes and their states.
type Routes struct {
	Routes []RouteState `json:"routes"`
	// Servers are the servers routes can go to.
	Servers []ServerOption `json:"servers"`
	// Tunnel means the tunnel is set up, so routes can be applied.
	Tunnel bool `json:"tunnel"`
	// Pending means something isn't applied yet.
	Pending bool `json:"pending"`
}

// webUI finds the port and scheme of a template's web interface link, e.g.
// http://[IP]:8080/.
func webUI(link string) (int, string) {
	u, err := url.Parse(strings.NewReplacer("[IP]", "localhost").Replace(link))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return 0, ""
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = map[string]int{"http": 80, "https": 443}[u.Scheme]
	}
	return port, u.Scheme
}

// Routes describes the routes without asking Cloudflare; their states come
// from what was last applied.
func (s *Service) Routes(ctx context.Context) (Routes, error) {
	out := Routes{Routes: []RouteState{}, Servers: []ServerOption{}}
	rs, err := s.routes()
	if err != nil {
		return out, err
	}
	tunnels, err := s.tunnels()
	if err != nil {
		return out, err
	}
	out.Tunnel = len(tunnels) > 0
	applied, err := s.appliedAll(tunnels)
	if err != nil {
		return out, err
	}
	out.Routes = s.states(ctx, rs, tunnels, applied)
	for _, r := range out.Routes {
		out.Pending = out.Pending || r.State == "pending"
	}
	// Records for routes that were removed are still in place until applied.
	for _, ap := range applied {
		for _, rec := range ap.Records {
			if !containsHost(rs, rec.Hostname) {
				out.Pending = true
			}
		}
	}
	servers, err := s.servers.List(ctx)
	if err != nil {
		return out, err
	}
	for _, srv := range servers {
		o := ServerOption{Name: srv.Name, Running: srv.State == "running", Ports: []WebPort{}}
		if info, err := s.servers.Inspect(ctx, srv.Name); err == nil {
			o.Ports = webPorts(info)
		}
		o.WebUIPort, o.WebUIScheme = webUI(srv.WebUI)
		out.Servers = append(out.Servers, o)
	}
	return out, nil
}

func containsHost(rs []Route, h string) bool {
	for _, r := range rs {
		if r.Hostname == h {
			return true
		}
	}
	return false
}

// states works out each route's tunnel, origin and state.
func (s *Service) states(ctx context.Context, rs []Route, tunnels map[string]Tunnel, applied map[string]appliedState) []RouteState {
	out := []RouteState{}
	for _, r := range rs {
		v := via(r, tunnels)
		st := RouteState{Route: r, URL: "https://" + r.Hostname, Via: v}
		ap := applied[v]
		var err error
		st.Origin, err = s.resolve(ctx, r, tunnels[v], v)
		switch {
		case err != nil:
			st.Error, st.State = err.Error(), "error"
		case ap.Conflicts[r.Hostname] != "":
			st.Error, st.State = ap.Conflicts[r.Hostname], "conflict"
		case published(ap, r, st.Origin):
			st.State = "published"
		default:
			st.State = "pending"
		}
		out = append(out, st)
	}
	return out
}

// published reports whether the applied state routes r to origin.
func published(ap appliedState, r Route, origin string) bool {
	hasRecord := false
	for _, rec := range ap.Records {
		hasRecord = hasRecord || rec.Hostname == r.Hostname
	}
	if !hasRecord {
		return false
	}
	want := ingressRule(r, origin)
	for _, rule := range ap.Ingress {
		if rule.Hostname == want.Hostname && rule.Service == want.Service && (rule.OriginRequest != nil) == (want.OriginRequest != nil) {
			return true
		}
	}
	return false
}

func ingressRule(r Route, origin string) cloudflare.IngressRule {
	rule := cloudflare.IngressRule{Hostname: r.Hostname, Service: origin}
	if r.NoTLSVerify {
		rule.OriginRequest = &cloudflare.OriginRequest{NoTLSVerify: true}
	}
	return rule
}

// Apply is the outcome of publishing the routes.
type Apply struct {
	Routes  []RouteState `json:"routes"`
	Created int          `json:"created"`
	Updated int          `json:"updated"`
	Removed int          `json:"removed"`
}

// SetRoutes replaces the routes and, if the tunnel is set up, applies them.
func (s *Service) SetRoutes(ctx context.Context, routes []Route) (Apply, error) {
	c, a, err := s.client()
	if err != nil {
		return Apply{}, err
	}
	zones, err := c.Zones(ctx, a.AccountID)
	if err != nil {
		return Apply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.validateRoutes(ctx, routes, zones)
	if err != nil {
		return Apply{}, err
	}
	if err := s.secrets.Set(secretRoutes, rs); err != nil {
		return Apply{}, err
	}
	s.log.Info("saved Cloudflare routes", "routes", len(rs))
	tunnels, err := s.tunnels()
	if err != nil {
		return Apply{}, err
	}
	if len(tunnels) == 0 {
		return Apply{Routes: s.states(ctx, rs, tunnels, nil)}, nil
	}
	return s.apply(ctx, c, a)
}

// Apply publishes the routes: the tunnel's ingress rules and the DNS
// records.
func (s *Service) Apply(ctx context.Context) (Apply, error) {
	c, a, err := s.client()
	if err != nil {
		return Apply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tunnels, err := s.tunnels()
	if err != nil {
		return Apply{}, err
	}
	if len(tunnels) == 0 {
		return Apply{}, ErrNoTunnel
	}
	return s.apply(ctx, c, a)
}

// apply does Apply's work for every tunnel: each carries the routes via
// it; s.mu must be held.
func (s *Service) apply(ctx context.Context, c *cloudflare.Client, a Account) (Apply, error) {
	var res Apply
	rs, err := s.routes()
	if err != nil {
		return res, err
	}
	tunnels, err := s.tunnels()
	if err != nil {
		return res, err
	}
	prevs, err := s.appliedAll(tunnels)
	if err != nil {
		return res, err
	}
	targets := map[string]bool{}
	for _, t := range tunnels {
		targets[cloudflare.TunnelTarget(t.ID)] = true
	}

	type plan struct {
		route  Route
		via    string
		origin string
		record *cloudflare.DNSRecord // ours or one to replace; nil to create
	}
	var plans []plan
	nexts := map[string]*appliedState{}
	ingress := map[string][]cloudflare.IngressRule{}
	for host, t := range tunnels {
		nexts[host] = &appliedState{TunnelID: t.ID, Conflicts: map[string]string{}, Records: []appliedRecord{}, At: time.Now().UTC()}
		ingress[host] = []cloudflare.IngressRule{}
	}
	conflicts := map[string]string{}

	// Look at each hostname's DNS records first: a hostname another record
	// already uses isn't routed.
	for _, r := range rs {
		v := via(r, tunnels)
		t, ok := tunnels[v]
		if !ok {
			continue // no tunnel reaches it; states says so
		}
		next, prev := nexts[v], prevs[v]
		o, err := s.resolve(ctx, r, t, v)
		if err != nil {
			// Reported by states. A server that's stopped, say, has no LAN
			// address now: keep what was published for it, so it works
			// again once the server is back.
			for _, rule := range prev.Ingress {
				if rule.Hostname == r.Hostname {
					ingress[v] = append(ingress[v], rule)
				}
			}
			for _, rec := range prev.Records {
				if rec.Hostname == r.Hostname {
					next.Records = append(next.Records, rec)
				}
			}
			continue
		}
		recs, err := c.DNSRecords(ctx, r.ZoneID, r.Hostname)
		if err != nil {
			return res, fmt.Errorf("look up %s: %w", r.Hostname, err)
		}
		p := plan{route: r, via: v, origin: o}
		for i, rec := range recs {
			ours := rec.Type == "CNAME" && (targets[rec.Content] || ((rec.Comment == RecordComment || rec.Comment == legacyRecordComment) && strings.HasSuffix(rec.Content, "."+cloudflare.TunnelDomain)))
			switch {
			case ours:
				p.record = &recs[i]
			case rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME":
				conflicts[r.Hostname] = fmt.Sprintf("%s already has a DNS record (%s %s) that hlm didn't make; delete it in Cloudflare or choose another hostname", r.Hostname, rec.Type, rec.Content)
				next.Conflicts[r.Hostname] = conflicts[r.Hostname]
			}
		}
		if conflicts[r.Hostname] != "" {
			continue
		}
		plans = append(plans, p)
		ingress[v] = append(ingress[v], ingressRule(r, o))
	}
	// Each tunnel's routes; anything else gets a 404 rather than reaching a
	// server.
	for _, host := range sortedHosts(tunnels) {
		rules := append(ingress[host], cloudflare.IngressRule{Service: "http_status:404"})
		if err := c.SetTunnelConfig(ctx, a.AccountID, tunnels[host].ID, cloudflare.TunnelConfig{Ingress: rules}); err != nil {
			return res, fmt.Errorf("set the routes of the tunnel %s: %w", tunnels[host].Name, err)
		}
		nexts[host].Ingress = rules
	}

	// Save what's done as it's done, so a failure part way still lets the
	// next apply (or Remove) clean up.
	save := func() error {
		for host, next := range nexts {
			if err := s.saveAppliedOn(host, *next); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range plans {
		target := cloudflare.TunnelTarget(tunnels[p.via].ID)
		want := cloudflare.DNSRecord{Type: "CNAME", Name: p.route.Hostname, Content: target, Proxied: true, TTL: 1, Comment: RecordComment}
		switch {
		case p.record == nil:
			rec, err := c.CreateDNSRecord(ctx, p.route.ZoneID, want)
			if err != nil {
				_ = save()
				return res, fmt.Errorf("add a DNS record for %s: %w", p.route.Hostname, err)
			}
			p.record = &rec
			res.Created++
			s.log.Info("added DNS record", "hostname", p.route.Hostname, "via", p.via)
		case p.record.Content != want.Content || !p.record.Proxied || p.record.Comment != want.Comment:
			if _, err := c.UpdateDNSRecord(ctx, p.route.ZoneID, p.record.ID, want); err != nil {
				_ = save()
				return res, fmt.Errorf("update the DNS record for %s: %w", p.route.Hostname, err)
			}
			res.Updated++
		}
		next := nexts[p.via]
		next.Records = append(next.Records, appliedRecord{Hostname: p.route.Hostname, ZoneID: p.route.ZoneID, RecordID: p.record.ID})
	}
	// Records for hostnames no longer routed (or now in conflict) go.
	kept := map[string]bool{}
	for _, next := range nexts {
		for _, rec := range next.Records {
			kept[rec.Hostname] = true
		}
	}
	for host, prev := range prevs {
		tunnelID := prev.TunnelID
		for _, old := range prev.Records {
			if kept[old.Hostname] {
				continue
			}
			if err := s.deleteRecord(ctx, c, old, tunnelID); err != nil {
				if next, ok := nexts[host]; ok {
					next.Records = append(next.Records, old) // try again next time
				}
				_ = save()
				return res, fmt.Errorf("remove the DNS record for %s: %w", old.Hostname, err)
			}
			res.Removed++
		}
	}
	if err := save(); err != nil {
		return res, err
	}
	applied := map[string]appliedState{}
	for host, next := range nexts {
		applied[host] = *next
	}
	res.Routes = s.states(ctx, rs, tunnels, applied)
	s.log.Info("applied Cloudflare routes", "routes", len(plans), "tunnels", len(tunnels), "created", res.Created, "updated", res.Updated, "removed", res.Removed)
	return res, nil
}

// ForgetServer drops a deleted server's routes and, if the tunnel is set
// up, unpublishes them.
func (s *Service) ForgetServer(ctx context.Context, name string) error {
	s.mu.Lock()
	rs, err := s.routes()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	kept := []Route{}
	for _, r := range rs {
		if r.Address != "" || r.Server != name {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rs) {
		s.mu.Unlock()
		return nil
	}
	err = s.secrets.Set(secretRoutes, kept)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if tunnels, _ := s.tunnels(); len(tunnels) == 0 {
		return nil
	}
	_, err = s.Apply(ctx)
	return err
}

// RunSync re-applies the routes every interval when a server's address or
// ports changed, e.g. after it was reconfigured, so its hostname keeps
// working. It does nothing until the routes were applied once.
func (s *Service) RunSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if tunnels, err := s.tunnels(); err != nil || len(tunnels) == 0 {
			continue
		}
		if applied, err := s.appliedAll(map[string]Tunnel{}); err != nil || len(applied) == 0 {
			continue
		}
		r, err := s.Routes(ctx)
		if err != nil || !r.Pending {
			continue
		}
		if _, err := s.Apply(ctx); err != nil {
			s.log.Warn("re-apply Cloudflare routes", "err", err)
		}
	}
}

// RenameServer moves a server's web addresses to its new name, when it moves to
// another Docker host (its name gains or changes its @host).
// The routes are applied again, so its hostnames reach it on its new host
// (through that host's own tunnel, if it has one).
func (s *Service) RenameServer(ctx context.Context, from, to string) error {
	s.mu.Lock()
	rs, err := s.routes()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	moved := false
	for i := range rs {
		if rs[i].Address == "" && rs[i].Server == from {
			rs[i].Server = to
			moved = true
		}
	}
	if !moved {
		s.mu.Unlock()
		return nil
	}
	err = s.secrets.Set(secretRoutes, rs)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if tunnels, _ := s.tunnels(); len(tunnels) == 0 {
		return nil
	}
	_, err = s.Apply(ctx)
	return err
}

// ServerHostnames lists the hostnames routed to a server, from the saved
// routes.
func (s *Service) ServerHostnames(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.routes()
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rs {
		if r.Address == "" && r.Server == name {
			out = append(out, r.Hostname)
		}
	}
	return out
}
