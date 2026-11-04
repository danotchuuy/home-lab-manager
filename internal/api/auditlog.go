package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
)

// --- The audit log ---

// auditFilter reads a filter from the query: actor, action, target, server,
// result, text, since and before (RFC 3339), limit.
func auditFilter(r *http.Request) (audit.Filter, error) {
	q := r.URL.Query()
	f := audit.Filter{
		Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target"), Server: q.Get("server"),
		Result: q.Get("result"), Text: q.Get("text"),
	}
	for _, p := range []struct {
		name string
		to   *time.Time
	}{{"since", &f.Since}, {"before", &f.Before}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return f, fmt.Errorf("%s must be a time like 2026-11-04T12:00:00Z", p.name)
			}
			*p.to = t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, errors.New("limit must be a positive number")
		}
		f.Limit = n
	}
	return f, nil
}

func (s *server) writeEntries(w http.ResponseWriter, f audit.Filter) {
	entries, next, err := s.Audit.Query(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{"entries": entries}
	if !next.IsZero() {
		out["next"] = next.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, out)
}

// auditEntries lists the audit log's entries, newest first, a page at a
// time.
func (s *server) auditEntries(w http.ResponseWriter, r *http.Request) {
	f, err := auditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.writeEntries(w, f)
}

// exportAudit downloads the entries that match, as CSV or JSON Lines.
func (s *server) exportAudit(w http.ResponseWriter, r *http.Request) {
	f, err := auditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "jsonl" {
		writeError(w, http.StatusBadRequest, errors.New("format must be csv or jsonl"))
		return
	}
	// Everything that matches, a page at a time.
	var all []audit.Entry
	f.Limit = audit.MaxLimit
	for len(all) < 100_000 {
		page, next, err := s.Audit.Query(f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		all = append(all, page...)
		if next.IsZero() {
			break
		}
		f.Before = next
	}
	name := "hlm-audit-" + time.Now().UTC().Format("20060102-150405") + "." + format
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, e := range all {
			_ = enc.Encode(e)
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	writeAuditCSV(w, all)
}

func writeAuditCSV(w io.Writer, entries []audit.Entry) {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "actor", "actor_kind", "ip", "action", "target", "server", "result", "status", "error", "details"})
	for _, e := range entries {
		var details []string
		for k, v := range e.Details {
			details = append(details, k+"="+v)
		}
		sort.Strings(details)
		status := ""
		if e.Status != 0 {
			status = strconv.Itoa(e.Status)
		}
		_ = cw.Write([]string{e.Time.Format(time.RFC3339), e.Actor, e.ActorKind, e.IP, e.Action, e.Target, e.Server,
			e.Result, status, e.Error, strings.Join(details, "; ")})
	}
	cw.Flush()
}

// serverActivity lists what was done to a server, newest first.
func (s *server) serverActivity(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	f, err := auditFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f.Server = name
	if f.Limit == 0 || f.Limit > 200 {
		f.Limit = 50
	}
	s.writeEntries(w, f)
}

// signInActions are what "your recent sign-ins" lists.
var signInActions = []string{"auth.login", "auth.logout", "auth.ssh_login", "auth.confirm"}

// myActivity lists the caller's own recent sign-ins, sign-outs and
// confirmations, newest first.
func (s *server) myActivity(w http.ResponseWriter, r *http.Request) {
	actor, _ := actorOf(r)
	entries, _, err := s.Audit.Query(audit.Filter{Actor: actor, Action: "auth.", Limit: audit.MaxLimit})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := []audit.Entry{}
	for _, e := range entries {
		// The filter matches names loosely; only the caller's own count.
		if e.Actor == actor && slices.Contains(signInActions, e.Action) && len(out) < 20 {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (s *server) auditSettings(w http.ResponseWriter, r *http.Request) {
	set := s.Audit.Settings()
	writeJSON(w, http.StatusOK, map[string]any{
		"retentionMonths": set.Months(), "mirror": set.Mirror, "defaultRetention": audit.DefaultRetention,
	})
}

func (s *server) setAuditSettings(w http.ResponseWriter, r *http.Request) {
	var in audit.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Addf(r.Context(), "retentionMonths", "%d", in.Months())
	audit.Addf(r.Context(), "mirror", "%v", in.Mirror)
	if err := s.Audit.SetSettings(in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.auditSettings(w, r)
}
