package console

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePalworld is Palworld's REST API, recording the calls it gets.
type fakePalworld struct {
	mu    sync.Mutex
	calls []string // "POST /v1/api/announce {"message":"hi"}"
}

func (f *fakePalworld) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "s3cret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
	f.mu.Unlock()
	if r.Method == http.MethodPost && len(body) > 0 && !json.Valid(body) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/api/info":
		w.Write([]byte(`{"version":"v0.6.1","servername":"Pals at home","description":"hi","worldguid":"A7E9"}`))
	case "GET /v1/api/players":
		w.Write([]byte(`{"players":[{"name":"PalUser","accountName":"paluser","playerId":"d7ad","userId":"steam_7656","ip":"10.0.0.2","ping":3.14,"location_x":1,"location_y":2,"level":12,"building_count":119}]}`))
	case "GET /v1/api/metrics":
		w.Write([]byte(`{"serverfps":57,"currentplayernum":1,"serverframetime":16.7671,"maxplayernum":32,"uptime":3600,"basecampnum":4,"days":12}`))
	case "POST /v1/api/save", "POST /v1/api/announce", "POST /v1/api/kick", "POST /v1/api/ban", "POST /v1/api/unban",
		"POST /v1/api/shutdown", "POST /v1/api/stop":
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePalworld) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

func palworldServer(t *testing.T, env ...string) (*Service, *fakePalworld, string, int) {
	t.Helper()
	pal := &fakePalworld{}
	srv := httptest.NewServer(pal)
	t.Cleanup(srv.Close)
	host, portS, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portS)
	c := container("pal", "thijsvanloef/palworld-server-docker:latest", env...)
	s, _ := newService(t, fakeServers{"pal": c})
	return s, pal, host, port
}

func TestPalworldREST(t *testing.T) {
	s, pal, host, port := palworldServer(t, "ADMIN_PASSWORD=s3cret", "REST_API_ENABLED=true")
	ctx := context.Background()

	// Wrong password: a clear error.
	if _, err := s.Save(ctx, "pal", Input{Settings: Settings{Mode: ModeREST, Host: host, Port: port, Password: "nope"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Test(ctx, "pal", Input{Settings: Settings{Mode: ModeREST, Host: host, Port: port, Password: "nope"}}); err == nil ||
		!strings.Contains(err.Error(), "AdminPassword") {
		t.Errorf("wrong password: %v", err)
	}

	// The password from the container's variable.
	st, err := s.Save(ctx, "pal", Input{Settings: Settings{Mode: ModeREST, Host: host, Port: port, PasswordVar: "ADMIN_PASSWORD"}, ClearPassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ready || !strings.Contains(strings.Join(st.QuickCommands, "|"), "Metrics") {
		t.Errorf("status = %+v", st)
	}
	msg, err := s.Test(ctx, "pal", Input{Settings: Settings{Mode: ModeREST, Host: host, Port: port, PasswordVar: "ADMIN_PASSWORD"}})
	if err != nil || !strings.Contains(msg, "Pals at home") || !strings.Contains(msg, "v0.6.1") {
		t.Errorf("test = %q, %v", msg, err)
	}

	for _, tt := range []struct{ cmd, call, out string }{
		{"Info", "GET /v1/api/info", "Server: Pals at home\nVersion: v0.6.1"},
		{"ShowPlayers", "GET /v1/api/players", "name,playeruid,steamid,level,ping\nPalUser,d7ad,steam_7656,12,3"},
		{"metrics", "GET /v1/api/metrics", "Players: 1/32"},
		{"Save", "POST /v1/api/save", "The world was saved."},
		{"Broadcast Restarting in 5 minutes", `POST /v1/api/announce {"message":"Restarting in 5 minutes"}`, "Announced"},
		{"KickPlayer steam_7656 be nice", `POST /v1/api/kick {"message":"be nice","userid":"steam_7656"}`, "Kicked steam_7656"},
		{"BanPlayer steam_7656", `POST /v1/api/ban {"userid":"steam_7656"}`, "Banned"},
		{"UnBanPlayer steam_7656", `POST /v1/api/unban {"userid":"steam_7656"}`, "Unbanned"},
		{"Shutdown 60 Back soon", `POST /v1/api/shutdown {"message":"Back soon","waittime":60}`, "60 seconds"},
		{"DoExit", "POST /v1/api/stop", "stopped"},
	} {
		res, err := s.Send(ctx, "pal", "tester-"+tt.cmd, tt.cmd)
		if err != nil {
			t.Errorf("%s: %v", tt.cmd, err)
			continue
		}
		if got := pal.last(); got != tt.call {
			t.Errorf("%s: called %q, want %q", tt.cmd, got, tt.call)
		}
		if !strings.Contains(res.Output, tt.out) || res.Mode != ModeREST {
			t.Errorf("%s: output %q, want %q", tt.cmd, res.Output, tt.out)
		}
	}
	for _, bad := range []string{"Broadcast", "KickPlayer", "Shutdown soon", "TeleportToMe"} {
		if _, err := s.Send(ctx, "pal", "tester-bad", bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}

	// hlm's own reads.
	if game, ok := s.Replies(ctx, "pal"); !ok || game != "palworld" || s.Mode(ctx, "pal") != ModeREST {
		t.Errorf("replies = %q %v", game, ok)
	}
	out, err := s.Query(ctx, "pal", "ShowPlayers")
	if err != nil || !strings.HasPrefix(out, "name,") {
		t.Errorf("query = %q, %v", out, err)
	}

	// Saved before a stop.
	s.SaveBeforeStop(ctx, "pal", 5*time.Second)
	if got := pal.last(); got != "POST /v1/api/save" {
		t.Errorf("before stop: %q", got)
	}
}

func TestPalworldRESTUnreachable(t *testing.T) {
	s, _, host, _ := palworldServer(t, "ADMIN_PASSWORD=s3cret")
	ctx := context.Background()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if _, err := s.Save(ctx, "pal", Input{Settings: Settings{Mode: ModeREST, Host: host, Port: closed, PasswordVar: "ADMIN_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Send(ctx, "pal", "x", "Info")
	if err == nil || !strings.Contains(err.Error(), "RESTAPIEnabled") {
		t.Errorf("unreachable: %v", err)
	}
	// Saving before a stop gives up quietly.
	s.SaveBeforeStop(ctx, "pal", time.Second)
}
