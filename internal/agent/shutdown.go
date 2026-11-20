package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// ShutdownPath is where hlm tells the agent to shut its host down (POST,
// an empty body), after a UPS battery ran low and hlm stopped the host's
// servers. The agent never does this on its own.
//
// The agent's container can't power the host off itself, so it starts a
// helper from its own image (already on the host, so nothing is pulled
// while the power is out) that shares the host's processes and runs the
// host's own poweroff in PID 1's namespaces, as nsenter does: systemd,
// OpenRC or SysV init shut down cleanly. hlm could start such a container
// through Docker anyway, so this gives it nothing new.
const ShutdownPath = "/_hlm/shutdown"

// shutdownContainerName names the agent's container as its installer does,
// for when the hostname isn't the container's ID.
const shutdownContainerName = "hlm-agent"

// LabelShutdownHelper marks the helper.
const LabelShutdownHelper = "io.hlm.helper"

// poweroffScript runs on the host; the pause lets the agent answer hlm
// first.
const poweroffScript = `sleep 3; systemctl poweroff || shutdown -h now || poweroff`

// Docker is what shutting down needs from the host's Docker.
type shutdownDocker interface {
	InspectContainer(ctx context.Context, id string) (docker.ContainerJSON, error)
	CreateContainer(ctx context.Context, name string, req docker.CreateRequest) (string, []string, error)
	StartContainer(ctx context.Context, id string) error
}

// shutdownHandler serves ShutdownPath.
func shutdownHandler(dc shutdownDocker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := shutdown(r.Context(), dc); err != nil {
			writeAgentError(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// ownImage is the image the agent's container runs.
func ownImage(ctx context.Context, dc shutdownDocker) (string, error) {
	var names []string
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	names = append(names, shutdownContainerName)
	for _, n := range names {
		if info, err := dc.InspectContainer(ctx, n); err == nil && info.Image != "" {
			return info.Image, nil
		}
	}
	return "", errors.New("can't find the agent's own container: shutting the host down needs the agent to run in Docker, as its install command starts it")
}

func shutdown(ctx context.Context, dc shutdownDocker) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	image, err := ownImage(ctx, dc)
	if err != nil {
		return err
	}
	id, _, err := dc.CreateContainer(ctx, "", docker.CreateRequest{
		Image:      image,
		Entrypoint: []string{"nsenter"},
		Cmd:        []string{"-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh", "-c", poweroffScript},
		Labels:     map[string]string{LabelShutdownHelper: "shutdown"},
		HostConfig: docker.HostConfig{Privileged: true, PidMode: "host", AutoRemove: true, NetworkMode: "none"},
	})
	if err != nil {
		return fmt.Errorf("create the shutdown helper: %w", err)
	}
	if err := dc.StartContainer(ctx, id); err != nil {
		return fmt.Errorf("start the shutdown helper: %w", err)
	}
	return nil
}

// Shutdown tells a host's agent to shut the host down.
func (h *Hub) Shutdown(ctx context.Context, host string) error {
	return h.post(ctx, host, ShutdownPath, struct{}{}, nil)
}
