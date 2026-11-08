// Package proxy runs game server traffic through nginx on another machine
// (e.g. a VPS), managed over SSH: players connect to that machine, which
// relays TCP and UDP to the home network, so the home address stays private
// and the relay keeps no record of players' IP addresses.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// ConfigPath is the only nginx file hlm writes (besides adding one include
// to nginx.conf).
const ConfigPath = "/etc/nginx/hlm-stream.conf"

// legacyConfigPath was ConfigPath before the short name changed from gsh to
// hlm. A proxy whose nginx includes it keeps using it, so nginx's own files
// needn't change.
const (
	legacyConfigFile = "gsh-stream.conf"
	legacyConfigPath = "/etc/nginx/" + legacyConfigFile
)

const (
	secretKey      = "ssh-key"
	secretSettings = "proxy"
	secretState    = "proxy-state"
	// secretApplied records the ports relayed for each server in the
	// configuration on the relay.
	secretApplied = "proxy-applied-ports"
)

// Settings describe the relay machine and what it proxies.
type Settings struct {
	remote.Target
	// Upstream is where the relay sends traffic: the home network's public
	// address or hostname, or e.g. a WireGuard address.
	Upstream string `json:"upstream"`
	// UpstreamSource is "manual" (Upstream as entered), "unifi" (the
	// gateway's public address, looked up from UniFi and kept up to date) or
	// "wireguard" (this host's end of the WireGuard tunnel hlm set up; see
	// SetupWireGuard).
	UpstreamSource string `json:"upstreamSource"`
	// UnifiWAN is the gateway WAN used with the "unifi" source: "wan" or
	// "wan2".
	UnifiWAN string `json:"unifiWan,omitempty"`
	// UpstreamPorts is the port traffic is sent to at home: "internet" (the
	// port players connect to, which the home router forwards) or "local"
	// (the server's LAN port, when the home address reaches this host
	// directly, e.g. over a VPN).
	UpstreamPorts string `json:"upstreamPorts"`
	// LogMode is "noip" (log connections without client addresses) or "off"
	// (no access log at all).
	LogMode string `json:"logMode"`
	// Servers are the names of the game servers to proxy.
	Servers []string `json:"servers"`
	// StatusPage serves the status page's sites on the proxy, through the
	// WireGuard tunnel. StatusTLS is how they get HTTPS: "" (none),
	// "letsencrypt" (certbot on the proxy; StatusEmail gets the expiry
	// notices) or "cloudflare" (Cloudflare in front, with an Origin CA
	// certificate on the proxy).
	StatusPage  bool   `json:"statusPage"`
	StatusTLS   string `json:"statusTls,omitempty"`
	StatusEmail string `json:"statusEmail,omitempty"`
}

// KeyInfo is the public half of hlm's SSH key.
type KeyInfo struct {
	Public      string    `json:"public"`
	Fingerprint string    `json:"fingerprint"`
	Created     time.Time `json:"created"`
}

// Lister finds game servers.
type Lister interface {
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
}

// Service manages the relay.
type Service struct {
	secrets *secrets.Store
	servers Lister
	log     *slog.Logger
	mu      sync.Mutex // one remote operation at a time

	// run runs a script as root on the relay, passing each line of output
	// to onLine (if non-nil) as it's printed; tests replace it.
	run func(ctx context.Context, script string, stdin []byte, onLine func(string)) (remote.Result, error)
	// dockerFor is other Docker hosts' Docker, for the tunnel's ends there;
	// see UseDockerHosts.
	dockerFor func(host string) (Containers, error)
	// statusSites finds the status page's sites; see UseStatusSites.
	statusSites func(ctx context.Context) ([]string, int, error)
	// cloudflare puts the status page's sites behind Cloudflare; see
	// UseCloudflare.
	cloudflare Cloudflare
	// ports lists a server's ports; see UsePorts.
	ports func(info docker.ContainerJSON) ([]forwarding.Mapping, error)
	// wanLookup finds the gateway's public address; see UseWANLookup.
	wanLookup func(ctx context.Context, wan string) (string, error)
	// linodeBase overrides the Linode API's address (for tests).
	linodeBase string
	// docker runs the home end of the WireGuard tunnel; see UseDocker.
	docker  Containers
	wgImage string
	applyMu sync.Mutex // one Apply at a time, so the recorded state matches the relay
	checkMu sync.Mutex // one WAN check at a time
	wgMu    sync.Mutex // one WireGuard setup or removal at a time

	// notifier hears about home address changes; see UseNotifier.
	notifier notify.Notifier
}

// New creates the service.
func New(store *secrets.Store, servers Lister, log *slog.Logger) *Service {
	s := &Service{secrets: store, servers: servers, log: log}
	s.run = s.runPrivileged
	s.ports = func(info docker.ContainerJSON) ([]forwarding.Mapping, error) {
		return forwarding.Mappings(info, nil), nil
	}
	return s
}

// UsePorts sets how servers' ports, with their internet ports, are found.
func (s *Service) UsePorts(fn func(info docker.ContainerJSON) ([]forwarding.Mapping, error)) {
	s.ports = fn
}

// --- key ---

func (s *Service) key() (remote.KeyPair, bool, error) {
	var kp remote.KeyPair
	ok, err := s.secrets.Get(secretKey, &kp)
	return kp, ok, err
}

// KeyPair returns hlm's SSH key pair, which other features (such as other
// Docker hosts reached over SSH) sign in with too.
func (s *Service) KeyPair() (remote.KeyPair, bool, error) { return s.key() }

// Key returns the public key, or nil if there is none yet.
func (s *Service) Key() (*KeyInfo, error) {
	kp, ok, err := s.key()
	if err != nil || !ok {
		return nil, err
	}
	return &KeyInfo{Public: kp.Public, Fingerprint: kp.Fingerprint, Created: kp.Created}, nil
}

// EnsureKey returns the key, generating one if there is none. With
// replace, a new key replaces the current one.
func (s *Service) EnsureKey(replace bool) (*KeyInfo, error) {
	if !replace {
		if k, err := s.Key(); err != nil || k != nil {
			return k, err
		}
	}
	kp, err := remote.GenerateKey("home-lab-manager")
	if err != nil {
		return nil, err
	}
	if err := s.secrets.Set(secretKey, kp); err != nil {
		return nil, err
	}
	s.log.Info("generated SSH key", "fingerprint", kp.Fingerprint)
	return s.Key()
}

// ImportKey stores an existing private key instead of a generated one.
func (s *Service) ImportKey(private string) (*KeyInfo, error) {
	kp, err := remote.ParseKey(private, "home-lab-manager")
	if err != nil {
		return nil, err
	}
	if err := s.secrets.Set(secretKey, kp); err != nil {
		return nil, err
	}
	s.log.Info("imported SSH key", "fingerprint", kp.Fingerprint)
	return s.Key()
}

// --- settings ---

// Settings returns the saved settings, if any.
func (s *Service) Settings() (Settings, bool, error) {
	var st Settings
	ok, err := s.secrets.Get(secretSettings, &st)
	return st, ok, err
}

var (
	hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	userName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
)

func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return hostName.MatchString(h)
}

// Validate checks and normalizes settings.
func Validate(st *Settings) error {
	st.Host, st.User, st.Upstream = strings.TrimSpace(st.Host), strings.TrimSpace(st.User), strings.TrimSpace(st.Upstream)
	st.StatusEmail = strings.TrimSpace(st.StatusEmail)
	if st.StatusEmail != "" && !statusEmail.MatchString(st.StatusEmail) {
		return fmt.Errorf("%q isn't an email address", st.StatusEmail)
	}
	switch st.StatusTLS {
	case "", TLSLetsEncrypt, TLSCloudflare:
	default:
		return fmt.Errorf("unknown HTTPS option %q for the status page's sites", st.StatusTLS)
	}
	if !validHost(st.Host) {
		return fmt.Errorf("invalid host %q", st.Host)
	}
	if st.Port == 0 {
		st.Port = 22
	}
	if st.Port < 1 || st.Port > 65535 {
		return fmt.Errorf("invalid SSH port %d", st.Port)
	}
	if !userName.MatchString(st.User) {
		return fmt.Errorf("invalid user name %q", st.User)
	}
	// The upstream ends up in nginx's config, so it must be a plain address
	// or hostname, with nothing nginx would parse as syntax.
	switch st.UpstreamSource {
	case "", "manual":
		st.UpstreamSource, st.UnifiWAN = "manual", ""
		if st.Upstream != "" && !validHost(st.Upstream) {
			return fmt.Errorf("invalid home address %q: use an IP address or hostname, without a port", st.Upstream)
		}
	case "wireguard":
		// The tunnel's home address, filled in by Plan. It reaches this host
		// directly, so traffic goes to the servers' own ports.
		st.Upstream, st.UnifiWAN, st.UpstreamPorts = "", "", "local"
	case "unifi":
		// The address is looked up whenever it's needed.
		st.Upstream = ""
		switch st.UnifiWAN {
		case "":
			st.UnifiWAN = "wan"
		case "wan", "wan2":
		default:
			return fmt.Errorf("unknown WAN %q: use wan or wan2", st.UnifiWAN)
		}
	default:
		return fmt.Errorf("unknown home address source %q", st.UpstreamSource)
	}
	switch st.UpstreamPorts {
	case "":
		st.UpstreamPorts = "internet"
	case "internet", "local":
	default:
		return fmt.Errorf("unknown home port setting %q", st.UpstreamPorts)
	}
	if st.UpstreamSource == "unifi" {
		// The gateway's address only has the forwarded ports open.
		st.UpstreamPorts = "internet"
	}
	switch st.LogMode {
	case "":
		st.LogMode = "noip"
	case "noip", "off":
	default:
		return fmt.Errorf("unknown log setting %q", st.LogMode)
	}
	if st.Servers == nil {
		st.Servers = []string{}
	}
	return nil
}

// Save stores the settings.
func (s *Service) Save(st Settings) (Settings, error) {
	if err := Validate(&st); err != nil {
		return st, err
	}
	return st, s.secrets.Set(secretSettings, st)
}

// SetRelayed adds a server to the servers the proxy relays, or removes it.
// It takes effect when the configuration is next applied.
func (s *Service) SetRelayed(server string, on bool) error {
	st, err := s.saved()
	if err != nil {
		return err
	}
	var names []string
	for _, n := range st.Servers {
		if n != server {
			names = append(names, n)
		}
	}
	if on {
		names = append(names, server)
	}
	sort.Strings(names)
	st.Servers = names
	_, err = s.Save(st)
	return err
}

// RenameServer moves a relayed server to its new name, when it moves to
// another Docker host, and applies the proxy's configuration again, so the
// relay reaches it there. It reports whether the server was relayed.
func (s *Service) RenameServer(ctx context.Context, from, to string) (bool, error) {
	if !s.Relayed(from) {
		return false, nil
	}
	if err := s.SetRelayed(from, false); err != nil {
		return true, err
	}
	if err := s.SetRelayed(to, true); err != nil {
		return true, err
	}
	_, err := s.Apply(ctx)
	return true, err
}

// Relayed reports whether the proxy relays a server.
func (s *Service) Relayed(server string) bool {
	st, ok, err := s.Settings()
	if err != nil || !ok {
		return false
	}
	return slices.Contains(st.Servers, server)
}

// Delete forgets the settings (the key is kept).
func (s *Service) Delete() error {
	if err := s.secrets.Delete(secretState); err != nil {
		return err
	}
	return s.secrets.Delete(secretSettings)
}

// --- remote operations ---

// ErrStreamBlock means nginx has its own stream block, which hlm must adopt
// (see Adopt) before it can apply.
var ErrStreamBlock = fmt.Errorf("nginx already has its own stream { } block; let hlm add \"%s\" inside it, then apply again", includeLine)

// ErrNotConfigured means no relay has been set up.
var ErrNotConfigured = errors.New("no proxy server is set up yet")

func (s *Service) dial(ctx context.Context, t remote.Target) (*remote.Conn, error) {
	kp, ok, err := s.key()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("generate or import an SSH key first")
	}
	return remote.Dial(ctx, t, kp)
}

// privileged wraps a script so it runs as root: directly for root, else
// through sudo, which must not ask for a password.
func privileged(t remote.Target, script string) string {
	if t.User == "root" {
		return "sh -c " + remote.Quote(script)
	}
	return "sudo -n sh -c " + remote.Quote(script)
}

// Info describes the relay machine.
type Info struct {
	OS          string `json:"os,omitempty"`
	Root        bool   `json:"root"`
	Sudo        bool   `json:"sudo"`            // passwordless sudo works
	Nginx       string `json:"nginx,omitempty"` // version, if installed
	Stream      bool   `json:"stream"`          // stream module available
	Running     bool   `json:"running"`
	Included    bool   `json:"included"`    // nginx's configuration includes hlm's file
	StreamBlock bool   `json:"streamBlock"` // nginx has its own stream block
	// StreamFile is the file holding nginx's own stream block.
	StreamFile string `json:"streamFile,omitempty"`
	Configured bool   `json:"configured"` // hlm's file exists
	// ConfigFile is hlm's file on the proxy: ConfigPath, or legacyConfigPath
	// when nginx includes that.
	ConfigFile     string `json:"configFile"`
	PackageManager string `json:"packageManager,omitempty"`
}

const infoScript = `
echo "os=$( . /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
[ "$(id -u)" = 0 ] && echo root=yes
sudo -n true 2>/dev/null && echo sudo=yes
PATH="$PATH:/usr/sbin:/sbin"
if command -v nginx >/dev/null 2>&1; then
  echo "nginx=$(nginx -v 2>&1 | sed -n 's|.*nginx/||p')"
  nginx -V 2>&1 | grep -q -- '--with-stream' && echo stream=yes
  ls /etc/nginx/modules-enabled 2>/dev/null | grep -q stream && echo stream=yes
  ls /usr/lib64/nginx/modules /usr/lib/nginx/modules 2>/dev/null | grep -q stream && echo stream=yes
fi
(pidof nginx || pgrep -x nginx) >/dev/null 2>&1 && echo running=yes
# The whole configuration, with the files nginx.conf includes; reading it
# may need root.
dump=$( (nginx -T || sudo -n nginx -T) 2>/dev/null ) || dump=$(cat /etc/nginx/nginx.conf 2>/dev/null)
` + chooseConfScript + `
included "${conf##*/}" && echo included=yes
printf '%s\n' "$dump" | awk '/^# configuration file .*:$/ { f = substr($0, 22, length($0) - 22) } /^[[:space:]]*stream[[:space:]]*[{]/ { print "streamblock=" f; exit }'
[ -f "$conf" ] && echo configured=yes
echo "conf=$conf"
if command -v apt-get >/dev/null 2>&1; then echo pkg=apt; elif command -v dnf >/dev/null 2>&1; then echo pkg=dnf; fi
exit 0
`

func parseInfo(out string) Info {
	var i Info
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "os":
			i.OS = v
		case "root":
			i.Root = true
		case "sudo":
			i.Sudo = true
		case "nginx":
			i.Nginx = v
			if i.Nginx == "" {
				i.Nginx = "installed"
			}
		case "stream":
			i.Stream = true
		case "running":
			i.Running = true
		case "included":
			i.Included = true
		case "streamblock":
			i.StreamBlock = true
			i.StreamFile = v
		case "configured":
			i.Configured = true
		case "pkg":
			i.PackageManager = v
		case "conf":
			i.ConfigFile = v
		}
	}
	return i
}

// Test connects with the given (possibly unsaved) settings and inspects the
// machine.
func (s *Service) Test(ctx context.Context, st Settings) (Info, error) {
	if err := Validate(&st); err != nil {
		return Info{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.dial(ctx, st.Target)
	if err != nil {
		return Info{}, err
	}
	defer c.Close()
	res, err := c.Run(ctx, infoScript, nil)
	if err != nil {
		return Info{}, err
	}
	return parseInfo(res.Stdout), nil
}

func (s *Service) saved() (Settings, error) {
	st, ok, err := s.Settings()
	if err != nil {
		return st, err
	}
	if !ok {
		return st, ErrNotConfigured
	}
	return st, nil
}

// runPrivileged runs a script as root on the saved relay.
func (s *Service) runPrivileged(ctx context.Context, script string, stdin []byte, onLine func(string)) (remote.Result, error) {
	st, err := s.saved()
	if err != nil {
		return remote.Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.dial(ctx, st.Target)
	if err != nil {
		return remote.Result{}, err
	}
	defer c.Close()
	res, err := c.RunLive(ctx, privileged(st.Target, script), stdin, onLine)
	if err == nil && st.User != "root" && strings.Contains(res.Stderr, "sudo: a password is required") {
		return res, fmt.Errorf("%s needs to use sudo without a password; allow it for hlm's commands (e.g. a file in /etc/sudoers.d) or connect as root", st.User)
	}
	return res, err
}

const installScript = `
PATH="$PATH:/usr/sbin:/sbin"
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q && apt-get install -y -q nginx libnginx-mod-stream || exit 1
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y nginx nginx-mod-stream || exit 1
else
  echo "Only apt (Debian, Ubuntu) and dnf (Fedora, RHEL) systems can be set up automatically; install nginx with its stream module yourself." >&2
  exit 2
fi
if [ -d /run/systemd/system ]; then systemctl enable --now nginx; else (pidof nginx || pgrep -x nginx) >/dev/null 2>&1 || nginx; fi
`

// Install installs nginx with its stream module.
func (s *Service) Install(ctx context.Context) (string, error) {
	res, err := s.run(ctx, installScript, nil, nil)
	if err != nil {
		return res.Output(), err
	}
	if res.ExitCode != 0 {
		return res.Output(), fmt.Errorf("installing nginx failed (exit status %d)", res.ExitCode)
	}
	s.log.Info("installed nginx on the proxy server")
	return res.Output(), nil
}

// --- configuration ---

// Proxied is a server and the ports the relay forwards for it.
type Proxied struct {
	Server string               `json:"server"`
	Ports  []forwarding.Mapping `json:"ports"`
	// Address, if set, is where the relay sends this server's traffic
	// instead of the home address: its own LAN address, reached through the
	// WireGuard tunnel. Traffic then goes to the server's own ports.
	Address string `json:"address,omitempty"`
	// Upstream, if set, replaces the home address for this server: the
	// WireGuard tunnel's end on the Docker host it runs on.
	Upstream string `json:"upstream,omitempty"`
	// Skipped are ports whose internet port another proxied server already
	// uses.
	Skipped []forwarding.Mapping `json:"skipped,omitempty"`
	Error   string               `json:"error,omitempty"`
}

// Plan works out what the relay forwards, from the saved settings. The
// returned settings have the upstream filled in when it comes from UniFi.
func (s *Service) Plan(ctx context.Context) (Settings, []Proxied, error) {
	st, err := s.saved()
	if err != nil {
		return st, nil, err
	}
	if st.UpstreamSource == "unifi" {
		if st.Upstream, err = s.lookupWAN(ctx, st.UnifiWAN); err != nil {
			return st, nil, fmt.Errorf("look up the home address from UniFi: %w", err)
		}
	}
	if st.UpstreamSource == "wireguard" {
		wg, ok, err := s.tunnel()
		if err != nil {
			return st, nil, err
		}
		if !ok {
			return st, nil, errors.New("the home address is the WireGuard tunnel, which isn't set up; set it up or choose another home address")
		}
		st.Upstream = wg.HomeAddress().String()
	}
	return st, s.relayed(ctx, st), nil
}

// relayed works out the ports relayed for each of the settings' servers.
func (s *Service) relayed(ctx context.Context, st Settings) []Proxied {
	names := append([]string(nil), st.Servers...)
	sort.Strings(names)
	var wg WireGuard
	if st.UpstreamSource == "wireguard" {
		wg, _, _ = s.tunnel()
	}
	taken := map[forwarding.Port]string{}
	var out []Proxied
	for _, name := range names {
		p := Proxied{Server: name, Ports: []forwarding.Mapping{}}
		info, err := s.servers.Inspect(ctx, name)
		var ports []forwarding.Mapping
		if err == nil {
			ports, err = s.ports(info)
		}
		if err != nil {
			p.Error = err.Error()
			out = append(out, p)
			continue
		}
		host := hostOfID(info.ID)
		switch {
		case st.UpstreamSource == "wireguard" && info.Config.Labels[deploy.LabelDirectIP] == "true":
			// A container with its own (macvlan/ipvlan) LAN address is
			// reached by whichever end of the tunnel is on its LAN.
			addr, _, err := wg.lanReach(info, host)
			if err != nil {
				p.Error = err.Error()
				out = append(out, p)
				continue
			}
			p.Address, p.Upstream = addr.String(), ""
		case st.UpstreamSource == "wireguard" && host != localHost:
			// A server on another host goes through that host's end.
			e, ok := wg.end(host)
			if !ok {
				p.Error = "is on the Docker host " + host + ", which has no end of the WireGuard tunnel: set one up for it under the tunnel"
				out = append(out, p)
				continue
			}
			p.Upstream = e.HomeAddress().String()
		}
		for _, m := range ports {
			if !m.Proxy {
				continue
			}
			key := forwarding.Port{Number: m.Port, Proto: m.Proto}
			if _, dup := taken[key]; dup {
				p.Skipped = append(p.Skipped, m)
				continue
			}
			taken[key] = name
			p.Ports = append(p.Ports, m)
		}
		out = append(out, p)
	}
	return out
}

// Render produces the nginx stream configuration.
func Render(st Settings, servers []Proxied) string {
	var b strings.Builder
	b.WriteString("# Managed by Home Lab Manager; changes made here are overwritten.\n")
	fmt.Fprintf(&b, "# Relays game server traffic to %s.\n\n", st.Upstream)
	if st.LogMode == "noip" {
		b.WriteString("# Connections are logged without the client's address.\n")
		b.WriteString("log_format hlm_noip '$time_local $protocol port=$server_port status=$status " +
			"sent=$bytes_sent received=$bytes_received duration=$session_time';\n")
	} else {
		b.WriteString("# Connections aren't logged.\n")
	}
	upstream := st.Upstream
	if a, err := netip.ParseAddr(upstream); err == nil && a.Is6() {
		upstream = "[" + upstream + "]"
	}
	for _, p := range servers {
		if len(p.Ports) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n# %s\n", p.Server)
		to := upstream
		if p.Upstream != "" {
			to = p.Upstream
		}
		if p.Address != "" {
			to = p.Address
		}
		for _, port := range p.Ports {
			listen := strconv.Itoa(port.Port)
			if port.Proto == "udp" {
				listen += " udp"
			}
			toPort := port.Port
			if st.UpstreamPorts == "local" || p.Address != "" {
				toPort = port.Local
			}
			fmt.Fprintf(&b, "server {\n    listen %s;\n    proxy_pass %s:%d;\n", listen, to, toPort)
			if st.LogMode == "noip" {
				b.WriteString("    access_log /var/log/nginx/hlm-stream.log hlm_noip;\n")
			} else {
				b.WriteString("    access_log off;\n")
			}
			// nginx's error messages include the client's address, so only
			// keep the ones that matter.
			b.WriteString("    error_log /var/log/nginx/hlm-stream-error.log crit;\n}\n")
		}
	}
	return b.String()
}

// Preview returns the configuration that Apply would write.
func (s *Service) Preview(ctx context.Context) (string, []Proxied, error) {
	st, servers, err := s.Plan(ctx)
	if err != nil {
		return "", nil, err
	}
	return Render(st, servers), servers, nil
}

// chooseConfScript sets conf to hlm's file on the proxy, given nginx's
// configuration in dump: ConfigPath, or legacyConfigPath when nginx
// includes that (and not ConfigPath). included reports whether nginx
// includes a file, by name.
const chooseConfScript = `included() { printf '%s\n' "$dump" | grep -E '^[[:space:]]*include[[:space:]]' | grep -qF "$1"; }
conf=` + ConfigPath + `
if ! included "${conf##*/}" && included ` + legacyConfigFile + `; then conf=` + legacyConfigPath + `; fi`

// applyScript installs the config read from stdin, makes sure nginx.conf
// includes it, and reloads nginx if "nginx -t" passes. If the test fails,
// the previous files are restored.
const applyScript = `
PATH="$PATH:/usr/sbin:/sbin"
main=/etc/nginx/nginx.conf
[ -f "$main" ] || { echo "hlm:nonginx"; exit 3; }
dump=$(nginx -T 2>/dev/null) || dump=$(cat "$main")
` + chooseConfScript + `
tmp=$(mktemp) || exit 1
cat > "$tmp"
had=0
if [ -f "$conf" ]; then had=1; cp -p "$conf" "$conf.bak"; fi
install -m 644 "$tmp" "$conf"; rm -f "$tmp"
added=0
if ! included "${conf##*/}"; then
  if printf '%s\n' "$dump" | grep -qE '^[[:space:]]*stream[[:space:]]*[{]'; then
    echo "hlm:streamblock"
    if [ $had = 1 ]; then mv "$conf.bak" "$conf"; else rm -f "$conf"; fi
    exit 3
  fi
  cp -p "$main" "$main.hlm-bak"
  printf '\n# Added by Home Lab Manager: relays for game servers.\nstream {\n    include %s;\n}\n' "$conf" >> "$main"
  added=1
fi
if ! out=$(nginx -t 2>&1); then
  echo "$out"
  if [ $had = 1 ]; then mv "$conf.bak" "$conf"; else rm -f "$conf"; fi
  if [ $added = 1 ]; then mv "$main.hlm-bak" "$main"; fi
  echo "hlm:testfailed"
  exit 4
fi
echo "$out"
if [ -d /run/systemd/system ] && systemctl cat nginx >/dev/null 2>&1; then
  systemctl reload-or-restart nginx || exit 5
elif ! nginx -s reload 2>/dev/null; then
  nginx || exit 5
fi
echo "hlm:applied"
`

// ApplyResult is the outcome of applying the configuration.
type ApplyResult struct {
	Config  string    `json:"config"`
	Output  string    `json:"output"`
	Servers []Proxied `json:"servers"`
	// Firewall is the outcome of updating the proxy server's cloud
	// firewall, if one is set up.
	Firewall *FirewallSync `json:"firewall,omitempty"`
	// Status is how serving the status page's sites went, if that's on.
	Status *StatusSiteResult `json:"status,omitempty"`
}

// Apply writes the configuration to the relay and reloads nginx.
func (s *Service) Apply(ctx context.Context) (ApplyResult, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	st, servers, err := s.Plan(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	if st.Upstream == "" {
		return ApplyResult{}, errors.New("set the home address the proxy should send traffic to")
	}
	config := Render(st, servers)
	res, err := s.run(ctx, applyScript, []byte(config), nil)
	out := strings.TrimSpace(strings.NewReplacer("hlm:applied", "", "hlm:testfailed", "", "hlm:streamblock", "", "hlm:nonginx", "").Replace(res.Output()))
	r := ApplyResult{Config: config, Output: out, Servers: servers}
	if err != nil {
		return r, err
	}
	switch {
	case strings.Contains(res.Stdout, "hlm:applied"):
		s.log.Info("applied proxy configuration", "servers", len(servers), "upstream", st.Upstream)
		if err := s.recordApplied(st.Upstream, servers); err != nil {
			s.log.Error("save proxy state", "err", err)
		}
		r.Status = s.applyStatusRecorded(ctx, st)
		r.Firewall = s.syncFirewallAfter(ctx, servers)
		return r, nil
	case strings.Contains(res.Stdout, "hlm:nonginx"):
		return r, errors.New("nginx isn't installed on the proxy server")
	case strings.Contains(res.Stdout, "hlm:streamblock"):
		return r, ErrStreamBlock
	case strings.Contains(res.Stdout, "hlm:testfailed"):
		if strings.Contains(out, `unknown directive "stream"`) {
			return r, errors.New("nginx's stream module isn't installed (on Debian/Ubuntu: libnginx-mod-stream); the previous configuration was kept")
		}
		return r, errors.New("nginx rejected the configuration, so the previous one was kept")
	default:
		return r, fmt.Errorf("applying the configuration failed (exit status %d)", res.ExitCode)
	}
}

// RemoveResult is the outcome of removing the relays.
type RemoveResult struct {
	Output   string        `json:"output"`
	Firewall *FirewallSync `json:"firewall,omitempty"`
}

// Remove deletes hlm's configuration from the relay and reloads nginx, and
// closes the relayed ports in the cloud firewall.
func (s *Service) Remove(ctx context.Context) (RemoveResult, error) {
	script := `PATH="$PATH:/usr/sbin:/sbin"
for f in ` + ConfigPath + ` ` + legacyConfigPath + ` ` + StatusConfigPath + `; do [ -f "$f" ] && : > "$f"; done
nginx -t 2>&1 || exit 4
if [ -d /run/systemd/system ] && systemctl cat nginx >/dev/null 2>&1; then systemctl reload nginx; else nginx -s reload; fi`
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	res, err := s.run(ctx, script, nil, nil)
	if err != nil {
		return RemoveResult{Output: res.Output()}, err
	}
	if res.ExitCode != 0 {
		return RemoveResult{Output: res.Output()}, fmt.Errorf("removing the proxies failed (exit status %d)", res.ExitCode)
	}
	// Nothing is applied any more, so a WAN change has nothing to update.
	if err := s.recordApplied("", nil); err != nil {
		s.log.Error("save proxy state", "err", err)
	}
	_ = s.secrets.Delete(secretStatusServed)
	_ = s.secrets.Delete(secretStatusResult)
	s.forgetCloudflare(ctx, nil)
	return RemoveResult{Output: res.Output(), Firewall: s.syncFirewallAfter(ctx, nil)}, nil
}

// Route is how the proxy reaches one server, for showing players' path to
// it.
type Route struct {
	// Source is the home address source: manual, unifi or wireguard.
	Source string `json:"source"`
	// ProxyHost is the proxy server players connect to.
	ProxyHost string `json:"proxyHost"`
	// Target is where nginx sends the server's traffic: the home address,
	// the tunnel's end on this host, or the server's own LAN address
	// through the tunnel. Empty when it isn't known yet (UniFi, before the
	// first apply).
	Target string `json:"target,omitempty"`
	// OwnPorts means traffic goes to the server's own ports rather than its
	// internet ports.
	OwnPorts bool `json:"ownPorts"`
	// ServerAddress means Target is the server's own LAN address (reached
	// through the tunnel in LAN mode), not this host.
	ServerAddress bool `json:"serverAddress,omitempty"`
	// Error is why the server can't be relayed this way.
	Error string `json:"error,omitempty"`
}

// RouteFor works out how the proxy would reach a server, without
// contacting UniFi or the proxy server.
func (s *Service) RouteFor(ctx context.Context, server string) (Route, error) {
	st, err := s.saved()
	if err != nil {
		return Route{}, err
	}
	r := Route{Source: st.UpstreamSource, ProxyHost: st.Host, Target: st.Upstream, OwnPorts: st.UpstreamPorts == "local"}
	switch st.UpstreamSource {
	case "unifi":
		state, _ := s.State()
		r.Target = state.Applied
	case "wireguard":
		wg, ok, err := s.tunnel()
		if err != nil {
			return r, err
		}
		if !ok {
			r.Error = "the WireGuard tunnel isn't set up"
			return r, nil
		}
		r.Target = wg.HomeAddress().String()
	}
	one := st
	one.Servers = []string{server}
	if p := s.relayed(ctx, one); len(p) == 1 {
		if p[0].Upstream != "" {
			r.Target = p[0].Upstream
		}
		if p[0].Address != "" {
			r.Target, r.OwnPorts, r.ServerAddress = p[0].Address, true, true
		}
		r.Error = p[0].Error
	}
	return r, nil
}
