// Package selfupdate checks for newer versions of hlm (GitHub releases, or
// the image's version tags when hlm runs in a container) and installs them,
// either by replacing the running binary or by recreating hlm's container
// on the new image.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Release is a published version.
type Release struct {
	Version   string    `json:"version"` // the git tag, e.g. v1.4.0
	Name      string    `json:"name,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	URL       string    `json:"url,omitempty"`
	Published time.Time `json:"published,omitzero"` // unknown for image tags
	assets    map[string]string
}

// ErrNoReleases is returned when the repository has no published release.
var ErrNoReleases = errors.New("no GitHub releases have been published yet; binary installs update from them, and pushing a v* tag runs the release workflow that makes one")

// latestRelease fetches the newest non-prerelease from the GitHub API.
func latestRelease(ctx context.Context, client *http.Client, apiBase, repo string) (*Release, error) {
	return fetchRelease(ctx, client, strings.TrimRight(apiBase, "/")+"/repos/"+repo+"/releases/latest")
}

// releaseByTag fetches the GitHub release for a tag.
func releaseByTag(ctx context.Context, client *http.Client, apiBase, repo, tag string) (*Release, error) {
	return fetchRelease(ctx, client, strings.TrimRight(apiBase, "/")+"/repos/"+repo+"/releases/tags/"+url.PathEscape(tag))
}

func fetchRelease(ctx context.Context, client *http.Client, url string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "home-lab-manager")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoReleases
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if raw.TagName == "" {
		return nil, errors.New("release has no tag")
	}
	r := &Release{
		Version:   raw.TagName,
		Name:      raw.Name,
		Notes:     raw.Body,
		URL:       raw.HTMLURL,
		Published: raw.PublishedAt,
		assets:    map[string]string{},
	}
	for _, a := range raw.Assets {
		r.assets[a.Name] = a.URL
	}
	return r, nil
}

type semver struct {
	major, minor, patch int
	pre                 string
}

func parseVersion(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+") // build metadata doesn't affect ordering
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return semver{}, false
		}
		n[i] = x
	}
	return semver{n[0], n[1], n[2], pre}, true
}

// newer reports whether version a is newer than b. Versions that aren't
// semantic versions (like "dev") are never newer or older.
func newer(a, b string) bool {
	va, ok1 := parseVersion(a)
	vb, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	for _, d := range [][2]int{{va.major, vb.major}, {va.minor, vb.minor}, {va.patch, vb.patch}} {
		if d[0] != d[1] {
			return d[0] > d[1]
		}
	}
	// A release is newer than its own prerelease; prereleases compare as text.
	switch {
	case va.pre == vb.pre:
		return false
	case va.pre == "":
		return true
	case vb.pre == "":
		return false
	default:
		return va.pre > vb.pre
	}
}
