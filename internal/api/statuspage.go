package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// --- public status page ---

// statusLimiter keeps one address from hammering the public endpoints (the
// page itself is cached, so this is about abuse, not cost).
var statusLimiter = &statuspage.Limiter{Rate: 2, Burst: 60}

// unlockLimiter slows down guessing the page's passwords: five tries, then
// one every 12 seconds, per address.
var unlockLimiter = &statuspage.Limiter{Rate: 1.0 / 12, Burst: 5}

// wakeLimiter keeps one address from starting servers over and over: three,
// then one every 10 minutes.
var wakeLimiter = &statuspage.Limiter{Rate: 1.0 / 600, Burst: 3}

func remoteHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// statusAccess is what the visitor's cookie lets them see.
func (s *server) statusAccess(r *http.Request) statuspage.Access {
	cookie := ""
	if c, err := r.Cookie(statuspage.CookieName); err == nil {
		cookie = c.Value
	}
	a, err := s.StatusPage.AccessFor(r.Host, cookie, time.Now())
	if err != nil {
		return statuspage.Access{ViewLocked: true, DetailsLocked: true}
	}
	return a
}

// statusViewGate is statusGate, then 401 while the page needs its view
// password (saying so, with the page's title, for the password form).
func (s *server) statusViewGate(w http.ResponseWriter, r *http.Request) (statuspage.Access, bool) {
	if !s.statusGate(w, r) {
		return statuspage.Access{}, false
	}
	a := s.statusAccess(r)
	if a.ViewLocked {
		title := statuspage.DefaultTitle
		if p, err := s.StatusPage.Page(r.Context(), r.Host, time.Now()); err == nil {
			title = p.Title
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "this page needs a password", "passwordRequired": true, "title": title})
		return a, false
	}
	return a, true
}

// statusGate answers 404 while the page is off or the caller isn't allowed
// to see it, and 429 when they ask too often.
func (s *server) statusGate(w http.ResponseWriter, r *http.Request) bool {
	if s.StatusPage == nil {
		http.NotFound(w, r)
		return false
	}
	set, err := s.StatusPage.Settings()
	// An empty list lets everyone in (auth.AllowedFrom's would let nobody).
	allowed := len(set.AllowFrom) == 0 || auth.AllowedFrom(set.AllowFrom, r.RemoteAddr)
	if err != nil || !set.Enabled || !allowed {
		writeError(w, http.StatusNotFound, statuspage.ErrDisabled)
		return false
	}
	if !statusLimiter.Allow(remoteHost(r), time.Now()) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, errors.New("too many requests; try again shortly"))
		return false
	}
	if set.ViewHash == "" && set.DetailsHash == "" && len(set.Sites) == 0 {
		w.Header().Set("Cache-Control", "public, max-age=15")
		w.Header().Set("Access-Control-Allow-Origin", "*") // badges and JSON may be embedded elsewhere
	} else {
		// What's shown depends on the visitor's cookie.
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Cookie")
	}
	return true
}

func (s *server) publicStatus(w http.ResponseWriter, r *http.Request) {
	access, ok := s.statusViewGate(w, r)
	if !ok {
		return
	}
	page, err := s.StatusPage.Page(r.Context(), r.Host, time.Now())
	if err != nil {
		writeStatusError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		statuspage.Page
		Access statuspage.Access `json:"access"`
	}{statuspage.Redact(page, access), access})
}

// unlockStatus checks a status page password, and remembers the access it
// gives in a cookie.
func (s *server) unlockStatus(w http.ResponseWriter, r *http.Request) {
	if !s.statusGate(w, r) {
		return
	}
	// As for the rest of the API: other sites can't send this header.
	if r.Header.Get(csrfHeader) == "" {
		writeError(w, http.StatusForbidden, errors.New("send the "+csrfHeader+" header"))
		return
	}
	if !unlockLimiter.Allow(remoteHost(r), time.Now()) {
		w.Header().Set("Retry-After", "12")
		writeError(w, http.StatusTooManyRequests, errors.New("too many tries; wait a moment"))
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_, token, expiry, err := s.StatusPage.Unlock(r.Host, in.Password, time.Now())
	if err != nil {
		if errors.Is(err, statuspage.ErrWrongPassword) {
			writeError(w, http.StatusUnauthorized, err)
			return
		}
		writeStatusError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: statuspage.CookieName, Value: token, Path: "/", Expires: expiry, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	a, _ := s.StatusPage.AccessFor(r.Host, token, time.Now())
	writeJSON(w, http.StatusOK, a)
}

// lockStatus forgets the visitor's status page password.
func (s *server) lockStatus(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: statuspage.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	w.WriteHeader(http.StatusNoContent)
}

// publicStatusServer is one server's JSON ({slug}.json) or badge
// ({slug}.svg).
func (s *server) publicStatusServer(w http.ResponseWriter, r *http.Request) {
	access, ok := s.statusViewGate(w, r)
	if !ok {
		return
	}
	file := r.PathValue("file")
	slug, kind, ok := strings.Cut(file, ".")
	if !ok || (kind != "json" && kind != "svg") {
		http.NotFound(w, r)
		return
	}
	srv, err := s.StatusPage.ServerBySlug(r.Context(), r.Host, slug, time.Now())
	if err != nil {
		writeStatusError(w, err)
		return
	}
	if kind == "json" {
		writeJSON(w, http.StatusOK, statuspage.Redact(statuspage.Page{Servers: []statuspage.Server{srv}}, access).Servers[0])
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Write(statuspage.Badge(srv))
}

func writeStatusError(w http.ResponseWriter, err error) {
	if errors.Is(err, statuspage.ErrDisabled) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadGateway, errors.New("the status isn't available right now"))
}

// statusSite is a site as admins see it: whether each password is set,
// never its hash.
type statusSite struct {
	statuspage.Site
	HasViewPassword    bool `json:"hasViewPassword"`
	HasDetailsPassword bool `json:"hasDetailsPassword"`
}

// statusSettingsView is the settings as admins see them.
func statusSettingsView(set statuspage.Settings) map[string]any {
	hasView, hasDetails := set.ViewHash != "", set.DetailsHash != ""
	set.ViewHash, set.DetailsHash = "", ""
	sites := make([]statusSite, len(set.Sites))
	for i, site := range set.Sites {
		v := statusSite{Site: site, HasViewPassword: site.ViewHash != "", HasDetailsPassword: site.DetailsHash != ""}
		v.ViewHash, v.DetailsHash = "", ""
		sites[i] = v
	}
	set.Sites = nil
	return map[string]any{"settings": set, "sites": sites, "hasViewPassword": hasView, "hasDetailsPassword": hasDetails}
}

// statusPageSettings returns the settings.
func (s *server) statusPageSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.StatusPage.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, statusSettingsView(set))
}

// statusPageSuggest offers addresses for one server (?server=).
func (s *server) statusPageSuggest(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("server")
	list := s.StatusPage.Suggestions(r.Context(), []string{name})[name]
	if list == nil {
		list = []statuspage.Suggestion{}
	}
	writeJSON(w, http.StatusOK, list)
}

// saveStatusPage saves the settings. viewPassword and detailsPassword set
// a password, "" removes it, and leaving them out keeps it.
func (s *server) saveStatusPage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		statuspage.Settings
		ViewPassword    *string `json:"viewPassword"`
		DetailsPassword *string `json:"detailsPassword"`
		// Sites, each with its own password changes.
		Sites []struct {
			statuspage.Site
			ViewPassword    *string `json:"viewPassword"`
			DetailsPassword *string `json:"detailsPassword"`
		} `json:"sites"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Settings.Sites = make([]statuspage.Site, len(in.Sites))
	changes := make([]statuspage.SitePasswords, len(in.Sites))
	for i, site := range in.Sites {
		in.Settings.Sites[i] = site.Site
		changes[i] = statuspage.SitePasswords{View: site.ViewPassword, Details: site.DetailsPassword}
		if site.ViewPassword != nil || site.DetailsPassword != nil {
			audit.Add(r.Context(), "sitePasswords", site.Hostname)
		}
	}
	if in.ViewPassword != nil {
		audit.Add(r.Context(), "viewPassword", map[bool]string{true: "removed", false: "set"}[*in.ViewPassword == ""])
	}
	if in.DetailsPassword != nil {
		audit.Add(r.Context(), "detailsPassword", map[bool]string{true: "removed", false: "set"}[*in.DetailsPassword == ""])
	}
	set, err := s.StatusPage.SavePasswords(in.Settings, in.ViewPassword, in.DetailsPassword, changes, auth.ParseAllowFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, statusSettingsView(set))
}

// statusOnlyPaths are what a status-only hostname reaches: the page, its
// API and the static files it loads.
func statusOnlyPath(p string) bool {
	switch {
	case p == "/status", p == "/api/status-page", strings.HasPrefix(p, "/api/status-page/"), strings.HasPrefix(p, "/assets/"):
		return true
	case p == "/favicon.svg", p == "/favicon.ico", p == "/apple-touch-icon.png", p == "/manifest.webmanifest",
		strings.HasPrefix(p, "/icon-") && strings.HasSuffix(p, ".png"):
		return true
	}
	return false
}

// statusMethodOK: the page is read-only, apart from giving and forgetting
// its password, and starting sleeping servers that allow it.
func statusMethodOK(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/status-page/unlock", "/api/status-page/lock":
		return r.Method == http.MethodPost
	}
	if strings.HasSuffix(r.URL.Path, "/wake") && strings.Count(r.URL.Path, "/") == 4 {
		return r.Method == http.MethodPost
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// statusHost limits the status sites' domains to the status page:
// everything else of hlm answers 404 there, whoever asks.
func (s *server) statusHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.StatusPage != nil && s.StatusPage.IsSiteHost(r.Host) {
			switch {
			case r.URL.Path == "/":
				http.Redirect(w, r, "/status", http.StatusFound)
				return
			case !statusOnlyPath(r.URL.Path) || !statusMethodOK(r):
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
