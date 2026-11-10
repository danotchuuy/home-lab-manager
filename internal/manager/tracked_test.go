package manager

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestCheckTracked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.47")
		case strings.Contains(r.URL.Path, "/images/"):
			w.Write([]byte(`{"Id":"sha256:hb1","RepoDigests":["homebridge/homebridge@sha256:old"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	versions := map[string]string{"sha256:old": "2026-11-02", "sha256:new": "2026-11-09"}
	m.UseStacks(StackHooks{
		Tracks: func(stack, service string) string {
			if stack == "hb" && service == "homebridge" {
				return "latest"
			}
			return ""
		},
		VersionOf: func(_ context.Context, _, digest string) (string, error) {
			if v, ok := versions[digest]; ok {
				return v, nil
			}
			return "", errors.New("none")
		},
	})
	latest := "sha256:new"
	remote := func(_ context.Context, _, ref string) (string, error) {
		if ref != "homebridge/homebridge:latest" {
			t.Errorf("looked up %s", ref)
		}
		return latest, nil
	}
	hb := Server{ID: "a", Name: "hb_homebridge_1", Image: "homebridge/homebridge:2026-11-02", Stack: "hb", StackService: "homebridge"}
	current := UpdateStatus{State: UpdateCurrent}

	st := m.checkTracked(context.Background(), hb, current, remote)
	if st.State != UpdateAvailable || st.Version != "2026-11-09" || !strings.Contains(st.Reason, "version 2026-11-09 is out") {
		t.Errorf("latest moved on: %+v", st)
	}
	// Latest is what it runs.
	latest = "sha256:old"
	if st := m.checkTracked(context.Background(), hb, current, remote); st != current {
		t.Errorf("up to date: %+v", st)
	}
	// No version for latest's digest: its own tag's status stands.
	latest = "sha256:unknown"
	if st := m.checkTracked(context.Background(), hb, current, remote); st != current {
		t.Errorf("no version: %+v", st)
	}
	// Servers that follow nothing aren't looked at.
	other := hb
	other.StackService = "db"
	if st := m.checkTracked(context.Background(), other, current, remote); st != current {
		t.Errorf("not followed: %+v", st)
	}
}
