// Package cloudflaretest provides a fake Cloudflare API for tests.
package cloudflaretest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
)

// Token is the API token the fake accepts.
const Token = "test-token"

// Fake imitates the parts of the Cloudflare API hlm uses.
type Fake struct {
	mu       sync.Mutex
	accounts []cloudflare.Account
	zones    []cloudflare.Zone
	tunnels  map[string]*tunnel // by ID
	records  map[string][]cloudflare.DNSRecord
	nextID   int
	// AccountsHidden makes GET /accounts list nothing, as Cloudflare does
	// for tokens without Account Settings: Read.
	AccountsHidden bool
	// NoTunnels refuses the tunnel API, as for a token without Cloudflare
	// Tunnel permissions.
	NoTunnels bool
}

type tunnel struct {
	cloudflare.Tunnel
	account string
	config  *cloudflare.TunnelConfig
}

// New starts a fake API with one account; its URL is the base URL for
// cloudflare.New.
func New() (*Fake, *httptest.Server) {
	f := &Fake{
		accounts: []cloudflare.Account{{ID: "acc1", Name: "Home"}},
		tunnels:  map[string]*tunnel{},
		records:  map[string][]cloudflare.DNSRecord{},
	}
	return f, httptest.NewServer(http.HandlerFunc(f.serve))
}

// AddZone adds a zone to the account.
func (f *Fake) AddZone(id, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.zones = append(f.zones, cloudflare.Zone{ID: id, Name: name, Status: "active", Account: f.accounts[0]})
}

// AddRecord adds a DNS record to a zone.
func (f *Fake) AddRecord(zone string, r cloudflare.DNSRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	r.ID = fmt.Sprintf("rec%d", f.nextID)
	f.records[zone] = append(f.records[zone], r)
}

// Records returns a zone's records, sorted by name.
func (f *Fake) Records(zone string) []cloudflare.DNSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]cloudflare.DNSRecord(nil), f.records[zone]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tunnels returns the tunnels that exist.
func (f *Fake) Tunnels() []cloudflare.Tunnel {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cloudflare.Tunnel
	for _, t := range f.tunnels {
		out = append(out, t.Tunnel)
	}
	return out
}

// Config returns a tunnel's configuration.
func (f *Fake) Config(id string) *cloudflare.TunnelConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tunnels[id]; ok {
		return t.config
	}
	return nil
}

// Connect makes a tunnel healthy with one connection, as cloudflared does
// once it runs.
func (f *Fake) Connect(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tunnels[id]
	t.Status = "healthy"
	t.Connections = []cloudflare.Connection{{ID: "c1", ColoName: "ams01", OpenedAt: time.Now().UTC()}}
}

func reply(w http.ResponseWriter, status int, result any, errs ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var e []map[string]any
	for _, msg := range errs {
		e = append(e, map[string]any{"code": 1003, "message": msg})
	}
	body := map[string]any{"success": len(errs) == 0, "errors": e, "messages": []any{}, "result": result}
	if items, isList := result.([]any); isList {
		body["result_info"] = map[string]int{"page": 1, "per_page": len(items), "total_pages": 1, "count": len(items)}
	}
	json.NewEncoder(w).Encode(body)
}

func asList[T any](items []T) []any {
	out := []any{}
	for _, x := range items {
		out = append(out, x)
	}
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+Token {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 6111, "message": "Invalid format for Authorization header"}}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && r.URL.Path == "/accounts":
		if f.AccountsHidden {
			reply(w, 200, []any{})
		} else {
			reply(w, 200, asList(f.accounts))
		}
	case r.Method == "GET" && r.URL.Path == "/zones":
		var out []cloudflare.Zone
		for _, z := range f.zones {
			if a := q.Get("account.id"); a == "" || a == z.Account.ID {
				out = append(out, z)
			}
		}
		reply(w, 200, asList(out))
	case len(parts) >= 3 && parts[0] == "accounts" && parts[2] == "cfd_tunnel":
		f.serveTunnels(w, r, parts[1], parts[3:])
	case len(parts) >= 3 && parts[0] == "zones" && parts[2] == "dns_records":
		f.serveRecords(w, r, parts[1], parts[3:])
	default:
		reply(w, 404, nil, "Not found")
	}
}

func (f *Fake) serveTunnels(w http.ResponseWriter, r *http.Request, account string, rest []string) {
	if account != f.accounts[0].ID || f.NoTunnels {
		reply(w, 403, nil, "Authentication error")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			var out []cloudflare.Tunnel
			for _, t := range f.tunnels {
				if n := r.URL.Query().Get("name"); n == "" || n == t.Name {
					out = append(out, t.Tunnel)
				}
			}
			reply(w, 200, asList(out))
		case "POST":
			var body struct {
				Name      string `json:"name"`
				ConfigSrc string `json:"config_src"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, t := range f.tunnels {
				if t.Name == body.Name {
					reply(w, 409, nil, "You already have a tunnel with this name")
					return
				}
			}
			f.nextID++
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.nextID)
			t := &tunnel{account: account, Tunnel: cloudflare.Tunnel{ID: id, Name: body.Name, Status: "inactive", CreatedAt: time.Now().UTC(), ConfigSrc: body.ConfigSrc, Connections: []cloudflare.Connection{}}}
			f.tunnels[id] = t
			reply(w, 200, t.Tunnel)
		default:
			reply(w, 405, nil, "Method not allowed")
		}
		return
	}
	t, ok := f.tunnels[rest[0]]
	if !ok {
		reply(w, 404, nil, "Tunnel not found")
		return
	}
	switch {
	case len(rest) == 1 && r.Method == "GET":
		reply(w, 200, t.Tunnel)
	case len(rest) == 1 && r.Method == "DELETE":
		if len(t.Connections) > 0 {
			reply(w, 400, nil, "Cannot delete tunnel because it has active connections")
			return
		}
		delete(f.tunnels, t.ID)
		reply(w, 200, t.Tunnel)
	case len(rest) == 2 && rest[1] == "token" && r.Method == "GET":
		reply(w, 200, "token-for-"+t.ID)
	case len(rest) == 2 && rest[1] == "connections" && r.Method == "DELETE":
		t.Connections, t.Status = []cloudflare.Connection{}, "inactive"
		reply(w, 200, nil)
	case len(rest) == 2 && rest[1] == "configurations" && r.Method == "GET":
		reply(w, 200, map[string]any{"tunnel_id": t.ID, "config": t.config})
	case len(rest) == 2 && rest[1] == "configurations" && r.Method == "PUT":
		var body struct {
			Config cloudflare.TunnelConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			reply(w, 400, nil, err.Error())
			return
		}
		n := len(body.Config.Ingress)
		if n == 0 || body.Config.Ingress[n-1].Hostname != "" {
			reply(w, 400, nil, "The last ingress rule must match all URLs (i.e. it should not have a hostname or path filter)")
			return
		}
		t.config = &body.Config
		reply(w, 200, map[string]any{"tunnel_id": t.ID, "config": t.config})
	default:
		reply(w, 405, nil, "Method not allowed")
	}
}

func (f *Fake) zone(id string) (cloudflare.Zone, bool) {
	for _, z := range f.zones {
		if z.ID == id {
			return z, true
		}
	}
	return cloudflare.Zone{}, false
}

func (f *Fake) serveRecords(w http.ResponseWriter, r *http.Request, zoneID string, rest []string) {
	zone, ok := f.zone(zoneID)
	if !ok {
		reply(w, 404, nil, "Zone not found")
		return
	}
	recs := f.records[zoneID]
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			var out []cloudflare.DNSRecord
			for _, rec := range recs {
				if n := r.URL.Query().Get("name"); n == "" || n == rec.Name {
					out = append(out, rec)
				}
			}
			reply(w, 200, asList(out))
		case "POST":
			var rec cloudflare.DNSRecord
			json.NewDecoder(r.Body).Decode(&rec)
			if rec.Name != zone.Name && !strings.HasSuffix(rec.Name, "."+zone.Name) {
				reply(w, 400, nil, "DNS name is invalid")
				return
			}
			for _, x := range recs {
				if x.Name == rec.Name && (x.Type == "CNAME" || rec.Type == "CNAME") {
					reply(w, 400, nil, "An A, AAAA, or CNAME record with that host already exists")
					return
				}
			}
			f.nextID++
			rec.ID = fmt.Sprintf("rec%d", f.nextID)
			f.records[zoneID] = append(recs, rec)
			reply(w, 200, rec)
		default:
			reply(w, 405, nil, "Method not allowed")
		}
		return
	}
	for i, rec := range recs {
		if rec.ID != rest[0] {
			continue
		}
		switch r.Method {
		case "PUT":
			var upd cloudflare.DNSRecord
			json.NewDecoder(r.Body).Decode(&upd)
			upd.ID = rec.ID
			recs[i] = upd
			reply(w, 200, upd)
		case "DELETE":
			f.records[zoneID] = append(recs[:i:i], recs[i+1:]...)
			reply(w, 200, map[string]string{"id": rec.ID})
		default:
			reply(w, 405, nil, "Method not allowed")
		}
		return
	}
	reply(w, 404, nil, "Record not found")
}
