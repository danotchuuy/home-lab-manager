package selfupdate

import (
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestAgentImage(t *testing.T) {
	for in, want := range map[string]string{
		"danotchuuy/home-lab-manager:latest":          "danotchuuy/home-lab-manager:v1.2.3",
		"danotchuuy/home-lab-manager":                 "danotchuuy/home-lab-manager:v1.2.3",
		"registry.lan:5000/hlm:v1.0.0":                "registry.lan:5000/hlm:v1.2.3",
		"danotchuuy/home-lab-manager@sha256:abcd1234": "danotchuuy/home-lab-manager:v1.2.3",
		"": "danotchuuy/home-lab-manager:v1.2.3",
	} {
		if got := AgentImage(in, "v1.2.3"); got != want {
			t.Errorf("AgentImage(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsRelease("v0.0.45") || IsRelease("v0.0.45-3-gabc1234-dirty") || IsRelease("dev") {
		t.Error("IsRelease")
	}
}

func TestAgentContainer(t *testing.T) {
	agent := docker.ContainerSummary{ID: "a1", State: "running", Command: "/usr/local/bin/hlm agent --server https://hlm.example.com"}
	hlm := docker.ContainerSummary{ID: "h1", State: "running", Command: "/usr/local/bin/hlm"}
	game := docker.ContainerSummary{ID: "g1", State: "running", Command: "/start.sh agent"}
	stopped := docker.ContainerSummary{ID: "a0", State: "exited", Command: "hlm agent --server x"}
	if id, err := AgentContainer([]docker.ContainerSummary{hlm, game, stopped, agent}); id != "a1" || err != nil {
		t.Errorf("found %q %v", id, err)
	}
	if id, err := AgentContainer([]docker.ContainerSummary{hlm, game}); id != "" || err != nil {
		t.Errorf("none: %q %v", id, err)
	}
	two := agent
	two.ID = "a2"
	if _, err := AgentContainer([]docker.ContainerSummary{agent, two}); err == nil || !strings.Contains(err.Error(), "2 hlm agents") {
		t.Errorf("two: %v", err)
	}
}

func TestAgentSocket(t *testing.T) {
	raw := func(cmd []any, env []any) map[string]any {
		return map[string]any{"Config": map[string]any{"Cmd": cmd, "Env": env}}
	}
	for _, tt := range []struct {
		raw  map[string]any
		want string
	}{
		{raw([]any{"agent", "--server", "x"}, nil), ""},
		{raw([]any{"agent", "--socket", "/var/run/docker.sock"}, nil), ""},
		{raw([]any{"agent", "--socket=/run/user/1000/docker.sock"}, nil), "/run/user/1000/docker.sock"},
		{raw([]any{"agent", "-socket", "/srv/docker.sock"}, nil), "/srv/docker.sock"},
		{raw([]any{"agent"}, []any{"HLM_AGENT_SOCKET=/host/docker.sock", "TZ=UTC"}), "/host/docker.sock"},
	} {
		if got := agentSocket(tt.raw); got != tt.want {
			t.Errorf("%v: %q, want %q", tt.raw, got, tt.want)
		}
	}
}
