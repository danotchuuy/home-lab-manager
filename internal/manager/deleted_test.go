package manager

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// fakeDocker serves the few Docker API calls deleting servers and their
// data uses.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]docker.ContainerJSON
	volumes    map[string]bool
	inUse      map[string]bool // volumes the daemon refuses to remove
}

var apiPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := apiPrefix.ReplaceAllString(r.URL.Path, "")
	switch {
	case p == "/_ping":
		w.Header().Set("API-Version", "1.45")
	case p == "/containers/json":
		var list []docker.ContainerSummary
		for _, c := range f.containers {
			list = append(list, docker.ContainerSummary{ID: c.ID, Names: []string{c.Name}, Mounts: c.Mounts, Labels: c.Config.Labels})
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(p, "/containers/") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		for _, c := range f.containers {
			if c.ID == id || c.Name == "/"+id {
				json.NewEncoder(w).Encode(c)
				return
			}
		}
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	case strings.HasPrefix(p, "/containers/") && r.Method == http.MethodDelete:
		delete(f.containers, strings.TrimPrefix(p, "/containers/"))
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/volumes/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(p, "/volumes/")
		switch {
		case f.inUse[name]:
			http.Error(w, `{"message":"volume is in use"}`, http.StatusConflict)
		case !f.volumes[name]:
			http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
		default:
			delete(f.volumes, name)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.Error(w, `{"message":"not implemented: `+r.Method+` `+p+`"}`, http.StatusNotImplemented)
	}
}

func gameServer(id, name string, mounts ...docker.MountPoint) docker.ContainerJSON {
	var c docker.ContainerJSON
	c.ID, c.Name, c.Mounts = id, "/"+name, mounts
	c.Config.Labels = map[string]string{deploy.LabelManaged: "true", deploy.LabelApp: "Valheim"}
	c.Config.Image = "lloesche/valheim-server"
	return c
}

func TestDeletedServerData(t *testing.T) {
	fd := &fakeDocker{
		containers: map[string]docker.ContainerJSON{
			"v1": gameServer("v1", "valheim",
				docker.MountPoint{Type: "bind", Source: "/srv/appdata/valheim/config", Destination: "/config"},
				docker.MountPoint{Type: "bind", Source: "/srv/appdata/shared-steamcmd", Destination: "/steamcmd"},
				docker.MountPoint{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
				docker.MountPoint{Type: "bind", Source: "/etc", Destination: "/host-etc"},
				docker.MountPoint{Type: "volume", Name: "valheim-worlds", Destination: "/worlds"},
				docker.MountPoint{Type: "tmpfs", Destination: "/tmp"},
			),
			"o1": gameServer("o1", "other", docker.MountPoint{Type: "bind", Source: "/srv/appdata/shared-steamcmd", Destination: "/steamcmd"}),
		},
		volumes: map[string]bool{"valheim-worlds": true},
		inUse:   map[string]bool{},
	}
	srv := httptest.NewServer(fd)
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	m.UseDeletedStore(filepath.Join(t.TempDir(), "deleted-servers.json"))
	ctx := context.Background()

	if err := m.Remove(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	list, _ := m.DeletedServers()
	if len(list) != 1 || list[0].Name != "valheim" || list[0].App != "Valheim" {
		t.Fatalf("record = %+v", list)
	}
	var got []string
	for _, f := range list[0].Folders {
		got = append(got, f.Path)
	}
	// Sockets, system paths and tmpfs are left out.
	if want := "/srv/appdata/valheim/config /srv/appdata/shared-steamcmd valheim-worlds"; strings.Join(got, " ") != want {
		t.Errorf("folders = %s", strings.Join(got, " "))
	}

	// A folder another server still uses can't be deleted.
	if err := m.DeleteDeletedData(ctx, "v1", "/srv/appdata/shared-steamcmd", false); err == nil || !strings.Contains(err.Error(), "still used by other") {
		t.Errorf("shared folder: %v", err)
	}
	// Nor anything that isn't in the record, or the appdata root.
	if err := m.DeleteDeletedData(ctx, "v1", "/srv/appdata", false); !errors.Is(err, ErrNotTracked) {
		t.Errorf("untracked: %v", err)
	}
	if err := m.checkDeletable(ctx, "/srv/appdata"); err == nil || !strings.Contains(err.Error(), "every server's data") {
		t.Errorf("appdata root: %v", err)
	}
	for _, p := range []string{"/srv", "/etc", "/srv/../etc", "relative/path"} {
		if err := m.checkDeletable(ctx, p); err == nil {
			t.Errorf("%s: want refusal", p)
		}
	}

	// Volumes are removed through Docker, which refuses while one is in use.
	fd.inUse["valheim-worlds"] = true
	if err := m.DeleteDeletedData(ctx, "v1", "valheim-worlds", true); err == nil {
		t.Error("in-use volume removed")
	}
	fd.inUse["valheim-worlds"] = false
	if err := m.DeleteDeletedData(ctx, "v1", "valheim-worlds", true); err != nil || fd.volumes["valheim-worlds"] {
		t.Errorf("volume: %v", err)
	}

	// Stop tracking one folder, then the rest.
	if err := m.ForgetDeleted("v1", "/srv/appdata/shared-steamcmd", false); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.DeletedServers(); len(list[0].Folders) != 1 {
		t.Errorf("after forgetting one: %+v", list[0].Folders)
	}
	if err := m.ForgetDeleted("v1", "/nope", false); !errors.Is(err, ErrNotTracked) {
		t.Errorf("forget unknown: %v", err)
	}
	if err := m.ForgetDeleted("v1", "", false); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.DeletedServers(); len(list) != 0 {
		t.Errorf("after forgetting all: %+v", list)
	}
}
