package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRegistryAuthHeader(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{} // path -> X-Registry-Auth
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("X-Registry-Auth")
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			w.Write([]byte(`{"status":"Pulling"}` + "\n"))
		case strings.Contains(r.URL.Path, "/distribution/"):
			w.Write([]byte(`{"Descriptor":{"digest":"sha256:abc"}}`))
		case strings.HasSuffix(r.URL.Path, "/auth"):
			var a RegistryAuth
			json.NewDecoder(r.Body).Decode(&a)
			if a.Password != "right" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"login attempt to http://reg.lan:5000/v2/ failed with status: 401 Unauthorized"}`))
				return
			}
			w.Write([]byte(`{"Status":"Login Succeeded"}`))
		}
	}))
	defer srv.Close()
	c, err := NewClient(Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// No credentials: no header.
	if err := c.PullImage(ctx, "nginx", nil); err != nil {
		t.Fatal(err)
	}
	if h := got["/v1.47/images/create"]; h != "" {
		t.Errorf("header without credentials: %q", h)
	}

	c.SetCredentials(func(_ context.Context, ref string) (RegistryAuth, bool) {
		if strings.HasPrefix(ref, "reg.lan:5000/") {
			return RegistryAuth{Username: "u", Password: "p+/=?", ServerAddress: "reg.lan:5000"}, true
		}
		return RegistryAuth{}, false
	})
	if err := c.PullImage(ctx, "reg.lan:5000/team/app:v1", nil); err != nil {
		t.Fatal(err)
	}
	if d, err := c.RemoteDigest(ctx, "reg.lan:5000/team/app:v1"); err != nil || d != "sha256:abc" {
		t.Fatalf("digest %q, %v", d, err)
	}
	for _, path := range []string{"/v1.47/images/create", "/v1.47/distribution/reg.lan:5000/team/app:v1/json"} {
		raw, err := base64.URLEncoding.DecodeString(got[path])
		var a RegistryAuth
		if err != nil || json.Unmarshal(raw, &a) != nil || a != (RegistryAuth{"u", "p+/=?", "reg.lan:5000"}) {
			t.Errorf("%s: header %q decodes to %+v (%v)", path, got[path], a, err)
		}
	}
	// Another registry's image gets nothing.
	c.PullImage(ctx, "ghcr.io/x/y", nil)
	if h := got["/v1.47/images/create"]; h != "" {
		t.Errorf("header sent for another registry: %q", h)
	}

	if s, err := c.Login(ctx, RegistryAuth{Username: "u", Password: "right", ServerAddress: "reg.lan:5000"}); err != nil || s != "Login Succeeded" {
		t.Errorf("login: %q, %v", s, err)
	}
	if _, err := c.Login(ctx, RegistryAuth{Username: "u", Password: "wrong"}); !IsAuthError(err) {
		t.Errorf("wrong password: %v", err)
	}
}

func TestIsAuthError(t *testing.T) {
	for _, msg := range []string{
		"pull access denied for x/y, repository does not exist or may require 'docker login': denied: requested access to the resource is denied",
		"Head \"https://ghcr.io/v2/x/y/manifests/latest\": unauthorized",
		"Get \"http://reg.lan:5000/v2/team/app/manifests/v1\": no basic auth credentials",
		"docker: unauthorized: authentication required (status 401)",
	} {
		if !IsAuthError(errors.New(msg)) {
			t.Errorf("not an auth error: %q", msg)
		}
	}
	for _, msg := range []string{"manifest for nginx:nope not found", "dial tcp: connection refused"} {
		if IsAuthError(errors.New(msg)) {
			t.Errorf("auth error: %q", msg)
		}
	}
	if IsAuthError(nil) {
		t.Error("nil")
	}
}
