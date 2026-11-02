// Package unifi manages port forwarding rules on a UniFi gateway through the
// UniFi Network application's API (the one its web UI uses).
//
// It works with UniFi OS consoles (UDM, UDR, UCG, Cloud Key Gen2+), signing
// in with a local account or an API key, and with self-hosted Network
// controllers, signing in with a local account.
package unifi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings say how to reach and sign in to the controller.
type Settings struct {
	URL      string `json:"url"`            // e.g. https://192.168.1.1
	Site     string `json:"site,omitempty"` // site name; "default" if empty
	AuthType string `json:"authType"`       // "password" or "apikey"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	APIKey   string `json:"apiKey,omitempty"`
	// Fingerprint pins the controller's certificate (SHA-256 of the leaf, in
	// hex). Consoles use self-signed certificates, so instead of turning
	// verification off, the user confirms this fingerprint once.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// PortForward is a port forwarding rule.
type PortForward struct {
	ID        string `json:"_id,omitempty"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Src       string `json:"src"`      // allowed source: "any", or an address/CIDR
	DstPort   string `json:"dst_port"` // WAN port or range, e.g. "2456-2458"
	Fwd       string `json:"fwd"`      // LAN address to forward to
	FwdPort   string `json:"fwd_port"` // LAN port or range
	Proto     string `json:"proto"`    // "tcp", "udp" or "tcp_udp"
	Log       bool   `json:"log"`
	Interface string `json:"pfwd_interface,omitempty"` // "wan", "wan2" or "both"
}

// CertError is returned when the controller's certificate isn't trusted. Its
// fingerprint can be pinned with Settings.Fingerprint after the user checks it.
type CertError struct {
	Fingerprint string
	Err         error
}

func (e *CertError) Error() string {
	return fmt.Sprintf("the controller's certificate isn't trusted (%v); its SHA-256 fingerprint is %s", e.Err, e.Fingerprint)
}

// APIError is an error reported by the controller.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("UniFi: %s (status %d)", e.Message, e.Status)
}

// Client talks to one controller.
type Client struct {
	s    Settings
	http *http.Client
	base string

	mu        sync.Mutex
	unifiOS   bool   // UniFi OS console (paths under /proxy/network)
	detected  bool   // unifiOS has been worked out
	loggedIn  bool   // a session cookie is set (password auth)
	csrfToken string // UniFi OS CSRF token
}

// New creates a client. It doesn't contact the controller.
func New(s Settings) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(s.URL), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid controller URL %q: use something like https://192.168.1.1", s.URL)
	}
	if u.Path != "" {
		return nil, fmt.Errorf("invalid controller URL %q: leave out the path", s.URL)
	}
	if s.Site == "" {
		s.Site = "default"
	}
	switch s.AuthType {
	case "password":
		if s.Username == "" || s.Password == "" {
			return nil, errors.New("a username and password are required")
		}
	case "apikey":
		if s.APIKey == "" {
			return nil, errors.New("an API key is required")
		}
	default:
		return nil, fmt.Errorf("unknown sign-in method %q", s.AuthType)
	}

	jar, _ := cookiejar.New(nil)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if pin := strings.ToLower(strings.ReplaceAll(s.Fingerprint, ":", "")); pin != "" {
		// Trust exactly the pinned certificate; the usual chain and hostname
		// checks are replaced by the fingerprint comparison.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || Fingerprint(raw[0]) != pin {
				return errors.New("the controller's certificate doesn't match the pinned fingerprint")
			}
			return nil
		}
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}
	return &Client{
		s:    s,
		base: u.String(),
		http: &http.Client{Transport: transport, Jar: jar, Timeout: 20 * time.Second},
	}, nil
}

// Fingerprint returns the SHA-256 fingerprint of a DER certificate in hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// certError turns an unknown-authority/hostname error into a CertError with
// the presented certificate's fingerprint, by fetching it again.
func (c *Client) certError(ctx context.Context, cause error) error {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if !errors.As(cause, &unknown) && !errors.As(cause, &hostname) && !errors.As(cause, &invalid) {
		return cause
	}
	u, _ := url.Parse(c.base)
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return cause
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return cause
	}
	return &CertError{Fingerprint: Fingerprint(certs[0].Raw), Err: cause}
}

// detect works out whether the controller is a UniFi OS console. API keys
// only exist on UniFi OS. Otherwise a UniFi OS console answers its root with
// 200, while a self-hosted controller redirects to /manage.
func (c *Client) detect(ctx context.Context) error {
	if c.detected {
		return nil
	}
	if c.s.AuthType == "apikey" {
		c.unifiOS, c.detected = true, true
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return c.certError(ctx, err)
	}
	resp.Body.Close()
	c.unifiOS, c.detected = resp.StatusCode == http.StatusOK, true
	return nil
}

func (c *Client) login(ctx context.Context) error {
	path := "/api/login"
	if c.unifiOS {
		path = "/api/auth/login"
	}
	body, _ := json.Marshal(map[string]any{"username": c.s.Username, "password": c.s.Password, "remember": true})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.certError(ctx, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusTooManyRequests {
		// Retrying would only extend the lockout.
		return &APIError{Status: resp.StatusCode, Message: "UniFi is refusing sign-ins for now because there were too many recently; wait a minute or two and try again"}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
		return &APIError{Status: resp.StatusCode, Message: "sign-in failed: check the username and password (use a local account; accounts with two-factor sign-in can't be used)"}
	}
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: "sign-in failed: " + apiMessage(data, resp.Status)}
	}
	c.csrfToken = resp.Header.Get("X-CSRF-Token")
	c.loggedIn = true
	return nil
}

// sitePath is the API path for a resource of the configured site.
func (c *Client) sitePath(rest string) string {
	p := "/api/s/" + url.PathEscape(c.s.Site) + "/" + rest
	if c.unifiOS {
		p = "/proxy/network" + p
	}
	return p
}

// call sends a request to the site API and decodes the "data" array into out.
func (c *Client) call(ctx context.Context, method, rest string, body, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.detect(ctx); err != nil {
		return err
	}
	relogged, throttled := false, 0
	for {
		if c.s.AuthType == "password" && !c.loggedIn {
			if err := c.login(ctx); err != nil {
				return err
			}
		}
		status, data, retryAfter, err := c.send(ctx, method, c.sitePath(rest), body)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && c.s.AuthType == "password" && !relogged {
			c.loggedIn, relogged = false, true // session expired; sign in again once
			continue
		}
		if status == http.StatusTooManyRequests && throttled < 2 {
			// Rate limited: wait as asked (within reason) and try again.
			throttled++
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter):
			}
			continue
		}
		if status == http.StatusTooManyRequests {
			return &APIError{Status: status, Message: "UniFi is rate-limiting requests; wait a minute and try again"}
		}
		var env struct {
			Meta struct {
				RC  string `json:"rc"`
				Msg string `json:"msg"`
			} `json:"meta"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(data, &env)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			msg := "not authorized"
			if c.s.AuthType == "apikey" {
				msg += ": check the API key, and that this Network version accepts API keys for port forwarding (otherwise sign in with a local account)"
			}
			return &APIError{Status: status, Message: msg}
		}
		if status == http.StatusNotFound {
			return &APIError{Status: status, Message: fmt.Sprintf("not found: check the site name (%q)", c.s.Site)}
		}
		if status >= 300 || env.Meta.RC == "error" {
			return &APIError{Status: status, Message: apiMessage(data, http.StatusText(status))}
		}
		if out != nil && len(env.Data) > 0 {
			return json.Unmarshal(env.Data, out)
		}
		return nil
	}
}

// send makes one request and returns the status, body and, for 429
// responses, how long the controller asked us to wait.
func (c *Client) send(ctx context.Context, method, path string, body any) (int, []byte, time.Duration, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, 0, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.s.AuthType == "apikey" {
		req.Header.Set("X-API-KEY", c.s.APIKey)
	} else if c.csrfToken != "" {
		req.Header.Set("X-CSRF-Token", c.csrfToken)
	} else if tok := c.cookie("csrf_token"); tok != "" {
		req.Header.Set("X-Csrf-Token", tok) // self-hosted controllers
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, 0, c.certError(ctx, err)
	}
	defer resp.Body.Close()
	if t := resp.Header.Get("X-Updated-CSRF-Token"); t != "" {
		c.csrfToken = t
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, retryDelay(resp.Header.Get("Retry-After")), err
}

// retryDelay reads a Retry-After header in seconds, defaulting to 2s and
// capped at 10s so a request never hangs for long.
func retryDelay(h string) time.Duration {
	d := 2 * time.Second
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	}
	return min(d, 10*time.Second)
}

func (c *Client) cookie(name string) string {
	u, _ := url.Parse(c.base)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func apiMessage(data []byte, fallback string) string {
	var env struct {
		Meta struct {
			Msg string `json:"msg"`
		} `json:"meta"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		for _, m := range []string{env.Meta.Msg, env.Message, env.Error} {
			if m != "" {
				return m
			}
		}
	}
	if s := strings.TrimSpace(string(data)); s != "" && len(s) < 200 && !strings.HasPrefix(s, "<") {
		return s
	}
	return fallback
}

// Controller describes what was found when testing the connection.
type Controller struct {
	UniFiOS      bool `json:"unifiOS"`
	PortForwards int  `json:"portForwards"`
}

// Test signs in and reads the port forwarding rules.
func (c *Client) Test(ctx context.Context) (Controller, error) {
	list, err := c.PortForwards(ctx)
	if err != nil {
		return Controller{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Controller{UniFiOS: c.unifiOS, PortForwards: len(list)}, nil
}

// PortForwards lists the site's port forwarding rules.
func (c *Client) PortForwards(ctx context.Context) ([]PortForward, error) {
	var out []PortForward
	err := c.call(ctx, http.MethodGet, "rest/portforward", nil, &out)
	if out == nil {
		out = []PortForward{}
	}
	return out, err
}

// CreatePortForward adds a rule and returns it as stored.
func (c *Client) CreatePortForward(ctx context.Context, pf PortForward) (PortForward, error) {
	pf.ID = ""
	var out []PortForward
	if err := c.call(ctx, http.MethodPost, "rest/portforward", pf, &out); err != nil {
		return PortForward{}, err
	}
	if len(out) > 0 {
		return out[0], nil
	}
	return pf, nil
}

// UpdatePortForward replaces the rule with pf.ID.
func (c *Client) UpdatePortForward(ctx context.Context, pf PortForward) error {
	if pf.ID == "" {
		return errors.New("rule has no ID")
	}
	return c.call(ctx, http.MethodPut, "rest/portforward/"+url.PathEscape(pf.ID), pf, nil)
}

// DeletePortForward removes a rule.
func (c *Client) DeletePortForward(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "rest/portforward/"+url.PathEscape(id), nil, nil)
}

// Device is a device UniFi knows about: currently connected (from
// stat/sta) or remembered with a fixed IP reservation (from rest/user).
type Device struct {
	MAC        string `json:"mac"`
	IP         string `json:"ip,omitempty"`
	Name       string `json:"name,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	UseFixedIP bool   `json:"use_fixedip,omitempty"`
	FixedIP    string `json:"fixed_ip,omitempty"`
}

// Label is how the device is best described to a person.
func (d Device) Label() string {
	switch {
	case d.Name != "":
		return d.Name
	case d.Hostname != "":
		return d.Hostname
	default:
		return "an unnamed device"
	}
}

// Network is a LAN network configured on the gateway.
type Network struct {
	Name        string `json:"name"`
	Purpose     string `json:"purpose"`   // corporate (LAN), guest, wan, vpn-…
	IPSubnet    string `json:"ip_subnet"` // gateway address with prefix, e.g. 192.168.1.1/24
	DHCPEnabled bool   `json:"dhcpd_enabled"`
	DHCPStart   string `json:"dhcpd_start"`
	DHCPStop    string `json:"dhcpd_stop"`
	VLANEnabled bool   `json:"vlan_enabled"`
	VLAN        any    `json:"vlan,omitempty"`
}

// ActiveClients lists the devices connected right now.
func (c *Client) ActiveClients(ctx context.Context) ([]Device, error) {
	var out []Device
	err := c.call(ctx, http.MethodGet, "stat/sta", nil, &out)
	return out, err
}

// KnownClients lists every device the controller remembers, including fixed
// IP reservations.
func (c *Client) KnownClients(ctx context.Context) ([]Device, error) {
	var out []Device
	err := c.call(ctx, http.MethodGet, "rest/user", nil, &out)
	return out, err
}

// Networks lists the site's networks.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.call(ctx, http.MethodGet, "rest/networkconf", nil, &out)
	return out, err
}

// WANAddresses returns the gateway's public addresses by WAN: "wan" for the
// primary internet connection and "wan2" for a second one. They come from
// the site's health status, or, if that has none, from the gateway device.
func (c *Client) WANAddresses(ctx context.Context) (map[string]string, error) {
	var health []struct {
		Subsystem string `json:"subsystem"`
		WANIP     string `json:"wan_ip"`
	}
	if err := c.call(ctx, http.MethodGet, "stat/health", nil, &health); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, h := range health {
		if strings.HasPrefix(h.Subsystem, "wan") && h.WANIP != "" {
			out[h.Subsystem] = h.WANIP
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	type iface struct {
		IP string `json:"ip"`
	}
	var devices []struct {
		Type string `json:"type"`
		WAN1 *iface `json:"wan1"`
		WAN2 *iface `json:"wan2"`
	}
	if err := c.call(ctx, http.MethodGet, "stat/device", nil, &devices); err != nil {
		return nil, err
	}
	for _, d := range devices {
		if d.WAN1 != nil && d.WAN1.IP != "" && out["wan"] == "" {
			out["wan"] = d.WAN1.IP
		}
		if d.WAN2 != nil && d.WAN2.IP != "" && out["wan2"] == "" {
			out["wan2"] = d.WAN2.IP
		}
	}
	return out, nil
}
