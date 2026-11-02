// Package unifitest provides a fake UniFi Network controller for tests.
package unifitest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/unifi"
)

// Fake imitates a UniFi Network controller's sign-in and
// port-forward API, closely enough to exercise the client: UniFi OS (TOKEN
// cookie + X-CSRF-Token, or X-API-KEY, paths under /proxy/network) or a
// self-hosted controller (unifises + csrf_token cookies).
type Fake struct {
	unifiOS bool
	apiKey  string

	mu      sync.Mutex
	rules   map[string]unifi.PortForward
	nextID  int
	logins  int
	expired bool // next authenticated call answers 401 once
	// loginLimit, if set, answers 429 to sign-ins beyond this many, like
	// UniFi OS's rate limit on /api/auth/login.
	loginLimit int
	throttle   int // answer this many API calls with 429 first

	active   []unifi.Device
	known    []unifi.Device
	networks []unifi.Network
	wan      map[string]string // subsystem -> wan_ip
}

// SetWAN sets the gateway's public address for a WAN ("wan" or "wan2").
func (f *Fake) SetWAN(wan, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wan == nil {
		f.wan = map[string]string{}
	}
	f.wan[wan] = ip
}

// SetDevices sets the connected devices (stat/sta) and remembered devices
// with their fixed IPs (rest/user).
func (f *Fake) SetDevices(active, known []unifi.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active, f.known = active, known
}

// SetNetworks sets the site's networks (rest/networkconf).
func (f *Fake) SetNetworks(n []unifi.Network) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks = n
}

// Throttle makes the next n API calls fail with 429 and "Retry-After: 1".
func (f *Fake) Throttle(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.throttle = n
}

// LimitLogins makes sign-ins after the first n fail with 429 Too Many
// Requests, as UniFi OS does when a client signs in too often.
func (f *Fake) LimitLogins(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginLimit = n
}

// Expire makes the next authenticated call fail as if the session expired.
func (f *Fake) Expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = true
}

// Logins returns how many times a client signed in.
func (f *Fake) Logins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// Rules returns the stored rules.
func (f *Fake) Rules() []unifi.PortForward {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []unifi.PortForward{}
	for _, pf := range f.rules {
		out = append(out, pf)
	}
	return out
}

// Add stores a rule directly, as if made in the UniFi UI.
func (f *Fake) Add(pf unifi.PortForward) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	pf.ID = fmt.Sprintf("pf%d", f.nextID)
	f.rules[pf.ID] = pf
}

const fakeCSRF = "csrf-123"

// New starts a fake controller with a self-signed certificate, closed when
// the test ends. API key auth accepts "key-abc"; password auth accepts
// admin/pw.
func New(t *testing.T, unifiOS bool) (*Fake, *httptest.Server) {
	f, srv := NewServer(unifiOS)
	t.Cleanup(srv.Close)
	return f, srv
}

// NewServer is New for use outside tests; the caller closes the server.
func NewServer(unifiOS bool) (*Fake, *httptest.Server) {
	f := &Fake{unifiOS: unifiOS, apiKey: "key-abc", rules: map[string]unifi.PortForward{}}
	return f, httptest.NewTLSServer(http.HandlerFunc(f.serve))
}

func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	rc := "ok"
	if status >= 300 {
		rc = "error"
	}
	json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": rc, "msg": fmt.Sprint(data)}, "data": data})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/":
		if f.unifiOS {
			w.Write([]byte("<html>UniFi OS</html>"))
		} else {
			http.Redirect(w, r, "/manage", http.StatusFound)
		}
		return
	case r.URL.Path == "/api/auth/login" && f.unifiOS, r.URL.Path == "/api/login" && !f.unifiOS:
		var body struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&body)
		if f.loginLimit > 0 && f.logins >= f.loginLimit {
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":"AUTHENTICATION_FAILED_LIMIT_REACHED","message":"Too many requests"}`))
			return
		}
		if body.Username != "admin" || body.Password != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.logins++
		if f.unifiOS {
			http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "session", Path: "/"})
			w.Header().Set("X-CSRF-Token", fakeCSRF)
		} else {
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "session", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: fakeCSRF, Path: "/"})
		}
		reply(w, 200, []any{})
		return
	}

	site := "/api/s/default/"
	if f.unifiOS {
		site = "/proxy/network" + site
	}
	prefix := site + "rest/portforward"
	if !strings.HasPrefix(r.URL.Path, site) {
		reply(w, http.StatusNotFound, "api.err.NotFound")
		return
	}

	// Authentication.
	authed := false
	if f.unifiOS && r.Header.Get("X-API-KEY") != "" {
		authed = r.Header.Get("X-API-KEY") == f.apiKey
	} else {
		name := "unifises"
		if f.unifiOS {
			name = "TOKEN"
		}
		ck, err := r.Cookie(name)
		authed = err == nil && ck.Value == "session"
		if authed && r.Method != http.MethodGet && r.Header.Get("X-Csrf-Token") != fakeCSRF {
			reply(w, http.StatusForbidden, "api.err.InvalidCSRF")
			return
		}
	}
	if f.expired && authed && r.Header.Get("X-API-KEY") == "" {
		f.expired = false
		authed = false
	}
	if !authed {
		reply(w, http.StatusUnauthorized, "api.err.LoginRequired")
		return
	}
	if f.throttle > 0 {
		f.throttle--
		w.Header().Set("Retry-After", "1")
		reply(w, http.StatusTooManyRequests, "api.err.TooManyRequests")
		return
	}

	switch strings.TrimPrefix(r.URL.Path, site) {
	case "stat/sta":
		reply(w, 200, nonNil(f.active))
		return
	case "rest/user":
		reply(w, 200, nonNil(f.known))
		return
	case "rest/networkconf":
		reply(w, 200, nonNil(f.networks))
		return
	case "stat/health":
		health := []map[string]string{{"subsystem": "lan"}}
		for sub, ip := range f.wan {
			health = append(health, map[string]string{"subsystem": sub, "wan_ip": ip, "status": "ok"})
		}
		reply(w, 200, health)
		return
	case "stat/device":
		reply(w, 200, []any{})
		return
	}
	if !strings.HasPrefix(r.URL.Path, prefix) {
		reply(w, http.StatusNotFound, "api.err.NotFound")
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	switch r.Method {
	case http.MethodGet:
		list := []unifi.PortForward{}
		for _, pf := range f.rules {
			list = append(list, pf)
		}
		reply(w, 200, list)
	case http.MethodPost:
		var pf unifi.PortForward
		json.NewDecoder(r.Body).Decode(&pf)
		for _, other := range f.rules {
			if other.DstPort == pf.DstPort && overlaps(other.Proto, pf.Proto) {
				reply(w, 400, "api.err.unifi.PortForwardConflict")
				return
			}
		}
		f.nextID++
		pf.ID = fmt.Sprintf("pf%d", f.nextID)
		f.rules[pf.ID] = pf
		reply(w, 200, []unifi.PortForward{pf})
	case http.MethodPut:
		if _, ok := f.rules[id]; !ok {
			reply(w, 400, "api.err.IdInvalid")
			return
		}
		var pf unifi.PortForward
		json.NewDecoder(r.Body).Decode(&pf)
		pf.ID = id
		f.rules[id] = pf
		reply(w, 200, []unifi.PortForward{pf})
	case http.MethodDelete:
		delete(f.rules, id)
		reply(w, 200, []any{})
	}
}

func overlaps(a, b string) bool {
	return a == b || a == "tcp_udp" || b == "tcp_udp"
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
