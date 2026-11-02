// Package linode manages Linode Cloud Firewalls through the Linode API (v4),
// with a personal access token.
package linode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Linode's API.
const DefaultBaseURL = "https://api.linode.com/v4"

// Limits Linode puts on firewall rules.
const (
	MaxRules      = 25 // inbound and outbound together
	MaxPortPieces = 15 // per rule; a range counts as two
)

// Rule is a firewall rule.
type Rule struct {
	Action      string     `json:"action"`   // ACCEPT or DROP
	Protocol    string     `json:"protocol"` // TCP, UDP, ICMP or IPENCAP
	Ports       string     `json:"ports,omitempty"`
	Addresses   *Addresses `json:"addresses"`
	Label       string     `json:"label,omitempty"`
	Description string     `json:"description,omitempty"`
	// Rules can carry fields this package doesn't know; they're kept so
	// writing the rules back doesn't drop them.
	extra map[string]json.RawMessage
}

// Addresses are the sources (inbound) or destinations (outbound) a rule
// matches.
type Addresses struct {
	IPv4 []string `json:"ipv4,omitempty"`
	IPv6 []string `json:"ipv6,omitempty"`
}

var ruleFields = []string{"action", "protocol", "ports", "addresses", "label", "description"}

func (r *Rule) UnmarshalJSON(data []byte) error {
	type plain Rule
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for _, f := range ruleFields {
		delete(all, f)
	}
	if len(all) > 0 {
		r.extra = all
	}
	return nil
}

func (r Rule) MarshalJSON() ([]byte, error) {
	type plain Rule
	data, err := json.Marshal(plain(r))
	if err != nil || len(r.extra) == 0 {
		return data, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for k, v := range r.extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

// Rules are a firewall's rulesets.
type Rules struct {
	Inbound        []Rule `json:"inbound"`
	InboundPolicy  string `json:"inbound_policy"`
	Outbound       []Rule `json:"outbound"`
	OutboundPolicy string `json:"outbound_policy"`
}

// Firewall is a Cloud Firewall.
type Firewall struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // enabled, disabled, deleted
}

// Instance is a Linode.
type Instance struct {
	ID    int      `json:"id"`
	Label string   `json:"label"`
	IPv4  []string `json:"ipv4"`
	IPv6  string   `json:"ipv6"` // "2600:3c00::f03c:91ff:fe00:1/128"
}

// APIError is an error reported by the API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Linode: %s (status %d)", e.Message, e.Status)
}

// Client calls the Linode API.
type Client struct {
	token string
	base  string
	http  *http.Client
}

// New creates a client. baseURL is DefaultBaseURL if empty.
func New(token, baseURL string) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("enter a Linode API token")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{token: token, base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Linode: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(resp.StatusCode, data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func errorMessage(status int, data []byte) string {
	var e struct {
		Errors []struct {
			Reason string `json:"reason"`
			Field  string `json:"field"`
		} `json:"errors"`
	}
	var msgs []string
	if json.Unmarshal(data, &e) == nil {
		for _, x := range e.Errors {
			if x.Field != "" {
				msgs = append(msgs, x.Field+": "+x.Reason)
			} else if x.Reason != "" {
				msgs = append(msgs, x.Reason)
			}
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return "the API token isn't valid (it may have expired)"
	case status == http.StatusForbidden:
		return "the API token isn't allowed to do this; give it Firewalls: Read/Write and Linodes: Read Only access"
	case len(msgs) > 0:
		return strings.Join(msgs, "; ")
	}
	return http.StatusText(status)
}

// list fetches every page of a paginated collection.
func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T
	for page := 1; ; page++ {
		var resp struct {
			Data  []T `json:"data"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		if err := c.do(ctx, http.MethodGet, path+sep+"page_size=500&page="+strconv.Itoa(page), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if page >= resp.Pages {
			return out, nil
		}
	}
}

// Firewalls lists the account's firewalls.
func (c *Client) Firewalls(ctx context.Context) ([]Firewall, error) {
	return list[Firewall](ctx, c, "/networking/firewalls")
}

// Firewall returns one firewall.
func (c *Client) Firewall(ctx context.Context, id int) (Firewall, error) {
	var f Firewall
	err := c.do(ctx, http.MethodGet, "/networking/firewalls/"+strconv.Itoa(id), nil, &f)
	return f, err
}

// Instances lists the account's Linodes.
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	return list[Instance](ctx, c, "/linode/instances")
}

// InstanceFirewalls lists the firewalls a Linode is assigned to.
func (c *Client) InstanceFirewalls(ctx context.Context, linodeID int) ([]Firewall, error) {
	return list[Firewall](ctx, c, "/linode/instances/"+strconv.Itoa(linodeID)+"/firewalls")
}

// FirewallRules returns a firewall's rules.
func (c *Client) FirewallRules(ctx context.Context, id int) (Rules, error) {
	var r Rules
	err := c.do(ctx, http.MethodGet, "/networking/firewalls/"+strconv.Itoa(id)+"/rules", nil, &r)
	return r, err
}

// SetFirewallRules replaces all of a firewall's rules.
func (c *Client) SetFirewallRules(ctx context.Context, id int, r Rules) (Rules, error) {
	if r.Inbound == nil {
		r.Inbound = []Rule{}
	}
	if r.Outbound == nil {
		r.Outbound = []Rule{}
	}
	var out Rules
	err := c.do(ctx, http.MethodPut, "/networking/firewalls/"+strconv.Itoa(id)+"/rules", r, &out)
	return out, err
}
