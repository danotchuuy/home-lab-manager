package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// RegistryAuth is a login for an image registry, as the Docker Engine API
// takes it.
type RegistryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

// Encode is the X-Registry-Auth header value for a: base64url-encoded JSON.
func (a RegistryAuth) Encode() string {
	data, _ := json.Marshal(a)
	return base64.URLEncoding.EncodeToString(data)
}

// SetCredentials sets how the client finds the registry login for an image.
// Pulls and registry lookups (RemoteDigest) send what it returns to the
// daemon, which passes it only to that image's registry.
func (c *Client) SetCredentials(find func(ctx context.Context, ref string) (RegistryAuth, bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.credentials = find
}

// authHeader is the X-Registry-Auth header for pulling ref, or nil.
func (c *Client) authHeader(ctx context.Context, ref string) http.Header {
	c.mu.Lock()
	find := c.credentials
	c.mu.Unlock()
	if find == nil {
		return nil
	}
	a, ok := find(ctx, ref)
	if !ok {
		return nil
	}
	return http.Header{"X-Registry-Auth": {a.Encode()}}
}

// Login checks a registry login with the daemon (POST /auth), which talks to
// the registry. It returns the registry's status message.
func (c *Client) Login(ctx context.Context, a RegistryAuth) (string, error) {
	var out struct {
		Status string `json:"Status"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/auth", nil, a, &out)
	return out.Status, err
}

// IsAuthError reports whether err looks like a registry refusing a pull for
// want of a (valid) login: Docker reports these in different words for
// different registries.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"unauthorized", "authentication required", "access denied", "access to the resource is denied",
		"denied:", "requested access to the resource is denied", "no basic auth credentials", "status 401", "status 403",
		"incorrect username or password", "pull access denied",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
