package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestDigestLookups(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	d := newDigestLookups(func(_ context.Context, host, ref string) (string, error) {
		calls.Add(1)
		<-release
		if host == "down" {
			return "", errors.New("unreachable")
		}
		return "sha256:" + ref, nil
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	got := make([]string, 3)
	for i, host := range []string{"local", "nas", "vps"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = d.get(ctx, host, "img:1")
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 || got[0] != "sha256:img:1" || got[1] != got[0] || got[2] != got[0] {
		t.Errorf("one image on three hosts: %d lookups, %v", calls.Load(), got)
	}
	// The first host's lookup failing: the others ask themselves.
	calls.Store(0)
	if _, err := d.get(ctx, "down", "img:2"); err == nil {
		t.Error("down host's lookup succeeded")
	}
	if dg, err := d.get(ctx, "local", "img:2"); err != nil || dg != "sha256:img:2" || calls.Load() != 2 {
		t.Errorf("after a failure: %q %v, %d lookups", dg, err, calls.Load())
	}
}

// updateDocker is a Docker API with servers on several images, whose
// registry answers slowly.
type updateDocker struct {
	images  int
	delay   time.Duration
	lookups atomic.Int32
}

func (f *updateDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v1.") && i > 0 {
		path = path[i+1:]
	}
	switch {
	case path == "/containers/json" && !strings.Contains(r.URL.Query().Get("filters"), "io.hlm.managed"):
		w.Write([]byte("[]"))
	case path == "/containers/json":
		var list []map[string]any
		for i := range f.images {
			list = append(list, map[string]any{
				"Id": fmt.Sprintf("c%d", i), "Names": []string{fmt.Sprintf("/srv%d", i)}, "Image": fmt.Sprintf("img%d:latest", i),
				"ImageID": fmt.Sprintf("sha256:local%d", i), "State": "running",
				"Labels": map[string]string{"io.hlm.managed": "true", "io.hlm.app": "test"},
			})
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		n := strings.TrimSuffix(strings.TrimPrefix(ref, "img"), ":latest")
		json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:local" + n, "RepoDigests": []string{"img" + n + "@sha256:old" + n}})
	case strings.HasPrefix(path, "/distribution/"):
		f.lookups.Add(1)
		time.Sleep(f.delay)
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/distribution/"), "/json")
		n := strings.TrimSuffix(strings.TrimPrefix(ref, "img"), ":latest")
		digest := "sha256:old" + n
		if n == "0" {
			digest = "sha256:new0"
		}
		json.NewEncoder(w).Encode(map[string]any{"Descriptor": map[string]string{"digest": digest}})
	case strings.HasPrefix(path, "/containers/c") && strings.HasSuffix(path, "/json"):
		n := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/c"), "/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Id": "c" + n, "Name": "/srv" + n, "Image": "sha256:local" + n,
			"State":  map[string]any{"Status": "running", "Running": true},
			"Config": map[string]any{"Image": "img" + n + ":latest", "Labels": map[string]string{"io.hlm.managed": "true", "io.hlm.app": "test"}},
		})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container"}`))
	case path == "/_ping":
		w.Write([]byte("OK"))
	default:
		w.Write([]byte("[]"))
	}
}

func TestStartCheckUpdates(t *testing.T) {
	fd := &updateDocker{images: 10, delay: 150 * time.Millisecond}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	m := New(dc, nil, "/srv/appdata", slog.New(slog.DiscardHandler))

	start := time.Now()
	m.StartCheckUpdates()
	m.StartCheckUpdates() // already going: the same check
	if info := m.UpdateInfo(); !info.Checking || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("not started in the background: %+v after %v", info, time.Since(start))
	}
	sawProgress := false
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		info := m.UpdateInfo()
		if info.Checking && info.Total == 10 && info.Done > 0 && info.Done < 10 {
			sawProgress = true
		}
		if !info.Checking {
			if info.Available != 1 || info.LastCheck == nil || info.Done != 0 {
				t.Errorf("finished: %+v", info)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still checking")
		}
	}
	if !sawProgress {
		t.Error("no progress seen")
	}
	if st, _ := m.updates.get("c0"); st.State != UpdateAvailable {
		t.Errorf("c0 = %+v", st)
	}
	if st, _ := m.updates.get("c5"); st.State != UpdateCurrent {
		t.Errorf("c5 = %+v", st)
	}
	// Ten lookups of 150ms, eight at a time.
	if el := time.Since(start); el > 1200*time.Millisecond || fd.lookups.Load() != 10 {
		t.Errorf("%d lookups took %v", fd.lookups.Load(), el)
	}
}
