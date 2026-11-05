package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// --- public status page ---

// statusLimiter keeps one address from hammering the public endpoints (the
// page itself is cached, so this is about abuse, not cost).
var statusLimiter = &statuspage.Limiter{Rate: 2, Burst: 60}

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
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !statusLimiter.Allow(host, time.Now()) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, errors.New("too many requests; try again shortly"))
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.Header().Set("Access-Control-Allow-Origin", "*") // badges and JSON may be embedded elsewhere
	return true
}

func (s *server) publicStatus(w http.ResponseWriter, r *http.Request) {
	if !s.statusGate(w, r) {
		return
	}
	page, err := s.StatusPage.Page(r.Context(), time.Now())
	if err != nil {
		writeStatusError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// publicStatusServer is one server's JSON ({slug}.json) or badge
// ({slug}.svg).
func (s *server) publicStatusServer(w http.ResponseWriter, r *http.Request) {
	if !s.statusGate(w, r) {
		return
	}
	file := r.PathValue("file")
	slug, kind, ok := strings.Cut(file, ".")
	if !ok || (kind != "json" && kind != "svg") {
		http.NotFound(w, r)
		return
	}
	srv, err := s.StatusPage.ServerBySlug(r.Context(), slug, time.Now())
	if err != nil {
		writeStatusError(w, err)
		return
	}
	if kind == "json" {
		writeJSON(w, http.StatusOK, srv)
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

// statusPageSettings returns the settings, with addresses hlm can suggest
// for the servers on the page.
func (s *server) statusPageSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.StatusPage.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": set})
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

func (s *server) saveStatusPage(w http.ResponseWriter, r *http.Request) {
	var in statuspage.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	set, err := s.StatusPage.Save(in, auth.ParseAllowFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": set})
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

// statusHost limits the status page's own hostname, if one is set, to the
// status page: everything else of hlm answers 404 there, whoever asks.
func (s *server) statusHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.StatusPage != nil {
			if set, err := s.StatusPage.Settings(); err == nil && set.Hostname != "" {
				host := r.Host
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				if strings.EqualFold(strings.TrimSuffix(host, "."), set.Hostname) {
					switch {
					case r.URL.Path == "/":
						http.Redirect(w, r, "/status", http.StatusFound)
						return
					case !statusOnlyPath(r.URL.Path) || (r.Method != http.MethodGet && r.Method != http.MethodHead):
						http.NotFound(w, r)
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
