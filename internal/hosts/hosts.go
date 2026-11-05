// Package hosts keeps the other Docker hosts hlm manages servers on, and
// connects to them: Docker's API over TCP with TLS, or Docker's socket on
// the host reached over SSH.
package hosts

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const (
	secretHosts = "hosts"
	// The TLS client key of a host, kept apart so it's never sent back.
	secretKeyPrefix = "host-tls-key-"
)

// Kinds of connection.
const (
	KindTCP = "tcp"
	KindSSH = "ssh"
)

// DefaultSocket is where Docker's socket usually is.
const DefaultSocket = "/var/run/docker.sock"

// ErrNoDocker is returned for an SSH host without Docker; Inspect and
// SetUpDocker can install it.
var ErrNoDocker = errors.New("Docker doesn't seem to be installed on the host")

// ErrNotFound is returned for a host that doesn't exist.
var ErrNotFound = errors.New("no such host")

// Host is another Docker host.
type Host struct {
	// ID names the host in server names (minecraft@nas): lower-case
	// letters, digits and dashes. It can't change.
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`

	// URL is the TCP address of Docker's API, tcp://nas.lan:2376.
	URL string `json:"url,omitempty"`
	// CA is the certificate authority that signed the daemon's
	// certificate (PEM); without it the system's roots are used.
	CA string `json:"ca,omitempty"`
	// Cert is hlm's client certificate (PEM); its key is kept apart.
	Cert   string `json:"cert,omitempty"`
	HasKey bool   `json:"hasKey,omitempty"`

	// SSH is the machine to sign in to, as hlm's SSH key; its HostKey
	// must be trusted. Socket is Docker's socket there.
	SSH    *remote.Target `json:"ssh,omitempty"`
	Socket string         `json:"socket,omitempty"`

	// AppdataRoot is where servers' data folders go on the host.
	AppdataRoot string `json:"appdataRoot"`
	// Address is where the host's published ports are reached from hlm
	// and the LAN: for Web UI links, game queries, port forwards and
	// tunnels.
	Address string `json:"address"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Input is a host to save, with its TLS client key (PEM). An empty Key
// keeps the one saved before.
type Input struct {
	Host
	Key string `json:"key,omitempty"`
}

// Options configures a Registry.
type Options struct {
	Secrets *secrets.Store
	// SSHKey is hlm's SSH key pair.
	SSHKey func() (remote.KeyPair, bool, error)
	// Credentials finds registry logins for pulls, as for the local host.
	Credentials func(ctx context.Context, ref string) (docker.RegistryAuth, bool)
	// OnChange gets the hosts whenever they change, and at Load.
	OnChange func([]manager.RemoteHost)
	Log      *slog.Logger
}

// Registry keeps the hosts and their connections.
type Registry struct {
	opts  Options
	saveM sync.Mutex

	mu    sync.Mutex
	conns map[string]*conn
}

type conn struct {
	host   Host
	client *docker.Client
	ssh    *sshDialer
}

func (c *conn) close() {
	c.client.Close()
	if c.ssh != nil {
		c.ssh.close()
	}
}

// New creates a registry; Load connects the saved hosts.
func New(o Options) *Registry {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Registry{opts: o, conns: map[string]*conn{}}
}

func (r *Registry) load() ([]Host, error) {
	var list []Host
	if _, err := r.opts.Secrets.Get(secretHosts, &list); err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// List returns the hosts.
func (r *Registry) List() ([]Host, error) {
	list, err := r.load()
	if list == nil {
		list = []Host{}
	}
	return list, err
}

// Get returns one host.
func (r *Registry) Get(id string) (Host, error) {
	list, err := r.load()
	if err != nil {
		return Host{}, err
	}
	for _, h := range list {
		if h.ID == id {
			return h, nil
		}
	}
	return Host{}, ErrNotFound
}

// Load connects the saved hosts (lazily: nothing is dialled until a
// server is listed) and passes them to OnChange.
func (r *Registry) Load() error {
	list, err := r.load()
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, h := range list {
		c, err := r.connect(h)
		if err != nil {
			r.opts.Log.Warn("set up Docker host", "host", h.ID, "err", err)
			continue
		}
		r.conns[h.ID] = c
	}
	r.mu.Unlock()
	r.changed()
	return nil
}

func (r *Registry) changed() {
	if r.opts.OnChange == nil {
		return
	}
	r.mu.Lock()
	out := make([]manager.RemoteHost, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, manager.RemoteHost{
			ID: c.host.ID, Name: c.host.Name, Docker: c.client, AppdataRoot: c.host.AppdataRoot, Address: c.host.Address,
		})
	}
	r.mu.Unlock()
	r.opts.OnChange(out)
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// validate checks and tidies a host. key is the client key to use (the new
// one, or the saved one).
func validate(h *Host, key string) error {
	h.ID = strings.TrimSpace(h.ID)
	if !idPattern.MatchString(h.ID) {
		return errors.New("the host's ID is lower-case letters, digits and dashes (up to 32), starting with a letter or digit")
	}
	if h.ID == manager.LocalHost {
		return errors.New(`"local" is the host hlm runs with`)
	}
	h.Name = strings.TrimSpace(h.Name)
	if h.Name == "" {
		h.Name = h.ID
	}
	h.AppdataRoot = strings.TrimSpace(h.AppdataRoot)
	if !path.IsAbs(h.AppdataRoot) {
		return errors.New("the data folder on the host must be an absolute path, such as /mnt/user/appdata")
	}
	h.AppdataRoot = path.Clean(h.AppdataRoot)
	if h.AppdataRoot == "/" {
		return errors.New("the data folder can't be /")
	}
	h.Address = strings.TrimSpace(h.Address)
	switch h.Kind {
	case KindTCP:
		h.SSH, h.Socket = nil, ""
		u, err := url.Parse(strings.TrimSpace(h.URL))
		if err != nil || (u.Scheme != "tcp" && u.Scheme != "https") || u.Hostname() == "" {
			return errors.New("the address is like tcp://nas.lan:2376")
		}
		port := u.Port()
		if port == "" {
			port = "2376"
		}
		h.URL = "tcp://" + net.JoinHostPort(u.Hostname(), port)
		if h.Address == "" {
			h.Address = u.Hostname()
		}
		h.CA, h.Cert = strings.TrimSpace(h.CA), strings.TrimSpace(h.Cert)
		if _, err := tlsConfig(*h, key); err != nil {
			return err
		}
	case KindSSH:
		h.URL, h.CA, h.Cert, h.HasKey = "", "", "", false
		if h.SSH == nil || strings.TrimSpace(h.SSH.Host) == "" || strings.TrimSpace(h.SSH.User) == "" {
			return errors.New("an SSH host and user are needed")
		}
		h.SSH.Host, h.SSH.User = strings.TrimSpace(h.SSH.Host), strings.TrimSpace(h.SSH.User)
		if h.SSH.Port == 0 {
			h.SSH.Port = 22
		}
		if h.SSH.Port < 1 || h.SSH.Port > 65535 {
			return errors.New("the SSH port is 1–65535")
		}
		h.Socket = strings.TrimSpace(h.Socket)
		if h.Socket == "" {
			h.Socket = DefaultSocket
		}
		if !path.IsAbs(h.Socket) {
			return errors.New("Docker's socket is an absolute path, such as " + DefaultSocket)
		}
		if h.Address == "" {
			h.Address = h.SSH.Host
		}
	default:
		return errors.New(`connect over "tcp" (Docker's API with TLS) or "ssh"`)
	}
	return nil
}

// tlsConfig is the TLS setup for a TCP host. The daemon's certificate is
// always checked: plain or unchecked connections would let anyone on the
// way run containers there.
func tlsConfig(h Host, key string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if h.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(h.CA)) {
			return nil, errors.New("the CA certificate isn't a PEM certificate (ca.pem)")
		}
		tc.RootCAs = pool
	}
	switch {
	case h.Cert != "" && key != "":
		pair, err := tls.X509KeyPair([]byte(h.Cert), []byte(key))
		if err != nil {
			return nil, fmt.Errorf("the client certificate and key don't go together: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	case h.Cert != "" || key != "":
		return nil, errors.New("a client certificate (cert.pem) needs its key (key.pem), and the other way round")
	}
	return tc, nil
}

// connect sets up the client for a host; nothing is dialled yet.
func (r *Registry) connect(h Host) (*conn, error) {
	key := ""
	if h.Kind == KindTCP && h.HasKey {
		if _, err := r.opts.Secrets.Get(secretKeyPrefix+h.ID, &key); err != nil {
			return nil, err
		}
	}
	return r.connectWith(h, key)
}

func (r *Registry) connectWith(h Host, key string) (*conn, error) {
	c := &conn{host: h}
	cfg := docker.Config{Host: h.URL}
	switch h.Kind {
	case KindTCP:
		tc, err := tlsConfig(h, key)
		if err != nil {
			return nil, err
		}
		cfg.TLS = tc
	case KindSSH:
		c.ssh = &sshDialer{target: *h.SSH, socket: h.Socket, key: r.opts.SSHKey, log: r.opts.Log.With("host", h.ID)}
		cfg.Host = "ssh://" + net.JoinHostPort(h.SSH.Host, strconv.Itoa(h.SSH.Port)) + h.Socket
		cfg.Dial = c.ssh.dial
	}
	client, err := docker.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if r.opts.Credentials != nil {
		client.SetCredentials(r.opts.Credentials)
	}
	c.client = client
	return c, nil
}

// TestResult is what a host's Docker says about itself.
type TestResult struct {
	Version    string `json:"version"`
	APIVersion string `json:"apiVersion"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

// Test connects to a host as given, without saving it. For SSH hosts
// whose key isn't trusted yet it returns a *remote.HostKeyError with the
// key to confirm.
func (r *Registry) Test(ctx context.Context, in Input) (TestResult, error) {
	h, key, err := r.prepare(in)
	if err != nil {
		return TestResult{}, err
	}
	c, err := r.connectWith(h, key)
	if err != nil {
		return TestResult{}, err
	}
	defer c.close()
	return test(ctx, c)
}

func test(ctx context.Context, c *conn) (TestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	v, err := c.client.Version(ctx)
	if err != nil {
		var hk *remote.HostKeyError
		if errors.As(err, &hk) || errors.Is(err, remote.ErrHostKeyChanged) {
			return TestResult{}, unwrapHostKey(err)
		}
		return TestResult{}, fmt.Errorf("can't reach Docker on %s: %w", c.host.Name, err)
	}
	return TestResult{Version: v.Version, APIVersion: v.APIVersion, OS: v.OS, Arch: v.Arch}, nil
}

// unwrapHostKey returns the SSH host key error itself, which the http
// transport wraps.
func unwrapHostKey(err error) error {
	var hk *remote.HostKeyError
	if errors.As(err, &hk) {
		return hk
	}
	return remote.ErrHostKeyChanged
}

// prepare validates an input against the saved host (whose ID, creation
// time and client key carry over), and returns the host and the key.
func (r *Registry) prepare(in Input) (Host, string, error) {
	h := in.Host
	key := strings.TrimSpace(in.Key)
	if old, err := r.Get(h.ID); err == nil {
		h.Created = old.Created
		if key == "" && old.HasKey && h.Kind == KindTCP && h.Cert != "" {
			if _, err := r.opts.Secrets.Get(secretKeyPrefix+h.ID, &key); err != nil {
				return Host{}, "", err
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return Host{}, "", err
	}
	if key = strings.TrimSpace(key); key != "" && h.Kind == KindTCP {
		key += "\n"
	} else {
		key = ""
	}
	if err := validate(&h, key); err != nil {
		return Host{}, "", err
	}
	h.HasKey = h.Kind == KindTCP && key != ""
	return h, key, nil
}

// Save adds a host (create) or changes one, after checking that hlm can
// reach its Docker.
func (r *Registry) Save(ctx context.Context, in Input, create bool) (Host, error) {
	r.saveM.Lock()
	defer r.saveM.Unlock()
	_, err := r.Get(in.ID)
	switch {
	case create && err == nil:
		return Host{}, fmt.Errorf("there's already a host %q", in.ID)
	case !create && errors.Is(err, ErrNotFound):
		return Host{}, ErrNotFound
	case err != nil && !errors.Is(err, ErrNotFound):
		return Host{}, err
	}
	h, key, err := r.prepare(in)
	if err != nil {
		return Host{}, err
	}
	if h.Kind == KindSSH && h.SSH.HostKey == "" {
		return Host{}, errors.New("test the connection and trust the host's SSH key first")
	}
	c, err := r.connectWith(h, key)
	if err != nil {
		return Host{}, err
	}
	if _, err := test(ctx, c); err != nil {
		c.close()
		return Host{}, err
	}
	now := time.Now().UTC()
	if h.Created.IsZero() {
		h.Created = now
	}
	h.Updated = now
	list, err := r.load()
	if err != nil {
		c.close()
		return Host{}, err
	}
	list = slicesReplace(list, h)
	if h.HasKey {
		err = r.opts.Secrets.Set(secretKeyPrefix+h.ID, key)
	} else {
		err = r.opts.Secrets.Delete(secretKeyPrefix + h.ID)
	}
	if err == nil {
		err = r.opts.Secrets.Set(secretHosts, list)
	}
	if err != nil {
		c.close()
		return Host{}, err
	}
	r.mu.Lock()
	if old, ok := r.conns[h.ID]; ok {
		defer old.close()
	}
	r.conns[h.ID] = c
	r.mu.Unlock()
	r.opts.Log.Info("saved Docker host", "host", h.ID, "kind", h.Kind)
	r.changed()
	return h, nil
}

func slicesReplace(list []Host, h Host) []Host {
	for i := range list {
		if list[i].ID == h.ID {
			list[i] = h
			return list
		}
	}
	return append(list, h)
}

// Delete forgets a host. Its containers keep running there; hlm just
// stops managing them.
func (r *Registry) Delete(id string) error {
	r.saveM.Lock()
	defer r.saveM.Unlock()
	list, err := r.load()
	if err != nil {
		return err
	}
	kept := list[:0]
	found := false
	for _, h := range list {
		if h.ID == id {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		return ErrNotFound
	}
	if err := r.opts.Secrets.Set(secretHosts, kept); err != nil {
		return err
	}
	if err := r.opts.Secrets.Delete(secretKeyPrefix + id); err != nil {
		return err
	}
	r.mu.Lock()
	if c, ok := r.conns[id]; ok {
		c.close()
		delete(r.conns, id)
	}
	r.mu.Unlock()
	r.opts.Log.Info("removed Docker host", "host", id)
	r.changed()
	return nil
}

// Check tests a saved host's connection.
func (r *Registry) Check(ctx context.Context, id string) (TestResult, error) {
	r.mu.Lock()
	c, ok := r.conns[id]
	r.mu.Unlock()
	if !ok {
		return TestResult{}, ErrNotFound
	}
	return test(ctx, c)
}

// Close closes every connection.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.close()
	}
}

// --- SSH ---

// sshDialer opens connections to Docker's socket over one SSH connection,
// reconnecting when it drops.
type sshDialer struct {
	target remote.Target
	socket string
	key    func() (remote.KeyPair, bool, error)
	log    *slog.Logger

	mu     sync.Mutex
	conn   *remote.Conn
	closed bool
	// viaCLI is set once forwarding was refused: connections then go
	// through the docker command instead.
	viaCLI bool
}

func (d *sshDialer) dial(ctx context.Context) (net.Conn, error) {
	c, err := d.session(ctx)
	if err != nil {
		return nil, err
	}
	nc, err := d.open(ctx, c)
	if err == nil {
		return nc, nil
	}
	// The connection may have dropped without anyone noticing: try once
	// more on a new one.
	d.drop(c)
	if c, err = d.session(ctx); err != nil {
		return nil, err
	}
	return d.open(ctx, c)
}

// open connects to Docker's socket: forwarded straight to it, like ssh -L,
// or, where the SSH server doesn't allow forwarding (AllowTcpForwarding
// no), through the docker command on the host, as the docker CLI does.
func (d *sshDialer) open(ctx context.Context, c *remote.Conn) (net.Conn, error) {
	d.mu.Lock()
	viaCLI := d.viaCLI
	d.mu.Unlock()
	if !viaCLI {
		nc, err := c.Dial(ctx, "unix", d.socket)
		if err == nil {
			return nc, nil
		}
		if !strings.Contains(err.Error(), "rejected") {
			return nil, fmt.Errorf("open %s over SSH: %w", d.socket, err)
		}
		forwardErr := err
		// The docker command fails quietly as a connection (its output just
		// ends), so check it can reach Docker first.
		res, err := c.Run(ctx, d.docker("version --format '{{.Server.Version}}'"), nil)
		if err == nil && res.ExitCode != 0 {
			err = errors.New(res.Output())
		}
		if err == nil {
			nc, err = d.cli(ctx, c)
		}
		if err != nil && strings.Contains(err.Error(), "not found") {
			return nil, fmt.Errorf("%w: there's no docker command there, and %s can't be opened", ErrNoDocker, d.socket)
		}
		if err != nil {
			return nil, fmt.Errorf("the SSH server doesn't forward %s (%v), and the docker command there didn't work either (%w): allow forwarding (AllowTcpForwarding yes in sshd_config), or let the user run docker",
				d.socket, forwardErr, err)
		}
		d.mu.Lock()
		d.viaCLI = true
		d.mu.Unlock()
		d.log.Info("SSH forwarding is off on the host; reaching Docker with its docker command")
		return nc, nil
	}
	return d.cli(ctx, c)
}

// cli connects through "docker system dial-stdio".
func (d *sshDialer) cli(ctx context.Context, c *remote.Conn) (net.Conn, error) {
	return c.DialCommand(ctx, d.docker("system dial-stdio"))
}

// docker is a docker command line for the host's socket.
func (d *sshDialer) docker(args string) string {
	if d.socket != DefaultSocket {
		return "docker -H " + remote.Quote("unix://"+d.socket) + " " + args
	}
	return "docker " + args
}

func (d *sshDialer) session(ctx context.Context) (*remote.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errors.New("the host was removed")
	}
	if d.conn != nil {
		return d.conn, nil
	}
	if d.key == nil {
		return nil, errors.New("hlm has no SSH key")
	}
	kp, ok, err := d.key()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("hlm has no SSH key yet: make one on the Hosts page")
	}
	c, err := remote.Dial(ctx, d.target, kp)
	if err != nil {
		return nil, err
	}
	d.conn = c
	go d.watch(c)
	return c, nil
}

// watch keeps an idle connection alive, and forgets it once it ends.
func (d *sshDialer) watch(c *remote.Conn) {
	done := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(done)
	}()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			d.drop(c)
			return
		case <-t.C:
			if err := c.KeepAlive(); err != nil {
				d.log.Debug("SSH connection to Docker host dropped", "err", err)
				d.drop(c)
				return
			}
		}
	}
}

func (d *sshDialer) drop(c *remote.Conn) {
	d.mu.Lock()
	if d.conn == c {
		d.conn = nil
	}
	d.mu.Unlock()
	c.Close()
}

// reset drops the SSH connection, so the next request signs in again (to
// pick up a new group membership, say).
func (d *sshDialer) reset() {
	d.mu.Lock()
	c := d.conn
	d.conn, d.viaCLI = nil, false
	d.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (d *sshDialer) close() {
	d.mu.Lock()
	d.closed = true
	c := d.conn
	d.conn = nil
	d.mu.Unlock()
	if c != nil {
		c.Close()
	}
}
