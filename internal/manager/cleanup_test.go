package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// cleanupDocker serves what cleanup reads and deletes.
type cleanupDocker struct {
	mu         sync.Mutex
	images     []docker.ImageSummary
	containers []docker.ContainerSummary
	volumes    []docker.Volume
	sizes      map[string]int64 // volume sizes
	buildCache int64
	conflict   map[string]bool // images Docker refuses to delete
	deleted    []string
}

func (f *cleanupDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := apiPrefix.ReplaceAllString(r.URL.Path, "")
	switch {
	case p == "/_ping":
	case p == "/images/json":
		json.NewEncoder(w).Encode(f.images)
	case p == "/containers/json":
		json.NewEncoder(w).Encode(f.containers)
	case p == "/volumes":
		json.NewEncoder(w).Encode(map[string]any{"Volumes": f.volumes})
	case p == "/system/df":
		var vols []map[string]any
		for name, size := range f.sizes {
			vols = append(vols, map[string]any{"Name": name, "UsageData": map[string]int64{"Size": size}})
		}
		json.NewEncoder(w).Encode(map[string]any{"Volumes": vols, "BuildCache": []map[string]any{{"Size": f.buildCache}}})
	case p == "/build/prune":
		f.deleted = append(f.deleted, "build-cache")
		json.NewEncoder(w).Encode(map[string]int64{"SpaceReclaimed": f.buildCache})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/images/"):
		id := strings.TrimPrefix(p, "/images/")
		if f.conflict[id] || r.URL.Query().Get("force") == "true" {
			http.Error(w, `{"message":"image is being used by stopped container"}`, http.StatusConflict)
			return
		}
		f.deleted = append(f.deleted, "image "+id)
		w.Write([]byte("[]"))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		for _, img := range f.images {
			if img.ID == id {
				json.NewEncoder(w).Encode(map[string]any{"Id": img.ID, "RepoTags": img.RepoTags})
				return
			}
		}
		http.Error(w, `{"message":"no such image"}`, http.StatusNotFound)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/containers/"):
		f.deleted = append(f.deleted, "container "+strings.TrimPrefix(p, "/containers/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/volumes/"):
		f.deleted = append(f.deleted, "volume "+strings.TrimPrefix(p, "/volumes/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"not implemented: `+p+`"}`, http.StatusNotImplemented)
	}
}

func container(id, name, image, state string, labels map[string]string) docker.ContainerSummary {
	return docker.ContainerSummary{ID: id, Names: []string{"/" + name}, ImageID: image, State: state, Labels: labels, SizeRw: 1000}
}

func newCleanupDocker() *cleanupDocker {
	managed := map[string]string{"io.hlm.managed": "true"}
	return &cleanupDocker{
		images: []docker.ImageSummary{
			{ID: "sha256:running", RepoTags: []string{"game:latest"}, Size: 5e9},
			{ID: "sha256:stopped", RepoTags: []string{"<none>:<none>"}, Size: 4e9}, // a stopped container's: kept
			{ID: "sha256:old", RepoTags: []string{"<none>:<none>"}, RepoDigests: []string{"game@sha256:aa"}, Size: 3e9},
			{ID: "sha256:spare", RepoTags: []string{"busybox:1.37"}, Size: 4e6},
		},
		containers: []docker.ContainerSummary{
			container("c1", "valheim", "sha256:running", "running", managed),
			container("c2", "stopped-game", "sha256:stopped", "exited", managed),
			container("c3", "valheim-old-1700000000", "sha256:running", "exited", managed), // left after recreating valheim
			container("c4", "lost-old-1700000000", "sha256:running", "exited", managed),    // lost wasn't recreated: kept
			container("c5", "", "sha256:running", "exited", map[string]string{"io.hlm.cleanup": "true"}),
			container("c6", "updater", "sha256:running", "running", map[string]string{"io.hlm.selfupdate-helper": "true"}),
			container("c7", "someone-elses", "sha256:running", "exited", nil),
		},
		volumes:    []docker.Volume{{Name: "orphan"}, {Name: "deleted-worlds"}},
		sizes:      map[string]int64{"orphan": 2e6, "deleted-worlds": 9e9},
		buildCache: 7e8,
		conflict:   map[string]bool{},
	}
}

func cleanupManager(t *testing.T, fd *cleanupDocker) *Manager {
	t.Helper()
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	m.UseDeletedStore(filepath.Join(t.TempDir(), "deleted.json"))
	if err := m.changeDeletedForTest(DeletedServer{ID: "x", Name: "gone",
		Folders: []DeletedFolder{{Path: "deleted-worlds", Volume: true, Target: "/worlds"}}}); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *Manager) changeDeletedForTest(d DeletedServer) error {
	return m.deleted.save([]DeletedServer{d})
}

func TestCleanupCandidates(t *testing.T) {
	fd := newCleanupDocker()
	m := cleanupManager(t, fd)
	plan, err := m.CleanupCandidates(context.Background(), LocalHost)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range plan.Items {
		d := " "
		if it.Default {
			d = "*"
		}
		got = append(got, d+it.Kind+" "+it.ID+" "+it.Name)
	}
	want := []string{
		"*old-image sha256:old game (old)",
		"*helper c3 valheim-old-1700000000",
		"*helper c5 ",
		" unused-image sha256:spare busybox:1.37",
		" build-cache  Build cache",
		" volume orphan orphan",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCleanup(t *testing.T) {
	fd := newCleanupDocker()
	fd.conflict["sha256:spare"] = true // in use by the time it's deleted
	m := cleanupManager(t, fd)
	res, err := m.Cleanup(context.Background(), LocalHost, CleanupRequest{
		Images:     []string{"sha256:old", "sha256:spare", "sha256:running"},
		Containers: []string{"c3", "c1"},
		Volumes:    []string{"orphan", "deleted-worlds"},
		BuildCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	deleted := slices.Clone(fd.deleted)
	fd.mu.Unlock()
	if !slices.Equal(deleted, []string{"container c3", "image sha256:old", "volume orphan", "build-cache"}) {
		t.Errorf("deleted %q", deleted)
	}
	if res.Freed != 1000+3e9+2e6+7e8 || len(res.Removed) != 4 {
		t.Errorf("result: freed %d, removed %+v", res.Freed, res.Removed)
	}
	var failed []string
	for _, f := range res.Failed {
		failed = append(failed, f.ID+": "+f.Error)
	}
	// Running containers, used images and deleted servers' volumes aren't
	// candidates; an image Docker refuses is reported as in use.
	if len(failed) != 4 || !strings.HasPrefix(failed[0], "c1: not something") ||
		!strings.HasPrefix(failed[1], "sha256:spare: in use") || !strings.HasPrefix(failed[2], "sha256:running: not something") ||
		!strings.HasPrefix(failed[3], "deleted-worlds: not something") {
		t.Errorf("failed: %q", failed)
	}

	// The weekly cleanup takes only the default items.
	fd.mu.Lock()
	fd.deleted = nil
	fd.mu.Unlock()
	if _, err := m.CleanupLeftovers(context.Background(), LocalHost); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for _, d := range fd.deleted {
		if strings.Contains(d, "spare") || strings.Contains(d, "volume") || d == "build-cache" {
			t.Errorf("weekly cleanup deleted %s", d)
		}
	}
}

func TestRemoveOldImage(t *testing.T) {
	fd := newCleanupDocker()
	m := cleanupManager(t, fd)
	on := true
	m.UseOldImageRemoval(func() bool { return on })
	var events []string
	emit := func(e Event) { events = append(events, e.Message) }
	ctx := context.Background()

	m.removeOldImage(ctx, LocalHost, "sha256:stopped", "sha256:new", emit) // a container uses it
	m.removeOldImage(ctx, LocalHost, "sha256:spare", "sha256:new", emit)   // still tagged
	m.removeOldImage(ctx, LocalHost, "sha256:old", "sha256:old", emit)     // the same image
	on = false
	m.removeOldImage(ctx, LocalHost, "sha256:old", "sha256:new", emit) // turned off
	on = true
	m.removeOldImage(ctx, LocalHost, "sha256:old", "sha256:new", emit)
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if !slices.Equal(fd.deleted, []string{"image sha256:old"}) || len(events) != 1 || !strings.Contains(events[0], "old image") {
		t.Errorf("deleted %q, events %q", fd.deleted, events)
	}
}
