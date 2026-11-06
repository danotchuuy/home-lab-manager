// Package modrinthtest is a fake Modrinth API, with its own file CDN, for
// tests.
package modrinthtest

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Project is a project in the fake.
type Project struct {
	ID, Slug, Title string
	ServerSide      string // required if empty
	Versions        []*Version
}

// Version is one of a project's versions.
type Version struct {
	ID, Number   string
	Type         string // release if empty
	GameVersions []string
	Loaders      []string
	Published    time.Time
	// Requires are the project IDs it needs.
	Requires []string
	// Incompatible are the project IDs it doesn't work with.
	Incompatible []string
	File         string
	Content      []byte
	// BadHash serves a hash that doesn't match the content.
	BadHash bool
}

// Hash is the SHA-512 of content, as Modrinth gives it.
func Hash(content []byte) string {
	h := sha512.Sum512(content)
	return hex.EncodeToString(h[:])
}

// Server is the fake.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	projects []*Project
	// Requests counts requests by path.
	Requests map[string]int
}

// New starts a fake with the projects.
func New(projects ...*Project) *Server {
	s := &Server{projects: projects, Requests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Host is the server's host, to allow as the CDN.
func (s *Server) Host() string {
	u, _ := url.Parse(s.URL)
	return u.Host
}

func (s *Server) find(id string) *Project {
	for _, p := range s.projects {
		if p.ID == id || p.Slug == id {
			return p
		}
	}
	return nil
}

func (s *Server) version(id string) (*Project, *Version) {
	for _, p := range s.projects {
		for _, v := range p.Versions {
			if v.ID == id {
				return p, v
			}
		}
	}
	return nil, nil
}

func (s *Server) byHash(h string) (*Project, *Version) {
	for _, p := range s.projects {
		for _, v := range p.Versions {
			if Hash(v.Content) == h {
				return p, v
			}
		}
	}
	return nil, nil
}

func (s *Server) projectJSON(p *Project) map[string]any {
	side := p.ServerSide
	if side == "" {
		side = "required"
	}
	return map[string]any{"id": p.ID, "slug": p.Slug, "title": p.Title, "description": p.Title + " does things",
		"server_side": side, "client_side": "optional", "downloads": 10}
}

func (s *Server) versionJSON(p *Project, v *Version) map[string]any {
	typ := v.Type
	if typ == "" {
		typ = "release"
	}
	deps := []map[string]any{}
	for _, r := range v.Requires {
		deps = append(deps, map[string]any{"project_id": r, "dependency_type": "required"})
	}
	for _, r := range v.Incompatible {
		deps = append(deps, map[string]any{"project_id": r, "dependency_type": "incompatible"})
	}
	hash := Hash(v.Content)
	if v.BadHash {
		hash = Hash(append([]byte("not "), v.Content...))
	}
	return map[string]any{
		"id": v.ID, "project_id": p.ID, "name": p.Title + " " + v.Number, "version_number": v.Number,
		"version_type": typ, "game_versions": v.GameVersions, "loaders": v.Loaders,
		"date_published": v.Published.Format(time.RFC3339), "dependencies": deps,
		"files": []map[string]any{{
			"hashes": map[string]string{"sha512": hash, "sha1": "x"}, "url": s.URL + "/cdn/" + v.ID + "/" + v.File,
			"filename": v.File, "primary": true, "size": len(v.Content),
		}},
	}
}

func listParam(r *http.Request, name string) []string {
	var out []string
	_ = json.Unmarshal([]byte(r.URL.Query().Get(name)), &out)
	return out
}

func fits(v *Version, loaders, games []string) bool {
	if len(loaders) > 0 && !slices.ContainsFunc(v.Loaders, func(l string) bool { return slices.Contains(loaders, l) }) {
		return false
	}
	return len(games) == 0 || slices.ContainsFunc(v.GameVersions, func(g string) bool { return slices.Contains(games, g) })
}

// newest is the newest version that fits, by date.
func newest(p *Project, loaders, games []string) *Version {
	var best *Version
	for _, v := range p.Versions {
		if fits(v, loaders, games) && (best == nil || v.Published.After(best.Published)) {
			best = v
		}
	}
	return best
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests[r.URL.Path]++
	if r.Header.Get("User-Agent") == "" {
		http.Error(w, "no user agent", http.StatusBadRequest)
		return
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	p := r.URL.Path
	switch {
	case p == "/search":
		var facets [][]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("facets")), &facets)
		var loaders, games []string
		for _, f := range facets {
			for _, x := range f {
				if l, ok := strings.CutPrefix(x, "categories:"); ok {
					loaders = append(loaders, l)
				}
				if g, ok := strings.CutPrefix(x, "versions:"); ok {
					games = append(games, g)
				}
			}
		}
		q := strings.ToLower(r.URL.Query().Get("query"))
		hits := []map[string]any{}
		for _, pr := range s.projects {
			if !strings.Contains(strings.ToLower(pr.Title), q) || newest(pr, loaders, games) == nil {
				continue
			}
			h := s.projectJSON(pr)
			h["project_id"] = pr.ID
			hits = append(hits, h)
		}
		reply(map[string]any{"hits": hits})
	case p == "/projects":
		out := []map[string]any{}
		for _, id := range listParam(r, "ids") {
			if pr := s.find(id); pr != nil {
				out = append(out, s.projectJSON(pr))
			}
		}
		reply(out)
	case strings.HasPrefix(p, "/project/") && strings.HasSuffix(p, "/version"):
		pr := s.find(strings.TrimSuffix(strings.TrimPrefix(p, "/project/"), "/version"))
		if pr == nil {
			http.NotFound(w, r)
			return
		}
		loaders, games := listParam(r, "loaders"), listParam(r, "game_versions")
		out := []map[string]any{}
		vs := slices.Clone(pr.Versions)
		slices.SortFunc(vs, func(a, b *Version) int { return b.Published.Compare(a.Published) })
		for _, v := range vs {
			if fits(v, loaders, games) {
				out = append(out, s.versionJSON(pr, v))
			}
		}
		reply(out)
	case strings.HasPrefix(p, "/version/"):
		pr, v := s.version(strings.TrimPrefix(p, "/version/"))
		if v == nil {
			http.NotFound(w, r)
			return
		}
		reply(s.versionJSON(pr, v))
	case p == "/version_files" || p == "/version_files/update":
		var body struct {
			Hashes       []string `json:"hashes"`
			Loaders      []string `json:"loaders"`
			GameVersions []string `json:"game_versions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := map[string]any{}
		for _, h := range body.Hashes {
			pr, v := s.byHash(h)
			if v == nil {
				continue
			}
			if p == "/version_files/update" {
				if v = newest(pr, body.Loaders, body.GameVersions); v == nil {
					continue
				}
			}
			out[h] = s.versionJSON(pr, v)
		}
		reply(out)
	case strings.HasPrefix(p, "/cdn/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/cdn/"), "/")
		_, v := s.version(id)
		if v == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(v.Content)
	default:
		http.NotFound(w, r)
	}
}
