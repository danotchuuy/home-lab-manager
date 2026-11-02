package linode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaginationAndUnknownFields(t *testing.T) {
	var put string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/networking/firewalls":
			page := r.URL.Query().Get("page")
			fmt.Fprintf(w, `{"data":[{"id":%s,"label":"fw%s","status":"enabled"}],"page":%s,"pages":2,"results":2}`, page, page, page)
		case r.Method == "GET":
			w.Write([]byte(`{"fingerprint":"x","version":3,"inbound_policy":"DROP","outbound_policy":"ACCEPT","outbound":[],
				"inbound":[{"action":"ACCEPT","protocol":"TCP","ports":"22","addresses":{"ipv4":["0.0.0.0/0"]},"label":"ssh","future_field":{"a":1}}]}`))
		default:
			b, _ := io.ReadAll(r.Body)
			put = string(b)
			w.Write(b)
		}
	}))
	defer srv.Close()
	c, _ := New("tok", srv.URL)
	ctx := context.Background()
	fws, err := c.Firewalls(ctx)
	if err != nil || len(fws) != 2 || fws[1].Label != "fw2" {
		t.Fatalf("firewalls = %+v, %v", fws, err)
	}
	rules, err := c.FirewallRules(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetFirewallRules(ctx, 1, rules); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	json.Unmarshal([]byte(put), &body)
	if _, ok := body["fingerprint"]; ok {
		t.Error("read-only fields were sent back")
	}
	if !strings.Contains(put, `"future_field":{"a":1}`) {
		t.Errorf("unknown rule field dropped: %s", put)
	}
	if _, err := New(" ", ""); err == nil {
		t.Error("empty token accepted")
	}
}
