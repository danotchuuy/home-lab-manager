package manager

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// TestDockerCleanup runs against the real daemon (HLM_DOCKER_TESTS=1): an
// "update" moves a tag to a new image, leaving the old one untagged. While a
// stopped container uses it, it isn't a candidate; once that's gone, it's
// an old image, and removing it after the update deletes it.
func TestDockerCleanup(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run against Docker")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	build := func(n string) string {
		cmd := exec.Command("docker", "build", "-q", "-t", "hlm-cleanup-test:latest", "-")
		cmd.Stdin = strings.NewReader("FROM busybox:1.37\nLABEL io.hlm.test=cleanup-" + n + "\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("pull", "-q", "busybox:1.37")
	oldID := build("1")
	run("create", "--name", "hlm-cleanup-test", "--label", "io.hlm.managed=true", "hlm-cleanup-test:latest", "true")
	newID := build("2") // the tag moves: oldID is untagged now
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", "hlm-cleanup-test").Run()
		exec.Command("docker", "rmi", "hlm-cleanup-test:latest", oldID).Run()
	})

	dc, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	m.UseOldImageRemoval(func() bool { return true })
	ctx := context.Background()
	has := func(id string) bool {
		plan, err := m.CleanupCandidates(ctx, LocalHost)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range plan.Items {
			if it.ID == id {
				return it.Kind == CleanOldImage && it.Default
			}
		}
		return false
	}
	if has(oldID) {
		t.Fatal("an image a stopped container uses is a candidate")
	}
	if res, _ := m.Cleanup(ctx, LocalHost, CleanupRequest{Images: []string{oldID}}); len(res.Removed) != 0 {
		t.Fatal("deleted an image in use")
	}
	m.removeOldImage(ctx, LocalHost, oldID, newID, func(Event) {}) // still used: kept
	if _, err := dc.InspectImage(ctx, oldID); err != nil {
		t.Fatalf("the old image went while in use: %v", err)
	}

	run("rm", "hlm-cleanup-test")
	if !has(oldID) {
		t.Fatal("the unused old image isn't a candidate")
	}
	var msgs []string
	m.removeOldImage(ctx, LocalHost, oldID, newID, func(e Event) { msgs = append(msgs, e.Message) })
	if _, err := dc.InspectImage(ctx, oldID); !docker.IsNotFound(err) || len(msgs) != 1 {
		t.Errorf("old image after removal: %v, events %q", err, msgs)
	}
	if _, err := dc.InspectImage(ctx, newID); err != nil {
		t.Errorf("the new image went too: %v", err)
	}
}
