// Package api serves the JSON HTTP API used by the web frontend.
package api

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/autoupdate"
	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/cftunnel"
	"github.com/danotchuuy/home-lab-manager/internal/cleanup"
	"github.com/danotchuuy/home-lab-manager/internal/cloudflare"
	"github.com/danotchuuy/home-lab-manager/internal/configfiles"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/discord"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/forwarding"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/health"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/hosts"
	"github.com/danotchuuy/home-lab-manager/internal/idle"
	"github.com/danotchuuy/home-lab-manager/internal/linode"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/metrics"
	"github.com/danotchuuy/home-lab-manager/internal/metricstore"
	"github.com/danotchuuy/home-lab-manager/internal/mods"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/players"
	"github.com/danotchuuy/home-lab-manager/internal/prefs"
	"github.com/danotchuuy/home-lab-manager/internal/proxy"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/registry"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
	"github.com/danotchuuy/home-lab-manager/internal/selfupdate"
	"github.com/danotchuuy/home-lab-manager/internal/stack"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
	"github.com/danotchuuy/home-lab-manager/internal/tlscert"
	"github.com/danotchuuy/home-lab-manager/internal/transfer"
	"github.com/danotchuuy/home-lab-manager/internal/unifi"
	"github.com/danotchuuy/home-lab-manager/web"
)

// Options configures the API server.
type Options struct {
	Docker     *docker.Client
	Catalog    *catalog.Catalog
	Manager    *manager.Manager
	Host       *hostmetrics.Collector
	Updater    *selfupdate.Updater
	Forwarding *forwarding.Service
	Proxy      *proxy.Service
	Tunnel     *cftunnel.Service
	Backup     *backup.Service
	Auth       *auth.Service
	Groups     *rbac.Store
	// HTTPS serves hlm with a Let's Encrypt certificate, if it's turned on.
	HTTPS *tlscert.Service
	// Notify sends alerts to notification targets.
	Notify *notify.Service
	// Console sends servers commands.
	Console *console.Service
	// Schedule runs servers' scheduled tasks.
	Schedule *schedule.Service
	// AutoUpdate applies updates automatically.
	AutoUpdate *autoupdate.Service
	// Query asks servers how they're doing, in their games' own protocols.
	Query *gamequery.Service
	// ConfigFiles keeps servers' pinned settings files.
	ConfigFiles *configfiles.Service
	// Audit records who changed what.
	Audit *audit.Log
	// Registries holds the logins for private image registries.
	Registries *registry.Store
	// History keeps a month of host and server resource use.
	History *metricstore.Store
	// Sampler has servers' latest samples, for /metrics.
	Sampler *metrics.Sampler
	// MetricsConfig is /metrics' settings.
	MetricsConfig *metrics.Config
	// Health keeps servers' health settings, and HealthMonitor restarts
	// those whose health keeps failing.
	Health        *health.Store
	HealthMonitor *health.Monitor
	// Transfer exports, imports and clones servers.
	Transfer *transfer.Service
	// Stacks runs Docker Compose stacks.
	Stacks *stack.Service
	// Hosts are the other Docker hosts servers can be on.
	Hosts *hosts.Registry
	// Agents are the connections of hosts' agents.
	Agents *agent.Hub
	// Cleanup keeps the disk cleanup settings.
	Cleanup *cleanup.Service
	// Prefs keeps servers' tags and users' favourites.
	Prefs *prefs.Store
	// StatusPage is the public status page.
	StatusPage *statuspage.Service
	// GameProfiles keeps your own game profiles.
	GameProfiles *gameprofile.Store
	// Idle stops servers nobody plays on and wakes them.
	Idle *idle.Service
	// Players kicks, bans and keeps servers' player lists.
	Players *players.Service
	// Mods installs servers' mods from Modrinth.
	Mods *mods.Service
	// Discord is the Discord bot.
	Discord *discord.Service
	// UpdateCheckEnv is HLM_UPDATE_CHECK, which overrides the setting for
	// how often updates are checked for, if set.
	UpdateCheckEnv string
	Version        string
	AppdataRoot    string
	// Token, when set, must be sent as a bearer token (or ?token= for
	// EventSource requests) on every API call, unless the caller is signed
	// in through Auth.
	Token string
	// WebDir, when set, is a directory of built frontend files to serve.
	// Otherwise the frontend embedded in release builds is served, if any.
	WebDir string
	Log    *slog.Logger
}

type server struct {
	Options
}

// Handler returns the HTTP handler for the API and, if configured, the
// frontend.
func Handler(opts Options) http.Handler {
	s := &server{opts}
	mux := http.NewServeMux()

	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, s.audited(rt.pattern, s.authorize(rt.policy, rt.handler)))
	}

	if s.WebDir != "" {
		mux.Handle("/", spaHandler(os.DirFS(s.WebDir)))
	} else if dist := web.Dist(); dist != nil {
		mux.Handle("/", spaHandler(dist))
	}
	return s.logRequests(s.statusHost(s.auth(mux)))
}

// csrfHeader must be sent with requests that change something when the
// caller is signed in with a session cookie. Browsers only send custom
// headers cross-site after a CORS preflight, which hlm never allows, so
// other sites can't make use of the cookie.
const csrfHeader = "X-HLM-Request"

// auth works out who an API request is from: the token (HLM_TOKEN), an API
// key or a signed-in session, or nobody, which is only allowed when sign-in
// isn't required and HLM_TOKEN isn't set. The web UI, signing in and out, and
// the sign-in status are always reachable. What the caller may do is decided
// per route; see authorize.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/status", "/api/auth/login", "/api/auth/logout", "/api/auth/ssh/challenge", "/api/auth/ssh/login",
			"/api/auth/login/2fa", "/api/auth/login/2fa/setup", "/api/auth/login/2fa/enable",
			"/api/auth/login/passkey/begin", "/api/auth/login/passkey", "/api/auth/login/2fa/passkey/begin",
			"/api/auth/login/2fa/passkey", "/api/auth/login/2fa/setup/passkey/begin", "/api/auth/login/2fa/setup/passkey":
			next.ServeHTTP(w, r)
			return
		}
		// The public status page (which answers 404 unless it's on), and
		// hosts' agents, which have their own credentials.
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/status-page" || strings.HasPrefix(r.URL.Path, "/api/status-page/") ||
			strings.HasPrefix(r.URL.Path, "/api/agent/") {
			next.ServeHTTP(w, r)
			return
		}
		p, session, ok := s.identify(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("sign in, or send a valid API key"))
			return
		}
		if session != nil && !safeMethod(r.Method) && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, errors.New("requests made with a session cookie need the "+csrfHeader+" header"))
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		if session != nil {
			ctx = context.WithValue(ctx, sessionKey{}, session)
		}
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		if !safeMethod(r.Method) {
			s.Log.Info("api request", "by", p.Name, "role", p.Role, "method", r.Method, "path", r.URL.Path, "status", rw.status, "remote", r.RemoteAddr)
		}
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// spaHandler serves static files from fsys, falling back to index.html so
// that client-side routes work on reload.
func spaHandler(fsys fs.FS) http.Handler {
	// For "add to home screen"; not every system's MIME table has it.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() {
			http.ServeFileFS(w, r, fsys, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}

// --- status ---

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	type dockerStatus struct {
		OK      bool   `json:"ok"`
		Host    string `json:"host"`
		Error   string `json:"error,omitempty"`
		Version string `json:"version,omitempty"`
		API     string `json:"apiVersion,omitempty"`
	}
	admin := s.access(r).Admin()
	ds := dockerStatus{}
	if admin {
		ds.Host = s.Docker.Host()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if v, err := s.Docker.Version(ctx); err != nil {
		ds.Error = err.Error()
	} else {
		ds.OK, ds.Version, ds.API = true, v.Version, v.APIVersion
	}
	var fetched *time.Time
	if t := s.Catalog.FetchedAt(); !t.IsZero() {
		fetched = &t
	}
	out := map[string]any{
		"docker": ds,
		"catalog": map[string]any{
			"apps":        s.Catalog.Count(),
			"fetchedAt":   fetched,
			"feedUpdated": s.Catalog.FeedUpdated(),
		},
	}
	if admin {
		out["appdataRoot"] = s.AppdataRoot
	}
	writeJSON(w, http.StatusOK, out)
}

// --- catalog ---

func (s *server) categories(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Catalog.Categories())
}

func (s *server) searchApps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 || limit > 200 {
		limit = 48
	}
	category := q.Get("category")
	if !q.Has("category") {
		category = "GameServers"
	}
	apps, total := s.Catalog.Search(catalog.Query{
		Text:     q.Get("q"),
		Category: category,
		Sort:     q.Get("sort"),
		Offset:   max(offset, 0),
		Limit:    limit,
	})
	items := make([]catalog.Summary, 0, len(apps))
	for _, a := range apps {
		items = append(items, a.Summary())
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "items": items})
}

// getApp returns a template, with its paths for the ?host= it's for.
func (s *server) getApp(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	app, ok := s.Manager.AppOn(r.PathValue("id"), host)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("app not found"))
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// importImage pulls an image and makes a template from its defaults,
// streaming the download. The final "done" event's message is the new
// template's ID.
func (s *server) importImage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "image", body.Image)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		app, err := s.Manager.ImportImage(ctx, body.Image, emit)
		if err != nil {
			return err
		}
		emit(manager.Event{Stage: "done", Message: app.ID})
		return nil
	})
}

func (s *server) deleteCustomApp(w http.ResponseWriter, r *http.Request) {
	if err := s.Catalog.DeleteCustom(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) refreshCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.Catalog.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": s.Catalog.Count()})
}

// --- networks ---

// Networks are per host (?host=, the local one by default).
func (s *server) listNetworks(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	networks, err := s.Manager.Networks(r.Context(), host)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, networks)
}

func (s *server) createNetwork(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	var req manager.NetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.Manager.CreateNetwork(r.Context(), host, req)
	if err != nil {
		var apiErr *docker.APIError
		if errors.As(err, &apiErr) {
			writeError(w, http.StatusBadRequest, errors.New(apiErr.Message))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (s *server) removeNetwork(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	if err := s.Manager.RemoveNetwork(r.Context(), host, r.PathValue("name")); err != nil {
		if errors.Is(err, manager.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hostInterfaces lists the Docker host's interfaces and subnets, to suggest
// settings for a LAN network.
func (s *server) hostInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces := s.Manager.HostInterfaces()
	if ifaces == nil {
		ifaces = []hostmetrics.HostInterface{}
	}
	writeJSON(w, http.StatusOK, ifaces)
}

// --- servers ---

// pollTimeout bounds the endpoints the web UI polls, so a Docker daemon
// that stops answering gives errors instead of requests that pile up.
const pollTimeout = 20 * time.Second

func (s *server) listServers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	servers, err := s.visibleServers(ctx, s.access(r), rbac.Viewer)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.decorate(r.Context(), servers))
}

func (s *server) getServer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	srv, err := s.Manager.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.presentOne(r, srv))
}

func (s *server) removeServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	if err := s.Manager.Remove(r.Context(), srv.ID); err != nil {
		writeManagerError(w, err)
		return
	}
	// Rules forwarding to a server that's gone would be holes to nowhere.
	if s.Forwarding.Configured() {
		if _, err := s.Forwarding.Remove(r.Context(), srv.Name); err != nil {
			s.Log.Warn("remove port forwards of deleted server", "server", srv.Name, "err", err)
		}
	}
	if err := s.Forwarding.ForgetServer(srv.Name); err != nil {
		s.Log.Warn("forget internet ports of deleted server", "server", srv.Name, "err", err)
	}
	// Its hostnames would only show an error page now.
	if err := s.Tunnel.ForgetServer(r.Context(), srv.Name); err != nil {
		s.Log.Warn("unpublish hostnames of deleted server", "server", srv.Name, "err", err)
	}
	// A new server with this name shouldn't get its access.
	if err := s.Groups.ForgetServer(srv.Name); err != nil {
		s.Log.Warn("take deleted server out of its groups", "server", srv.Name, "err", err)
	}
	if s.ConfigFiles != nil {
		if err := s.ConfigFiles.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's pinned files", "server", srv.Name, "err", err)
		}
	}
	if s.Query != nil {
		if err := s.Query.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's query settings", "server", srv.Name, "err", err)
		}
	}
	if s.Health != nil {
		if err := s.Health.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's health settings", "server", srv.Name, "err", err)
		}
	}
	if s.AutoUpdate != nil {
		if err := s.AutoUpdate.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's update policy", "server", srv.Name, "err", err)
		}
	}
	if s.Schedule != nil {
		if err := s.Schedule.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's tasks", "server", srv.Name, "err", err)
		}
	}
	if s.Console != nil {
		if err := s.Console.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's console settings", "server", srv.Name, "err", err)
		}
	}
	if s.Notify != nil {
		if err := s.Notify.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("take deleted server out of notification targets", "server", srv.Name, "err", err)
		}
	}
	if s.Players != nil {
		if err := s.Players.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's player records", "server", srv.Name, "err", err)
		}
	}
	if s.Mods != nil {
		s.Mods.ForgetServer(srv.Name)
	}
	if s.Idle != nil {
		if err := s.Idle.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's idle settings", "server", srv.Name, "err", err)
		}
	}
	if s.StatusPage != nil {
		if err := s.StatusPage.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("take deleted server off the status page", "server", srv.Name, "err", err)
		}
	}
	if s.Prefs != nil {
		if err := s.Prefs.ForgetServer(srv.Name); err != nil {
			s.Log.Warn("forget deleted server's tags and favourites", "server", srv.Name, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) serverAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "start":
		err = s.Manager.Start(r.Context(), id)
	case "stop":
		err = s.Manager.Stop(r.Context(), id)
	case "restart":
		err = s.Manager.Restart(r.Context(), id)
	default:
		writeError(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	if err != nil {
		writeManagerError(w, err)
		return
	}
	srv, err := s.Manager.Get(r.Context(), id)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.presentOne(r, srv))
}

func (s *server) installServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		deploy.Request
		// Group is the server group a deployer's server goes in.
		Group string `json:"group"`
		// Host is the Docker host it goes on; the local one if empty.
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req := body.Request
	if strings.Contains(req.Name, "@") {
		writeError(w, http.StatusBadRequest, errors.New("choose the host with \"host\", not in the name"))
		return
	}
	if body.Host != "" && body.Host != manager.LocalHost {
		if !s.Manager.KnownHost(body.Host) {
			writeError(w, http.StatusNotFound, errors.New("there is no Docker host "+body.Host))
			return
		}
		req.Name = manager.JoinID(body.Host, req.Name)
	}
	audit.SetServer(r.Context(), req.Name)
	audit.Add(r.Context(), "template", req.TemplateID)
	access := s.access(r)
	if access.Admin() {
		s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
			_, err := s.Manager.Install(ctx, req, emit)
			return err
		})
		return
	}
	group, err := deployGroup(access, body.Group)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		// Only registry logins an admin allowed for deployers are used.
		if _, err := s.Manager.InstallConfined(registry.Confined(ctx), req, emit); err != nil {
			return err
		}
		if err := s.Groups.AddServer(group, req.Name); err != nil {
			return fmt.Errorf("%s is running, but an admin has to add it to its group: %w", req.Name, err)
		}
		return nil
	})
}

// checkServerPorts reports the host ports a server's settings would use
// that are already in use, for the install and settings forms. Settings
// that can't be built yet (e.g. a half-typed port) give no conflicts and
// an error, which the form doesn't need to show: installing reports it.
func (s *server) checkServerPorts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Request deploy.Request `json:"request"`
		// Server is the server being reconfigured, whose own ports are fine.
		Server string `json:"server"`
		// Host is the host a new server goes on.
		Host string `json:"host"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Anyone but an admin can only be checking a server they manage.
	access := s.access(r)
	if !access.Admin() && !s.manages(r.Context(), access, body.Server) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	if !validServerName.MatchString(body.Request.Name) {
		body.Request.Name = "port-check" // only the ports matter here
	}
	if body.Server == "" && body.Host != "" {
		if !s.Manager.KnownHost(body.Host) {
			writeError(w, http.StatusNotFound, errors.New("there is no Docker host "+body.Host))
			return
		}
		body.Request.Name = manager.JoinID(body.Host, body.Request.Name)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	conflicts, err := s.Manager.PortConflicts(ctx, body.Request, body.Server)
	res := map[string]any{"conflicts": hideConflicts(access, conflicts)}
	if err != nil {
		res["conflicts"], res["error"] = []manager.PortConflict{}, err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

var validServerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func (s *server) updateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		_, err := s.Manager.Update(ctx, id, emit)
		return err
	})
}

func (s *server) getServerConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, req, err := s.Manager.Config(r.Context(), id)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	info, err := s.Manager.Inspect(r.Context(), id)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	// Operators can start and update a server but not change it, so they
	// needn't see what look like its secrets.
	if !s.access(r).Can(strings.TrimPrefix(info.Name, "/"), rbac.Manager) {
		app, req, hidden := deploy.HideSecrets(app, req)
		writeJSON(w, http.StatusOK, map[string]any{"app": app, "request": req, "hidden": hidden})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app, "request": req})
}

func (s *server) reconfigureServer(w http.ResponseWriter, r *http.Request) {
	var req deploy.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	admin := s.access(r).Admin()
	// Which settings change, by name; never their values.
	if _, old, err := s.Manager.Config(r.Context(), id); err == nil {
		audit.Add(r.Context(), "changed", strings.Join(deploy.ChangedSettings(old, req), ", "))
	}
	if !admin {
		app, old, err := s.Manager.Config(r.Context(), id)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		if changed := deploy.RestrictedChanges(app, old, req); len(changed) > 0 {
			writeError(w, http.StatusForbidden, errors.New("only an admin can change these settings: "+strings.Join(changed, ", ")))
			return
		}
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		if admin {
			_, err := s.Manager.ReconfigureAsAdmin(ctx, id, req, emit)
			return err
		}
		_, err := s.Manager.Reconfigure(ctx, id, req, emit)
		return err
	})
}

// --- UniFi port forwarding ---

func (s *server) unifiConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Forwarding.PublicConfig())
}

func decodeUnifi(w http.ResponseWriter, r *http.Request) (forwarding.Config, bool) {
	var cfg forwarding.Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return cfg, false
	}
	return cfg, true
}

// writeUnifiError reports an error from the controller. An untrusted
// certificate comes with its fingerprint, so the user can choose to trust it.
func writeUnifiError(w http.ResponseWriter, err error) {
	var certErr *unifi.CertError
	if errors.As(err, &certErr) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "certFingerprint": certErr.Fingerprint})
		return
	}
	if errors.Is(err, forwarding.ErrNotConfigured) {
		writeError(w, http.StatusConflict, err)
		return
	}
	var apiErr *unifi.APIError
	if errors.As(err, &apiErr) {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

// unifiWAN looks up the gateway's public address on a WAN (?wan=wan2; wan
// by default).
func (s *server) unifiWAN(w http.ResponseWriter, r *http.Request) {
	wan := r.URL.Query().Get("wan")
	if wan == "" {
		wan = "wan"
	}
	if wan != "wan" && wan != "wan2" {
		writeError(w, http.StatusBadRequest, errors.New("wan must be wan or wan2"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ip, err := s.Forwarding.WANAddress(ctx, wan)
	if err != nil {
		writeUnifiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"wan": wan, "address": ip})
}

func (s *server) testUnifi(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decodeUnifi(w, r)
	if !ok {
		return
	}
	info, err := s.Forwarding.Test(r.Context(), cfg)
	if err != nil {
		writeUnifiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) saveUnifi(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decodeUnifi(w, r)
	if !ok {
		return
	}
	pub, err := s.Forwarding.Save(r.Context(), cfg)
	if err != nil {
		writeUnifiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) deleteUnifi(w http.ResponseWriter, r *http.Request) {
	if err := s.Forwarding.Delete(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkIP asks UniFi whether a LAN address is free. With ?server=, that
// server's own container doesn't count as a conflict.
func (s *server) checkIP(w http.ResponseWriter, r *http.Request) {
	var ignore []string
	if name := r.URL.Query().Get("server"); name != "" {
		if info, err := s.Manager.Inspect(r.Context(), name); err == nil {
			ignore = append(ignore, deploy.StableMAC(strings.TrimPrefix(info.Name, "/")))
			for _, ep := range info.NetworkSettings.Networks {
				ignore = append(ignore, ep.MacAddress)
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	res, err := s.Forwarding.CheckIP(ctx, r.URL.Query().Get("ip"), ignore)
	if err != nil {
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// internetPort is one of a server's ports and the other servers that use
// the same internet port.
type internetPort struct {
	forwarding.Mapping
	Conflicts []string `json:"conflicts,omitempty"`
}

// serverPorts is everything the Port forwarding tab edits: the server's
// ports with their options, and which integrations are set up.
type serverPorts struct {
	Ports []internetPort `json:"ports"`
	UniFi bool           `json:"unifi"`
	Proxy struct {
		Configured bool `json:"configured"`
		// Relayed means the proxy relays this server; Applied that the
		// proxy's configuration is on the proxy server.
		Relayed bool `json:"relayed"`
		Applied bool `json:"applied"`
		// Route is how the proxy reaches this server, e.g. through the
		// WireGuard tunnel.
		Route *proxy.Route `json:"route,omitempty"`
	} `json:"proxy"`
	// Firewall means a Linode firewall is managed for the proxy.
	Firewall bool `json:"firewall"`
}

// exposed reports whether a port is reachable from the internet in any way
// hlm sets up.
func exposed(m forwarding.Mapping) bool { return m.Forward || m.Proxy }

// serverPortSettings lists a server's ports with their options and any
// other managed servers whose internet ports clash with them.
func (s *server) serverPortSettings(ctx context.Context, id string) (serverPorts, error) {
	var out serverPorts
	srv, err := s.Manager.Get(ctx, id)
	if err != nil {
		return out, err
	}
	info, err := s.Manager.Inspect(ctx, srv.ID)
	if err != nil {
		return out, err
	}
	ports, err := s.Forwarding.ServerPorts(info)
	if err != nil {
		return out, err
	}
	access := s.accessOf(ctx)
	used := map[forwarding.Port][]string{}
	if all, err := s.Manager.List(ctx); err == nil {
		for _, other := range all {
			if other.ID == srv.ID {
				continue
			}
			oi, err := s.Manager.Inspect(ctx, other.ID)
			if err != nil {
				continue
			}
			op, _ := s.Forwarding.ServerPorts(oi)
			for _, m := range op {
				if exposed(m) {
					k := forwarding.Port{Number: m.Port, Proto: m.Proto}
					used[k] = append(used[k], visibleName(access, other.Name))
				}
			}
		}
	}
	out.Ports = make([]internetPort, 0, len(ports))
	for _, m := range ports {
		p := internetPort{Mapping: m}
		if exposed(m) {
			p.Conflicts = used[forwarding.Port{Number: m.Port, Proto: m.Proto}]
		}
		out.Ports = append(out.Ports, p)
	}
	out.UniFi = s.Forwarding.Configured()
	if _, ok, _ := s.Proxy.Settings(); ok {
		out.Proxy.Configured = true
		out.Proxy.Relayed = s.Proxy.Relayed(srv.Name)
		if route, err := s.Proxy.RouteFor(ctx, srv.Name); err == nil {
			out.Proxy.Route = &route
		}
		if st, err := s.Proxy.State(); err == nil {
			out.Proxy.Applied = st.Applied != ""
		}
		out.Firewall = s.Proxy.FirewallChosen()
	}
	return out, nil
}

func (s *server) internetPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := s.serverPortSettings(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ports)
}

// setInternetPorts changes a server's port options:
// {"ports": {"2456/udp": {"port": 2466, "forward": true, "proxy": true,
// "firewall": true}}, "relay": true}. relay, if given, adds the server to
// the proxy's relays or removes it.
func (s *server) setInternetPorts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ports map[string]forwarding.PortSetting `json:"ports"`
		Relay *bool                             `json:"relay"`
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
	if _, err := s.Forwarding.SetOptions(r.Context(), srv.ID, body.Ports); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Relay != nil {
		if err := s.Proxy.SetRelayed(srv.Name, *body.Relay); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	ports, err := s.serverPortSettings(r.Context(), srv.ID)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ports)
}

// portSync is whether each of a server's ports is in place on the UniFi
// gateway, the proxy and its firewall. A part that isn't set up is null.
type portSync struct {
	UniFi         map[string]forwarding.SyncState `json:"unifi"`
	UniFiError    string                          `json:"unifiError,omitempty"`
	Proxy         map[string]forwarding.SyncState `json:"proxy"`
	ProxyError    string                          `json:"proxyError,omitempty"`
	Firewall      map[string]forwarding.SyncState `json:"firewall"`
	FirewallError string                          `json:"firewallError,omitempty"`
}

func (s *server) portSyncStatus(w http.ResponseWriter, r *http.Request) {
	srv, err := s.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var out portSync
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		st, err := s.Forwarding.PortStates(ctx, srv.ID)
		out.UniFi = st
		if err != nil {
			out.UniFiError = err.Error()
		}
	}()
	go func() {
		defer wg.Done()
		st, err := s.Proxy.PortStates(ctx, srv.ID)
		out.Proxy, out.Firewall, out.FirewallError = st.Proxy, st.Firewall, st.FirewallError
		if err != nil {
			out.ProxyError = err.Error()
		}
	}()
	wg.Wait()
	if !s.access(r).Admin() {
		hideSyncDetails(out.UniFi, out.Proxy, out.Firewall)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) portForwardStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Forwarding.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		writeForwardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hideRules(s.access(r), st))
}

// syncPortForward creates or updates the server's rules. Rules that couldn't
// be made are listed in "error" alongside the resulting status.
func (s *server) syncPortForward(w http.ResponseWriter, r *http.Request) {
	st, err := s.Forwarding.Sync(r.Context(), r.PathValue("id"))
	if err != nil && st.Configured {
		st.Error = err.Error()
		writeJSON(w, http.StatusOK, hideRules(s.access(r), st))
		return
	}
	if err != nil {
		writeForwardError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hideRules(s.access(r), st))
}

func (s *server) removePortForward(w http.ResponseWriter, r *http.Request) {
	srv, err := s.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	n, err := s.Forwarding.Remove(r.Context(), srv.Name)
	if err != nil {
		writeUnifiError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": n})
}

func writeForwardError(w http.ResponseWriter, err error) {
	if errors.Is(err, manager.ErrNotFound) || errors.Is(err, manager.ErrNotManaged) {
		writeManagerError(w, err)
		return
	}
	writeUnifiError(w, err)
}

// --- nginx proxy over SSH ---

func (s *server) proxyConfig(w http.ResponseWriter, r *http.Request) {
	st, ok, err := s.Proxy.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	key, err := s.Proxy.Key()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	state, err := s.Proxy.State()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	statusSites, _ := s.Proxy.StatusSites()
	writeJSON(w, http.StatusOK, map[string]any{"configured": ok, "settings": st, "key": key, "upstream": state, "statusSites": statusSites})
}

// checkProxyWAN looks up the home address from UniFi now, updating the
// proxy if it changed.
func (s *server) checkProxyWAN(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	state, err := s.Proxy.CheckWAN(ctx)
	// A failed lookup or update is recorded in the state, which is what the
	// page shows; anything else is an error.
	if err != nil && (errors.Is(err, proxy.ErrNotConfigured) || state.Error == "") {
		writeProxyError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func decodeProxySettings(w http.ResponseWriter, r *http.Request) (proxy.Settings, bool) {
	var st proxy.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&st); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return st, false
	}
	return st, true
}

func (s *server) saveProxy(w http.ResponseWriter, r *http.Request) {
	st, ok := decodeProxySettings(w, r)
	if !ok {
		return
	}
	st, err := s.Proxy.Save(st)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	if err := s.Proxy.Delete(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) generateProxyKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Replace bool `json:"replace"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
	key, err := s.Proxy.EnsureKey(body.Replace)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *server) importProxyKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PrivateKey string `json:"privateKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	key, err := s.Proxy.ImportKey(body.PrivateKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

// writeProxyError reports a failed remote operation. An untrusted host key
// comes with the key itself, so the user can choose to trust it; output from
// the remote machine is passed along.
func writeProxyError(w http.ResponseWriter, err error, output string) {
	var hk *remote.HostKeyError
	if errors.As(err, &hk) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "hostKey": hk.Key, "hostKeyFingerprint": hk.Fingerprint, "hostKeyType": hk.Type,
		})
		return
	}
	status := http.StatusBadGateway
	if errors.Is(err, proxy.ErrNotConfigured) {
		status = http.StatusConflict
	}
	body := map[string]any{"error": err.Error(), "output": output}
	if errors.Is(err, proxy.ErrStreamBlock) {
		// The page offers to adopt the block.
		status, body["streamBlock"] = http.StatusConflict, true
	}
	writeJSON(w, status, body)
}

func (s *server) testProxy(w http.ResponseWriter, r *http.Request) {
	st, ok := decodeProxySettings(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	info, err := s.Proxy.Test(ctx, st)
	if err != nil {
		writeProxyError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) installProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	out, err := s.Proxy.Install(ctx)
	if err != nil {
		writeProxyError(w, err, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

func (s *server) previewProxy(w http.ResponseWriter, r *http.Request) {
	config, servers, err := s.Proxy.Preview(r.Context())
	if err != nil {
		writeProxyError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": config, "servers": servers, "path": proxy.ConfigPath})
}

func (s *server) applyProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.Proxy.Apply(ctx)
	if err != nil {
		writeProxyError(w, err, res.Output)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) removeProxy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.Proxy.Remove(ctx)
	if err != nil {
		writeProxyError(w, err, res.Output)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// planAdoption shows how hlm would add its include to nginx's own stream
// block.
func (s *server) planAdoption(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	a, err := s.Proxy.PlanAdoption(ctx)
	if err != nil {
		writeProxyError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// adoptStreamBlock adds hlm's include to nginx's own stream block.
func (s *server) adoptStreamBlock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	a, out, err := s.Proxy.Adopt(ctx)
	if err != nil {
		writeProxyError(w, err, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"adoption": a, "output": out})
}

// --- WireGuard tunnel between the proxy server and the Docker host ---

func (s *server) proxyWireGuard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := s.Proxy.WireGuardStatus(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// setupProxyWireGuard sets up or updates the tunnel, streaming NDJSON
// progress (proxy.WireGuardEvent): steps and console output as they
// happen, then "done" with the result or "error". The setup carries on if
// the client goes away.
func (s *server) setupProxyWireGuard(w http.ResponseWriter, r *http.Request) {
	var in proxy.WireGuardInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	send := ndjson(w)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	res, err := s.Proxy.SetupWireGuard(ctx, in, func(e proxy.WireGuardEvent) { send(e) })
	if err != nil {
		s.Log.Warn("set up WireGuard", "err", err)
		send(proxy.WireGuardEvent{Stage: "error", Message: err.Error(), Output: res.Output})
		return
	}
	send(proxy.WireGuardEvent{Stage: "done", Result: &res})
}

func (s *server) removeProxyWireGuard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.Proxy.RemoveWireGuard(ctx)
	if err != nil {
		writeProxyError(w, err, res.Output)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// setupHostWireGuard sets up the tunnel's end on another Docker host,
// streaming its progress as setupProxyWireGuard does.
func (s *server) setupHostWireGuard(w http.ResponseWriter, r *http.Request) {
	var in proxy.HostWireGuardInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	host := r.PathValue("host")
	audit.Add(r.Context(), "host", host)
	send := ndjson(w)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	res, err := s.Proxy.SetupHostWireGuard(ctx, host, in, func(e proxy.WireGuardEvent) { send(e) })
	if err != nil {
		s.Log.Warn("set up WireGuard on another host", "host", host, "err", err)
		send(proxy.WireGuardEvent{Stage: "error", Message: err.Error(), Output: res.Output})
		return
	}
	send(proxy.WireGuardEvent{Stage: "done", Result: &res})
}

// hostLANNetworks lists another host's networks its end can use for LAN
// mode.
func (s *server) hostLANNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.Proxy.HostLANNetworks(r.Context(), r.PathValue("host"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, nets)
}

func (s *server) removeHostWireGuard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.Proxy.RemoveHostWireGuard(ctx, r.PathValue("host"))
	if err != nil {
		writeProxyError(w, err, res.Output)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) checkProxyWireGuard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := s.Proxy.CheckWireGuard(ctx)
	if err != nil {
		writeProxyError(w, err, res.Output)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- the proxy server's cloud firewall ---

func writeFirewallError(w http.ResponseWriter, err error) {
	var apiErr *linode.APIError
	if errors.As(err, &apiErr) {
		status := http.StatusBadGateway
		if apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *server) proxyFirewall(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.Proxy.FirewallStatus(ctx))
}

func (s *server) testProxyFirewall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := s.Proxy.TestFirewall(ctx, body.Token)
	if err != nil {
		writeFirewallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) saveProxyFirewall(w http.ResponseWriter, r *http.Request) {
	var cfg proxy.FirewallConfig
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	st, err := s.Proxy.SaveFirewall(ctx, cfg)
	if err != nil {
		writeFirewallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// deleteProxyFirewall forgets the Linode connection, closing hlm's ports on
// the firewall first unless ?keepRules=1.
func (s *server) deleteProxyFirewall(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.Proxy.DeleteFirewall(ctx, r.URL.Query().Get("keepRules") == "1"); err != nil {
		writeFirewallError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) syncProxyFirewall(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if _, err := s.Proxy.SyncFirewall(ctx); err != nil {
		writeFirewallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Proxy.FirewallStatus(ctx))
}

// systemInfo returns hlm's version and update status.
func (s *server) systemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.Version, "update": s.Updater.Status()})
}

func (s *server) checkSelfUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"version": s.Version, "update": s.Updater.Check(ctx)})
}

// applySelfUpdate installs the latest hlm release, streaming progress. hlm
// restarts (or its container is replaced) right after the stream ends.
func (s *server) applySelfUpdate(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		return s.Updater.Apply(ctx, func(e selfupdate.Event) {
			emit(manager.Event{Stage: e.Stage, Message: e.Message, Progress: e.Progress})
		})
	})
}

func (s *server) dismissSelfUpdate(w http.ResponseWriter, r *http.Request) {
	s.Updater.DismissResult()
	w.WriteHeader(http.StatusNoContent)
}

// hostMetrics returns the host's CPU, memory, network and filesystem usage,
// with up to 30 minutes of history: this host's, or (?host=) another's,
// from its agent. When they can't be had, the snapshot says why.
func (s *server) hostMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("host")
	if id == "" || id == manager.LocalHost {
		writeJSON(w, http.StatusOK, s.Host.Snapshot())
		return
	}
	if s.Hosts == nil {
		writeError(w, http.StatusNotFound, hosts.ErrNotFound)
		return
	}
	snap, err := s.Hosts.HostMetrics(r.Context(), id)
	switch {
	case errors.Is(err, hosts.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		snap = hostmetrics.Snapshot{Reason: err.Error()}
	}
	writeJSON(w, http.StatusOK, snap)
}

// hostStorage returns a host's last disk usage report per server
// (?host=, this one by default).
func (s *server) hostStorage(w http.ResponseWriter, r *http.Request) {
	host, ok := s.storageHost(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Storage(host))
}

// storageHost reads ?host= for the storage report: a known Docker host.
func (s *server) storageHost(w http.ResponseWriter, r *http.Request) (string, bool) {
	host := cmp.Or(r.URL.Query().Get("host"), manager.LocalHost)
	if !s.Manager.KnownHost(host) {
		writeError(w, http.StatusNotFound, fmt.Errorf("there's no Docker host %q", host))
		return "", false
	}
	return host, true
}

// deletedDataRequest names a deleted server's folder (host path) or volume.
type deletedDataRequest struct {
	Path   string `json:"path"`
	Volume bool   `json:"volume"`
}

func decodeDeletedData(w http.ResponseWriter, r *http.Request) (deletedDataRequest, bool) {
	var body deletedDataRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return body, false
	}
	return body, true
}

// deleteDeletedData deletes a deleted server's folder or volume from disk.
func (s *server) deleteDeletedData(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeDeletedData(w, r)
	if !ok {
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("name the folder or volume to delete"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if err := s.Manager.DeleteDeletedData(ctx, r.PathValue("id"), body.Path, body.Volume); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, manager.ErrNotTracked) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Storage(r.URL.Query().Get("host")))
}

// forgetDeletedData stops tracking a deleted server's folder or volume, or
// all of its data without a path. Nothing is deleted from disk.
func (s *server) forgetDeletedData(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeDeletedData(w, r)
	if !ok {
		return
	}
	if err := s.Manager.ForgetDeleted(r.PathValue("id"), body.Path, body.Volume); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, manager.ErrNotTracked) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Storage(r.URL.Query().Get("host")))
}

// refreshStorage starts recalculating disk usage in the background.
func (s *server) refreshStorage(w http.ResponseWriter, r *http.Request) {
	s.Manager.RefreshStorage()
	writeJSON(w, http.StatusAccepted, s.Manager.Storage(r.URL.Query().Get("host")))
}

// listJobs returns installs and updates in progress (and those that finished
// in the last minute), whoever started them.
func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	access := s.access(r)
	jobs := s.Manager.Jobs()
	if !access.Admin() {
		jobs = slices.DeleteFunc(jobs, func(j manager.Job) bool { return !access.Can(j.Server, rbac.Viewer) })
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) updateInfo(w http.ResponseWriter, r *http.Request) {
	info := s.Manager.UpdateInfo()
	if access := s.access(r); !access.Admin() {
		ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
		defer cancel()
		servers, err := s.visibleServers(ctx, access, rbac.Viewer)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		info.Available = 0
		for _, srv := range servers {
			if srv.Update != nil && srv.Update.State == manager.UpdateAvailable {
				info.Available++
			}
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// checkUpdates checks every server for image updates and waits for the
// result.
func (s *server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.CheckUpdates(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.UpdateInfo())
}

// applyUpdates updates every server the caller may update that has an update
// available, streaming progress.
func (s *server) applyUpdates(w http.ResponseWriter, r *http.Request) {
	access := s.access(r)
	keep := func(srv manager.Server) bool { return access.Can(srv.Name, rbac.Operator) }
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		return s.Manager.UpdateAll(ctx, keep, emit)
	})
}

func (s *server) setAutostart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	srv, err := s.Manager.SetAutostart(r.Context(), r.PathValue("id"), body.Enabled)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.presentOne(r, srv))
}

// listConnections returns the containers hlm runs for connections
// (tunnels), with their update status.
func (s *server) listConnections(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	conns, err := s.Manager.Connections(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, conns)
}

func (s *server) getConnection(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	c, err := s.Manager.Connection(ctx, r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) connectionStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	st, err := s.Manager.ConnectionStats(ctx, r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// connectionLogs streams a connection container's log lines as server-sent
// events, like serverLogs.
func (s *server) connectionLogs(w http.ResponseWriter, r *http.Request) {
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "500"
	}
	logs, err := s.Manager.ConnectionLogs(r.Context(), r.PathValue("id"), docker.LogsOptions{Follow: r.URL.Query().Get("follow") != "0", Tail: tail})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	streamLogs(w, r, logs)
}

func (s *server) connectionAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" {
		writeError(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	if err := s.Manager.ConnectionAction(r.Context(), r.PathValue("id"), action); err != nil {
		writeManagerError(w, err)
		return
	}
	s.listConnections(w, r)
}

// updateConnection recreates a connection container on the newest image,
// streaming NDJSON progress like a server update.
func (s *server) updateConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		return s.Manager.UpdateConnection(ctx, id, emit)
	})
}

// allStats returns resource usage for all running servers, keyed by container ID.
func (s *server) allStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	access := s.access(r)
	if !access.Admin() {
		servers, err := s.visibleServers(ctx, access, rbac.Viewer)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, s.Manager.StatsOf(ctx, servers))
		return
	}
	stats, err := s.Manager.AllStats(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// --- files ---

func (s *server) listFiles(w http.ResponseWriter, r *http.Request) {
	l, err := s.Manager.ListFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	f, err := s.Manager.ReadFile(r.Context(), r.PathValue("id"), pathParam(r))
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path          string    `json:"path"`
		Content       string    `json:"content"`
		ExpectedMtime time.Time `json:"expectedMtime"`
	}
	// JSON escaping can make the body larger than the content itself; the
	// content's own size is checked when saving.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, files.MaxEditSize*6+4096)).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, files.ErrTooLarge)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "path", body.Path)
	f, err := s.Manager.WriteFile(r.Context(), r.PathValue("id"), body.Path, body.Content, body.ExpectedMtime)
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *server) searchFiles(w http.ResponseWriter, r *http.Request) {
	matches, truncated, err := s.Manager.SearchFiles(r.Context(), r.PathValue("id"), pathParam(r), r.URL.Query().Get("q"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches, "truncated": truncated})
}

func pathParam(r *http.Request) string {
	if p := r.URL.Query().Get("path"); p != "" {
		return p
	}
	return "/"
}

func writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, files.ErrConflict):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, files.ErrReadOnly):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, files.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, files.ErrNotEditable):
		writeError(w, http.StatusUnsupportedMediaType, err)
	case errors.Is(err, files.ErrBadPath), errors.Is(err, files.ErrIsDir), errors.Is(err, files.ErrNotDir),
		errors.Is(err, files.ErrNotRegular):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeManagerError(w, err)
	}
}

func (s *server) serverStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), pollTimeout)
	defer cancel()
	st, err := s.Manager.Stats(ctx, r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// serverLogs streams log lines as server-sent events.
func (s *server) serverLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tail := q.Get("tail")
	if tail == "" {
		tail = "500"
	}
	follow := q.Get("follow") != "0"
	logs, err := s.Manager.Logs(r.Context(), r.PathValue("id"), docker.LogsOptions{Follow: follow, Tail: tail})
	if err != nil {
		writeManagerError(w, err)
		return
	}
	streamLogs(w, r, logs)
}

// streamLogs sends a container's log lines as server-sent events until the
// log ends (an "end" event) or the client goes away.
func streamLogs(w http.ResponseWriter, r *http.Request, logs io.ReadCloser) {
	defer logs.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	// Lines are read on another goroutine so that log output can be batched
	// into a flush every 100ms rather than one per line.
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(logs, 64<<10)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				select {
				case lines <- strings.TrimRight(line, "\r\n"):
				case <-r.Context().Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	dirty := false
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				io.WriteString(w, "event: end\ndata: \n\n")
				_ = rc.Flush()
				return
			}
			// A carriage return within a line is a progress bar redraw; keep
			// only the final state.
			if i := strings.LastIndexByte(line, '\r'); i >= 0 {
				line = line[i+1:]
			}
			io.WriteString(w, "data: "+line+"\n\n")
			dirty = true
		case <-tick.C:
			if dirty {
				_ = rc.Flush()
				dirty = false
			}
		case <-keepalive.C:
			io.WriteString(w, ": keepalive\n\n")
			_ = rc.Flush()
		}
	}
}

// stream runs a long operation and writes its progress events as
// newline-delimited JSON. The operation keeps running if the client
// disconnects, so closing the browser tab doesn't leave a half-installed
// server.
func (s *server) stream(w http.ResponseWriter, r *http.Request, op func(context.Context, func(manager.Event)) error) {
	send := ndjson(w)
	var mu sync.Mutex
	lastPull := time.Time{}
	emit := func(e manager.Event) {
		// Image pulls produce a flood of progress messages; send at most ten a
		// second, but never drop status changes like "Pull complete".
		if e.Stage == "pull" && e.Progress != nil &&
			(e.Progress.Status == "Downloading" || e.Progress.Status == "Extracting") {
			mu.Lock()
			skip := time.Since(lastPull) < 100*time.Millisecond
			if !skip {
				lastPull = time.Now()
			}
			mu.Unlock()
			if skip {
				return
			}
		}
		send(e)
	}

	ctx := context.WithoutCancel(r.Context())
	if err := op(ctx, emit); err != nil {
		s.Log.Warn("operation failed", "path", r.URL.Path, "err", err)
		emit(manager.Event{Stage: "error", Message: err.Error()})
		// The response said 200 before this was known; the audit log
		// records how it went.
		audit.Fail(r.Context(), err)
	}
}

// ndjson starts a stream of JSON lines and returns a function that sends
// one, flushed straight away. Sends after the client has gone are dropped.
func ndjson(w http.ResponseWriter) func(v any) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	var mu sync.Mutex
	gone := false
	enc := json.NewEncoder(w)
	return func(v any) {
		mu.Lock()
		defer mu.Unlock()
		if gone {
			return
		}
		if err := enc.Encode(v); err != nil || rc.Flush() != nil {
			gone = true
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, manager.ErrNotManaged):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, manager.ErrStopSelf):
		writeError(w, http.StatusConflict, err)
	default:
		var apiErr *docker.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			writeError(w, apiErr.Status, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
	}
}

// --- Cloudflare Tunnel ---

func writeTunnelError(w http.ResponseWriter, err error) {
	var apiErr *cloudflare.APIError
	switch {
	case errors.As(err, &apiErr):
		status := http.StatusBadGateway
		if apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
	case errors.Is(err, cftunnel.ErrNotConfigured), errors.Is(err, cftunnel.ErrNoTunnel):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) tunnelStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := s.Tunnel.Status(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) testCloudflare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	res, err := s.Tunnel.Test(ctx, body.Token)
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) saveCloudflare(w http.ResponseWriter, r *http.Request) {
	var in cftunnel.AccountInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.Tunnel.SaveAccount(ctx, in); err != nil {
		writeTunnelError(w, err)
		return
	}
	s.tunnelStatus(w, r)
}

func (s *server) deleteCloudflare(w http.ResponseWriter, r *http.Request) {
	if err := s.Tunnel.DeleteAccount(); err != nil {
		writeTunnelError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setupTunnel creates or updates the tunnel, streaming NDJSON progress
// (cftunnel.Event): steps and console output as they happen, then "done"
// with the status or "error". The setup carries on if the client goes away.
func (s *server) setupTunnel(w http.ResponseWriter, r *http.Request) {
	var in cftunnel.SetupInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	send := ndjson(w)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	st, applied, err := s.Tunnel.Setup(ctx, in, func(e cftunnel.Event) { send(e) })
	if err != nil {
		s.Log.Warn("set up the Cloudflare Tunnel", "err", err)
		send(cftunnel.Event{Stage: "error", Message: err.Error()})
		return
	}
	send(cftunnel.Event{Stage: "done", Status: &st, Apply: applied})
}

func (s *server) removeTunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	st, err := s.Tunnel.RemoveOn(ctx, r.URL.Query().Get("host"))
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// controlTunnel starts, stops or restarts the cloudflared container.
func (s *server) controlTunnel(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" && action != "restart" {
		writeError(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	st, err := s.Tunnel.ControlOn(ctx, r.URL.Query().Get("host"), action)
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// tunnelLogs streams cloudflared's log as server-sent events.
func (s *server) tunnelLogs(w http.ResponseWriter, r *http.Request) {
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "500"
	}
	logs, err := s.Tunnel.LogsOn(r.Context(), r.URL.Query().Get("host"), docker.LogsOptions{Follow: r.URL.Query().Get("follow") != "0", Tail: tail})
	if err != nil {
		if docker.IsNotFound(err) {
			writeError(w, http.StatusNotFound, errors.New("the "+cftunnel.Container+" container is gone"))
			return
		}
		writeTunnelError(w, err)
		return
	}
	streamLogs(w, r, logs)
}

func (s *server) tunnelRoutes(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Tunnel.Routes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// setTunnelRoutes replaces the routes and publishes them if the tunnel is
// set up.
func (s *server) setTunnelRoutes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Routes []cftunnel.Route `json:"routes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := s.Tunnel.SetRoutes(ctx, body.Routes)
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) applyTunnel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := s.Tunnel.Apply(ctx)
	if err != nil {
		writeTunnelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- backups ---

func (s *server) backupSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Backup.PublicSettings())
}

func decodeBackupSettings(w http.ResponseWriter, r *http.Request) (backup.Settings, bool) {
	var c backup.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return c, false
	}
	return c, true
}

// writeBackupError reports a failure. Errors from the storage server are
// a bad gateway; the rest are about what was asked.
func writeBackupError(w http.ResponseWriter, err error) {
	var s3Err *s3.Error
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, manager.ErrNotManaged):
		writeManagerError(w, err)
	case errors.Is(err, backup.ErrNoConfig):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, backup.ErrNotConfigured), errors.Is(err, backup.ErrRunning):
		writeError(w, http.StatusConflict, err)
	case errors.As(err, &s3Err):
		writeError(w, http.StatusBadGateway, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) testBackupSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeBackupSettings(w, r)
	if !ok {
		return
	}
	n, err := s.Backup.Test(r.Context(), c)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"backups": n})
}

func (s *server) saveBackupSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeBackupSettings(w, r)
	if !ok {
		return
	}
	pub, err := s.Backup.Save(r.Context(), c)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) deleteBackupSettings(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.Delete(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) serverBackups(w http.ResponseWriter, r *http.Request) {
	o, err := s.Backup.Overview(r.Context(), r.PathValue("id"))
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *server) setBackupPlan(w http.ResponseWriter, r *http.Request) {
	var p backup.Plan
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Backup.SetPlan(r.Context(), r.PathValue("id"), p)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// startBackup starts a backup in the background; GET .../backups shows its
// progress.
func (s *server) startBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.Start(r.Context(), r.PathValue("id")); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// downloadBackup streams a backup from the bucket (?key=).
func (s *server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	rc, size, err := s.Backup.Download(r.Context(), r.PathValue("id"), key, r.URL.Query().Get("target"))
	if err != nil {
		writeBackupError(w, err)
		return
	}
	sendBackup(w, key, rc, size)
}

// sendBackup sends a backup from the bucket as a download, and closes rc.
func sendBackup(w http.ResponseWriter, key string, rc io.ReadCloser, size int64) {
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(key)}))
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, _ = io.Copy(w, rc)
}

// restoreBackup starts putting a backup's files back in the server's
// container ({key, backupFirst}); GET .../backups shows its progress.
func (s *server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key         string `json:"key"`
		Target      string `json:"target"`
		BackupFirst bool   `json:"backupFirst"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "key", body.Key)
	if body.BackupFirst {
		audit.Add(r.Context(), "backupFirst", "yes")
	}
	if body.Target != "" {
		audit.Add(r.Context(), "target", body.Target)
	}
	if err := s.Backup.Restore(r.Context(), r.PathValue("id"), body.Key, body.Target, body.BackupFirst); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.DeleteBackup(r.Context(), r.PathValue("id"), r.URL.Query().Get("key"), r.URL.Query().Get("target")); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verifyBackup reads one of a server's backups back (?key=&target=), and
// says how it went.
func (s *server) verifyBackup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	audit.Add(r.Context(), "key", q.Get("key"))
	v, err := s.Backup.VerifyBackup(r.Context(), r.PathValue("id"), q.Get("key"), q.Get("target"))
	if err != nil {
		writeBackupError(w, err)
		return
	}
	if !v.OK {
		audit.Fail(r.Context(), errors.New(v.Error))
	}
	writeJSON(w, http.StatusOK, v)
}

// --- backup storage targets ---

func (s *server) backupTargets(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Backup.Targets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *server) saveBackupTarget(w http.ResponseWriter, r *http.Request) {
	var t backup.Target
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t.ID = r.PathValue("target") // empty when adding
	audit.Add(r.Context(), "name", t.Name)
	if t.Encrypt {
		audit.Add(r.Context(), "encrypt", "yes")
	}
	pub, err := s.Backup.SaveTarget(r.Context(), t)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) deleteBackupTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.DeleteTarget(r.PathValue("target")); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// backupTargetKey shows a storage's encryption key, to keep it safe.
func (s *server) backupTargetKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.Backup.Key(r.PathValue("target"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": k})
}

// --- backups of hlm's own configuration ---

func (s *server) configBackups(w http.ResponseWriter, r *http.Request) {
	o, err := s.Backup.ConfigOverview(r.Context())
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *server) setConfigBackupPlan(w http.ResponseWriter, r *http.Request) {
	var p backup.ConfigPlan
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Backup.SetConfigPlan(p)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) startConfigBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.StartConfig(); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) downloadConfigBackup(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	rc, size, err := s.Backup.DownloadConfig(r.Context(), key)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	sendBackup(w, key, rc, size)
}

func (s *server) deleteConfigBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.DeleteConfigBackup(r.Context(), r.URL.Query().Get("key")); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- sign-in ---

// authStatus tells the web UI how to sign in, and who is signed in. It needs
// no authentication.
func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		auth.Status
		// Token is set when HLM_TOKEN is, so a token can be entered.
		Token bool `json:"token"`
		// Role is the caller's global role, if they're let in at all.
		Role rbac.Role `json:"role,omitempty"`
		// ConfirmedUntil is when the session's confirmation runs out.
		ConfirmedUntil time.Time `json:"confirmedUntil,omitzero"`
	}{Status: s.Auth.Status(r), Token: s.Token != ""}
	if p, _, ok := s.identify(r); ok {
		out.Role = p.Role
	}
	if u, ok := s.Auth.Session(r); ok && s.Auth.Confirmed(u) {
		out.ConfirmedUntil = u.Confirmed.Add(auth.ConfirmWindow)
	}
	writeJSON(w, http.StatusOK, out)
}

// requireCSRFHeader refuses requests that change something without
// csrfHeader, for the endpoints the auth middleware lets through.
func requireCSRFHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(csrfHeader) == "" {
		writeError(w, http.StatusForbidden, errors.New("this request needs the "+csrfHeader+" header"))
		return false
	}
	return true
}

func (s *server) localLogin(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	u, err := s.Auth.LocalLogin(w, r)
	var le *auth.LoginError
	var tf *auth.TwoFactorNeeded
	switch {
	case errors.As(err, &tf):
		// The password was right; the second step comes next.
		if tf.Setup {
			writeJSON(w, http.StatusOK, map[string]bool{"twoFactorSetup": true})
		} else {
			// Which second factors the account has.
			writeJSON(w, http.StatusOK, map[string]bool{"twoFactor": true, "code": tf.Code, "passkey": tf.Passkey})
		}
	case errors.As(err, &le):
		if le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
		}
		writeError(w, http.StatusUnauthorized, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, u)
	}
}

// decodeCode reads {code} from a request.
func decodeCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return "", false
	}
	return in.Code, true
}

// writeLoginError answers a failed sign-in step.
func writeLoginError(w http.ResponseWriter, err error) {
	var le *auth.LoginError
	switch {
	case errors.As(err, &le):
		if le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
		}
		writeError(w, http.StatusUnauthorized, err)
	case errors.Is(err, auth.ErrHalfExpired):
		writeError(w, http.StatusUnauthorized, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// loginCode finishes a sign-in with a two-factor code.
func (s *server) loginCode(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	code, ok := decodeCode(w, r)
	if !ok {
		return
	}
	u, err := s.Auth.LoginCode(w, r, code)
	if err != nil {
		writeLoginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// loginSetup starts two-factor for an account that has to have it, during
// sign-in.
func (s *server) loginSetup(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	setup, err := s.Auth.LoginSetup(r)
	if err != nil {
		writeLoginError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, setup)
}

// loginEnable turns two-factor on during sign-in and finishes it.
func (s *server) loginEnable(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	code, ok := decodeCode(w, r)
	if !ok {
		return
	}
	codes, u, err := s.Auth.LoginEnable(w, r, code)
	if err != nil {
		writeLoginError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes, "user": u})
}

// ownAccount reports whether the request is signed in at a browser as the
// local account in the path.
func (s *server) ownAccount(r *http.Request) bool {
	u, ok := s.Auth.Session(r)
	return ok && u.Method == auth.MethodLocal && strings.EqualFold(u.Username, r.PathValue("username"))
}

// beginTwoFactor starts setting up two-factor for the caller's own account.
func (s *server) beginTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errors.New("two-factor is set up by the account's owner, signed in at a browser"))
		return
	}
	if !s.stepUp(w, r) {
		return
	}
	setup, err := s.Auth.BeginTwoFactor(r.PathValue("username"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, setup)
}

func (s *server) enableTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	code, ok := decodeCode(w, r)
	if !ok {
		return
	}
	codes, err := s.Auth.EnableTwoFactor(r.PathValue("username"), code)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// disableTwoFactor turns off an account's two-factor: the owner's own (if
// it isn't required), or an admin resetting someone's.
func (s *server) disableTwoFactor(w http.ResponseWriter, r *http.Request) {
	own := s.ownAccount(r)
	if !own && !s.access(r).Admin() {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	if !s.stepUp(w, r) {
		return
	}
	err := s.Auth.DisableTwoFactor(r.PathValue("username"), !own)
	switch {
	case errors.Is(err, auth.ErrNoAccount):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) newRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if !s.ownAccount(r) {
		writeError(w, http.StatusForbidden, errNotAllowed)
		return
	}
	if !s.stepUp(w, r) {
		return
	}
	codes, err := s.Auth.NewRecoveryCodes(r.PathValue("username"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if !requireCSRFHeader(w, r) {
		return
	}
	s.Auth.Logout(w, r)
}

func (s *server) authConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.Config())
}

func (s *server) saveAuthConfig(w http.ResponseWriter, r *http.Request) {
	var in auth.Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.Auth.SaveConfig(r, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) listAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.Accounts())
}

type accountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Current is the password being replaced, needed from non-admins.
	Current string    `json:"current"`
	Role    rbac.Role `json:"role"`
}

func decodeAccount(w http.ResponseWriter, r *http.Request) (accountRequest, bool) {
	var in accountRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	return in, true
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAccount(w, r)
	if !ok {
		return
	}
	// The first account can be made with HLM_TOKEN, or a new install could
	// never get one. After that, an account is a way in like a key is.
	if len(s.Auth.Accounts()) > 0 && !s.stepUp(w, r) {
		return
	}
	audit.Add(r.Context(), "username", in.Username)
	audit.Add(r.Context(), "role", string(in.Role))
	a, err := s.Auth.CreateAccount(in.Username, in.Password, in.Role)
	if errors.Is(err, auth.ErrAccountExists) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAccount(w, r)
	if !ok {
		return
	}
	username := r.PathValue("username")
	if s.access(r).Admin() {
		// Setting a password, even an admin's own, is a way in.
		if !s.stepUp(w, r) {
			return
		}
	} else {
		// Their own, then, which they have to know. An API key isn't an account.
		u, signedIn := s.Auth.Session(r)
		if !signedIn || u.Method != auth.MethodLocal || !strings.EqualFold(u.Username, username) {
			writeError(w, http.StatusForbidden, errNotAllowed)
			return
		}
		if err := s.Auth.VerifyPassword(username, in.Current); err != nil {
			var le *auth.LoginError
			if errors.As(err, &le) && le.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
			}
			writeError(w, http.StatusForbidden, err)
			return
		}
	}
	err := s.Auth.ChangePassword(w, r, username, in.Password)
	if errors.Is(err, auth.ErrNoAccount) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sshChallenge gives the text to sign to sign in with an SSH key.
func (s *server) sshChallenge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	text, expires, err := s.Auth.SSHChallenge(in.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenge": text, "namespace": auth.SSHNamespace, "expires": expires})
}

// sshLogin swaps a signed challenge for a token. It's a bearer token rather
// than a cookie, so unlike a password sign-in it needs no CSRF header.
func (s *server) sshLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Challenge string `json:"challenge"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, expires, err := s.Auth.SSHLogin(in.Challenge, in.Signature, r.RemoteAddr)
	if err != nil {
		var le *auth.LoginError
		if errors.As(err, &le) && le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
		}
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires": expires})
}

// mayManageSSHKeys reports whether the caller is an admin or the account in
// the path, answering them if they're neither.
func (s *server) mayManageSSHKeys(w http.ResponseWriter, r *http.Request) (admin, ok bool) {
	if s.access(r).Admin() {
		return true, true
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	me := rbac.Subject{Kind: rbac.SubjectLocal, Name: r.PathValue("username")}
	if slices.ContainsFunc(p.Subjects, func(o rbac.Subject) bool { return o.Kind == me.Kind && strings.EqualFold(o.Name, me.Name) }) {
		return false, true
	}
	writeError(w, http.StatusForbidden, errNotAllowed)
	return false, false
}

func (s *server) listSSHKeys(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.mayManageSSHKeys(w, r); !ok {
		return
	}
	keys, err := s.Auth.SSHKeys(r.PathValue("username"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// addSSHKey adds a way into an account. That's as strong as changing its
// password, so it takes a confirmed session: otherwise a stolen session, API
// key or token could turn into access that outlives it.
func (s *server) addSSHKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.mayManageSSHKeys(w, r); !ok || !s.stepUp(w, r) {
		return
	}
	var in struct {
		Name      string   `json:"name"`
		PublicKey string   `json:"publicKey"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	username := r.PathValue("username")
	k, err := s.Auth.AddSSHKey(username, in.PublicKey, in.Name, in.AllowFrom)
	switch {
	case errors.Is(err, auth.ErrNoAccount):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusCreated, k)
	}
}

// setSSHKeyAllowFrom changes where an SSH key can be used from.
func (s *server) setSSHKeyAllowFrom(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.mayManageSSHKeys(w, r); !ok || !s.stepUp(w, r) {
		return
	}
	var in struct {
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	k, err := s.Auth.SetSSHKeyAllowFrom(r.PathValue("username"), r.PathValue("id"), in.AllowFrom)
	switch {
	case errors.Is(err, auth.ErrNoAccount), errors.Is(err, auth.ErrNoSSHKey):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, k)
	}
}

func (s *server) deleteSSHKey(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.mayManageSSHKeys(w, r); !ok {
		return
	}
	if err := s.Auth.DeleteSSHKey(r.PathValue("username"), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setAccountRole changes an account's global role.
func (s *server) setAccountRole(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeAccount(w, r)
	if !ok {
		return
	}
	a, err := s.Auth.SetAccountRole(r, r.PathValue("username"), in.Role)
	switch {
	case errors.Is(err, auth.ErrNoAccount):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, auth.ErrLastAdmin):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	err := s.Auth.DeleteAccount(r, username)
	if errors.Is(err, auth.ErrNoAccount) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// A new account with this name shouldn't get its access.
	if err := s.Groups.ForgetSubject(rbac.Subject{Kind: rbac.SubjectLocal, Name: username}); err != nil {
		s.Log.Warn("remove deleted account's access", "user", username, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.APIKeys())
}

func (s *server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string    `json:"name"`
		Days      int       `json:"days"`
		Role      rbac.Role `json:"role"`
		AllowFrom []string  `json:"allowFrom"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "name", in.Name)
	audit.Add(r.Context(), "role", string(in.Role))
	key, info, err := s.Auth.CreateAPIKey(in.Name, in.Days, in.Role, in.AllowFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Key string `json:"key"`
		auth.APIKeyInfo
	}{key, info})
}

// setAPIKeyAllowFrom changes where a key can be used from.
func (s *server) setAPIKeyAllowFrom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.Auth.SetAPIKeyAllowFrom(r.PathValue("id"), in.AllowFrom)
	switch {
	case errors.Is(err, auth.ErrNoAPIKey):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, info)
	}
}

func (s *server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.Auth.DeleteAPIKey(id)
	if errors.Is(err, auth.ErrNoAPIKey) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.Groups.ForgetSubject(rbac.Subject{Kind: rbac.SubjectKey, Name: id}); err != nil {
		s.Log.Warn("remove deleted key's access", "key", id, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) oidcSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Auth.PublicOIDC())
}

func decodeOIDC(w http.ResponseWriter, r *http.Request) (auth.OIDCSettings, bool) {
	var in auth.OIDCSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	return in, true
}

func (s *server) saveOIDC(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeOIDC(w, r)
	if !ok {
		return
	}
	pub, err := s.Auth.SaveOIDC(r, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) testOIDC(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeOIDC(w, r)
	if !ok {
		return
	}
	res, err := s.Auth.TestOIDC(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// generateClientSecret makes a client secret for registering hlm with a
// provider. Nothing is saved until the settings are.
func (s *server) generateClientSecret(w http.ResponseWriter, r *http.Request) {
	cs, err := auth.GenerateClientSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, cs)
}

func (s *server) deleteOIDC(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.DeleteOIDC(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- HTTPS ---

var errNoHTTPS = errors.New("HTTPS isn't available in this build")

func (s *server) httpsStatus(w http.ResponseWriter, r *http.Request) {
	if s.HTTPS == nil {
		writeError(w, http.StatusNotFound, errNoHTTPS)
		return
	}
	writeJSON(w, http.StatusOK, s.HTTPS.Status())
}

// saveHTTPS turns HTTPS on or off. It changes how everyone reaches hlm, so it
// takes a confirmed session, like the other sign-in settings.
func (s *server) saveHTTPS(w http.ResponseWriter, r *http.Request) {
	if s.HTTPS == nil {
		writeError(w, http.StatusNotFound, errNoHTTPS)
		return
	}
	var in tlscert.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.HTTPS.Save(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) renewHTTPS(w http.ResponseWriter, r *http.Request) {
	if s.HTTPS == nil {
		writeError(w, http.StatusNotFound, errNoHTTPS)
		return
	}
	s.HTTPS.Renew()
	writeJSON(w, http.StatusAccepted, s.HTTPS.Status())
}
