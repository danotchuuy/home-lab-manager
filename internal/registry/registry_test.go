package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestHostOf(t *testing.T) {
	tests := map[string]string{
		"nginx":                               "docker.io",
		"nginx:1.27":                          "docker.io",
		"library/nginx":                       "docker.io",
		"itzg/minecraft-server:java21":        "docker.io",
		"docker.io/x/y":                       "docker.io",
		"index.docker.io/x/y":                 "docker.io",
		"ghcr.io/x/y:tag":                     "ghcr.io",
		"GHCR.io/x/y":                         "ghcr.io",
		"host:5000/x@sha256:0123456789abcdef": "host:5000",
		"registry.lan:5000/team/app:v2":       "registry.lan:5000",
		"localhost/app":                       "localhost",
		"localhost:5000/app":                  "localhost:5000",
	}
	for ref, want := range tests {
		if got := HostOf(ref); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	good := map[string]string{
		"docker.io":                   "docker.io",
		"https://index.docker.io/v1/": "docker.io",
		" GHCR.IO ":                   "ghcr.io",
		"https://registry.lan:5000":   "registry.lan:5000",
		"10.0.0.5:5000":               "10.0.0.5:5000",
	}
	for in, want := range good {
		if got, err := NormalizeHost(in); err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "user@host", "host:port", "a b"} {
		if got, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want an error", bad, got)
		}
	}
}

func TestStore(t *testing.T) {
	sec, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	s := New(sec)
	ctx := context.Background()

	if _, ok := s.For(ctx, "nginx"); ok {
		t.Error("login with nothing saved")
	}
	if _, err := s.Add(Entry{Host: "https://index.docker.io/v1/", Username: "me", Password: "hub-token"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Entry{Host: "ghcr.io", Username: "me", Password: "ghp_x", Deployers: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Entry{Host: "docker.io", Username: "x", Password: "y"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.Add(Entry{Host: "reg.lan", Username: "x"}); err == nil {
		t.Error("added without a password")
	}

	list, _ := s.List()
	if len(list) != 2 || list[0].Host != "docker.io" || list[1].Host != "ghcr.io" || !list[1].Deployers {
		t.Errorf("list = %+v", list)
	}

	a, ok := s.For(ctx, "library/nginx")
	if !ok || a.Password != "hub-token" || a.ServerAddress != "https://index.docker.io/v1/" {
		t.Errorf("docker hub login = %+v, %v", a, ok)
	}
	if _, ok := s.For(ctx, "registry.lan/x"); ok {
		t.Error("login for an unknown registry")
	}
	// Confined installs only get logins allowed for deployers.
	if _, ok := s.For(Confined(ctx), "nginx"); ok {
		t.Error("confined install got Docker Hub's login")
	}
	if a, ok := s.For(Confined(ctx), "ghcr.io/me/private"); !ok || a.ServerAddress != "ghcr.io" {
		t.Errorf("confined ghcr = %+v, %v", a, ok)
	}

	// A blank password keeps the saved one.
	if _, err := s.Update("docker.io", Entry{Username: "me2", Deployers: true}); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get("index.docker.io"); e.Username != "me2" || e.Password != "hub-token" || !e.Deployers {
		t.Errorf("after update: %+v", e)
	}
	// ...but isn't moved to another registry.
	if _, err := s.Update("docker.io", Entry{Host: "evil.example", Username: "me2"}); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("moved the password: %v", err)
	}
	if _, err := s.Update("docker.io", Entry{Host: "ghcr.io", Username: "me2", Password: "p"}); !errors.Is(err, ErrExists) {
		t.Errorf("renamed onto another: %v", err)
	}
	if _, err := s.Update("quay.io", Entry{Username: "x", Password: "y"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}

	if err := s.Delete("docker.io"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("docker.io"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, ok := s.For(ctx, "nginx"); ok {
		t.Error("login after delete")
	}
	var nilStore *Store
	if _, ok := nilStore.For(ctx, "nginx"); ok {
		t.Error("nil store")
	}
}
