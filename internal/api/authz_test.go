package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// expected is who may use each endpoint that isn't admin-only. It's written
// out here rather than read from routes(), so that a mistake in the table
// fails a test.
var expected = map[string]string{
	"GET /auth/login":       "public",
	"GET /auth/callback":    "public",
	"GET /api/auth/status":  "public",
	"POST /api/auth/login":  "public",
	"POST /api/auth/logout": "public",

	"POST /api/auth/ssh/challenge": "public",
	"POST /api/auth/ssh/login":     "public",

	"GET /api/status":                                    "signedIn",
	"GET /api/system":                                    "signedIn",
	"GET /api/catalog/categories":                        "signedIn",
	"GET /api/catalog/apps":                              "signedIn",
	"GET /api/catalog/apps/{id}":                         "signedIn",
	"GET /api/jobs":                                      "signedIn",
	"GET /api/updates":                                   "signedIn",
	"POST /api/updates/apply":                            "signedIn",
	"GET /api/servers":                                   "signedIn",
	"GET /api/stats":                                     "signedIn",
	"GET /api/groups":                                    "signedIn",
	"PUT /api/auth/accounts/{username}/password":         "signedIn",
	"GET /api/auth/accounts/{username}/ssh-keys":         "signedIn",
	"POST /api/auth/accounts/{username}/ssh-keys":        "signedIn",
	"DELETE /api/auth/accounts/{username}/ssh-keys/{id}": "signedIn",
	"POST /api/auth/confirm":                             "signedIn",
	"POST /api/auth/download-ticket":                     "signedIn",
	"PUT /api/auth/accounts/{username}/ssh-keys/{id}":    "signedIn",

	// Changes that could give lasting access take a confirmed browser
	// session, so even an admin's API key or token is refused.
	"PUT /api/auth/config":                   "confirmed",
	"PUT /api/system/https":                  "confirmed",
	"POST /api/notifications/targets":        "confirmed",
	"PUT /api/notifications/targets/{id}":    "confirmed",
	"POST /api/auth/keys":                    "confirmed",
	"PUT /api/auth/keys/{id}":                "confirmed",
	"PUT /api/auth/oidc":                     "confirmed",
	"DELETE /api/auth/oidc":                  "confirmed",
	"POST /api/groups":                       "confirmed",
	"PUT /api/groups/{id}":                   "confirmed",
	"PUT /api/servers/{id}/groups":           "confirmed",
	"PUT /api/auth/accounts/{username}/role": "confirmed",
	"POST /api/servers/check-ports":          "signedIn",
	"/api/":                                  "signedIn",

	"GET /api/servers/{id}":              "viewer",
	"GET /api/servers/{id}/stats":        "viewer",
	"GET /api/servers/{id}/ports":        "viewer",
	"GET /api/servers/{id}/ports/status": "viewer",
	"GET /api/servers/{id}/portforward":  "viewer",
	"GET /api/servers/{id}/backups":      "viewer",

	"POST /api/servers/{id}/{action}":         "operator",
	"POST /api/servers/{id}/update":           "operator",
	"GET /api/servers/{id}/logs":              "operator",
	"GET /api/servers/{id}/console":           "operator",
	"PUT /api/servers/{id}/console":           "manager",
	"POST /api/servers/{id}/console/test":     "manager",
	"POST /api/servers/{id}/console/command":  "operator",
	"GET /api/servers/{id}/tasks":             "viewer",
	"POST /api/servers/{id}/tasks":            "manager",
	"PUT /api/servers/{id}/tasks/{task}":      "manager",
	"DELETE /api/servers/{id}/tasks/{task}":   "manager",
	"POST /api/servers/{id}/tasks/{task}/run": "operator",
	"GET /api/schedule/upcoming":              "signedIn",
	"GET /api/servers/{id}/update-policy":     "viewer",
	"GET /api/query":                          "signedIn",
	"GET /api/servers/{id}/query":             "viewer",
	"PUT /api/servers/{id}/query/settings":    "manager",
	"POST /api/servers/{id}/query/test":       "manager",
	"PUT /api/servers/{id}/update-policy":     "manager",
	"GET /api/updates/auto":                   "signedIn",
	"GET /api/system/timezone":                "signedIn",
	"GET /api/servers/{id}/config":            "operator",
	"GET /api/servers/{id}/files":             "operator",
	"GET /api/servers/{id}/files/content":     "operator",
	"GET /api/servers/{id}/files/search":      "operator",
	"GET /api/servers/{id}/config-files":      "operator",
	"PUT /api/servers/{id}/config-files":      "manager",
	"POST /api/servers/{id}/backups":          "operator",
	"GET /api/servers/{id}/backups/download":  "operator",

	"POST /api/servers": "deployer",

	"PUT /api/servers/{id}/config":           "manager",
	"PUT /api/servers/{id}/files/content":    "manager",
	"PUT /api/servers/{id}/autostart":        "manager",
	"PUT /api/servers/{id}/ports":            "manager",
	"POST /api/servers/{id}/portforward":     "manager",
	"DELETE /api/servers/{id}/portforward":   "manager",
	"PUT /api/servers/{id}/backups/plan":     "manager",
	"POST /api/servers/{id}/backups/restore": "manager",
	"DELETE /api/servers/{id}/backups":       "manager",
}

// fakeDocker answers enough of Docker's API for looking up containers. Like
// Docker, it finds them by name, ID or the start of an ID.
type fakeDocker struct {
	mu         sync.Mutex
	containers []docker.ContainerJSON
	created    []docker.CreateRequest
}

var apiVersion = regexp.MustCompile(`^/v[0-9.]+`)

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := apiVersion.ReplaceAllString(r.URL.Path, "")
	switch {
	case p == "/_ping":
		w.Header().Set("API-Version", "1.45")
	case r.Method == http.MethodPost && p == "/containers/create":
		var req docker.CreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.created = append(f.created, req)
		c := container(fmt.Sprintf("ffff%04d", len(f.created)), r.URL.Query().Get("name"), true, "")
		f.containers = slices.DeleteFunc(f.containers, func(o docker.ContainerJSON) bool { return o.Name == c.Name })
		c.Config.Labels = req.Labels
		c.Config.Image = req.Image
		c.HostConfig.Binds = req.HostConfig.Binds
		c.HostConfig.RestartPolicy = req.HostConfig.RestartPolicy
		f.containers = append(f.containers, c)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"Id": c.ID})
	case p == "/images/create":
		w.Write([]byte(`{"status":"Status: Image is up to date"}` + "\n"))
	case p == "/networks":
		w.Write([]byte("[]"))
	case p == "/containers/json":
		list := []docker.ContainerSummary{}
		for _, c := range f.containers {
			if r.URL.Query().Get("filters") == "" || c.Config.Labels[deploy.LabelManaged] == "true" {
				list = append(list, docker.ContainerSummary{ID: c.ID, Names: []string{c.Name}, Labels: c.Config.Labels, State: c.State.Status, Mounts: c.Mounts})
			}
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		for _, c := range f.containers {
			if c.Name == "/"+id || strings.HasPrefix(c.ID, id) {
				json.NewEncoder(w).Encode(c)
				return
			}
		}
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	case p == "/images/json" || strings.HasPrefix(p, "/images/"):
		w.Write([]byte("{}"))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func container(id, name string, managed bool, request string) docker.ContainerJSON {
	var c docker.ContainerJSON
	c.ID, c.Name = id, "/"+name
	c.Config.Labels = map[string]string{deploy.LabelTemplate: "t1"}
	if managed {
		c.Config.Labels[deploy.LabelManaged] = "true"
	}
	if request != "" {
		c.Config.Labels[deploy.LabelRequest] = request
	}
	c.HostConfig.RestartPolicy.Name = "no"
	return c
}

// env is an API with three servers and a key for each kind of caller, all
// sent as bearer tokens: anonymous callers are refused because HLM_TOKEN is
// set.
type env struct {
	opts   Options
	docker *fakeDocker
	groups *rbac.Store
	keys   map[string]string
	game   rbac.Group
}

const adminToken = "the-token"

func setup(t *testing.T) *env {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	store, err := secrets.Open(t.TempDir(), "test key")
	if err != nil {
		t.Fatal(err)
	}
	fd := &fakeDocker{containers: []docker.ContainerJSON{
		container("aaaa1111", "valheim", true, `{"templateId":"t1","name":"valheim","values":{"pw":"s3cret","data":"/srv/valheim"}}`),
		container("bbbb2222", "plex", true, `{"templateId":"t1","name":"plex","values":{"pw":"other-s3cret"}}`),
		container("cccc3333", "postgres", false, ""),
	}}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.New("http://127.0.0.1:0/feed", t.TempDir(), log)
	if err := cat.UseCustomStore(filepath.Join(t.TempDir(), "templates.json")); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddCustom(&catalog.App{ID: "t1", Name: "Game", Repository: "game:latest", Network: "bridge", Config: []catalog.Config{
		{ID: "pw", Name: "Password", Type: catalog.TypeVariable, Target: "PW"},
		{ID: "data", Name: "Data", Type: catalog.TypePath, Target: "/data", Value: "/srv/valheim"},
	}}); err != nil {
		t.Fatal(err)
	}
	e := &env{docker: fd, groups: rbac.NewStore(store, log), keys: map[string]string{"admin": adminToken}}
	e.opts = Options{
		Auth: auth.New(store, false, log), Groups: e.groups, Manager: manager.New(dc, cat, "/srv/appdata", log),
		Docker: dc, Catalog: cat, Token: adminToken, Log: log,
	}
	var grants []rbac.Grant
	for _, name := range []string{"member", "viewer", "operator", "manager"} {
		key, info, err := e.opts.Auth.CreateAPIKey(name, 0, rbac.Member, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.keys[name] = key
		if name != "member" {
			grants = append(grants, rbac.Grant{Subject: rbac.Subject{Kind: rbac.SubjectKey, Name: info.ID}, Role: rbac.Role(name)})
		}
	}
	key, _, err := e.opts.Auth.CreateAPIKey("deployer", 0, rbac.Deployer, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.keys["deployer"] = key
	e.game, err = e.groups.Create(rbac.Group{Name: "games", Servers: []string{"valheim"}, Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func call(h http.Handler, who, method, path, body string, keys map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie, ok := strings.CutPrefix(keys[who], "cookie:"); ok {
		r.Header.Set("Cookie", cookie)
		r.Header.Set("X-HLM-Request", "1")
	} else if k := keys[who]; k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// stubbed is the API with every handler replaced, so that what's tested is
// only whether a request gets to it.
func (e *env) stubbed() http.Handler {
	s := &server{e.opts}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, s.authorize(rt.policy, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))
	}
	return s.auth(mux)
}

var pathVar = regexp.MustCompile(`\{\w+\}`)

// want is the status a caller should get from an endpoint.
func want(policy, who string) int {
	reached := http.StatusTeapot
	rank := map[string]int{"anonymous": 0, "member": 1, "deployer": 1, "viewer": 2, "operator": 3, "manager": 4, "admin": 5}
	switch policy {
	case "public":
		return reached
	case "signedIn":
		if who == "anonymous" {
			return http.StatusUnauthorized
		}
		return reached
	case "confirmed":
		if who == "anonymous" {
			return http.StatusUnauthorized
		}
		return http.StatusForbidden // every caller here has a key or token
	}
	if who == "anonymous" {
		return http.StatusUnauthorized
	}
	if who == "admin" {
		return reached
	}
	if policy == "admin" {
		return http.StatusForbidden
	}
	if policy == "deployer" {
		if who == "deployer" {
			return reached
		}
		return http.StatusForbidden
	}
	if need, ok := rank[policy]; ok {
		switch {
		case who == "member", who == "deployer":
			return http.StatusNotFound // can't see the server at all
		case rank[who] < need:
			return http.StatusForbidden
		}
		return reached
	}
	return http.StatusForbidden
}

func TestEveryRoute(t *testing.T) {
	e := setup(t)
	h := e.stubbed()
	seen := map[string]bool{}
	for _, rt := range (&server{e.opts}).routes() {
		seen[rt.pattern] = true
		method, target, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			method, target = "GET", rt.pattern+"nothing"
		}
		// valheim is the only server with grants.
		target = strings.NewReplacer("{id}", "valheim", "{action}", "start").Replace(target)
		target = pathVar.ReplaceAllString(target, "x")
		policy := expected[rt.pattern]
		if policy == "" {
			policy = "admin"
		}
		for _, who := range []string{"anonymous", "member", "deployer", "viewer", "operator", "manager", "admin"} {
			if got := call(h, who, method, target, "{}", e.keys).Code; got != want(policy, who) {
				t.Errorf("%s as %s = %d, want %d (%s)", rt.pattern, who, got, want(policy, who), policy)
			}
		}
	}
	for pattern := range expected {
		if !seen[pattern] {
			t.Errorf("%s is expected but isn't a route", pattern)
		}
	}
}

func TestServerLookupCantBeBypassed(t *testing.T) {
	e := setup(t)
	h := e.stubbed()
	const reached = http.StatusTeapot
	for _, try := range []struct {
		who, id string
		want    int
	}{
		{"operator", "valheim", reached},
		{"operator", "aaaa1111", reached},
		{"operator", "aaaa", reached},                 // part of the ID
		{"operator", "plex", http.StatusNotFound},     // another group's server
		{"operator", "bbbb", http.StatusNotFound},     // by part of its ID
		{"operator", "postgres", http.StatusNotFound}, // not managed by hlm
		{"operator", "nothing", http.StatusNotFound},
		{"member", "valheim", http.StatusNotFound},
	} {
		if got := call(h, try.who, "GET", "/api/servers/"+try.id+"/logs", "", e.keys).Code; got != try.want {
			t.Errorf("%s reading logs of %s = %d, want %d", try.who, try.id, got, try.want)
		}
	}
	// A hidden server and a missing one look the same.
	hidden := call(h, "member", "GET", "/api/servers/plex", "", e.keys).Body.String()
	missing := call(h, "member", "GET", "/api/servers/nothing", "", e.keys).Body.String()
	unmanaged := call(h, "member", "GET", "/api/servers/postgres", "", e.keys).Body.String()
	if hidden != missing || unmanaged != missing {
		t.Errorf("hidden %q, unmanaged %q, missing %q", hidden, unmanaged, missing)
	}

	// Recreating a server gives it a new ID; its access stays with the name.
	e.docker.mu.Lock()
	e.docker.containers[0].ID = "dddd4444"
	e.docker.mu.Unlock()
	if got := call(h, "operator", "GET", "/api/servers/dddd4444/logs", "", e.keys).Code; got != reached {
		t.Errorf("after a recreate = %d", got)
	}
}

func TestListsShowOnlyWhatCallerCanSee(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	list := func(who string) []manager.Server {
		t.Helper()
		w := call(h, who, "GET", "/api/servers", "", e.keys)
		var out []manager.Server
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("%s: %d %s", who, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "other-s3cret") && who != "admin" {
			t.Errorf("%s sees another server's settings: %s", who, w.Body)
		}
		return out
	}
	if got := list("admin"); len(got) != 2 || got[0].Access != "admin" {
		t.Errorf("admin = %+v", got)
	}
	if got := list("member"); len(got) != 0 {
		t.Errorf("member = %+v", got)
	}
	viewer := list("viewer")
	if len(viewer) != 1 || viewer[0].Name != "valheim" || viewer[0].Access != "viewer" || viewer[0].Request != nil {
		t.Errorf("viewer = %+v", viewer)
	}
	if len(viewer[0].Groups) != 1 || viewer[0].Groups[0] != "games" {
		t.Errorf("viewer's groups = %v", viewer[0].Groups)
	}
	if got := list("operator"); len(got) != 1 || got[0].Request == nil {
		t.Errorf("operator = %+v", got)
	}

	// One server, too: its settings can hold passwords.
	if w := call(h, "viewer", "GET", "/api/servers/valheim", "", e.keys); w.Code != 200 || strings.Contains(w.Body.String(), "s3cret") {
		t.Errorf("viewer's server = %d %s", w.Code, w.Body)
	}
	if w := call(h, "member", "GET", "/api/groups", "", e.keys); w.Code != 200 || w.Body.String() != "[]\n" {
		t.Errorf("member's groups = %d %s", w.Code, w.Body)
	}
	w := call(h, "viewer", "GET", "/api/groups", "", e.keys)
	if !strings.Contains(w.Body.String(), `"games"`) || strings.Contains(w.Body.String(), `"subject"`) {
		t.Errorf("viewer's groups = %s", w.Body)
	}
	if w := call(h, "admin", "GET", "/api/groups", "", e.keys); !strings.Contains(w.Body.String(), `"subject"`) {
		t.Errorf("admin's groups = %s", w.Body)
	}
	if w := call(h, "viewer", "GET", "/api/status", "", e.keys); strings.Contains(w.Body.String(), "appdataRoot") {
		t.Errorf("viewer's status = %s", w.Body)
	}
}

func TestReconfigureIsRestricted(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	put := func(who, body string) *httptest.ResponseRecorder {
		return call(h, who, "PUT", "/api/servers/valheim/config", body, e.keys)
	}
	for name, body := range map[string]string{
		"host path":  `{"values":{"pw":"x","data":"/"}}`,
		"privileged": `{"values":{"data":"/srv/valheim"},"privileged":true}`,
		"network":    `{"values":{"data":"/srv/valheim"},"network":"host"}`,
		"template":   `{"templateId":"t2","values":{"data":"/srv/valheim"}}`,
		"extra":      `{"values":{"data":"/srv/valheim"},"extra":[{"type":"Path","target":"/h","value":"/"}]}`,
	} {
		if w := put("manager", body); w.Code != http.StatusForbidden {
			t.Errorf("manager changing %s = %d %s", name, w.Code, w.Body)
		}
	}
	// Allowed changes get as far as the stream; admins aren't restricted.
	if w := put("manager", `{"values":{"pw":"new","data":"/srv/valheim"}}`); w.Code != http.StatusOK {
		t.Errorf("manager changing a variable = %d %s", w.Code, w.Body)
	}
	if w := put("admin", `{"values":{"data":"/"},"privileged":true}`); w.Code != http.StatusOK {
		t.Errorf("admin = %d %s", w.Code, w.Body)
	}
	if w := put("operator", `{"values":{"pw":"new","data":"/srv/valheim"}}`); w.Code != http.StatusForbidden {
		t.Errorf("operator = %d %s", w.Code, w.Body)
	}
}

func TestGroupEndpoints(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	e.browser(t, h, "root", rbac.Admin, true)
	e.keys["admin"] = e.keys["root"]
	if w := call(h, "admin", "POST", "/api/groups", `{"name":"typo","servers":["vallheim"]}`, e.keys); w.Code != http.StatusBadRequest {
		t.Errorf("unknown server = %d %s", w.Code, w.Body)
	}
	if w := call(h, "admin", "POST", "/api/groups", `{"name":"prefix","servers":["bbbb"]}`, e.keys); w.Code != http.StatusBadRequest {
		t.Errorf("ID instead of name = %d %s", w.Code, w.Body)
	}
	if w := call(h, "admin", "POST", "/api/groups", `{"name":"GAMES"}`, e.keys); w.Code != http.StatusConflict {
		t.Errorf("taken name = %d", w.Code)
	}
	if w := call(h, "admin", "PUT", "/api/servers/plex/groups", `{"groups":["`+e.game.ID+`"]}`, e.keys); w.Code != 200 {
		t.Fatalf("assign = %d %s", w.Code, w.Body)
	}
	if !e.groups.Access(rbac.Principal{Role: rbac.Admin}).Admin() {
		t.Fatal("admin isn't")
	}
	if w := call(h, "viewer", "GET", "/api/servers/plex", "", e.keys); w.Code != 200 {
		t.Errorf("viewer after assigning = %d", w.Code)
	}
	if w := call(h, "admin", "PUT", "/api/servers/plex/groups", `{"groups":[]}`, e.keys); w.Code != 200 {
		t.Fatalf("unassign = %d", w.Code)
	}
	if w := call(h, "viewer", "GET", "/api/servers/plex", "", e.keys); w.Code != http.StatusNotFound {
		t.Errorf("viewer after unassigning = %d", w.Code)
	}
	// A deleted server's name doesn't keep its access.
	if err := e.groups.ForgetServer("valheim"); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "viewer", "GET", "/api/servers/valheim", "", e.keys); w.Code != http.StatusNotFound {
		t.Errorf("after forgetting = %d", w.Code)
	}
}

// A request that never went through auth has no caller, and gets nothing.
func TestAuthorizeWithoutPrincipal(t *testing.T) {
	e := setup(t)
	s := &server{e.opts}
	for _, pol := range []policy{signedIn, admin, serverRole(rbac.Viewer), {}} {
		w := httptest.NewRecorder()
		s.authorize(pol, func(http.ResponseWriter, *http.Request) { t.Error("reached the handler") })(w, httptest.NewRequest("GET", "/api/x", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%+v = %d", pol, w.Code)
		}
	}
}

// deployerEnv adds a key that's a deployer and a manager of the group dev.
func deployerEnv(t *testing.T) (*env, http.Handler, rbac.Group) {
	t.Helper()
	e := setup(t)
	key, info, err := e.opts.Auth.CreateAPIKey("ci", 0, rbac.Deployer, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.keys["ci"] = key
	dev, err := e.groups.Create(rbac.Group{Name: "dev", Grants: []rbac.Grant{
		{Subject: rbac.Subject{Kind: rbac.SubjectKey, Name: info.ID}, Role: rbac.Manager},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e, Handler(e.opts), dev
}

func TestDeployer(t *testing.T) {
	e, h, dev := deployerEnv(t)
	if got := call(h, "deployer", "POST", "/api/servers", `{"templateId":"t1","name":"a"}`, e.keys); got.Code != http.StatusForbidden {
		t.Errorf("without a group = %d", got.Code)
	}
	if got := call(h, "ci", "POST", "/api/servers", `{"templateId":"t1","name":"a","group":"`+e.game.ID+`"}`, e.keys); got.Code != http.StatusForbidden {
		t.Errorf("in someone else's group = %d", got.Code)
	}
	body := call(h, "ci", "POST", "/api/servers", `{"templateId":"t1","name":"new1","values":{"data":"/","pw":"x"},"privileged":false}`, e.keys).Body.String()
	if strings.Contains(body, `"error"`) {
		t.Fatalf("install: %s", body)
	}
	req := e.docker.created[0]
	if req.Labels[deploy.LabelConfined] != "true" {
		t.Error("not confined")
	}
	if want := []string{"/srv/appdata/new1/data:/data:rw"}; !slices.Equal(req.HostConfig.Binds, want) {
		t.Errorf("binds = %v", req.HostConfig.Binds)
	}
	groups, _ := e.groups.Groups()
	i := slices.IndexFunc(groups, func(g rbac.Group) bool { return g.ID == dev.ID })
	if !slices.Contains(groups[i].Servers, "new1") {
		t.Error("server isn't in the group")
	}
	if got := call(h, "ci", "GET", "/api/servers/new1", "", e.keys).Code; got != http.StatusOK {
		t.Errorf("reading its own server = %d", got)
	}
	if got := call(h, "ci", "GET", "/api/servers/valheim", "", e.keys).Code; got != http.StatusNotFound {
		t.Errorf("reading another server = %d", got)
	}

	// What's refused creates nothing and joins no group.
	for _, bad := range []string{
		`{"templateId":"t1","name":"new2","privileged":true}`,
		`{"templateId":"t1","name":"new2","network":"host"}`,
		`{"templateId":"t1","name":"new2","extraParams":"-v /:/h"}`,
	} {
		out := call(h, "ci", "POST", "/api/servers", bad, e.keys).Body.String()
		if !strings.Contains(out, "not allowed without an admin") {
			t.Errorf("%s: %s", bad, out)
		}
	}
	if len(e.docker.created) != 1 {
		t.Errorf("created %d containers", len(e.docker.created))
	}
	// An admin isn't confined.
	call(h, "admin", "POST", "/api/servers", `{"templateId":"t1","name":"new3","privileged":true}`, e.keys)
	last := e.docker.created[len(e.docker.created)-1]
	if !last.HostConfig.Privileged || last.Labels[deploy.LabelConfined] != "" {
		t.Errorf("admin's server = %+v", last.HostConfig)
	}
}

func TestDeployerKeepsOldDataOut(t *testing.T) {
	e, h, _ := deployerEnv(t)
	var other docker.ContainerJSON
	other.ID, other.Name = "eeee5555", "/admin-made"
	other.Config.Labels = map[string]string{}
	other.Mounts = []docker.MountPoint{{Type: "bind", Source: "/srv/appdata/taken/data"}}
	e.docker.mu.Lock()
	e.docker.containers = append(e.docker.containers, other)
	e.docker.mu.Unlock()
	out := call(h, "ci", "POST", "/api/servers", `{"templateId":"t1","name":"taken"}`, e.keys).Body.String()
	if !strings.Contains(out, "already has data") {
		t.Errorf("reusing a folder: %s", out)
	}
}

// A template can change after a server is made, and updating runs it again.
func TestConfinedStaysConfined(t *testing.T) {
	e, h, _ := deployerEnv(t)
	if out := call(h, "ci", "POST", "/api/servers", `{"templateId":"t1","name":"new1"}`, e.keys).Body.String(); strings.Contains(out, `"error"`) {
		t.Fatal(out)
	}
	if err := e.opts.Catalog.AddCustom(&catalog.App{ID: "t1", Name: "Game", Repository: "game:latest", Network: "bridge", Privileged: true, Config: []catalog.Config{
		{ID: "pw", Name: "Password", Type: catalog.TypeVariable, Target: "PW"},
		{ID: "data", Name: "Data", Type: catalog.TypePath, Target: "/data", Value: "/srv/valheim"},
	}}); err != nil {
		t.Fatal(err)
	}
	if out := call(h, "ci", "POST", "/api/servers/new1/update", "", e.keys).Body.String(); !strings.Contains(out, "has to stay confined") {
		t.Errorf("update: %s", out)
	}
	if len(e.docker.created) != 1 {
		t.Fatalf("recreated it: %d", len(e.docker.created))
	}
	// An admin can choose otherwise, and hlm says so.
	out := call(h, "admin", "PUT", "/api/servers/new1/config", `{"templateId":"t1","name":"new1","values":{"data":"/srv/appdata/new1/data"}}`, e.keys).Body.String()
	if !strings.Contains(out, "no longer confined") {
		t.Errorf("reconfigure: %s", out)
	}
	last := e.docker.created[len(e.docker.created)-1]
	if len(e.docker.created) != 2 || last.Labels[deploy.LabelConfined] != "" || !last.HostConfig.Privileged {
		t.Errorf("after the admin's change: %+v", last.Labels)
	}
}

func TestSSHKeysBelongToTheirAccount(t *testing.T) {
	e := setup(t)
	h := Handler(e.opts)
	if _, err := e.opts.Auth.CreateAccount("first", "correct horse battery", rbac.Admin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.opts.Auth.CreateAccount("bob", "correct horse battery", rbac.Member); err != nil {
		t.Fatal(err)
	}
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJHQ3BE1VvCjqxVhQ0Y3MPFqWqBPJ5Bq2wUqzBLHH4Yq test"
	body := `{"publicKey":"` + pub + `"}`
	for _, who := range []string{"member", "manager", "deployer"} { // API keys aren't accounts
		for method, path := range map[string]string{"GET": "/api/auth/accounts/bob/ssh-keys", "POST": "/api/auth/accounts/bob/ssh-keys", "DELETE": "/api/auth/accounts/bob/ssh-keys/x"} {
			if got := call(h, who, method, path, body, e.keys).Code; got != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d", method, path, who, got)
			}
		}
	}
	// An admin's token can't add a way into an account; an admin at a
	// browser who's confirmed it's them can.
	if got := call(h, "admin", "POST", "/api/auth/accounts/bob/ssh-keys", body, e.keys); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "interactive_required") {
		t.Fatalf("as an admin token = %d: %s", got.Code, got.Body)
	}
	e.browser(t, h, "root", rbac.Admin, true)
	if got := call(h, "root", "POST", "/api/auth/accounts/bob/ssh-keys", body, e.keys); got.Code != http.StatusCreated {
		t.Fatalf("as an admin = %d: %s", got.Code, got.Body)
	}
	if got := call(h, "root", "POST", "/api/auth/accounts/nobody/ssh-keys", body, e.keys).Code; got != http.StatusNotFound {
		t.Errorf("no such account = %d", got)
	}
	if got := call(h, "anonymous", "POST", "/api/auth/ssh/challenge", `{"username":"nobody"}`, e.keys); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "hlm-login") {
		t.Errorf("challenge = %d: %s", got.Code, got.Body)
	}
	if got := call(h, "anonymous", "POST", "/api/auth/ssh/login", `{"challenge":"x","signature":"y"}`, e.keys).Code; got != http.StatusUnauthorized {
		t.Errorf("bad login = %d", got)
	}
}

// browser signs in as a new local account with role, at a browser, so that
// call(h, user, ...) sends its session cookie; confirm also confirms it.
func (e *env) browser(t *testing.T, h http.Handler, user string, role rbac.Role, confirm bool) {
	t.Helper()
	if _, err := e.opts.Auth.CreateAccount(user, "correct horse battery", role); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"`+user+`","password":"correct horse battery"}`))
	r.Header.Set("X-HLM-Request", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("sign in as %s = %d %s", user, w.Code, w.Body)
	}
	e.keys[user] = "cookie:" + w.Result().Cookies()[0].Name + "=" + w.Result().Cookies()[0].Value
	if !confirm {
		return
	}
	w = call(h, user, "POST", "/api/auth/confirm", `{"password":"correct horse battery"}`, e.keys)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm as %s = %d %s", user, w.Code, w.Body)
	}
	e.keys[user] = "cookie:" + w.Result().Cookies()[0].Name + "=" + w.Result().Cookies()[0].Value
}
