package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryRepo(t *testing.T) {
	for in, want := range map[string]string{
		"danotchuuy/home-lab-manager": "https://registry-1.docker.io danotchuuy/home-lab-manager",
		"docker.io/Owner/App":         "https://registry-1.docker.io owner/app",
		"nginx":                       "https://registry-1.docker.io library/nginx",
		"ghcr.io/owner/app":           "https://ghcr.io owner/app",
		"registry.lan:5000/team/hlm":  "https://registry.lan:5000 team/hlm",
		"localhost/hlm":               "https://localhost hlm",
	} {
		if b, r := registryRepo(in); b+" "+r != want {
			t.Errorf("%s: got %s %s, want %s", in, b, r, want)
		}
	}
}

// fakeRegistry imitates Docker Hub: tag lists need an anonymous token and
// come in pages.
func fakeRegistry(t *testing.T, tags []string) *httptest.Server {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:me/hlm:pull" || r.URL.Query().Get("service") != "fake" {
			http.Error(w, "bad scope", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
	})
	mux.HandleFunc("/v2/me/hlm/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="fake",scope="repository:me/hlm:pull"`)
			w.WriteHeader(401)
			return
		}
		page := tags[:len(tags)/2]
		if r.URL.Query().Get("last") != "" {
			page = tags[len(tags)/2:]
		} else {
			w.Header().Set("Link", `</v2/me/hlm/tags/list?n=1000&last=x>; rel="next"`)
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "me/hlm", "tags": page})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestImageTag(t *testing.T) {
	// The newest version is on the second page; prereleases and moving tags
	// are skipped.
	srv := fakeRegistry(t, []string{"latest", "v0.0.1", "v0.0.3", "v0.0.10-rc1", "sha-abc", "v0.0.9", "v0.0.2"})
	rel, err := latestImageTag(context.Background(), srv.Client(), "me/hlm", srv.URL)
	if err != nil || rel.Version != "v0.0.9" {
		t.Fatalf("latest = %+v, %v", rel, err)
	}
	srv = fakeRegistry(t, []string{"latest", "dev"})
	if _, err := latestImageTag(context.Background(), srv.Client(), "me/hlm", srv.URL); !errors.Is(err, ErrNoImageTags) {
		t.Errorf("no versions: %v", err)
	}
	if _, err := latestImageTag(context.Background(), srv.Client(), "other/hlm", srv.URL); err == nil || !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "no image") {
		t.Errorf("unknown image: %v", err)
	}
}

func TestContainerCheckUsesImageTags(t *testing.T) {
	reg := fakeRegistry(t, []string{"v0.0.2", "latest", "v0.0.3"})
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/me/hlm/releases/tags/v0.0.3" {
			http.NotFound(w, r) // no "latest" release on GitHub
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.0.3", "body": "Faster installs.", "html_url": "https://example.com/r"})
	}))
	defer gh.Close()
	u := &Updater{
		cfg:  Config{Version: "v0.0.2", Repo: "me/hlm", Image: "me/hlm", Registry: reg.URL, APIBase: gh.URL},
		http: http.DefaultClient, mode: ModeContainer,
	}
	st := u.Check(context.Background())
	if st.CheckError != "" || !st.Available || st.Latest.Version != "v0.0.3" || st.Latest.Notes != "Faster installs." {
		t.Fatalf("status = %+v (latest %+v)", st, st.Latest)
	}
	u.cfg.Version = "v0.0.3"
	if st := u.Check(context.Background()); st.Available || st.CheckError != "" {
		t.Errorf("up to date: %+v", st)
	}
}
