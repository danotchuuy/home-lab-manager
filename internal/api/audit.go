package api

import (
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// auditNames overrides the action recorded for a route; "-" doesn't record
// it. Other changes (anything but GET) are recorded with a name made from
// their path; reads aren't, except those listed here.
var auditNames = map[string]string{
	// Recorded by the sign-in service, with the username and outcome.
	"POST /api/auth/login":         "-",
	"POST /api/auth/logout":        "-",
	"POST /api/auth/ssh/challenge": "-",
	"POST /api/auth/ssh/login":     "-",
	"POST /api/auth/confirm":       "-",
	// Not changes.
	"POST /api/auth/download-ticket":         "-",
	"POST /api/servers/check-ports":          "-",
	"GET /api/servers/{id}/backups/download": "servers.backups.download",
	"GET /api/system/backup/download":        "system.backup.download",
	"GET /api/audit/export":                  "audit.export",
	"GET /api/servers/{id}/export":           "servers.export",
	"POST /api/servers/import/preview":       "-",
	"POST /api/servers/{id}/clone/preview":   "-",
}

// actionVerbs are path ends that already say what a POST does; other POSTs
// create something (".create").
var actionVerbs = []string{
	"test", "check", "apply", "run", "restore", "start", "stop", "restart", "update", "renew", "import", "refresh",
	"sync", "install", "command", "adopt", "dismiss", "forget", "delete", "remove", "key", "secret", "wan-check",
	"detect", "confirm", "login", "logout", "challenge", "download-ticket", "check-ports", "check-ip", "clone",
}

var wildcardRE = regexp.MustCompile(`\{(\w+)\.*\}`)

// actionOf names what a request does, from its route's pattern: the path
// without /api and placeholders (an {action} placeholder is filled in), and
// for PUT, DELETE and most POSTs a verb.
func actionOf(pattern string, r *http.Request) string {
	method, path, _ := strings.Cut(pattern, " ")
	var parts []string
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/api/"), "/") {
		if m := wildcardRE.FindStringSubmatch(seg); m != nil {
			if m[1] == "action" {
				parts = append(parts, r.PathValue("action"))
			}
			continue
		}
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	base := strings.Join(parts, ".")
	switch method {
	case http.MethodPut, http.MethodPatch:
		return base + ".set"
	case http.MethodDelete:
		return base + ".delete"
	case http.MethodPost:
		if len(parts) > 0 && slices.Contains(actionVerbs, parts[len(parts)-1]) {
			return base
		}
		return base + ".create"
	}
	return base
}

// auditWriter notes a response's status, and the start of an error's body.
type auditWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (w *auditWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.body) < 2048 {
		w.body = append(w.body, b[:min(len(b), 2048-len(w.body))]...)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// actorOf says who a request is from, for the audit log.
func actorOf(r *http.Request) (name, kind string) {
	if u, ok := r.Context().Value(sessionKey{}).(*auth.User); ok && u != nil {
		if u.Method == auth.MethodOIDC {
			return "sso:" + u.Display(), audit.ActorSSO
		}
		return cmpOr(u.Username, u.Display()), audit.ActorLocal
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	switch {
	case p.Name == "token":
		return "token", audit.ActorToken
	case strings.HasPrefix(p.Name, "key:"):
		return p.Name, audit.ActorKey
	case strings.HasPrefix(p.Name, "ssh:"):
		return p.Name, audit.ActorSSH
	case p.Name == anonymous:
		return anonymous, audit.ActorAnonymous
	}
	return cmpOr(p.Name, "unknown"), ""
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// audited records a route's requests in the audit log: every change, and
// the reads auditNames lists.
func (s *server) audited(pattern string, next http.HandlerFunc) http.HandlerFunc {
	if s.Audit == nil {
		return next
	}
	name, named := auditNames[pattern]
	method, _, hasMethod := strings.Cut(pattern, " ")
	if name == "-" || !hasMethod || (!named && safeMethod(method)) {
		return next
	}
	wildcards := wildcardRE.FindAllStringSubmatch(pattern, -1)
	serverRoute := strings.Contains(pattern, " /api/servers/{id}")
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, details := audit.WithDetails(r.Context())
		// The backup a download, restore or delete is about.
		audit.Add(ctx, "key", r.URL.Query().Get("key"))
		aw := &auditWriter{ResponseWriter: w}
		next(aw, r.WithContext(ctx))

		e := audit.Entry{Action: name, IP: remoteIP(r), Status: cmpStatus(aw.status), Details: details}
		if e.Action == "" {
			e.Action = actionOf(pattern, r)
		}
		e.Actor, e.ActorKind = actorOf(r)
		var targets []string
		for _, m := range wildcards {
			if v := r.PathValue(m[1]); v != "" && m[1] != "action" {
				targets = append(targets, v)
			}
		}
		e.Target = strings.Join(targets, "/")
		if serverRoute {
			e.Server = r.PathValue("id")
		}
		if named := audit.TakeServer(details); named != "" && e.Server == "" {
			e.Server, e.Target = named, named
		}
		switch {
		case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
			e.Result = audit.ResultDenied
		case e.Status >= 400:
			e.Result = audit.ResultError
		default:
			e.Result = audit.ResultOK
		}
		if e.Status >= 400 {
			var body struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(aw.body, &body) == nil {
				e.Error = body.Error
			}
		}
		if failed := audit.TakeFailure(details); failed != "" && e.Result == audit.ResultOK {
			e.Result, e.Error = audit.ResultError, failed
		}
		if len(e.Details) == 0 {
			e.Details = nil
		}
		s.Audit.Record(e)
	}
}

func cmpStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}
