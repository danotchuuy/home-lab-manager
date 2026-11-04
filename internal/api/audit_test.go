package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// audited records requests as they pass through the real sign-in and
// permission checks.
func TestAuditedRequests(t *testing.T) {
	e := setup(t)
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{e.opts}
	s.Audit = log
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, s.audited(rt.pattern, s.authorize(rt.policy, func(w http.ResponseWriter, r *http.Request) {
			audit.Add(r.Context(), "note", "handled")
			w.WriteHeader(http.StatusNoContent)
		})))
	}
	h := s.auth(mux)

	call(h, "admin", "POST", "/api/servers/valheim/restart", "", e.keys)                   // ok
	call(h, "viewer", "POST", "/api/servers/valheim/restart", "", e.keys)                  // denied: viewers can't
	call(h, "admin", "GET", "/api/servers", "", e.keys)                                    // a read: not recorded
	call(h, "operator", "GET", "/api/servers/valheim/backups/download?key=k1", "", e.keys) // a recorded read
	call(h, "admin", "PUT", "/api/servers/valheim/tasks/abc", "{}", e.keys)                // two path values
	call(h, "admin", "POST", "/api/auth/login", "{}", e.keys)                              // left to the sign-in service
	call(h, "anonymous", "DELETE", "/api/servers/valheim", "", e.keys)                     // not signed in: stopped before routing

	got, _, err := log.Query(audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var summary [][]string
	for i := len(got) - 1; i >= 0; i-- {
		x := got[i]
		summary = append(summary, []string{x.Actor, x.ActorKind, x.Action, x.Target, x.Server, x.Result, x.Details["note"], x.Details["key"]})
	}
	want := [][]string{
		{"token", audit.ActorToken, "servers.restart", "valheim", "valheim", audit.ResultOK, "handled", ""},
		{"key:viewer", audit.ActorKey, "servers.restart", "valheim", "valheim", audit.ResultDenied, "", ""},
		{"key:operator", audit.ActorKey, "servers.backups.download", "valheim", "valheim", audit.ResultOK, "handled", "k1"},
		{"token", audit.ActorToken, "servers.tasks.set", "valheim/abc", "valheim", audit.ResultOK, "handled", ""},
	}
	if !slices.EqualFunc(summary, want, slices.Equal) {
		t.Errorf("recorded:\n%q\nwant:\n%q", summary, want)
	}
	denied := got[len(got)-2]
	if denied.Status != http.StatusForbidden || denied.Error == "" || denied.IP == "" {
		t.Errorf("denied entry = %+v", denied)
	}
}

func TestActionNames(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("action", "stop")
	tests := map[string]string{
		"POST /api/servers":                          "servers.create",
		"POST /api/servers/{id}/{action}":            "servers.stop",
		"PUT /api/servers/{id}/config":               "servers.config.set",
		"DELETE /api/auth/keys/{id}":                 "auth.keys.delete",
		"POST /api/servers/{id}/backups/restore":     "servers.backups.restore",
		"POST /api/notifications/targets":            "notifications.targets.create",
		"POST /api/servers/{id}/console/command":     "servers.console.command",
		"PUT /api/servers/{id}/files/content":        "servers.files.content.set",
		"POST /api/host/storage/deleted/{id}/forget": "host.storage.deleted.forget",
	}
	for pattern, want := range tests {
		if got := actionOf(pattern, r); got != want {
			t.Errorf("actionOf(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestMyActivity(t *testing.T) {
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []audit.Entry{
		{Actor: "key:ci", Action: "auth.login"},
		{Actor: "key:ci-other", Action: "auth.login"}, // a name that only contains the caller's
		{Actor: "key:ci", Action: "auth.config.set"},  // not a sign-in
		{Actor: "key:ci", Action: "auth.ssh_login"},
		{Actor: "key:ci", Action: "servers.restart"},
	} {
		log.Record(e)
	}
	s := &server{Options{Audit: log}}
	r := httptest.NewRequest("GET", "/api/auth/activity", nil)
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, rbac.Principal{Name: "key:ci"}))
	w := httptest.NewRecorder()
	s.myActivity(w, r)
	var got struct{ Entries []audit.Entry }
	json.Unmarshal(w.Body.Bytes(), &got)
	var actions []string
	for _, e := range got.Entries {
		actions = append(actions, e.Actor+" "+e.Action)
	}
	if !slices.Equal(actions, []string{"key:ci auth.ssh_login", "key:ci auth.login"}) {
		t.Errorf("got %q", actions)
	}
}

// A streamed operation that fails after its 200 is recorded as failed.
func TestAuditedStreamFailure(t *testing.T) {
	log, err := audit.Open(filepath.Join(t.TempDir(), "audit"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{Options{Audit: log, Log: slog.New(slog.DiscardHandler)}}
	h := s.audited("POST /api/servers", func(w http.ResponseWriter, r *http.Request) {
		audit.SetServer(r.Context(), "valheim")
		s.stream(w, r, func(context.Context, func(manager.Event)) error { return errors.New("pull failed: no such image") })
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/api/servers", nil))
	got, _, _ := log.Query(audit.Filter{})
	if len(got) != 1 || got[0].Result != audit.ResultError || got[0].Error != "pull failed: no such image" ||
		got[0].Status != http.StatusOK || got[0].Server != "valheim" || got[0].Target != "valheim" || len(got[0].Details) != 0 {
		t.Errorf("entry = %+v", got)
	}
}
