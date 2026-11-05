// Package cloudflare manages Cloudflare Tunnels and DNS records through the
// Cloudflare API (v4), with an API token.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Cloudflare's API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// TunnelDomain is where a tunnel's DNS records point: <tunnel ID>.cfargotunnel.com.
const TunnelDomain = "cfargotunnel.com"

// TunnelTarget is the CNAME target that routes a hostname to a tunnel.
func TunnelTarget(tunnelID string) string { return tunnelID + "." + TunnelDomain }

// Account is a Cloudflare account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Zone is a domain on Cloudflare.
type Zone struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Status  string  `json:"status"` // active, pending, …
	Account Account `json:"account"`
}

// Tunnel is a Cloudflare Tunnel.
type Tunnel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"` // inactive, degraded, healthy, down
	CreatedAt time.Time `json:"created_at"`
	// DeletedAt is set for deleted tunnels, which keep their name for a while.
	DeletedAt   *time.Time   `json:"deleted_at,omitempty"`
	ConfigSrc   string       `json:"config_src"` // "cloudflare" when its ingress is set through the API
	Connections []Connection `json:"connections"`
}

// Connection is one of a tunnel's connections to Cloudflare's edge.
type Connection struct {
	ID       string    `json:"id"`
	ColoName string    `json:"colo_name"`
	OriginIP string    `json:"origin_ip"`
	OpenedAt time.Time `json:"opened_at"`
	Version  string    `json:"client_version"`
	Pending  bool      `json:"is_pending_reconnect"`
}

// IngressRule sends requests for Hostname to Service. The last rule has no
// hostname and catches everything else.
type IngressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Service       string         `json:"service"`
	OriginRequest *OriginRequest `json:"originRequest,omitempty"`
}

// OriginRequest configures how cloudflared talks to the origin.
type OriginRequest struct {
	// NoTLSVerify accepts the origin's certificate without checking it,
	// for https origins with self-signed certificates.
	NoTLSVerify bool `json:"noTLSVerify,omitempty"`
}

// TunnelConfig is a remotely managed tunnel's configuration.
type TunnelConfig struct {
	Ingress []IngressRule `json:"ingress"`
}

// DNSRecord is a DNS record in a zone.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"` // 1 is automatic
	Comment string `json:"comment,omitempty"`
}

// APIError is an error reported by the API.
type APIError struct {
	Status  int
	Code    int // Cloudflare's error code, if it gave one
	Message string
	// Forbidden means the token is valid but lacks a permission.
	Forbidden bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Cloudflare: %s (status %d)", e.Message, e.Status)
}

// IsForbidden reports whether err is the API saying the token lacks a
// permission.
func IsForbidden(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Forbidden
}

// needs names the permission a call needs when the token lacks it, so the
// error says which one to add rather than listing them all.
func needs(err error, permission string) error {
	var e *APIError
	if errors.As(err, &e) && e.Forbidden {
		e.Message = "the API token doesn't have " + permission + " (Cloudflare said: " + e.Message + ")"
	}
	return err
}

const (
	permTunnel = "Account → Cloudflare Tunnel: Edit for this account"
	permZone   = "Zone → Zone: Read"
	permDNS    = "Zone → DNS: Edit for this domain"
	permSSL    = "Zone → SSL and Certificates: Edit for this domain"
)

// IsNotFound reports whether err is the API saying there is no such object.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Client calls the Cloudflare API.
type Client struct {
	token string
	base  string
	http  *http.Client
}

// New creates a client. baseURL is DefaultBaseURL if empty.
func New(token, baseURL string) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("enter a Cloudflare API token")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{token: token, base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}, nil
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []apiMessage    `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body any) (envelope, error) {
	var env envelope
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return env, err
		}
		rd = bytes.NewReader(data)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return env, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return env, fmt.Errorf("Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return env, err
	}
	decodeErr := json.Unmarshal(data, &env)
	if resp.StatusCode >= 300 || (decodeErr == nil && !env.Success) {
		return env, apiError(resp.StatusCode, env.Errors)
	}
	if decodeErr != nil {
		return env, fmt.Errorf("Cloudflare: unexpected response: %w", decodeErr)
	}
	return env, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	env, err := c.call(ctx, method, path, query, body)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(env.Result, out)
}

func apiError(status int, errs []apiMessage) *APIError {
	e := &APIError{Status: status}
	var msgs []string
	for _, x := range errs {
		if x.Message != "" {
			msgs = append(msgs, x.Message)
		}
		if e.Code == 0 {
			e.Code = x.Code
		}
	}
	switch {
	case status == http.StatusUnauthorized, e.Code == 1000 || e.Code == 6003 || e.Code == 6111:
		e.Message = "the API token isn't valid (it may have expired or been rolled)"
	case status == http.StatusForbidden, e.Code == 10000 || e.Code == 9109:
		e.Forbidden = true
		e.Message = "the API token isn't allowed to do this"
		if len(msgs) > 0 {
			e.Message = strings.Join(msgs, "; ")
		}
	case len(msgs) > 0:
		e.Message = strings.Join(msgs, "; ")
	default:
		e.Message = http.StatusText(status)
	}
	if e.Status < 300 {
		e.Status = http.StatusBadRequest // a 200 with success: false
	}
	return e
}

// list fetches every page of a paginated collection.
func list[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	out := []T{}
	if query == nil {
		query = url.Values{}
	}
	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))
		if query.Get("per_page") == "" {
			query.Set("per_page", "50")
		}
		env, err := c.call(ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(env.Result, &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages || len(items) == 0 {
			return out, nil
		}
	}
}

// Accounts lists the accounts the token can use. Cloudflare only lists
// them for tokens with Account Settings: Read, so it can be empty even if
// the token has other permissions in an account.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	return list[Account](ctx, c, "/accounts", nil)
}

// Zones lists the account's zones (all the token can see if accountID is empty).
func (c *Client) Zones(ctx context.Context, accountID string) ([]Zone, error) {
	q := url.Values{}
	if accountID != "" {
		q.Set("account.id", accountID)
	}
	zones, err := list[Zone](ctx, c, "/zones", q)
	return zones, needs(err, permZone)
}

func tunnelPath(accountID string, rest ...string) string {
	p := "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel"
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

// Tunnels lists the account's tunnels that aren't deleted, optionally only
// those called name.
func (c *Client) Tunnels(ctx context.Context, accountID, name string) ([]Tunnel, error) {
	q := url.Values{"is_deleted": {"false"}}
	if name != "" {
		q.Set("name", name)
	}
	tunnels, err := list[Tunnel](ctx, c, tunnelPath(accountID), q)
	return tunnels, needs(err, permTunnel)
}

// CreateTunnel creates a remotely managed tunnel: its ingress rules are set
// with SetTunnelConfig, and cloudflared only needs its token.
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (Tunnel, error) {
	var t Tunnel
	err := c.do(ctx, http.MethodPost, tunnelPath(accountID), nil, map[string]string{"name": name, "config_src": "cloudflare"}, &t)
	return t, needs(err, permTunnel)
}

// Tunnel returns one tunnel.
func (c *Client) Tunnel(ctx context.Context, accountID, id string) (Tunnel, error) {
	var t Tunnel
	err := c.do(ctx, http.MethodGet, tunnelPath(accountID, id), nil, nil, &t)
	return t, needs(err, permTunnel)
}

// TunnelToken returns the token cloudflared runs the tunnel with.
func (c *Client) TunnelToken(ctx context.Context, accountID, id string) (string, error) {
	var token string
	err := c.do(ctx, http.MethodGet, tunnelPath(accountID, id, "token"), nil, nil, &token)
	return token, needs(err, permTunnel)
}

// TunnelConfig returns a remotely managed tunnel's configuration.
func (c *Client) TunnelConfig(ctx context.Context, accountID, id string) (TunnelConfig, error) {
	var out struct {
		Config *TunnelConfig `json:"config"`
	}
	err := needs(c.do(ctx, http.MethodGet, tunnelPath(accountID, id, "configurations"), nil, nil, &out), permTunnel)
	if out.Config == nil {
		return TunnelConfig{}, err
	}
	return *out.Config, err
}

// SetTunnelConfig replaces a remotely managed tunnel's configuration;
// cloudflared picks it up within seconds.
func (c *Client) SetTunnelConfig(ctx context.Context, accountID, id string, cfg TunnelConfig) error {
	return needs(c.do(ctx, http.MethodPut, tunnelPath(accountID, id, "configurations"), nil, map[string]any{"config": cfg}, nil), permTunnel)
}

// CleanupConnections drops a tunnel's stale connections, which Cloudflare
// requires before it's deleted.
func (c *Client) CleanupConnections(ctx context.Context, accountID, id string) error {
	return needs(c.do(ctx, http.MethodDelete, tunnelPath(accountID, id, "connections"), nil, nil, nil), permTunnel)
}

// DeleteTunnel deletes a tunnel.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, id string) error {
	return needs(c.do(ctx, http.MethodDelete, tunnelPath(accountID, id), nil, nil, nil), permTunnel)
}

func recordsPath(zoneID string, rest ...string) string {
	p := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p
}

// DNSRecords lists a zone's records called name (all of them if empty).
func (c *Client) DNSRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	q := url.Values{"per_page": {"100"}}
	if name != "" {
		q.Set("name", name)
	}
	records, err := list[DNSRecord](ctx, c, recordsPath(zoneID), q)
	return records, needs(err, permDNS)
}

// CreateDNSRecord adds a record.
func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, r DNSRecord) (DNSRecord, error) {
	var out DNSRecord
	err := c.do(ctx, http.MethodPost, recordsPath(zoneID), nil, r, &out)
	return out, needs(err, permDNS)
}

// UpdateDNSRecord replaces a record.
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, id string, r DNSRecord) (DNSRecord, error) {
	var out DNSRecord
	r.ID = ""
	err := c.do(ctx, http.MethodPut, recordsPath(zoneID, id), nil, r, &out)
	return out, needs(err, permDNS)
}

// DeleteDNSRecord removes a record.
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, id string) error {
	return needs(c.do(ctx, http.MethodDelete, recordsPath(zoneID, id), nil, nil, nil), permDNS)
}

// OriginCertificate is a Cloudflare Origin CA certificate: trusted by
// Cloudflare (not browsers), for its connections to an origin server.
type OriginCertificate struct {
	ID          string   `json:"id"`
	Certificate string   `json:"certificate"`
	Hostnames   []string `json:"hostnames"`
	ExpiresOn   string   `json:"expires_on"`
}

// CreateOriginCertificate has Cloudflare's Origin CA sign a certificate
// request (PEM) for hostnames, valid for days (up to 5475).
func (c *Client) CreateOriginCertificate(ctx context.Context, csr string, hostnames []string, days int) (OriginCertificate, error) {
	var out OriginCertificate
	body := map[string]any{"csr": csr, "hostnames": hostnames, "request_type": "origin-ecc", "requested_validity": days}
	err := c.do(ctx, http.MethodPost, "/certificates", nil, body, &out)
	return out, needs(err, permSSL)
}

// SSLMode is a zone's SSL/TLS encryption mode: off, flexible, full or
// strict.
func (c *Client) SSLMode(ctx context.Context, zoneID string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/settings/ssl", nil, nil, &out)
	return out.Value, err
}

// IPRanges are the addresses Cloudflare connects to origin servers from.
type IPRanges struct {
	IPv4 []string `json:"ipv4_cidrs"`
	IPv6 []string `json:"ipv6_cidrs"`
}

// IPs returns Cloudflare's address ranges.
func (c *Client) IPs(ctx context.Context) (IPRanges, error) {
	var out IPRanges
	err := c.do(ctx, http.MethodGet, "/ips", nil, nil, &out)
	return out, err
}
