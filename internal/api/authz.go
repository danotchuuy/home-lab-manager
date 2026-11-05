package api

// Who may do what. auth works out who a request is from; authorize, around
// every route, decides whether they may use it; and the endpoints open to
// everyone narrow what they return to what the caller can see.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// policyKind is how a route decides who may use it.
type policyKind int

const (
	kindAdmin policyKind = iota // the zero value: admins only
	kindPublic
	kindSignedIn
	kindServer
	kindDeploy
	kindConfirmed
)

// policy says who may use a route.
type policy struct {
	kind policyKind
	role rbac.Role // for kindServer
}

var (
	// public needs nobody to be signed in.
	public = policy{kind: kindPublic}
	// signedIn is for everyone who's let in.
	signedIn = policy{kind: kindSignedIn}
	// admin is for admins only.
	admin = policy{kind: kindAdmin}
	// deployers is for admins and keys with the deployer role.
	deployers = policy{kind: kindDeploy}
	// confirmed is for admins at a browser who've confirmed it's them in the
	// last auth.ConfirmWindow: for changes that could give lasting access,
	// which a stolen API key or token mustn't be able to make.
	confirmed = policy{kind: kindConfirmed}
)

// serverRole needs at least role on the server in the path's {id}.
func serverRole(role rbac.Role) policy { return policy{kind: kindServer, role: role} }

var errNotAllowed = errors.New("you don't have access to do this")

// principalKey is the request context's key for who a request is from, and
// sessionKey for their session (an *auth.User), if it's from one.
type (
	principalKey struct{}
	sessionKey   struct{}
)

// anonymous is who a request is from when sign-in isn't required.
const anonymous = "anonymous"

// identify works out who a request is from, and their session if it's
// because of a session cookie. HLM_TOKEN is always an admin, and so is anyone
// at all when sign-in isn't required, so grants mean nothing until it is.
//
// Tokens are only taken from the Authorization header: in a URL they end up
// in logs and browser history. Downloads, which can't send a header, use a
// ticket that works once for that URL (see downloadTicket).
func (s *server) identify(r *http.Request) (p rbac.Principal, session *auth.User, ok bool) {
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != "" {
		if s.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1 {
			return rbac.Principal{Name: "token", Role: rbac.Admin}, nil, true
		}
		if p, ok := s.Auth.CheckAPIKey(got, r.RemoteAddr); ok {
			return p, nil, true
		}
		if p, ok := s.Auth.CheckSSHToken(got, r.RemoteAddr); ok {
			return p, nil, true
		}
	}
	if t := r.URL.Query().Get("ticket"); t != "" && r.Method == http.MethodGet && ticketPath.MatchString(r.URL.Path) {
		if p, ok := s.Auth.RedeemTicket(t, ticketTarget(r.URL), r.RemoteAddr); ok {
			return p, nil, true
		}
	}
	if u, ok := s.Auth.Session(r); ok {
		return s.Auth.Principal(u), &u, true
	}
	if s.Token == "" && !s.Auth.Required() {
		return rbac.Principal{Name: anonymous, Role: rbac.Admin}, nil, true
	}
	return rbac.Principal{}, nil, false
}

// ticketPath matches the downloads a ticket can be for.
var ticketPath = regexp.MustCompile(`^/api/(servers/[^/]+/backups/download|system/backup/download)$`)

// ticketTarget is what a ticket is for: a URL's path and query, without the
// ticket, in a fixed order.
func ticketTarget(u *url.URL) string {
	q := u.Query()
	q.Del("ticket")
	return u.Path + "?" + q.Encode()
}

// downloadTicket gives someone signed in with a token a link they can open
// once, from where they are, in the next minute.
func (s *server) downloadTicket(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.IsAbs() || u.Host != "" || !ticketPath.MatchString(u.Path) {
		writeError(w, http.StatusBadRequest, errors.New("tickets are only for backup downloads"))
		return
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	t, err := s.Auth.IssueTicket(p, ticketTarget(u), r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	q := u.Query()
	q.Set("ticket", t)
	writeJSON(w, http.StatusOK, map[string]string{"url": u.Path + "?" + q.Encode()})
}

// stepUp reports whether the caller may make a change that could give
// lasting access, and answers them if not. It takes a session confirmed in
// the last auth.ConfirmWindow, unless sign-in isn't required, when everyone
// can do everything anyway.
func (s *server) stepUp(w http.ResponseWriter, r *http.Request) bool {
	if p, _ := r.Context().Value(principalKey{}).(rbac.Principal); p.Name == anonymous {
		return true
	}
	u, _ := r.Context().Value(sessionKey{}).(*auth.User)
	if u == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this can't be done with an API key or token: sign in at a browser and confirm it's you",
			"code":  "interactive_required",
		})
		return false
	}
	if !s.Auth.Confirmed(*u) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "confirm it's you first: this changes who can get in",
			"code":   "confirm_required",
			"method": u.Method,
			// The prompt asks for the authenticator app's code too, or offers
			// the account's passkeys.
			"twoFactor": u.Method == auth.MethodLocal && s.Auth.HasTwoFactor(u.Username),
			"passkey":   u.Method == auth.MethodLocal && s.Auth.HasPasskeys(u.Username),
		})
		return false
	}
	return true
}

// confirmSession confirms a password session for auth.ConfirmWindow. A
// single sign-on session confirms by signing in again at
// /auth/login?confirm=1.
func (s *server) confirmSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.Auth.ConfirmPassword(w, r, in.Password, in.Code)
	var le *auth.LoginError
	switch {
	case errors.Is(err, auth.ErrCodeNeeded):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error(), "code": "code_required"})
	case errors.As(err, &le):
		if le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
		}
		writeError(w, http.StatusForbidden, err)
	case err != nil:
		writeError(w, http.StatusForbidden, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"confirmedUntil": u.Confirmed.Add(auth.ConfirmWindow)})
	}
}

// accessOf returns what the caller may do. Without a caller, that's nothing.
func (s *server) accessOf(ctx context.Context) rbac.Access {
	p, _ := ctx.Value(principalKey{}).(rbac.Principal)
	return s.Groups.Access(p)
}

func (s *server) access(r *http.Request) rbac.Access { return s.accessOf(r.Context()) }

// authorize runs next if the caller may use the route. Someone who can't see
// a server gets the same answer as if it didn't exist.
func (s *server) authorize(pol policy, next http.HandlerFunc) http.HandlerFunc {
	if pol.kind == kindPublic {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := r.Context().Value(principalKey{}).(rbac.Principal)
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("sign in, or send a valid API key"))
			return
		}
		switch {
		case !p.Role.Valid():
			writeError(w, http.StatusForbidden, errNotAllowed)
		case pol.kind == kindConfirmed:
			if !p.Admin() {
				writeError(w, http.StatusForbidden, errNotAllowed)
			} else if s.stepUp(w, r) {
				next(w, r)
			}
		case p.Admin(), pol.kind == kindSignedIn, pol.kind == kindDeploy && p.Role == rbac.Deployer:
			next(w, r)
		case pol.kind != kindServer:
			writeError(w, http.StatusForbidden, errNotAllowed)
		default:
			s.authorizeServer(w, r, pol.role, next)
		}
	}
}

// authorizeServer checks the caller's role on the server in the path. {id}
// can be a name or part of an ID, so it's resolved before the check.
func (s *server) authorizeServer(w http.ResponseWriter, r *http.Request, want rbac.Role, next http.HandlerFunc) {
	info, err := s.Manager.Inspect(r.Context(), r.PathValue("id"))
	if errors.Is(err, manager.ErrNotFound) || errors.Is(err, manager.ErrNotManaged) {
		writeError(w, http.StatusNotFound, manager.ErrNotFound)
		return
	}
	if err != nil {
		writeManagerError(w, err)
		return
	}
	role := s.access(r).Role(strings.TrimPrefix(info.Name, "/"))
	switch {
	case role == rbac.None:
		writeError(w, http.StatusNotFound, manager.ErrNotFound)
	case !role.AtLeast(want):
		writeError(w, http.StatusForbidden, errNotAllowed)
	default:
		next(w, r)
	}
}

// deployGroup picks the group a deployer's new server goes in: one they
// manage, so that they can use the server afterwards. The group is only
// optional if there's a single one to choose.
func deployGroup(access rbac.Access, want string) (string, error) {
	managed := access.ManagedGroups()
	switch {
	case want != "" && slices.Contains(managed, want):
		return want, nil
	case want != "":
		return "", errors.New("you need the manager role in a server group to create servers in it")
	case len(managed) == 1:
		return managed[0], nil
	case len(managed) == 0:
		return "", errors.New("you need the manager role in a server group to create servers: ask an admin for one")
	}
	return "", errors.New(`you manage several server groups, so say which one with "group"`)
}

// manages reports whether the caller is a manager of the server named id.
func (s *server) manages(ctx context.Context, access rbac.Access, id string) bool {
	if id == "" {
		return false
	}
	info, err := s.Manager.Inspect(ctx, id)
	return err == nil && access.Can(strings.TrimPrefix(info.Name, "/"), rbac.Manager)
}

// present prepares a server for a caller: what they may do with it, and
// without its settings, which can hold passwords, unless they may see them.
// Operators see the settings without the ones that look like secrets.
func (s *server) present(access rbac.Access, srv manager.Server) manager.Server {
	role := access.Role(srv.Name)
	srv.Access = string(role)
	srv.Groups = access.GroupsOf(srv.Name)
	switch {
	case !role.AtLeast(rbac.Operator):
		srv.Request = nil
	case !role.AtLeast(rbac.Manager) && srv.Request != nil:
		app, _ := s.Manager.AppOn(srv.TemplateID, srv.Host)
		_, req, hidden := deploy.HideSecrets(app, *srv.Request)
		srv.Request, srv.Hidden = &req, hidden
	}
	return srv
}

// visibleServers lists the servers the caller has at least want on, prepared
// for them with present.
func (s *server) visibleServers(ctx context.Context, access rbac.Access, want rbac.Role) ([]manager.Server, error) {
	all, err := s.Manager.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]manager.Server, 0, len(all))
	for _, srv := range all {
		if access.Can(srv.Name, want) {
			out = append(out, s.present(access, srv))
		}
	}
	return out, nil
}

// visibleName is a server's name, if the caller can see that server.
func visibleName(access rbac.Access, name string) string {
	if access.Can(name, rbac.Viewer) {
		return name
	}
	return "another server"
}

// hideConflicts leaves out what port conflicts say about servers the caller
// can't see.
func hideConflicts(access rbac.Access, list []manager.PortConflict) []manager.PortConflict {
	if access.Admin() {
		return list
	}
	out := make([]manager.PortConflict, 0, len(list))
	for _, c := range list {
		if c.UsedBy != "" && !access.Can(c.UsedBy, rbac.Viewer) {
			c.UsedBy = ""
			c.Message = "This port is already in use on the host."
		}
		out = append(out, c)
	}
	return out
}

// hideSyncDetails drops the explanations that name other servers from port
// states.
func hideSyncDetails(states ...map[string]forwarding.SyncState) {
	for _, m := range states {
		for k, st := range m {
			if st.State == forwarding.SyncConflict || st.State == forwarding.SyncOff {
				st.Detail = ""
				m[k] = st
			}
		}
	}
}

// hideRules leaves out what other rules on the gateway are for.
func hideRules(access rbac.Access, st forwarding.Status) forwarding.Status {
	if access.Admin() {
		return st
	}
	for i := range st.Rules {
		for j := range st.Rules[i].Conflicts {
			st.Rules[i].Conflicts[j].Name, st.Rules[i].Conflicts[j].Fwd = "another rule", ""
		}
	}
	return st
}

// --- server groups ---

// listGroups returns every group with its grants to an admin, and to anyone
// else the groups they're in, without the grants.
func (s *server) listGroups(w http.ResponseWriter, r *http.Request) {
	access := s.access(r)
	if !access.Admin() {
		writeJSON(w, http.StatusOK, access.Groups())
		return
	}
	groups, err := s.Groups.Groups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func decodeGroup(w http.ResponseWriter, r *http.Request) (rbac.Group, bool) {
	var g rbac.Group
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&g); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return g, false
	}
	return g, true
}

func writeGroupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rbac.ErrNoGroup):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, rbac.ErrGroupExists):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// checkNewServers makes sure servers being added to a group exist: a mistyped
// name would quietly give access to whatever gets that name later.
func (s *server) checkNewServers(ctx context.Context, servers, had []string) error {
	known := map[string]bool{}
	for _, name := range had {
		known[name] = true
	}
	for _, name := range servers {
		if known[name] {
			continue
		}
		info, err := s.Manager.Inspect(ctx, name)
		if err != nil {
			return errors.New(name + ": " + err.Error())
		}
		if strings.TrimPrefix(info.Name, "/") != name {
			return errors.New(name + ": use the server's full name")
		}
	}
	return nil
}

// checkNewHosts makes sure Docker hosts being added to a group exist, as
// checkNewServers does for servers.
func (s *server) checkNewHosts(hosts, had []string) error {
	for _, h := range hosts {
		if !slices.Contains(had, h) && (s.Manager == nil || !s.Manager.KnownHost(h)) {
			return fmt.Errorf("there's no Docker host %q", h)
		}
	}
	return nil
}

func (s *server) createGroup(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeGroup(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "name", in.Name)
	if err := s.checkNewServers(r.Context(), in.Servers, nil); err != nil {
		writeGroupError(w, err)
		return
	}
	if err := s.checkNewHosts(in.Hosts, nil); err != nil {
		writeGroupError(w, err)
		return
	}
	g, err := s.Groups.Create(in)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *server) updateGroup(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeGroup(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "name", in.Name)
	id := r.PathValue("id")
	groups, err := s.Groups.Groups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var had, hadHosts []string
	for _, g := range groups {
		if g.ID == id {
			had, hadHosts = g.Servers, g.Hosts
		}
	}
	if err := s.checkNewServers(r.Context(), in.Servers, had); err != nil {
		writeGroupError(w, err)
		return
	}
	if err := s.checkNewHosts(in.Hosts, hadHosts); err != nil {
		writeGroupError(w, err)
		return
	}
	g, err := s.Groups.Update(id, in)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.Groups.Delete(r.PathValue("id")); err != nil {
		writeGroupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setServerGroups puts a server in the groups with the given IDs, and takes
// it out of the others.
func (s *server) setServerGroups(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups []string `json:"groups"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	srv, err := s.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	if err := s.Groups.SetServerGroups(srv.Name, body.Groups); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.presentOne(r, srv))
}
