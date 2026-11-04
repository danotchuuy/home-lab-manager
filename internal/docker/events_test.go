package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEvents(t *testing.T) {
	var gotFilters, gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.47")
			return
		}
		gotFilters, gotSince = r.URL.Query().Get("filters"), r.URL.Query().Get("since")
		w.Write([]byte(`{"Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"name":"valheim","exitCode":"1","io.gsh.managed":"true"}},"time":1700000000,"timeNano":1700000000123456789}` + "\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte(`{"Type":"container","Action":"oom","Actor":{"ID":"abc","Attributes":{}},"time":1700000001}`))
	}))
	defer srv.Close()
	c, err := NewClient(Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	var got []EventMessage
	err = c.Events(context.Background(), time.Unix(1700000000, 0), map[string][]string{"type": {"container"}}, func(e EventMessage) {
		got = append(got, e)
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("end of stream: err = %v", err)
	}
	var f map[string][]string
	if json.Unmarshal([]byte(gotFilters), &f) != nil || f["type"][0] != "container" || gotSince != "1700000000" {
		t.Errorf("filters = %q, since = %q", gotFilters, gotSince)
	}
	if len(got) != 2 || got[0].Action != "die" || got[0].Actor.Attributes["exitCode"] != "1" || got[1].Action != "oom" {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Actor.Attributes["io.hlm.managed"] != "true" {
		t.Errorf("legacy labels weren't given their current names: %v", got[0].Actor.Attributes)
	}
	if got[0].When().UnixNano() != 1700000000123456789 || got[1].When().Unix() != 1700000001 {
		t.Errorf("times = %v, %v", got[0].When(), got[1].When())
	}
}
