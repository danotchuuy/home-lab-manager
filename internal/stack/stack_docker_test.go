package stack

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeManager removes containers straight through Docker.
type fakeManager struct{ d *docker.Client }

func (f fakeManager) PortConflictsOn(context.Context, string, docker.CreateRequest, string) ([]manager.PortConflict, error) {
	return nil, nil
}
func (f fakeManager) Remove(ctx context.Context, id string) error {
	return f.d.RemoveContainer(ctx, id, true)
}
func (f fakeManager) AppdataRootOf(string) string { return os.TempDir() }

const testStack = `
services:
  db:
    image: busybox:1.37
    command: sh -c "sleep 2; touch /tmp/ready; sleep 600"
    healthcheck:
      test: ["CMD", "test", "-f", "/tmp/ready"]
      interval: 1s
      retries: 30
    networks: [back]
  init:
    image: busybox:1.37
    command: sh -c "echo migrated"
  web:
    image: busybox:1.37
    command: sh -c "nslookup db >/dev/null 2>&1; ping -c1 -W2 db && sleep 600"
    environment:
      GREETING: ${GREETING:-hello}
    depends_on:
      db: {condition: service_healthy}
      init: {condition: service_completed_successfully}
    networks: [back]
networks:
  back:
`

// Two services and a one-off, with depends_on conditions, against Docker.
// Enable with HLM_DOCKER_TESTS=1.
func TestDockerStack(t *testing.T) {
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d, err := docker.NewClient(docker.ConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	store, _ := secrets.Open(t.TempDir(), "k")
	s := New(store, func(string) (Docker, error) { return d, nil }, fakeManager{d}, slog.New(slog.DiscardHandler))
	name := fmt.Sprintf("hlmtest%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { s.Delete(context.Background(), name) })

	var events []string
	emit := func(e Event) {
		if e.Message != "" {
			events = append(events, e.Stage+": "+e.Message)
		}
	}
	if err := s.Apply(ctx, name, testStack, "GREETING=hi", false, "", emit); err != nil {
		t.Fatalf("apply: %v\n%s", err, strings.Join(events, "\n"))
	}
	joined := strings.Join(events, "\n")
	// web waited for db to be healthy and init to finish, after both started.
	iDB, iWait := strings.Index(joined, "Starting "+name+"-db"), strings.Index(joined, "Waiting for "+name+"-db to be healthy")
	iWeb := strings.Index(joined, "Starting "+name+"-web")
	if iDB < 0 || iWait < iDB || iWeb < iWait || !strings.Contains(joined, "Waiting for "+name+"-init to be finished") {
		t.Errorf("order:\n%s", joined)
	}
	st, err := s.Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, m := range st.Services {
		states[m.Service] = m.State
	}
	if states["db"] != "running" || states["web"] != "running" || states["init"] != "exited" {
		t.Errorf("states = %v", states)
	}
	// web reached db by its service name on the stack's network.
	web, _ := d.InspectContainer(ctx, name+"-web")
	if !web.State.Running || !strings.Contains(strings.Join(web.Config.Env, " "), "GREETING=hi") {
		t.Errorf("web = %+v", web.State)
	}

	// An edit recreates only what changed.
	plan, err := s.Preview(ctx, name, strings.Replace(testStack, "${GREETING:-hello}", "changed", 1), "", true, "")
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]string{}
	for _, sp := range plan.Services {
		changes[sp.Service] = sp.Change
	}
	if changes["web"] != ChangeUpdate || changes["db"] != ChangeUnchanged {
		t.Errorf("changes = %v", changes)
	}

	if err := s.Act(ctx, name, "stop", emit); err != nil {
		t.Fatal(err)
	}
	if db, _ := d.InspectContainer(ctx, name+"-db"); db.State.Running {
		t.Error("db still running after stop")
	}
	if err := s.Act(ctx, name, "start", emit); err != nil {
		t.Fatalf("start: %v\n%s", err, strings.Join(events, "\n"))
	}
	if err := s.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.ListContainers(ctx, "io.hlm.stack="+name); len(list) != 0 {
		t.Errorf("containers left: %d", len(list))
	}
	nets, _ := d.ListNetworks(ctx)
	for _, n := range nets {
		if strings.HasPrefix(n.Name, name+"_") {
			t.Errorf("network left: %s", n.Name)
		}
	}
}
