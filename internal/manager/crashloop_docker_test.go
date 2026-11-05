package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// A busybox server that exits every second with restart "always" is stopped
// after 3 exits. Enable with HLM_DOCKER_TESTS=1.
func TestDockerCrashLoop(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PullImage(ctx, "busybox:1.37", nil); err != nil {
		t.Fatal(err)
	}
	m := New(d, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	n := &fakeNotifier{}
	m.UseNotifier(n)
	m.UseHealthPolicy(limitPolicy{exits: 3, window: 5 * time.Minute})
	go m.WatchEvents(ctx)
	time.Sleep(500 * time.Millisecond) // let the event stream start

	name := fmt.Sprintf("hlm-test-crashloop-%d", time.Now().UnixNano())
	id, _, err := d.CreateContainer(ctx, name, docker.CreateRequest{
		Image:  "busybox:1.37",
		Cmd:    []string{"sh", "-c", "echo loading; sleep 1; echo 'ERROR: bad config'; exit 2"},
		Labels: map[string]string{deploy.LabelManaged: "true"},
		HostConfig: docker.HostConfig{
			RestartPolicy: docker.RestartPolicy{Name: "always"},
			NetworkMode:   "none",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveContainer(context.Background(), id, true) })
	if err := d.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}

	e := n.waitForWithin(t, notify.KindServerCrashLoop, time.Minute)
	if e.Server != name || e.Fields["Exit code"] != "2" {
		t.Errorf("event = %+v", e)
	}
	// It stays stopped: Docker doesn't restart a container that was
	// stopped.
	time.Sleep(3 * time.Second)
	info, err := d.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Running || info.State.Restarting {
		t.Errorf("still %s", info.State.Status)
	}
	srv, err := m.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if srv.CrashLoop == nil || srv.CrashLoop.Exits != 3 {
		t.Errorf("crash loop = %+v", srv.CrashLoop)
	}
	// Starting it again forgets it.
	if err := m.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.CrashLoopOf(name); ok {
		t.Error("crash loop kept after a start")
	}
	_ = m.Stop(ctx, id)
}
