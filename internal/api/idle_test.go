package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/idle"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// stoppedServers are servers that are stopped until started.
type stoppedServers struct {
	mu      sync.Mutex
	started []string
}

func (f *stoppedServers) Inspect(_ context.Context, id string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	info.Name = "/" + id
	return info, nil
}

func (f *stoppedServers) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return nil
}

func (f *stoppedServers) Stop(context.Context, string) error { return nil }
func (f *stoppedServers) JobRunning(string) bool             { return false }

// Waking from the status page: only servers that are asleep and allow it,
// and only through the page.
func TestWakeFromStatusPage(t *testing.T) {
	e := setup(t)
	store, _ := secrets.Open(t.TempDir(), "")
	servers := &stoppedServers{}
	idl := idle.New(idle.Options{Secrets: store, Servers: servers,
		Players: func(string) (int, int, bool) { return 0, 0, false }})
	ctx := context.Background()
	idl.Save(ctx, "valheim", idle.Settings{Minutes: 30, WakeFromStatusPage: true})
	idl.Save(ctx, "plex", idle.Settings{Minutes: 30})
	store.Set("idle-asleep", map[string]idle.Asleep{"valheim": {Minutes: 30}, "plex": {Minutes: 30}})
	sp := statuspage.New(statuspage.Options{Secrets: store, Servers: e.opts.Manager, Asleep: idl.AsleepInfo,
		HashPassword: auth.HashPassword, PasswordMatches: auth.PasswordMatches})
	e.opts.StatusPage, e.opts.Idle = sp, idl
	h := Handler(e.opts)
	oldStatus, oldWake := statusLimiter, wakeLimiter
	t.Cleanup(func() { statusLimiter, wakeLimiter = oldStatus, oldWake })
	statusLimiter, wakeLimiter = &statuspage.Limiter{Rate: 100, Burst: 100}, &statuspage.Limiter{Rate: 0.0001, Burst: 3}

	body := `{"enabled":true,"servers":[{"server":"valheim","displayName":"Vikings"},{"server":"plex","displayName":"Plex"}]}`
	if w := call(h, "admin", "PUT", "/api/system/status-page", body, e.keys); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	at := func(method, path string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host, r.RemoteAddr = "hlm.lan", "198.51.100.9:1"
		if csrf {
			r.Header.Set("X-HLM-Request", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	page := at("GET", "/api/status-page", false).Body.String()
	if !strings.Contains(page, `"name":"Vikings","state":"asleep","canWake":true`) ||
		!strings.Contains(page, `"name":"Plex","state":"asleep"}`) {
		t.Errorf("page: %s", page)
	}
	if w := at("POST", "/api/status-page/vikings/wake", false); w.Code != 403 {
		t.Errorf("without the header: %d", w.Code)
	}
	if w := at("POST", "/api/status-page/plex/wake", true); w.Code != 404 {
		t.Errorf("a server that doesn't allow it: %d %s", w.Code, w.Body)
	}
	if w := at("POST", "/api/status-page/nope/wake", true); w.Code != 404 {
		t.Errorf("not on the page: %d", w.Code)
	}
	if w := at("POST", "/api/status-page/vikings/wake", true); w.Code != 200 {
		t.Fatalf("wake: %d %s", w.Code, w.Body)
	}
	if len(servers.started) != 1 || servers.started[0] != "valheim" {
		t.Errorf("started = %v", servers.started)
	}
	if page := at("GET", "/api/status-page", false).Body.String(); strings.Contains(page, `"Vikings","state":"asleep"`) {
		t.Errorf("still asleep on the page: %s", page)
	}
	// Three tries from one address, then it has to wait.
	if w := at("POST", "/api/status-page/vikings/wake", true); w.Code != 429 {
		t.Errorf("fourth try: %d", w.Code)
	}

	// The server's own settings, for its viewers and managers.
	if w := call(h, "viewer", "GET", "/api/servers/valheim/idle", "", e.keys); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"minutes":30`) {
		t.Errorf("idle settings: %d %s", w.Code, w.Body)
	}
	if w := call(h, "manager", "PUT", "/api/servers/valheim/idle", `{"minutes":2}`, e.keys); w.Code != 400 {
		t.Errorf("2 minutes: %d %s", w.Code, w.Body)
	}
	if w := call(h, "manager", "PUT", "/api/servers/valheim/idle", `{"minutes":45,"wakeFromStatusPage":true}`, e.keys); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"minutes":45`) {
		t.Errorf("save: %d %s", w.Code, w.Body)
	}
}
