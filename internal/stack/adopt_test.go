package stack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/compose"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const adoptFile = `
services:
  web:
    image: nginx:1
    ports: ["8080:80"]
    volumes: ["./html:/usr/share/nginx/html:ro"]
    environment:
      GREETING: ${GREETING}
  db:
    image: postgres:16
    volumes: ["db:/var/lib/postgresql/data"]
  worker:
    image: busybox:1.37
volumes:
  db:
`

// composeContainer adds a container as Docker Compose makes them.
func composeContainer(d *memDocker, project, svc, file string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := fmt.Sprintf("c%d", d.n)
	d.containers[id] = docker.ContainerSummary{ID: id, Names: []string{"/" + project + "-" + svc + "-1"}, Image: "img-" + svc, State: "running",
		Labels: map[string]string{composeProject: project, composeService: svc, composeConfigFiles: file,
			composeWorkingDir: file[:strings.LastIndex(file, "/")], composeNumber: "1"}}
}

func TestAdopt(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	nas := newMemDocker()
	nas.files["/srv/app/docker-compose.yml"] = adoptFile
	nas.files["/srv/app/.env"] = "GREETING=hi\n"
	nas.files["/srv/app/html/index.html"] = "<h1>hi</h1>"
	// Compose named the project after its folder; db and web run, worker
	// was never started.
	composeContainer(nas, "app", "web", "/srv/app/docker-compose.yml")
	composeContainer(nas, "app", "db", "/srv/app/docker-compose.yml")
	composeContainer(nas, "other", "web", "/srv/other/compose.yaml")
	mgr := &hostsManager{hosts: map[string]*memDocker{"nas": nas}}
	s := New(store, func(host string) (Docker, error) {
		if d, ok := mgr.hosts[host]; ok {
			return d, nil
		}
		return nil, fmt.Errorf("there's no Docker host %q", host)
	}, mgr, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	for file, want := range map[string]string{
		"srv/app/docker-compose.yml": "full path",
		"/srv/nope/compose.yaml":     "isn't a folder",
		"/srv/app/html":              "no Compose file",
	} {
		if _, err := s.AdoptPreview(ctx, "nas", file); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", file, err, want)
		}
	}

	// The folder finds the file in it, as Compose does.
	ap, err := s.AdoptPreview(ctx, "nas", "/srv/app")
	if err != nil {
		t.Fatal(err)
	}
	if ap.Found.Name != "app" || ap.Found.Source != "/srv/app/docker-compose.yml" || ap.Found.Env != "GREETING=hi\n" || len(ap.Found.Containers) != 2 {
		t.Fatalf("found %+v", ap.Found)
	}
	changes := map[string]string{}
	for _, sp := range ap.Plan.Services {
		changes[sp.Service] = sp.Change + " " + sp.Container
		if sp.Service == "web" && !strings.Contains(strings.Join(sp.Mounts, " "), "/srv/app/html:/usr/share/nginx/html:ro") {
			t.Errorf("web's mounts aren't relative to the file: %v", sp.Mounts)
		}
	}
	if changes["web"] != "adopt app-web-1" || changes["db"] != "adopt app-db-1" || changes["worker"] != "create app-worker-1" {
		t.Errorf("changes %v", changes)
	}

	if err := s.Adopt(ctx, "nas", "/srv/app/docker-compose.yml", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	// Compose's containers are hlm's now, with the same names; the other
	// project's is untouched.
	byName := map[string]docker.ContainerSummary{}
	for _, c := range nas.containers {
		byName[strings.TrimPrefix(c.Names[0], "/")] = c
	}
	for _, n := range []string{"app-web-1", "app-db-1", "app-worker-1"} {
		c, ok := byName[n]
		if !ok || c.Labels[compose.LabelStack] != "app" || c.Labels[composeProject] != "" || c.State != "running" {
			t.Errorf("%s: %+v", n, c)
		}
	}
	if c := byName["other-web-1"]; c.Labels[composeProject] != "other" {
		t.Errorf("another project's container changed: %+v", c)
	}
	if len(nas.containers) != 4 {
		t.Errorf("%d containers left (helpers or old ones?)", len(nas.containers))
	}
	// The .env beside the file is the stack's.
	for id, req := range nas.reqs {
		if c, ok := nas.containers[id]; ok && c.Names[0] == "/app-web-1" && !strings.Contains(strings.Join(req.Env, " "), "GREETING=hi") {
			t.Errorf("web's .env wasn't used: %v", req.Env)
		}
	}

	st, err := s.Get(ctx, "app")
	if err != nil || st.Source != "/srv/app/docker-compose.yml" || st.Host != "nas" || len(st.Services) != 3 {
		t.Fatalf("stack %+v %v", st, err)
	}
	for _, m := range st.Services {
		if m.State != "running" || !strings.HasSuffix(m.Server, "@nas") {
			t.Errorf("member %+v", m)
		}
	}
	// Editing it later keeps where it came from and its names.
	if err := s.Apply(ctx, "app", strings.Replace(adoptFile, "nginx:1", "nginx:2", 1), "GREETING=hi\n", true, "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Definition("app")
	if d.Dir != "/srv/app" || d.Names["web"] != "app-web-1" {
		t.Errorf("definition after an edit: %+v", d)
	}
	if st, _ := s.Get(ctx, "app"); len(st.Services) != 3 || st.Services[0].State != "running" {
		t.Errorf("after an edit: %+v", st.Services)
	}
	// It's taken: adopting it again is refused.
	if _, err := s.AdoptPreview(ctx, "nas", "/srv/app"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("adopted twice: %v", err)
	}
	_ = manager.LocalHost

	// Started from several files: refused, as one would lose the others.
	composeContainer(nas, "multi", "web", "/srv/multi/compose.yaml,/srv/multi/compose.override.yaml")
	nas.files["/srv/multi/compose.yaml"] = adoptFile
	if _, err := s.AdoptPreview(ctx, "nas", "/srv/multi"); err == nil || !strings.Contains(err.Error(), "several files") {
		t.Errorf("several files: %v", err)
	}
}

func TestProjectName(t *testing.T) {
	for _, c := range []struct{ yml, dir, want string }{
		{"services: {}", "/srv/My App.v2", "myappv2"},
		{"name: media\nservices: {}", "/srv/x", "media"},
		{"name: ${NAME}\nservices: {}", "/srv/x", "x"},
	} {
		if got := projectName(c.yml, c.dir); got != c.want {
			t.Errorf("projectName(%q, %q) = %q, want %q", c.yml, c.dir, got, c.want)
		}
	}
}
