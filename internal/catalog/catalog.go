package catalog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultFeedURL is the Community Applications feed used by Unraid.
const DefaultFeedURL = "https://assets.ca.unraid.net/feed/applicationFeed.json"

// Catalog holds the app templates from the CA feed, cached on disk.
type Catalog struct {
	feedURL   string
	cachePath string
	http      *http.Client
	log       *slog.Logger

	mu          sync.RWMutex
	feedApps    []*App          // from the CA feed
	custom      map[string]*App // made from Docker images; see custom.go
	customPath  string
	apps        []*App // feed and custom apps together
	byID        map[string]*App
	repoWebUI   map[string]string // image name -> WebUI of the first template using it
	categories  []CategoryCount
	feedUpdated string
	fetchedAt   time.Time

	refreshMu sync.Mutex // serializes downloads
}

// CategoryCount is a category and the number of apps in it.
type CategoryCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// New creates a catalog that downloads feedURL and caches it in cacheDir.
func New(feedURL, cacheDir string, log *slog.Logger) *Catalog {
	if feedURL == "" {
		feedURL = DefaultFeedURL
	}
	return &Catalog{
		feedURL:   feedURL,
		cachePath: filepath.Join(cacheDir, "applicationFeed.json"),
		http:      &http.Client{Timeout: 2 * time.Minute},
		log:       log,
		byID:      map[string]*App{},
	}
}

// LoadCache loads the feed cached on disk by a previous run, if there is one.
func (c *Catalog) LoadCache() error {
	f, err := os.Open(c.cachePath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	if err := c.load(f); err != nil {
		return err
	}
	c.mu.Lock()
	c.fetchedAt = st.ModTime()
	c.mu.Unlock()
	return nil
}

// Refresh downloads the feed and replaces the catalog with it.
func (c *Catalog) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.feedURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("download feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download feed: %s", resp.Status)
	}

	// Write to a temporary file first so a failed download never replaces a
	// good cache.
	if err := os.MkdirAll(filepath.Dir(c.cachePath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.cachePath), "feed-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download feed: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return err
	}
	if err := c.load(tmp); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), c.cachePath); err != nil {
		return err
	}
	c.mu.Lock()
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// RunRefresher refreshes the feed every interval until ctx is done. It
// refreshes immediately if the cache is missing or older than interval.
func (c *Catalog) RunRefresher(ctx context.Context, interval time.Duration) {
	refresh := func() {
		start := time.Now()
		if err := c.Refresh(ctx); err != nil {
			c.log.Error("refresh app feed", "err", err)
			return
		}
		c.log.Info("refreshed app feed", "apps", c.Count(), "took", time.Since(start).Round(time.Millisecond))
	}

	if c.FetchedAt().IsZero() || time.Since(c.FetchedAt()) > interval {
		refresh()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

func (c *Catalog) load(r io.Reader) error {
	apps, updated, err := parseFeed(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.feedApps, c.feedUpdated = apps, updated
	c.rebuild()
	c.mu.Unlock()
	return nil
}

// rebuild merges the feed and custom apps into the searchable index. The
// caller holds c.mu.
func (c *Catalog) rebuild() {
	apps := make([]*App, 0, len(c.feedApps)+len(c.custom))
	apps = append(apps, c.feedApps...)
	ids := make([]string, 0, len(c.custom))
	for id := range c.custom {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		apps = append(apps, c.custom[id])
	}
	byID := make(map[string]*App, len(apps))
	repoWebUI := map[string]string{}
	counts := map[string]int{}
	for _, a := range apps {
		byID[a.ID] = a
		if key := repoName(a.Repository); a.WebUI != "" && repoWebUI[key] == "" {
			repoWebUI[key] = a.WebUI
		}
		for _, cat := range a.Categories {
			counts[cat]++
		}
	}
	cats := make([]CategoryCount, 0, len(counts))
	for name, n := range counts {
		cats = append(cats, CategoryCount{Name: name, Count: n})
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].Name < cats[j].Name })
	c.apps, c.byID, c.repoWebUI, c.categories = apps, byID, repoWebUI, cats
}

// repoName reduces an image reference to the name templates can be matched
// on, dropping the tag, the digest and the registry: projects publish the same
// image to Docker Hub and to registries such as ghcr.io.
func repoName(ref string) string {
	name, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ref)), "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	if host, rest, ok := strings.Cut(name, "/"); ok && strings.ContainsAny(host, ".:") {
		name = rest
	}
	return strings.TrimPrefix(name, "library/")
}

// WebUI returns the WebUI template for a server: its own template's if that
// has one, otherwise that of any template for the same image. A server
// installed by image name has no web UI of its own, since images rarely
// declare one, even when the catalog knows the app.
func (c *Catalog) WebUI(id, image string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if a, ok := c.byID[id]; ok && a.WebUI != "" {
		return a.WebUI
	}
	return c.repoWebUI[repoName(image)]
}

// Count returns the number of apps in the catalog.
func (c *Catalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.apps)
}

// FetchedAt returns when the feed was last downloaded.
func (c *Catalog) FetchedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt
}

// FeedUpdated returns the feed's own last-updated timestamp.
func (c *Catalog) FeedUpdated() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.feedUpdated
}

// Categories returns every category and how many apps are in it.
func (c *Catalog) Categories() []CategoryCount {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.categories
}

// Get returns the app with the given ID.
func (c *Catalog) Get(id string) (*App, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.byID[id]
	return a, ok
}

// Query selects apps from the catalog.
type Query struct {
	Text     string // matched against name, author, image and overview
	Category string // empty for all categories
	Sort     string // "popular" (default), "name" or "updated"
	Offset   int
	Limit    int
}

// Search returns the apps matching q and the total number of matches.
func (c *Catalog) Search(q Query) ([]*App, int) {
	c.mu.RLock()
	all := c.apps
	c.mu.RUnlock()

	terms := strings.Fields(strings.ToLower(q.Text))
	type scored struct {
		app   *App
		score int
	}
	var matches []scored
	for _, a := range all {
		if q.Category != "" && !a.HasCategory(q.Category) {
			continue
		}
		score, ok := matchScore(a, terms)
		if !ok {
			continue
		}
		matches = append(matches, scored{a, score})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.score != b.score {
			return a.score > b.score
		}
		switch q.Sort {
		case "name":
			return strings.ToLower(a.app.Name) < strings.ToLower(b.app.Name)
		case "updated":
			return a.app.LastUpdate > b.app.LastUpdate
		default:
			return a.app.Downloads > b.app.Downloads
		}
	})

	total := len(matches)
	if q.Offset > total {
		q.Offset = total
	}
	end := total
	if q.Limit > 0 && q.Offset+q.Limit < total {
		end = q.Offset + q.Limit
	}
	out := make([]*App, 0, end-q.Offset)
	for _, m := range matches[q.Offset:end] {
		out = append(out, m.app)
	}
	return out, total
}

// matchScore reports whether every term occurs in the app and ranks name
// matches above matches elsewhere. With no terms every app matches equally.
func matchScore(a *App, terms []string) (int, bool) {
	name := strings.ToLower(a.Name)
	score := 0
	for _, t := range terms {
		if !strings.Contains(a.searchText, t) {
			return 0, false
		}
		switch {
		case name == t:
			score += 100
		case strings.HasPrefix(name, t):
			score += 50
		case strings.Contains(name, t):
			score += 20
		}
	}
	return score, true
}
