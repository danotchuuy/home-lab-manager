// Package imagetag finds an image's version tags: which version a tag such
// as latest is, by the digest the registry gives it. hlm pins Compose
// stacks' latest images to versions with it, and follows latest from one
// version to the next.
package imagetag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Latest is the tag followed.
const Latest = "latest"

// ErrNoVersion is returned when no version tag has the digest.
var ErrNoVersion = errors.New("no version tag of the image has that digest")

// Ref is an image reference, split up.
type Ref struct {
	// Name is the image as written, without its tag ("homebridge/homebridge").
	Name string
	// Tag is its tag: latest when it has none.
	Tag string
	// Registry is the registry's host (docker.io for Docker Hub), and Repo
	// the repository on it ("library/nginx" for an official image).
	Registry, Repo string
}

// Parse splits an image reference. ok is false for a digest reference or
// one made from variables.
func Parse(image string) (Ref, bool) {
	image = strings.TrimSpace(image)
	if image == "" || strings.ContainsAny(image, "@$ ") || strings.HasPrefix(image, "sha256:") {
		return Ref{}, false
	}
	name, tag := docker.SplitImageRef(image)
	r := Ref{Name: name, Tag: tag}
	lower := strings.ToLower(name)
	first, rest, found := strings.Cut(lower, "/")
	switch {
	case found && first != "docker.io" && first != "index.docker.io" && first != "registry-1.docker.io" &&
		(strings.ContainsAny(first, ".:") || first == "localhost"):
		r.Registry, r.Repo = first, rest
	default:
		r.Registry = "docker.io"
		if found && (first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io") {
			lower = rest
		}
		if !strings.Contains(lower, "/") {
			lower = "library/" + lower
		}
		r.Repo = lower
	}
	return r, true
}

// String is the reference with its tag.
func (r Ref) String() string { return r.Name + ":" + r.Tag }

// WithTag is the image, as written, with another tag.
func (r Ref) WithTag(tag string) string { return r.Name + ":" + tag }

// Same says whether two references are the same repository.
func (r Ref) Same(o Ref) bool { return r.Registry == o.Registry && r.Repo == o.Repo }

// RepoDigest finds the digest an image was pulled as for ref's repository,
// among an image's RepoDigests ("name@sha256:…").
func RepoDigest(ref Ref, repoDigests []string) string {
	for _, rd := range repoDigests {
		name, digest, ok := strings.Cut(rd, "@")
		if !ok {
			continue
		}
		if r, ok := Parse(name); ok && r.Same(ref) {
			return digest
		}
	}
	return ""
}

// Finder asks registries about tags.
type Finder struct {
	HTTP *http.Client
	// Auth gives the login for an image's registry, if hlm has one.
	Auth func(ctx context.Context, image string) (docker.RegistryAuth, bool)
	// Registry and Hub replace the registry's and Docker Hub's API
	// addresses, for tests.
	Registry, Hub string

	mu     sync.Mutex
	cache  map[string]cached // by repository and digest
	tokens map[string]string // by registry API base and scope
}

type cached struct {
	tag string
	at  time.Time
}

// cacheFor is how long an answer is kept: a version tag's digest doesn't
// change, so only the misses need asking again.
const cacheFor = 6 * time.Hour

func (f *Finder) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *Finder) base(r Ref) string {
	if f.Registry != "" {
		return strings.TrimRight(f.Registry, "/")
	}
	if r.Registry == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + r.Registry
}

// manifestTypes are the manifests a registry may answer with: an image
// index's digest is what Docker records when it pulls a tag.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}

// Digest is the registry's digest for image's tag.
func (f *Finder) Digest(ctx context.Context, image string) (string, error) {
	r, ok := Parse(image)
	if !ok {
		return "", fmt.Errorf("%s isn't an image tag", image)
	}
	return f.digest(ctx, r, r.Tag)
}

func (f *Finder) digest(ctx context.Context, r Ref, tag string) (string, error) {
	u := f.base(r) + "/v2/" + r.Repo + "/manifests/" + url.PathEscape(tag)
	resp, err := f.get(ctx, r, http.MethodHead, u, strings.Join(manifestTypes, ", "))
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s:%s: registry says %s", r.Name, tag, resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", fmt.Errorf("%s:%s: the registry gave no digest", r.Name, tag)
	}
	return d, nil
}

// VersionOf finds the version tag of image's repository whose digest is
// digest: the most exact one when several are (1.2.3 rather than 1.2).
func (f *Finder) VersionOf(ctx context.Context, image, digest string) (string, error) {
	r, ok := Parse(image)
	if !ok {
		return "", fmt.Errorf("%s isn't an image tag", image)
	}
	key := r.Registry + "/" + r.Repo + "@" + digest
	f.mu.Lock()
	if c, ok := f.cache[key]; ok && (c.tag != "" || time.Since(c.at) < 10*time.Minute) && time.Since(c.at) < cacheFor {
		f.mu.Unlock()
		if c.tag == "" {
			return "", ErrNoVersion
		}
		return c.tag, nil
	}
	f.mu.Unlock()

	var matches []string
	var err error
	if r.Registry == "docker.io" && f.Registry == "" || f.Hub != "" {
		matches, err = f.hubMatches(ctx, r, digest)
		if err != nil {
			// A private repository, say: ask the registry itself.
			matches, err = f.registryMatches(ctx, r, digest)
		}
	} else {
		matches, err = f.registryMatches(ctx, r, digest)
	}
	if err != nil {
		return "", err
	}
	best := Best(matches)
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]cached{}
	}
	f.cache[key] = cached{tag: best, at: time.Now()}
	f.mu.Unlock()
	if best == "" {
		return "", ErrNoVersion
	}
	return best, nil
}

// hubMatches asks Docker Hub's API, which lists each tag's digest, newest
// first.
func (f *Finder) hubMatches(ctx context.Context, r Ref, digest string) ([]string, error) {
	base := cmpOr(strings.TrimRight(f.Hub, "/"), "https://hub.docker.com")
	ns, repo, _ := strings.Cut(r.Repo, "/")
	next := fmt.Sprintf("%s/v2/namespaces/%s/repositories/%s/tags?page_size=100", base, url.PathEscape(ns), url.PathEscape(repo))
	var matches []string
	extra := 0 // pages read after the first match: tags pushed together may straddle one
	for page := 0; next != "" && page < 20 && extra < 2; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "home-lab-manager")
		resp, err := f.client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("docker hub: %s", resp.Status)
		}
		var list struct {
			Next    string `json:"next"`
			Results []struct {
				Name   string `json:"name"`
				Digest string `json:"digest"`
			} `json:"results"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("docker hub: %w", err)
		}
		for _, t := range list.Results {
			if t.Digest == digest && VersionLike(t.Name) {
				matches = append(matches, t.Name)
			}
		}
		if len(matches) > 0 {
			extra++
		}
		next = list.Next
	}
	return matches, nil
}

// maxLookups is how many version tags are looked up on a registry that
// only lists their names, newest first.
const maxLookups = 60

// registryMatches lists the repository's tags and looks up the newest
// version tags' digests.
func (f *Finder) registryMatches(ctx context.Context, r Ref, digest string) ([]string, error) {
	tags, err := f.tags(ctx, r)
	if err != nil {
		return nil, err
	}
	var cand []string
	for _, t := range tags {
		if VersionLike(t) {
			cand = append(cand, t)
		}
	}
	slices.SortStableFunc(cand, func(a, b string) int { return compareNumbers(numbers(b), numbers(a)) })
	if len(cand) > maxLookups {
		cand = cand[:maxLookups]
	}
	var (
		mu      sync.Mutex
		matches []string
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
	)
	for _, t := range cand {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if d, err := f.digest(ctx, r, t); err == nil && d == digest {
				mu.Lock()
				matches = append(matches, t)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return matches, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// tags lists a repository's tags, following the registry's pages.
func (f *Finder) tags(ctx context.Context, r Ref) ([]string, error) {
	base := f.base(r)
	var tags []string
	next := base + "/v2/" + r.Repo + "/tags/list?n=1000"
	for page := 0; next != "" && page < 50; page++ {
		resp, err := f.get(ctx, r, http.MethodGet, next, "")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("%s: registry says %s: %s", r.Name, resp.Status, strings.TrimSpace(string(body)))
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
			if ref, err := url.Parse(m[1]); err == nil {
				b, _ := url.Parse(base)
				next = b.ResolveReference(ref).String()
			}
		}
	}
	return tags, nil
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// get makes a registry request, getting a token when the registry asks
// for one (with hlm's login for it, if it has one, else anonymously).
func (f *Finder) get(ctx context.Context, r Ref, method, u, accept string) (*http.Response, error) {
	do := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "home-lab-manager")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		return f.client().Do(req)
	}
	tokenKey := f.base(r) + " " + r.Repo
	f.mu.Lock()
	token := f.tokens[tokenKey]
	f.mu.Unlock()
	resp, err := do(token)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	var auth docker.RegistryAuth
	var haveAuth bool
	if f.Auth != nil {
		auth, haveAuth = f.Auth(ctx, r.Name)
	}
	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if !haveAuth {
			return nil, fmt.Errorf("%s's registry needs a login; add one under Settings → Registries", r.Name)
		}
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth(auth.Username, auth.Password)
		token = req.Header.Get("Authorization")
	case "bearer":
		p := map[string]string{}
		for _, m := range authParam.FindAllStringSubmatch(params, -1) {
			p[strings.ToLower(m[1])] = m[2]
		}
		if p["realm"] == "" {
			return nil, errors.New("the registry's sign-in challenge has no realm")
		}
		q := url.Values{}
		if p["service"] != "" {
			q.Set("service", p["service"])
		}
		q.Set("scope", cmpOr(p["scope"], "repository:"+r.Repo+":pull"))
		treq, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		treq.Header.Set("User-Agent", "home-lab-manager")
		if haveAuth {
			treq.SetBasicAuth(auth.Username, auth.Password)
		}
		tresp, err := f.client().Do(treq)
		if err != nil {
			return nil, err
		}
		defer tresp.Body.Close()
		if tresp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("registry token for %s: %s", r.Name, tresp.Status)
		}
		var tok struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(tresp.Body, 1<<20)).Decode(&tok); err != nil {
			return nil, fmt.Errorf("registry token: %w", err)
		}
		token = "Bearer " + cmpOr(tok.Token, tok.AccessToken)
	default:
		return nil, fmt.Errorf("%s's registry wants a sign-in hlm can't do (%q)", r.Name, scheme)
	}
	f.mu.Lock()
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	f.tokens[tokenKey] = token
	f.mu.Unlock()
	return do(token)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- Which tags are versions ---

var (
	digitRun  = regexp.MustCompile(`\d+`)
	allDigits = regexp.MustCompile(`^\d+$`)
	hexHash   = regexp.MustCompile(`^(sha-|sha256-|git-)?[0-9a-f]{7,64}$`)
	// archWords mark a single platform's tag, whose digest is never the
	// multi-platform one.
	archWords = map[string]bool{"amd64": true, "arm64": true, "arm64v8": true, "armhf": true, "arm32v6": true, "arm32v7": true,
		"armv6": true, "armv7": true, "armel": true, "aarch64": true, "i386": true, "386": true, "ppc64le": true, "s390x": true,
		"riscv64": true, "x86_64": true, "x86-64": true, "arm": true, "windows": true}
	// unstable are words of tags that move, like latest does.
	unstable = map[string]bool{"latest": true, "nightly": true, "dev": true, "develop": true, "edge": true, "master": true,
		"main": true, "testing": true, "unstable": true, "beta": true, "alpha": true, "next": true}
)

// VersionLike says whether a tag looks like a version: it has a number,
// and isn't a commit hash, one platform's, or a moving tag.
func VersionLike(tag string) bool {
	t := strings.ToLower(tag)
	if !digitRun.MatchString(t) || hexHash.MatchString(t) && !allDigits.MatchString(t) {
		return false
	}
	for _, w := range strings.FieldsFunc(t, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if archWords[w] || unstable[w] {
			return false
		}
	}
	return true
}

// numbers are a tag's numbers, in order.
func numbers(tag string) []int {
	var out []int
	for _, s := range digitRun.FindAllString(tag, -1) {
		n, err := strconv.Atoi(s)
		if err != nil {
			n = int(^uint(0) >> 1)
		}
		out = append(out, n)
	}
	return out
}

func compareNumbers(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// Best picks the tag to pin to among tags with the same digest: the most
// exact (the most numbers), then one that starts with its version (1.2.3
// over version-1.2.3), then the shortest (1.2.3 over 1.2.3-bookworm).
func Best(tags []string) string {
	best := ""
	for _, t := range tags {
		if !VersionLike(t) {
			continue
		}
		if best == "" || better(t, best) {
			best = t
		}
	}
	return best
}

func better(a, b string) bool {
	na, nb := len(numbers(a)), len(numbers(b))
	if na != nb {
		return na > nb
	}
	sa, sb := startsWithVersion(a), startsWithVersion(b)
	if sa != sb {
		return sa
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func startsWithVersion(t string) bool {
	t = strings.TrimPrefix(strings.ToLower(t), "v")
	return t != "" && t[0] >= '0' && t[0] <= '9'
}
