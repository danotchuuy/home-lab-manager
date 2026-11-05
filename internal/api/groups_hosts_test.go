package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// A group can hold a whole Docker host: its members see every server
// there. Hosts that don't exist can't be added.
func TestGroupHosts(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	names := func(who string) []string {
		w := call(h, who, "GET", "/api/servers", "", e.keys)
		var list []struct {
			Name string `json:"name"`
		}
		json.Unmarshal(w.Body.Bytes(), &list)
		var out []string
		for _, s := range list {
			out = append(out, s.Name)
		}
		return out
	}
	before := names("member")
	if len(before) != 0 {
		t.Fatalf("member sees %v before", before)
	}
	// Through the API (as an admin with a confirmed session would): an
	// unknown host is refused.
	s := &server{e.opts}
	if err := s.checkNewHosts([]string{"nope"}, nil); err == nil || !strings.Contains(err.Error(), "no Docker host") {
		t.Errorf("unknown host: %v", err)
	}
	if err := s.checkNewHosts([]string{"local"}, nil); err != nil {
		t.Errorf("this host: %v", err)
	}
	if err := s.checkNewHosts([]string{"gone"}, []string{"gone"}); err != nil {
		t.Errorf("a host already in the group: %v", err)
	}
	var memberKey string
	for _, k := range e.opts.Auth.APIKeys() {
		if k.Name == "member" {
			memberKey = k.ID
		}
	}
	if _, err := e.groups.Create(rbac.Group{Name: "Everything here", Hosts: []string{"local"},
		Grants: []rbac.Grant{{Subject: rbac.Subject{Kind: rbac.SubjectKey, Name: memberKey}, Role: rbac.Viewer}}}); err != nil {
		t.Fatal(err)
	}
	// Every server on the host (postgres isn't one of hlm's).
	if got := names("member"); !slices.Equal(got, []string{"plex", "valheim"}) && !slices.Equal(got, []string{"valheim", "plex"}) {
		t.Errorf("member sees %v with the host's group", got)
	}
}
