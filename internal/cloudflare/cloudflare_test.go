package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		json.NewEncoder(w).Encode(map[string]any{
			"success":     true,
			"result":      []Zone{{ID: "z" + strconv.Itoa(page), Name: "zone" + strconv.Itoa(page) + ".com"}},
			"result_info": map[string]int{"page": page, "total_pages": 3},
		})
	}))
	defer srv.Close()
	c, _ := New("t", srv.URL)
	zones, err := c.Zones(context.Background(), "acc")
	if err != nil || len(zones) != 3 || zones[2].ID != "z3" {
		t.Errorf("zones = %+v, %v", zones, err)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{403, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`, "doesn't have Account → Cloudflare Tunnel: Edit"},
		{400, `{"success":false,"errors":[{"code":6111,"message":"Invalid format for Authorization header"}]}`, "isn't valid"},
		{400, `{"success":false,"errors":[{"code":81053,"message":"An A, AAAA, or CNAME record with that host already exists."}]}`, "already exists"},
		{200, `{"success":false,"errors":[{"code":1,"message":"odd"}]}`, "odd"},
		{502, `<html>bad gateway</html>`, "Bad Gateway"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		cl, _ := New("t", srv.URL)
		_, err := cl.Tunnel(context.Background(), "acc", "id")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !strings.Contains(err.Error(), c.want) || apiErr.Status < 400 {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
		srv.Close()
	}
	if _, err := New(" ", ""); err == nil {
		t.Error("empty token accepted")
	}
}
