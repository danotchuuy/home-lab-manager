// Package linodetest provides a fake Linode API for tests.
package linodetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/linode"
)

// Token is the API token the fake accepts.
const Token = "test-token"

// Fake imitates the parts of the Linode API hlm uses, including the limits
// on firewall rules.
type Fake struct {
	mu        sync.Mutex
	firewalls map[int]*fw
	instances []linode.Instance
	assigned  map[int][]int // linode ID -> firewall IDs
	puts      int
	readOnly  bool
}

type fw struct {
	linode.Firewall
	rules   map[string]json.RawMessage
	version int
}

// New starts a fake API; its URL is the base URL for linode.New.
func New() (*Fake, *httptest.Server) {
	f := &Fake{firewalls: map[int]*fw{}, assigned: map[int][]int{}}
	return f, httptest.NewServer(http.HandlerFunc(f.serve))
}

// AddFirewall adds a firewall with the given rules and assigns it to the
// Linodes.
func (f *Fake) AddFirewall(id int, label string, rules linode.Rules, linodes ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fw{Firewall: linode.Firewall{ID: id, Label: label, Status: "enabled"}}
	w.rules = map[string]json.RawMessage{}
	data, _ := json.Marshal(rules)
	json.Unmarshal(data, &w.rules)
	f.firewalls[id] = w
	for _, l := range linodes {
		f.assigned[l] = append(f.assigned[l], id)
	}
}

// AddInstance adds a Linode.
func (f *Fake) AddInstance(in linode.Instance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances = append(f.instances, in)
}

// SetReadOnly makes the token unable to change firewalls.
func (f *Fake) SetReadOnly(ro bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readOnly = ro
}

// Rules returns a firewall's rules.
func (f *Fake) Rules(id int) linode.Rules {
	f.mu.Lock()
	defer f.mu.Unlock()
	var r linode.Rules
	data, _ := json.Marshal(f.firewalls[id].rules)
	json.Unmarshal(data, &r)
	return r
}

// Puts counts rule updates.
func (f *Fake) Puts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func fail(w http.ResponseWriter, status int, field, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"field": field, "reason": reason}}})
}

func page[T any](w http.ResponseWriter, data []T) {
	if data == nil {
		data = []T{}
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data, "page": 1, "pages": 1, "results": len(data)})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+Token {
		fail(w, 401, "", "Invalid Token")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/networking/firewalls":
		var list []linode.Firewall
		for id := 1; id <= 1000; id++ {
			if x, ok := f.firewalls[id]; ok {
				list = append(list, x.Firewall)
			}
		}
		page(w, list)
	case r.Method == "GET" && r.URL.Path == "/linode/instances":
		page(w, f.instances)
	case r.Method == "GET" && len(parts) == 4 && parts[0] == "linode" && parts[3] == "firewalls":
		id, _ := strconv.Atoi(parts[2])
		var list []linode.Firewall
		for _, fid := range f.assigned[id] {
			list = append(list, f.firewalls[fid].Firewall)
		}
		page(w, list)
	case len(parts) >= 3 && parts[0] == "networking" && parts[1] == "firewalls":
		id, _ := strconv.Atoi(parts[2])
		x, ok := f.firewalls[id]
		if !ok {
			fail(w, 404, "", "Not found")
			return
		}
		switch {
		case len(parts) == 3 && r.Method == "GET":
			json.NewEncoder(w).Encode(x.Firewall)
		case len(parts) == 4 && parts[3] == "rules" && r.Method == "GET":
			out := map[string]any{"fingerprint": "abc", "version": x.version}
			for k, v := range x.rules {
				out[k] = v
			}
			json.NewEncoder(w).Encode(out)
		case len(parts) == 4 && parts[3] == "rules" && r.Method == "PUT":
			if f.readOnly {
				fail(w, 403, "", "Unauthorized")
				return
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				fail(w, 400, "", err.Error())
				return
			}
			for k := range body {
				switch k {
				case "inbound", "outbound", "inbound_policy", "outbound_policy":
				default:
					fail(w, 400, k, "Unknown field")
					return
				}
			}
			var rules linode.Rules
			json.Unmarshal(mustJSON(body), &rules)
			if rules.InboundPolicy == "" || rules.OutboundPolicy == "" {
				fail(w, 400, "inbound_policy", "This field is required")
				return
			}
			if n := len(rules.Inbound) + len(rules.Outbound); n > linode.MaxRules {
				fail(w, 400, "inbound", fmt.Sprintf("Too many rules (%d)", n))
				return
			}
			for i, rule := range append(rules.Inbound, rules.Outbound...) {
				if err := checkRule(rule); err != "" {
					fail(w, 400, fmt.Sprintf("inbound[%d]", i), err)
					return
				}
			}
			x.rules = body
			x.version++
			f.puts++
			json.NewEncoder(w).Encode(x.rules)
		default:
			fail(w, 405, "", "Method not allowed")
		}
	default:
		fail(w, 404, "", "Not found")
	}
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func checkRule(r linode.Rule) string {
	if r.Action != "ACCEPT" && r.Action != "DROP" {
		return "invalid action"
	}
	if r.Addresses == nil || (len(r.Addresses.IPv4) == 0 && len(r.Addresses.IPv6) == 0) {
		return "addresses required"
	}
	if n := len(r.Label); r.Label != "" && (n < 3 || n > 32) {
		return "label must be 3-32 characters"
	}
	if len(r.Description) > 100 {
		return "description too long"
	}
	pieces := 0
	for _, p := range strings.Split(r.Ports, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(p, "-")
		a, err1 := strconv.Atoi(lo)
		if err1 != nil || a < 1 || a > 65535 || lo != strconv.Itoa(a) {
			return "invalid port " + p
		}
		pieces++
		if isRange {
			b, err := strconv.Atoi(hi)
			if err != nil || b <= a || b > 65535 {
				return "invalid range " + p
			}
			pieces++
		}
	}
	if pieces > linode.MaxPortPieces {
		return "too many ports"
	}
	return ""
}
