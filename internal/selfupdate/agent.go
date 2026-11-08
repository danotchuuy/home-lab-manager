package selfupdate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// Updating hlm's agents on other Docker hosts the way hlm updates itself:
// the new image is pulled there, and a helper container (a copy of the
// agent's, running the new image's helper command) swaps the agent's
// container for one on the new image, restoring the old one if the new one
// doesn't stay up. hlm starts the helper through the agent, before the
// agent goes; the helper finishes on its own.

// releaseVersion matches the versions hlm's images are published under.
var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// IsRelease reports whether version is a published release (vX.Y.Z), which
// has an image to update agents to.
func IsRelease(version string) bool { return releaseVersion.MatchString(version) }

// AgentImage is the image for an agent of version: the agent's own image
// (from its container, say danotchuuy/home-lab-manager:latest) with the
// version's tag.
func AgentImage(current, version string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(current), "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	if name == "" {
		name = "danotchuuy/home-lab-manager"
	}
	return name + ":" + version
}

// AgentContainer finds the container an agent runs in among a host's
// containers: one running hlm with the agent command. It returns "" when
// there's none, and an error when there are several (agents of several
// hlms, which can't be told apart).
func AgentContainer(list []docker.ContainerSummary) (string, error) {
	var found []string
	for _, c := range list {
		f := strings.Fields(c.Command)
		if c.State != "running" || len(f) < 2 || f[1] != "agent" {
			continue
		}
		if b := path.Base(f[0]); b != "hlm" && b != "gsh" {
			continue
		}
		found = append(found, c.ID)
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d hlm agents run on this host, so hlm can't tell which is its own: update it yourself", len(found))
}

// StartHelper starts the update helper for the hlm container id (an
// agent's) on d, to swap it for one running image (already pulled there),
// and returns the helper's name. from and to are the versions, for its log.
func StartHelper(ctx context.Context, d *docker.Client, id, image, from, to string) (string, error) {
	img, err := d.InspectImage(ctx, image)
	if err != nil {
		return "", err
	}
	if len(img.Config.Entrypoint) == 0 {
		return "", fmt.Errorf("image %s has no entrypoint; it doesn't look like a hlm image", image)
	}
	if v := img.Config.Labels[versionLabel]; v != "" && v != to {
		return "", fmt.Errorf("%s is %s, not %s; the new image may not be published yet, so try again in a few minutes", image, v, to)
	}
	raw, err := d.InspectContainerRaw(ctx, id)
	if err != nil {
		return "", err
	}
	// The image it runs now, so what it only has from that image is left
	// for the new one's defaults.
	oldImg, err := d.InspectImage(ctx, str(raw, "Image"))
	if err != nil {
		return "", fmt.Errorf("read the agent's image: %w", err)
	}
	oldJSON, _ := json.Marshal(oldImg)
	body := helperConfig(raw, oldImg, image, img, []string{HelperCommand, id, image, from, to,
		base64.StdEncoding.EncodeToString(oldJSON)})
	// The helper reaches Docker where the agent does.
	if sock := agentSocket(raw); sock != "" {
		env, _ := body["Env"].([]any)
		body["Env"] = append(env, "DOCKER_HOST=unix://"+sock)
	}
	name := fmt.Sprintf("hlm-agent-update-%d", time.Now().Unix())
	hid, err := d.CreateContainerRaw(ctx, name, body)
	if err != nil {
		return "", fmt.Errorf("create the update helper: %w", err)
	}
	if err := d.StartContainer(ctx, hid); err != nil {
		_ = d.RemoveContainer(context.Background(), hid, true)
		return "", fmt.Errorf("start the update helper: %w", err)
	}
	return name, nil
}

// agentSocket is the Docker socket an agent was told to use (--socket, or
// HLM_AGENT_SOCKET), if not the default.
func agentSocket(raw map[string]any) string {
	cfg, _ := raw["Config"].(map[string]any)
	var sock string
	if env, ok := cfg["Env"].([]any); ok {
		for _, e := range env {
			if v, ok := strings.CutPrefix(fmt.Sprint(e), "HLM_AGENT_SOCKET="); ok {
				sock = v
			}
		}
	}
	if args, ok := cfg["Cmd"].([]any); ok {
		for i, a := range args {
			s := fmt.Sprint(a)
			if v, ok := strings.CutPrefix(s, "--socket="); ok {
				sock = v
			} else if (s == "--socket" || s == "-socket") && i+1 < len(args) {
				sock = fmt.Sprint(args[i+1])
			}
		}
	}
	if sock == "/var/run/docker.sock" {
		return ""
	}
	return sock
}
