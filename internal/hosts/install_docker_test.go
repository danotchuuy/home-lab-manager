package hosts

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDockerInstallSteps runs each distribution's install steps in a
// throwaway container of it, and checks Docker's packages end up installed.
// (Starting Docker needs systemd, which these containers don't run.) It
// downloads a few hundred MB, so it's opt-in twice over:
// HLM_DOCKER_TESTS=1 HLM_INSTALL_TESTS=1.
func TestDockerInstallSteps(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" || os.Getenv("HLM_INSTALL_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 and HLM_INSTALL_TESTS=1 to install Docker in throwaway containers")
	}
	for _, tt := range []struct{ distro, image string }{
		{"ubuntu", "ubuntu:24.04"},
		{"debian", "debian:13"},
		{"fedora", "fedora:43"},
		{"centos", "quay.io/centos/centos:stream9"},
	} {
		t.Run(tt.distro, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			script := "set -e\n" + installSteps(tt.distro) + "\ndocker --version\ndockerd --version\ncontainerd --version\n"
			if tt.distro == "ubuntu" || tt.distro == "debian" {
				// Base images lack what the probe would find on a real host.
				script = "apt-get update -qq && apt-get install -y -qq ca-certificates curl >/dev/null\n" + script
			}
			out, err := exec.CommandContext(ctx, "docker", "run", "--rm", tt.image, "sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, tail(string(out), 40))
			}
			if !strings.Contains(string(out), "Docker version") || !strings.Contains(string(out), "containerd") {
				t.Errorf("no Docker after installing:\n%s", tail(string(out), 20))
			}
			t.Log(tail(string(out), 3))
		})
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
