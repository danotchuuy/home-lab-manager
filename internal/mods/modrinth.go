package mods

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultAPI is Modrinth's API.
const DefaultAPI = "https://api.modrinth.com/v2"

// cdnHosts are the only hosts mod files are downloaded from.
var cdnHosts = []string{"cdn.modrinth.com"}

// MaxFileSize is the largest mod file downloaded.
const MaxFileSize = 256 << 20

// Modrinth talks to Modrinth's API. No account is needed.
type Modrinth struct {
	// API is the API's base URL (DefaultAPI).
	API string
	// CDN are the hosts files may come from (cdnHosts); tests point them
	// at their own server, and allow plain HTTP.
	CDN       []string
	AllowHTTP bool
	Client    *http.Client
	UserAgent string
}

// NewModrinth is a client for Modrinth's API.
func NewModrinth(userAgent string) *Modrinth {
	return &Modrinth{API: DefaultAPI, CDN: cdnHosts, Client: &http.Client{Timeout: 30 * time.Second}, UserAgent: userAgent}
}

// Project is a mod (or plugin) on Modrinth.
type Project struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	IconURL     string `json:"icon_url,omitempty"`
	ServerSide  string `json:"server_side,omitempty"`
	ClientSide  string `json:"client_side,omitempty"`
	Downloads   int    `json:"downloads"`
}

// SearchHit is a project found by Search.
type SearchHit struct {
	ProjectID   string   `json:"project_id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	IconURL     string   `json:"icon_url,omitempty"`
	Downloads   int      `json:"downloads"`
	ServerSide  string   `json:"server_side,omitempty"`
	ClientSide  string   `json:"client_side,omitempty"`
	Categories  []string `json:"categories,omitempty"`
}

// Version is one release of a project.
type Version struct {
	ID            string       `json:"id"`
	ProjectID     string       `json:"project_id"`
	Name          string       `json:"name"`
	VersionNumber string       `json:"version_number"`
	VersionType   string       `json:"version_type"` // release, beta, alpha
	GameVersions  []string     `json:"game_versions"`
	Loaders       []string     `json:"loaders"`
	Published     time.Time    `json:"date_published"`
	Dependencies  []Dependency `json:"dependencies"`
	Files         []VFile      `json:"files"`
}

// Dependency is another project (or version) a version needs.
type Dependency struct {
	VersionID string `json:"version_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	// Type is required, optional, incompatible or embedded.
	Type string `json:"dependency_type"`
}

// VFile is a version's file.
type VFile struct {
	Hashes   map[string]string `json:"hashes"`
	URL      string            `json:"url"`
	Filename string            `json:"filename"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
}

// File is the version's file to install: the primary one, or the first.
func (v *Version) File() (VFile, bool) {
	for _, f := range v.Files {
		if f.Primary {
			return f, true
		}
	}
	if len(v.Files) > 0 {
		return v.Files[0], true
	}
	return VFile{}, false
}

// Fit is what a server can load: its Modrinth loaders and game version.
type Fit struct {
	Loaders     []string `json:"loaders"`
	GameVersion string   `json:"gameVersion"`
}

// APIError is an error answer from Modrinth.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("modrinth: %s (HTTP %d)", e.Message, e.Status)
	}
	return fmt.Sprintf("modrinth: HTTP %d", e.Status)
}

// ErrNotFound is a project or version Modrinth doesn't have.
var ErrNotFound = errors.New("modrinth doesn't have that")

func (m *Modrinth) do(ctx context.Context, method, p string, q url.Values, body, out any) error {
	u := strings.TrimRight(m.API, "/") + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", m.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return fmt.Errorf("modrinth: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("modrinth: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Message: e.Description}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("modrinth: unexpected answer: %w", err)
	}
	return nil
}

func jsonList(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Search finds projects that fit the server.
func (m *Modrinth) Search(ctx context.Context, query string, fit Fit, limit int) ([]SearchHit, error) {
	facets := [][]string{
		{"project_type:mod", "project_type:plugin"},
		{"server_side:required", "server_side:optional"},
	}
	var loaders []string
	for _, l := range fit.Loaders {
		loaders = append(loaders, "categories:"+l)
	}
	facets = append(facets, loaders)
	if fit.GameVersion != "" {
		facets = append(facets, []string{"versions:" + fit.GameVersion})
	}
	f, _ := json.Marshal(facets)
	q := url.Values{"query": {query}, "facets": {string(f)}, "limit": {fmt.Sprint(limit)}}
	if strings.TrimSpace(query) == "" {
		q.Set("index", "downloads")
	}
	var res struct {
		Hits []SearchHit `json:"hits"`
	}
	if err := m.do(ctx, http.MethodGet, "/search", q, nil, &res); err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// idRE matches Modrinth project and version IDs, and slugs.
var idRE = regexp.MustCompile(`^[A-Za-z0-9_.!@$()'+-]{1,64}$`)

func checkID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("%q isn't a Modrinth ID", id)
	}
	return nil
}

// Projects looks projects up by ID.
func (m *Modrinth) Projects(ctx context.Context, ids []string) ([]Project, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	for _, id := range ids {
		if err := checkID(id); err != nil {
			return nil, err
		}
	}
	var out []Project
	err := m.do(ctx, http.MethodGet, "/projects", url.Values{"ids": {jsonList(ids)}}, nil, &out)
	return out, err
}

// Versions are a project's versions that fit, newest first.
func (m *Modrinth) Versions(ctx context.Context, project string, fit Fit) ([]Version, error) {
	if err := checkID(project); err != nil {
		return nil, err
	}
	q := url.Values{"loaders": {jsonList(fit.Loaders)}}
	if fit.GameVersion != "" {
		q.Set("game_versions", jsonList([]string{fit.GameVersion}))
	}
	var out []Version
	err := m.do(ctx, http.MethodGet, "/project/"+url.PathEscape(project)+"/version", q, nil, &out)
	return out, err
}

// Version looks a version up.
func (m *Modrinth) Version(ctx context.Context, id string) (Version, error) {
	if err := checkID(id); err != nil {
		return Version{}, err
	}
	var v Version
	err := m.do(ctx, http.MethodGet, "/version/"+url.PathEscape(id), nil, nil, &v)
	return v, err
}

// ByHash finds the versions files belong to, by their SHA-512.
func (m *Modrinth) ByHash(ctx context.Context, hashes []string) (map[string]Version, error) {
	out := map[string]Version{}
	if len(hashes) == 0 {
		return out, nil
	}
	err := m.do(ctx, http.MethodPost, "/version_files", nil, map[string]any{"hashes": hashes, "algorithm": "sha512"}, &out)
	return out, err
}

// Latest finds the newest version that fits for each file, by SHA-512.
// Without a game version, any game version counts.
func (m *Modrinth) Latest(ctx context.Context, hashes []string, fit Fit) (map[string]Version, error) {
	out := map[string]Version{}
	if len(hashes) == 0 {
		return out, nil
	}
	body := map[string]any{"hashes": hashes, "algorithm": "sha512", "loaders": fit.Loaders}
	if fit.GameVersion != "" {
		body["game_versions"] = []string{fit.GameVersion}
	}
	err := m.do(ctx, http.MethodPost, "/version_files/update", nil, body, &out)
	return out, err
}

// Best picks the version to install: the newest release, or the newest of
// any kind if there's no release.
func Best(vs []Version) (Version, bool) {
	var best *Version
	for i := range vs {
		v := &vs[i]
		if _, ok := v.File(); !ok {
			continue
		}
		better := best == nil ||
			(v.VersionType == "release") != (best.VersionType == "release") && v.VersionType == "release" ||
			(v.VersionType == "release") == (best.VersionType == "release") && v.Published.After(best.Published)
		if better {
			best = v
		}
	}
	if best == nil {
		return Version{}, false
	}
	return *best, true
}

// checkURL makes sure a file comes from Modrinth's own CDN.
func (m *Modrinth) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && !(m.AllowHTTP && u.Scheme == "http") {
		return fmt.Errorf("%s: not HTTPS", raw)
	}
	for _, h := range m.CDN {
		if strings.EqualFold(u.Host, h) {
			return nil
		}
	}
	return fmt.Errorf("%s isn't on Modrinth's CDN", u.Host)
}

// Download fetches a file into a temporary file in dir, checking its size
// and SHA-512. The caller removes the file.
func (m *Modrinth) Download(ctx context.Context, f VFile, dir string) (string, error) {
	if err := m.checkURL(f.URL); err != nil {
		return "", err
	}
	want := strings.ToLower(f.Hashes["sha512"])
	if len(want) != 128 {
		return "", fmt.Errorf("%s: Modrinth gave no SHA-512 for it", f.Filename)
	}
	if f.Size > MaxFileSize {
		return "", fmt.Errorf("%s is larger than %d MiB", f.Filename, MaxFileSize>>20)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", m.UserAgent)
	// The client's timeout is for API calls; files take as long as they
	// take, within ctx.
	c := *m.Client
	c.Timeout = 0
	// Redirects have to stay on the CDN too.
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return m.checkURL(r.URL.String())
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", f.Filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", f.Filename, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, "mod-*.part")
	if err != nil {
		return "", err
	}
	h := sha512.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MaxFileSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	fail := func(err error) (string, error) {
		os.Remove(tmp.Name())
		return "", err
	}
	if err != nil {
		return fail(fmt.Errorf("download %s: %w", f.Filename, err))
	}
	if n > MaxFileSize {
		return fail(fmt.Errorf("%s is larger than %d MiB", f.Filename, MaxFileSize>>20))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fail(fmt.Errorf("%s doesn't match its SHA-512 on Modrinth; not installed", f.Filename))
	}
	return tmp.Name(), nil
}
