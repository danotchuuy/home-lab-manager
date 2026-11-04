// Package rbac decides what a signed-in principal may do.
//
// Everyone has a global role: Admin may do everything, Member only what
// their grants allow, and Deployer, for API keys, also creates confined
// servers (see deploy.Confine) in groups it manages. Servers are put in groups, and a group's grants give
// subjects (local accounts, OIDC users or groups, API keys) a role on all of
// its servers. A server can be in several groups; the highest role applies.
//
// Membership is by container name. A container's ID changes whenever it's
// recreated and its labels can't be changed without recreating it, while
// servers can't be renamed.
package rbac

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "rbac"

// Role is what a principal may do, globally (Admin, Member) or on a group's
// servers (Viewer, Operator, Manager). Each role includes the ones before it.
type Role string

const (
	// None is no access at all.
	None Role = ""
	// Member is the global role of everyone but admins. It gives no access
	// to servers by itself.
	Member Role = "member"
	// Deployer is a Member that may also create servers, as long as they're
	// confined and go in a group it manages. Only API keys can have it.
	Deployer Role = "deployer"
	// Metrics may only read /metrics (Prometheus): nothing under /api. Only
	// API keys can have it.
	Metrics Role = "metrics"
	// Viewer sees a server's status, without its settings, logs or files,
	// which can hold passwords.
	Viewer Role = "viewer"
	// Operator runs a server: controls, updates, logs, files (reading) and
	// taking backups.
	Operator Role = "operator"
	// Manager changes a server: safe settings, files, ports and restores.
	Manager Role = "manager"
	// Admin may do everything.
	Admin Role = "admin"
)

func (r Role) rank() int {
	switch r {
	case Member, Deployer:
		return 1
	case Viewer:
		return 2
	case Operator:
		return 3
	case Manager:
		return 4
	case Admin:
		return 5
	}
	return 0
}

// AtLeast reports whether r includes want. An unknown role includes nothing.
func (r Role) AtLeast(want Role) bool { return r.rank() >= want.rank() && r.rank() > 0 }

// Valid reports whether r can be someone's global role.
func (r Role) Valid() bool { return r == Admin || r == Member || r == Deployer }

// ForKey reports whether r can be an API key's role: a global role, or
// Metrics.
func (r Role) ForKey() bool { return r.Valid() || r == Metrics }

// ForAccount reports whether r can be the global role of an account.
func (r Role) ForAccount() bool { return r == Admin || r == Member }

func (r Role) grantable() bool { return r == Viewer || r == Operator || r == Manager }

// Kinds of Subject.
const (
	SubjectLocal     = "local"      // a local account, by username
	SubjectOIDCUser  = "oidc-user"  // a single sign-on user, by username or email
	SubjectOIDCGroup = "oidc-group" // a group from the single sign-on provider
	SubjectKey       = "key"        // an API key, by ID
)

// Subject is who a grant is for.
type Subject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// same reports whether two subjects are the same. Group names are matched
// exactly, as the sign-in allow list does; usernames and emails aren't.
func (s Subject) same(o Subject) bool {
	if s.Kind != o.Kind {
		return false
	}
	if s.Kind == SubjectLocal || s.Kind == SubjectOIDCUser {
		return strings.EqualFold(s.Name, o.Name)
	}
	return s.Name == o.Name
}

// Grant gives a subject a role on a group's servers.
type Grant struct {
	Subject Subject `json:"subject"`
	Role    Role    `json:"role"`
}

// Group is a set of servers and who may use them.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Servers are container names.
	Servers []string `json:"servers"`
	Grants  []Grant  `json:"grants"`
}

// Principal is who a request comes from.
type Principal struct {
	// Name identifies the principal in logs.
	Name string
	// Role is the global role.
	Role Role
	// Subjects are what grants to this principal are given to.
	Subjects []Subject
}

// Admin reports whether the principal may do everything.
func (p Principal) Admin() bool { return p.Role == Admin }

// holds reports whether a grant to s applies to p.
func (p Principal) holds(s Subject) bool {
	return slices.ContainsFunc(p.Subjects, s.same)
}

// Access is what one principal may do, as of when it was made.
type Access struct {
	admin   bool
	roles   map[string]Role     // by server
	groups  map[string][]string // the groups each server is seen in, by server
	visible []Group             // the principal's groups, without grants
	manages []string            // the IDs of the groups they have Manager in
}

// Role returns the principal's role on a server: None if they can't see it.
func (a Access) Role(server string) Role {
	if a.admin {
		return Admin
	}
	return a.roles[server]
}

// Can reports whether the principal has at least want on a server.
func (a Access) Can(server string, want Role) bool {
	return a.Role(server).AtLeast(want)
}

// Admin reports whether the principal may do everything.
func (a Access) Admin() bool { return a.admin }

// GroupsOf returns the names of a server's groups that the principal may
// see.
func (a Access) GroupsOf(server string) []string { return a.groups[server] }

// Groups returns the groups the principal has a grant in, without the grants.
func (a Access) Groups() []Group { return a.visible }

// ManagedGroups returns the IDs of the groups the principal has Manager in.
func (a Access) ManagedGroups() []string { return a.manages }

// computeAccess works out p's access from the groups.
func computeAccess(p Principal, groups []Group) Access {
	a := Access{admin: p.Admin(), roles: map[string]Role{}, groups: map[string][]string{}, visible: []Group{}}
	if !p.Role.Valid() {
		return Access{roles: a.roles, groups: a.groups, visible: a.visible} // an unknown role has no grants
	}
	for _, g := range groups {
		role := None
		for _, gr := range g.Grants {
			if p.holds(gr.Subject) && gr.Role.grantable() && gr.Role.rank() > role.rank() {
				role = gr.Role
			}
		}
		if role == None && !a.admin {
			continue
		}
		if role == Manager {
			a.manages = append(a.manages, g.ID)
		}
		a.visible = append(a.visible, Group{ID: g.ID, Name: g.Name, Servers: g.Servers, Grants: []Grant{}})
		for _, srv := range g.Servers {
			a.groups[srv] = append(a.groups[srv], g.Name)
			if role.rank() > a.roles[srv].rank() {
				a.roles[srv] = role
			}
		}
	}
	return a
}

// Errors from changing groups.
var (
	ErrNoGroup     = errors.New("there's no server group with that ID")
	ErrGroupExists = errors.New("there's already a server group with that name")
)

var validServer = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Store keeps the groups in the encrypted secrets store, so hlm's
// configuration backups include them.
type Store struct {
	secrets *secrets.Store
	log     *slog.Logger
	mu      sync.Mutex // serializes changes
}

// NewStore creates a store of groups.
func NewStore(s *secrets.Store, log *slog.Logger) *Store {
	return &Store{secrets: s, log: log}
}

func (s *Store) load() ([]Group, error) {
	var list []Group
	_, err := s.secrets.Get(secretName, &list)
	return list, err
}

func (s *Store) save(list []Group) error {
	if list == nil {
		list = []Group{}
	}
	return s.secrets.Set(secretName, list)
}

// Groups lists every group, sorted by name.
func (s *Store) Groups() ([]Group, error) {
	list, err := s.load()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b Group) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if list == nil {
		list = []Group{}
	}
	return list, nil
}

// Access returns what p may do now. If the groups can't be read, p has only
// what their global role gives.
func (s *Store) Access(p Principal) Access {
	list, err := s.Groups()
	if err != nil {
		s.log.Error("read server groups", "err", err)
		list = nil
	}
	return computeAccess(p, list)
}

// tidy checks a group and puts it in its stored form.
func tidy(g Group) (Group, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || len(g.Name) > 64 {
		return g, errors.New("give the server group a name of up to 64 characters")
	}
	servers := make([]string, 0, len(g.Servers))
	for _, srv := range g.Servers {
		if !validServer.MatchString(srv) {
			return g, fmt.Errorf("%q isn't a server name", srv)
		}
		if !slices.Contains(servers, srv) {
			servers = append(servers, srv)
		}
	}
	g.Servers = servers
	grants := make([]Grant, 0, len(g.Grants))
	for _, gr := range g.Grants {
		gr.Subject.Name = strings.TrimSpace(gr.Subject.Name)
		switch gr.Subject.Kind {
		case SubjectLocal, SubjectOIDCUser, SubjectOIDCGroup, SubjectKey:
		default:
			return g, fmt.Errorf("unknown kind of subject %q", gr.Subject.Kind)
		}
		if gr.Subject.Name == "" {
			return g, errors.New("say who the access is for")
		}
		if !gr.Role.grantable() {
			return g, fmt.Errorf("the role in a server group is viewer, operator or manager, not %q", gr.Role)
		}
		if slices.ContainsFunc(grants, func(o Grant) bool { return o.Subject.same(gr.Subject) }) {
			return g, fmt.Errorf("%s has more than one role in this group", gr.Subject.Name)
		}
		grants = append(grants, gr)
	}
	g.Grants = grants
	return g, nil
}

func nameTaken(list []Group, g Group) bool {
	return slices.ContainsFunc(list, func(o Group) bool {
		return o.ID != g.ID && strings.EqualFold(o.Name, g.Name)
	})
}

// AddServer puts a server in a group.
func (s *Store) AddServer(id, server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(g Group) bool { return g.ID == id })
	if i < 0 {
		return ErrNoGroup
	}
	if slices.Contains(list[i].Servers, server) {
		return nil
	}
	g := list[i]
	g.Servers = append(slices.Clone(g.Servers), server)
	if list[i], err = tidy(g); err != nil {
		return err
	}
	return s.save(list)
}

// Create adds a group.
func (s *Store) Create(g Group) (Group, error) {
	g, err := tidy(g)
	if err != nil {
		return Group{}, err
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return Group{}, err
	}
	g.ID = hex.EncodeToString(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Group{}, err
	}
	if nameTaken(list, g) {
		return Group{}, ErrGroupExists
	}
	if err := s.save(append(list, g)); err != nil {
		return Group{}, err
	}
	s.log.Info("created server group", "group", g.Name)
	return g, nil
}

// Update replaces a group's name, servers and grants.
func (s *Store) Update(id string, g Group) (Group, error) {
	g, err := tidy(g)
	if err != nil {
		return Group{}, err
	}
	g.ID = id
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Group{}, err
	}
	i := slices.IndexFunc(list, func(o Group) bool { return o.ID == id })
	if i < 0 {
		return Group{}, ErrNoGroup
	}
	if nameTaken(list, g) {
		return Group{}, ErrGroupExists
	}
	list[i] = g
	if err := s.save(list); err != nil {
		return Group{}, err
	}
	s.log.Info("changed server group", "group", g.Name)
	return g, nil
}

// Delete removes a group.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(o Group) bool { return o.ID == id })
	if i < 0 {
		return ErrNoGroup
	}
	name := list[i].Name
	if err := s.save(slices.Delete(list, i, i+1)); err != nil {
		return err
	}
	s.log.Info("removed server group", "group", name)
	return nil
}

// SetServerGroups makes a server a member of exactly the groups in ids.
func (s *Store) SetServerGroups(server string, ids []string) error {
	if !validServer.MatchString(server) {
		return fmt.Errorf("%q isn't a server name", server)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(list, func(g Group) bool { return g.ID == id }) {
			return ErrNoGroup
		}
	}
	for i := range list {
		list[i].Servers = slices.DeleteFunc(list[i].Servers, func(n string) bool { return n == server })
		if slices.Contains(ids, list[i].ID) {
			list[i].Servers = append(list[i].Servers, server)
		}
	}
	return s.save(list)
}

// ForgetServer takes a deleted server out of its groups, so a new server
// with the same name doesn't get the old one's access.
func (s *Store) ForgetServer(server string) error {
	return s.SetServerGroups(server, nil)
}

// ForgetSubject removes a deleted account's or API key's grants, so a new
// one with the same name doesn't get them.
func (s *Store) ForgetSubject(subject Subject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Grants = slices.DeleteFunc(list[i].Grants, func(g Grant) bool { return g.Subject.same(subject) })
	}
	return s.save(list)
}
