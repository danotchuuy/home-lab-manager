package manager

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

// silentDocker is a Docker host that, while silent, takes connections but
// never answers, like a machine that's asleep or gone from the network.
type silentDocker struct {
	silent atomic.Bool
	calls  atomic.Int32
	next   http.Handler
	stop   chan struct{}
}

func (s *silentDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)
	if s.silent.Load() {
		select {
		case <-r.Context().Done():
		case <-s.stop:
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/version") {
		w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.45"}`))
		return
	}
	s.next.ServeHTTP(w, r)
}

// A host that stops answering is waited for once; after that listings
// leave it out straight away, while it's tried again in the background,
// and it's back as soon as it answers.
func TestSilentHost(t *testing.T) {
	oldTimeout, oldRetry := remoteTimeout, retryDown
	remoteTimeout, retryDown = 300*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { remoteTimeout, retryDown = oldTimeout, oldRetry })

	var wg docker.ContainerJSON
	wg.ID, wg.Name = "w1", "/hlm-wireguard"
	wg.Config.Labels = map[string]string{"io.hlm.wireguard": "home"}
	local := &labelDocker{containers: []docker.ContainerJSON{gameServer("l1", "valheim"), wg}}
	nas := &silentDocker{next: &labelDocker{containers: []docker.ContainerJSON{gameServer("n1", "valheim")}}, stop: make(chan struct{})}
	srv := httptest.NewServer(nas)
	t.Cleanup(func() { close(nas.stop); srv.Close() })
	nasClient, _ := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})

	m := New(fakeClient(t, local), nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	m.UseConnection("io.hlm.wireguard", ConnectionKind{Kind: "wireguard"})
	m.SetHosts([]RemoteHost{{ID: "nas", Docker: nasClient}})
	ctx := context.Background()

	if list, err := m.List(ctx); err != nil || len(list) != 2 {
		t.Fatalf("both up: %d servers, %v", len(list), err)
	}

	nas.silent.Store(true)
	start := time.Now()
	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].Host != LocalHost {
		t.Fatalf("nas silent: %+v %v", list, err)
	}
	if took := time.Since(start); took < remoteTimeout/2 || took > 3*remoteTimeout {
		t.Errorf("first listing took %s", took)
	}
	if st := m.HostStatuses()["nas"]; st.OK {
		t.Errorf("status %+v", st)
	}

	// Now it's known to be down: nothing waits for it.
	for i := 0; i < 3; i++ {
		start = time.Now()
		list, err = m.List(ctx)
		conns, cerr := m.Connections(ctx)
		if took := time.Since(start); took > remoteTimeout/3 {
			t.Errorf("listing %d took %s", i, took)
		}
		if err != nil || len(list) != 1 || cerr != nil || len(conns) != 1 {
			t.Fatalf("listing %d: %d servers %v, %d connections %v", i, len(list), err, len(conns), cerr)
		}
	}
	if _, err := m.docker.ListContainersOn(ctx, "nas"); err == nil || !strings.Contains(err.Error(), "can't be reached") {
		t.Errorf("listing nas alone: %v", err)
	}

	// Back: a background retry notices, and its servers are listed again.
	nas.silent.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ = m.List(ctx)
		if len(list) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nas didn't come back: %+v", m.HostStatuses())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !m.HostStatuses()["nas"].OK {
		t.Errorf("status %+v", m.HostStatuses()["nas"])
	}
}

// lockedDocker never answers inspects of one container, as Docker does
// while a long archive copy holds the container's lock.
type lockedDocker struct {
	locked string
	next   http.Handler
	stop   chan struct{}
}

func (l *lockedDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/containers/"+l.locked+"/json") {
		select {
		case <-r.Context().Done():
		case <-l.stop:
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/version") {
		w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.45"}`))
		return
	}
	l.next.ServeHTTP(w, r)
}

// One container that doesn't answer (a backup copying its files) isn't its
// host being down: the host's other servers stay listed, and so does that
// one, from the list's own details and marked as not answering.
func TestStuckContainer(t *testing.T) {
	oldTimeout, oldRetry := remoteTimeout, retryDown
	remoteTimeout, retryDown = 300*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { remoteTimeout, retryDown = oldTimeout, oldRetry })

	nas := &lockedDocker{locked: "n2", next: &labelDocker{containers: []docker.ContainerJSON{gameServer("n1", "valheim"), gameServer("n2", "palworld")}}, stop: make(chan struct{})}
	srv := httptest.NewServer(nas)
	t.Cleanup(func() { close(nas.stop); srv.Close() })
	nasClient, _ := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	m := New(fakeClient(t, &labelDocker{}), nil, "/srv/appdata", slog.New(slog.DiscardHandler))
	m.SetHosts([]RemoteHost{{ID: "nas", Docker: nasClient}})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		list, err := m.List(ctx)
		if err != nil || len(list) != 2 {
			t.Fatalf("listing %d: %+v %v", i, list, err)
		}
		for _, s := range list {
			if stuck := s.ID == "n2@nas"; (s.Unresponsive != "") != stuck {
				t.Errorf("listing %d: %s unresponsive %q", i, s.ID, s.Unresponsive)
			}
		}
		if st := m.HostStatuses()["nas"]; !st.OK {
			t.Fatalf("listing %d: host taken as down: %+v", i, st)
		}
	}
	gctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	if _, err := m.Get(gctx, "n2@nas"); err == nil || !strings.Contains(err.Error(), "isn't answering") {
		t.Errorf("getting it: %v", err)
	}
}
