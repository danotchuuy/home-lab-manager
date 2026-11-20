package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

type fakeShutdownDocker struct {
	containers map[string]string // name → image
	created    []docker.CreateRequest
	started    []string
}

func (f *fakeShutdownDocker) InspectContainer(_ context.Context, id string) (docker.ContainerJSON, error) {
	img, ok := f.containers[id]
	if !ok {
		return docker.ContainerJSON{}, errors.New("no such container")
	}
	return docker.ContainerJSON{ID: id, Image: img}, nil
}

func (f *fakeShutdownDocker) CreateContainer(_ context.Context, _ string, req docker.CreateRequest) (string, []string, error) {
	f.created = append(f.created, req)
	return "helper1", nil, nil
}

func (f *fakeShutdownDocker) StartContainer(_ context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}

func TestShutdown(t *testing.T) {
	host, _ := os.Hostname()
	// Found by its hostname (the container's ID), or else by name.
	for _, containers := range []map[string]string{
		{host: "sha256:agentimage"},
		{"hlm-agent": "sha256:agentimage"},
	} {
		fd := &fakeShutdownDocker{containers: containers}
		h := Handler(http.NotFoundHandler(), nil, nil, nil, nil, nil, shutdownHandler(fd))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", ShutdownPath, strings.NewReader("{}")))
		if w.Code != http.StatusAccepted {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		if len(fd.created) != 1 || !slices.Equal(fd.started, []string{"helper1"}) {
			t.Fatalf("created %+v, started %v", fd.created, fd.started)
		}
		req := fd.created[0]
		hc := req.HostConfig
		if req.Image != "sha256:agentimage" || !hc.Privileged || hc.PidMode != "host" || !hc.AutoRemove ||
			!slices.Equal(req.Entrypoint, []string{"nsenter"}) || !slices.Contains(req.Cmd, "-t") ||
			!strings.Contains(req.Cmd[len(req.Cmd)-1], "poweroff") || req.Labels[LabelShutdownHelper] != "shutdown" {
			t.Errorf("helper = %+v", req)
		}
	}

	// Not in Docker: it says so, and starts nothing.
	fd := &fakeShutdownDocker{containers: map[string]string{}}
	if err := shutdown(context.Background(), fd); err == nil || !strings.Contains(err.Error(), "run in Docker") {
		t.Errorf("outside Docker: %v", err)
	}
	if len(fd.created) != 0 {
		t.Error("a helper was made outside Docker")
	}
	if Allowed("POST", ShutdownPath) {
		t.Error("shutdown path in the Docker allow-list")
	}
	// Only POST.
	fd = &fakeShutdownDocker{containers: map[string]string{"hlm-agent": "img"}}
	h := Handler(http.NotFoundHandler(), nil, nil, nil, nil, nil, shutdownHandler(fd))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", ShutdownPath, nil))
	if len(fd.created) != 0 {
		t.Error("GET shut the host down")
	}
}
