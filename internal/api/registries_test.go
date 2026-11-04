package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/registry"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestRegistryEndpoints(t *testing.T) {
	// A Docker daemon whose registry takes one password.
	var lastAuth docker.RegistryAuth
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth"):
			json.NewDecoder(r.Body).Decode(&lastAuth)
			if lastAuth.Password != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"unauthorized: incorrect username or password"}`))
				return
			}
			w.Write([]byte(`{"Status":"Login Succeeded"}`))
		case strings.Contains(r.URL.Path, "/distribution/"):
			w.Write([]byte(`{"Descriptor":{"digest":"sha256:feed"}}`))
		}
	}))
	defer daemon.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(daemon.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{Options{Docker: dc, Registries: registry.New(store)}}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		if strings.Contains(rt.pattern, "/api/registries") {
			mux.HandleFunc(rt.pattern, rt.handler)
		}
	}
	do := func(method, path, body string) (int, string) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code, w.Body.String()
	}

	if code, body := do("POST", "/api/registries", `{"host":"https://Registry.lan:5000","username":"me","password":"s3cret"}`); code != 201 || !strings.Contains(body, `"host":"registry.lan:5000"`) {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, body := do("GET", "/api/registries", ""); code != 200 || strings.Contains(body, "s3cret") || !strings.Contains(body, `"username":"me"`) {
		t.Errorf("list: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/registries", `{"host":"registry.lan:5000","username":"x","password":"y"}`); code != http.StatusConflict {
		t.Errorf("duplicate: %d", code)
	}
	// Test the saved login, and look up an image with it.
	if code, body := do("POST", "/api/registries/test", `{"host":"registry.lan:5000","image":"registry.lan:5000/team/app:v1"}`); code != 200 || !strings.Contains(body, "sha256:feed") {
		t.Errorf("test saved: %d %s", code, body)
	}
	if lastAuth.Username != "me" || lastAuth.ServerAddress != "registry.lan:5000" {
		t.Errorf("sent %+v", lastAuth)
	}
	// A wrong password is a 400, not a 401 (which would sign the browser out).
	if code, body := do("POST", "/api/registries/test", `{"host":"registry.lan:5000","username":"me","password":"nope"}`); code != 400 || !strings.Contains(body, "incorrect username") {
		t.Errorf("test wrong: %d %s", code, body)
	}
	if code, _ := do("POST", "/api/registries/test", `{"host":"registry.lan:5000","image":"ghcr.io/x/y"}`); code != 400 {
		t.Errorf("image on another registry: %d", code)
	}
	if code, _ := do("POST", "/api/registries/test", `{"host":"quay.io","username":"me"}`); code != 400 {
		t.Errorf("test unsaved without a password: %d", code)
	}
	// Change it, keeping the password.
	if code, body := do("PUT", "/api/registries/registry.lan:5000", `{"username":"me2","deployers":true}`); code != 200 || !strings.Contains(body, `"deployers":true`) {
		t.Errorf("update: %d %s", code, body)
	}
	if e, _ := s.Registries.Get("registry.lan:5000"); e.Password != "s3cret" || e.Username != "me2" {
		t.Errorf("after update: %+v", e.Username)
	}
	if code, _ := do("DELETE", "/api/registries/registry.lan:5000", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do("DELETE", "/api/registries/registry.lan:5000", ""); code != 404 {
		t.Errorf("delete again: %d", code)
	}
}
