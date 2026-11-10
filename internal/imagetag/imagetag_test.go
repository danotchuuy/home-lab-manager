package imagetag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]Ref{
		"nginx":                        {Name: "nginx", Tag: "latest", Registry: "docker.io", Repo: "library/nginx"},
		"homebridge/homebridge:latest": {Name: "homebridge/homebridge", Tag: "latest", Registry: "docker.io", Repo: "homebridge/homebridge"},
		"docker.io/library/redis:7":    {Name: "docker.io/library/redis", Tag: "7", Registry: "docker.io", Repo: "library/redis"},
		"lscr.io/linuxserver/plex":     {Name: "lscr.io/linuxserver/plex", Tag: "latest", Registry: "lscr.io", Repo: "linuxserver/plex"},
		"localhost:5000/app:v1":        {Name: "localhost:5000/app", Tag: "v1", Registry: "localhost:5000", Repo: "app"},
		"ghcr.io/Owner/App:1.2.3-ls45": {Name: "ghcr.io/Owner/App", Tag: "1.2.3-ls45", Registry: "ghcr.io", Repo: "owner/app"},
	} {
		if got, ok := Parse(in); !ok || got != want {
			t.Errorf("%s: %+v", in, got)
		}
	}
	for _, in := range []string{"", "nginx@sha256:abc", "${IMAGE}", "sha256:abc"} {
		if _, ok := Parse(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
	r, _ := Parse("nginx")
	if got := RepoDigest(r, []string{"redis@sha256:1", "docker.io/library/nginx@sha256:2"}); got != "sha256:2" {
		t.Errorf("repo digest %q", got)
	}
}

func TestBest(t *testing.T) {
	for want, tags := range map[string][]string{
		"1.27.3":       {"latest", "1", "1.27", "1.27.3", "1.27.3-bookworm", "mainline", "bookworm"},
		"v2.1.7-ls236": {"latest", "v2.1.7", "v2.1.7-ls236", "version-v2.1.7"},
		"2026-11-02":   {"latest", "2026-11-02", "ubuntu"},
		"":             {"latest", "nightly-20260101", "amd64-1.2.3", "3f2a1b9c", "sha-3f2a1b9"},
	} {
		if got := Best(tags); got != want {
			t.Errorf("%v: %q, want %q", tags, got, want)
		}
	}
}

// A registry like GHCR: a token for anonymous pulls, tags by name only.
func TestRegistryVersionOf(t *testing.T) {
	digests := map[string]string{"latest": "sha256:new", "1.2.4": "sha256:new", "1.2": "sha256:new", "1.2.3": "sha256:old", "amd64-1.2.4": "sha256:x"}
	var heads atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(map[string]string{"token": "t"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:owner/app:pull"`)
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/v2/owner/app/tags/list":
			tags := []string{}
			for t := range digests {
				tags = append(tags, t)
			}
			json.NewEncoder(w).Encode(map[string]any{"tags": tags})
		case strings.HasPrefix(r.URL.Path, "/v2/owner/app/manifests/"):
			heads.Add(1)
			d, ok := digests[strings.TrimPrefix(r.URL.Path, "/v2/owner/app/manifests/")]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				t.Error("no index media type asked for")
			}
			w.Header().Set("Docker-Content-Digest", d)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	f := &Finder{Registry: srv.URL}
	ctx := context.Background()
	d, err := f.Digest(ctx, "ghcr.io/owner/app:latest")
	if err != nil || d != "sha256:new" {
		t.Fatalf("digest %q %v", d, err)
	}
	if v, err := f.VersionOf(ctx, "ghcr.io/owner/app", "sha256:new"); err != nil || v != "1.2.4" {
		t.Errorf("version of new: %q %v", v, err)
	}
	if v, err := f.VersionOf(ctx, "ghcr.io/owner/app", "sha256:old"); err != nil || v != "1.2.3" {
		t.Errorf("version of old: %q %v", v, err)
	}
	if _, err := f.VersionOf(ctx, "ghcr.io/owner/app", "sha256:gone"); !errors.Is(err, ErrNoVersion) {
		t.Errorf("unknown digest: %v", err)
	}
	// Asked again: from the cache.
	n := heads.Load()
	f.VersionOf(ctx, "ghcr.io/owner/app", "sha256:new")
	if heads.Load() != n {
		t.Error("looked up again")
	}
}

// Docker Hub lists each tag's digest.
func TestHubVersionOf(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/namespaces/library/repositories/nginx/tags" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{{"name": "1.27.3-bookworm", "digest": "sha256:a"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"next": "http://" + r.Host + r.URL.Path + "?page=2",
			"results": []map[string]string{{"name": "latest", "digest": "sha256:a"}, {"name": "1.27", "digest": "sha256:a"},
				{"name": "1.27.3", "digest": "sha256:a"}, {"name": "1.26.0", "digest": "sha256:b"}},
		})
	}))
	defer hub.Close()
	f := &Finder{Hub: hub.URL}
	if v, err := f.VersionOf(context.Background(), "nginx:latest", "sha256:a"); err != nil || v != "1.27.3" {
		t.Errorf("%q %v", v, err)
	}
}
