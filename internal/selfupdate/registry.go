package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// When hlm runs in a container, new versions are the version tags pushed
// to its image's registry (e.g. by `make docker-push`), whether or not a
// GitHub release was made for them.

// ErrNoImageTags is returned when the image has no version tags.
var ErrNoImageTags = errors.New("the image has no version tags (like v1.2.0) yet")

// registryRepo splits an image name into its registry's API base and the
// repository path, e.g. "owner/name" → ("https://registry-1.docker.io",
// "owner/name").
func registryRepo(image string) (base, repo string) {
	name := strings.ToLower(image)
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		if first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io" {
			name = rest
		} else {
			return "https://" + first, rest
		}
	}
	return "https://registry-1.docker.io", normalizeRepo(name)
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// registryGet fetches a registry API path, getting an anonymous token when
// the registry asks for one (as Docker Hub and GHCR do).
func registryGet(ctx context.Context, client *http.Client, u string) (*http.Response, error) {
	do := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "home-lab-manager")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}
	resp, err := do("")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return nil, errors.New("the registry requires sign-in; only public images can be checked")
	}
	params := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	if params["realm"] == "" {
		return nil, errors.New("the registry's sign-in challenge has no realm")
	}
	q := url.Values{}
	if params["service"] != "" {
		q.Set("service", params["service"])
	}
	if params["scope"] != "" {
		q.Set("scope", params["scope"])
	}
	treq, err := http.NewRequestWithContext(ctx, http.MethodGet, params["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	tresp, err := client.Do(treq)
	if err != nil {
		return nil, err
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry token: %s", tresp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tresp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("registry token: %w", err)
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	return do(tok.Token)
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// imageTags lists every tag of an image, following the registry's pages.
func imageTags(ctx context.Context, client *http.Client, base, repo string) ([]string, error) {
	var tags []string
	next := base + "/v2/" + repo + "/tags/list?n=1000"
	for page := 0; next != "" && page < 50; page++ {
		resp, err := registryGet(ctx, client, next)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return nil, fmt.Errorf("the registry has no image %s", repo)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("registry: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var list struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags: %w", err)
		}
		tags = append(tags, list.Tags...)
		next = ""
		if m := nextLink.FindStringSubmatch(link); m != nil {
			ref, err := url.Parse(m[1])
			if err == nil {
				b, _ := url.Parse(base)
				next = b.ResolveReference(ref).String()
			}
		}
	}
	return tags, nil
}

// latestImageTag finds the newest version tag of image. registry, if set,
// replaces the registry address (for tests). Prereleases are skipped.
func latestImageTag(ctx context.Context, client *http.Client, image, registry string) (*Release, error) {
	base, repo := registryRepo(image)
	if registry != "" {
		base = strings.TrimRight(registry, "/")
	}
	tags, err := imageTags(ctx, client, base, repo)
	if err != nil {
		return nil, err
	}
	best := ""
	for _, t := range tags {
		if v, ok := parseVersion(t); !ok || v.pre != "" {
			continue
		}
		if best == "" || newer(t, best) {
			best = t
		}
	}
	if best == "" {
		return nil, ErrNoImageTags
	}
	rel := &Release{Version: best, assets: map[string]string{}}
	if base == "https://registry-1.docker.io" {
		rel.URL = "https://hub.docker.com/r/" + strings.TrimPrefix(repo, "library/") + "/tags"
	}
	return rel, nil
}
