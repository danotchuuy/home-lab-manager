package gamequery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestParseHTTPStatus(t *testing.T) {
	for _, body := range []string{
		`{"status": "ready", "player_count": 2, "player_names": ["Alice", "Bob"]}`,
		// As Nightingale's documentation shows it, without commas.
		"{\n  \"status\": \"ready\"\n  \"player_count\": 2\n  \"player_names\": [\"Alice\", \"Bob\"]\n}",
	} {
		st, err := parseHTTPStatus([]byte(body))
		if err != nil || st.Status != "ready" || st.Count != 2 || !slices.Equal(st.Names, []string{"Alice", "Bob"}) {
			t.Errorf("%q: %+v %v", body, st, err)
		}
	}
	if _, err := parseHTTPStatus([]byte("<html>hi</html>")); err == nil {
		t.Error("parsed HTML")
	}
}

func TestHTTPStatus(t *testing.T) {
	code, body := http.StatusServiceUnavailable, `{"status": "loading", "player_count": 0, "player_names": []}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if _, err := HTTPStatus(context.Background(), addr); !errors.Is(err, ErrLoading) {
		t.Errorf("loading: %v", err)
	}
	code, body = http.StatusOK, "{\n \"status\": \"ready\"\n \"player_count\": 1\n \"player_names\": [\"Alice\"]\n}"
	res, err := HTTPStatus(context.Background(), addr)
	if err != nil || res.Protocol != ProtoHTTPStatus || res.Players != 1 || !res.HasPlayers || !slices.Equal(res.PlayerNames, []string{"Alice"}) {
		t.Errorf("ready: %+v %v", res, err)
	}
	code, body = http.StatusNotFound, "not here"
	if _, err := HTTPStatus(context.Background(), addr); err == nil || errors.Is(err, ErrLoading) {
		t.Errorf("not a status page: %v", err)
	}
}

func TestArgPortAndCandidates(t *testing.T) {
	var info docker.ContainerJSON
	info.Config.Image = "ghcr.io/ekman/nightingale:1"
	info.HostConfig.PortBindings = map[string][]docker.PortBinding{"7777/udp": {{HostPort: "7777"}}}
	// Off: only on the ports images set themselves (fireblade004's 8888,
	// 8889), not the launch option's.
	if got := candidates(info, nil, false); slices.Contains(got, candidate{ProtoHTTPStatus, 7778}) ||
		!slices.Contains(got, candidate{ProtoHTTPStatus, 8888}) || !slices.Contains(got, candidate{ProtoHTTPStatus, 8889}) {
		t.Errorf("off: %+v", got)
	}
	// In the command.
	info.Config.Cmd = []string{"-statusPort=7778", "-ini:Engine:[HTTPServer.Listeners]:+ListenerOverrides=(Port=7778,BindAddress=0.0.0.0)"}
	if got := candidates(info, nil, false); len(got) < 3 || got[0] != (candidate{ProtoHTTPStatus, 7778}) || got[1] != (candidate{ProtoHTTPStatus, 8888}) {
		t.Errorf("command: %+v", got)
	}
	// In a variable, and with a space.
	info.Config.Cmd = nil
	info.Config.Env = []string{"GAME_PARAMS=-log -statusPort 8899"}
	if got := argPort(info, "-statusPort"); got != 8899 {
		t.Errorf("variable: %d", got)
	}
	info.Config.Env = []string{"GAME_PARAMS=-statusPort=8890"}
	if got := argPort(info, "-statusPort"); got != 8890 {
		t.Errorf("right after the variable's =: %d", got)
	}
	// Not some other argument that ends the same way.
	info.Config.Env = []string{"GAME_PARAMS=-otherstatusPort=1234"}
	if got := argPort(info, "-statusPort"); got != 0 {
		t.Errorf("another argument: %d", got)
	}
}

// A Nightingale server with its status page on is asked there every poll
// while it loads (its profile says that's the way), not given up on until
// the next look around, and is online, with the game's player limit, as
// soon as it's ready.
func TestStatusPageWhileLoading(t *testing.T) {
	r := newRig(t)
	loading := true
	r.s.opts.Probes[ProtoHTTPStatus] = func(_ context.Context, addr string) (Result, error) {
		r.w.mu.Lock()
		defer r.w.mu.Unlock()
		r.w.asked = append(r.w.asked, "http "+addr)
		if loading {
			return Result{}, ErrLoading
		}
		return Result{Protocol: ProtoHTTPStatus, Address: addr, Players: 2, HasPlayers: true, PlayerNames: []string{"Alice", "Bob"}}, nil
	}
	c := ctr("ng", "ghcr.io/ekman/nightingale:1", "192.168.11.30", r.now.Add(time.Minute))
	c.Config.Cmd = []string{"-statusPort=7778"}
	r.w.servers["ng"] = c

	r.step(2 * time.Minute)
	st := r.s.Status("ng", true)
	if st.Status != StatusStarting || !strings.Contains(st.Error, "still loading") {
		t.Fatalf("loading: %+v", st)
	}
	// Asked again at the next poll, well before detection would look again.
	before := len(r.w.asked)
	r.step(pollStarting)
	if len(r.w.asked) == before || !strings.HasPrefix(r.w.asked[len(r.w.asked)-1], "http 192.168.11.30:7778") {
		t.Errorf("not asked again: %q", r.w.asked)
	}
	loading = false
	r.step(pollStarting)
	st = r.s.Status("ng", true)
	if st.Status != StatusOnline || st.Result.Players != 2 || st.Result.MaxPlayers != 6 || st.Method == nil || st.Method.Protocol != ProtoHTTPStatus {
		t.Fatalf("ready: %+v %+v", st, st.Result)
	}
}

// The app's own probes (not a test's) know every protocol.
func TestDefaultProbes(t *testing.T) {
	s := New(Options{})
	for _, p := range protocols {
		if p == ProtoAuto || p == ProtoRCON || p == ProtoOff {
			continue
		}
		if s.opts.Probes[p] == nil {
			t.Errorf("no probe for %s", p)
		}
	}
}

// An image that starts the game with its own -statusPort (fireblade004's
// 8888) has its status page where the launch option didn't ask: it's found
// as soon as it answers, without waiting for the next look around.
func TestStatusPageOnImagePort(t *testing.T) {
	r := newRig(t)
	up := false
	r.s.opts.Probes[ProtoHTTPStatus] = func(_ context.Context, addr string) (Result, error) {
		r.w.mu.Lock()
		defer r.w.mu.Unlock()
		r.w.asked = append(r.w.asked, "http "+addr)
		if up && strings.HasSuffix(addr, ":8888") {
			return Result{Protocol: ProtoHTTPStatus, Address: addr, Players: 1, HasPlayers: true}, nil
		}
		return Result{}, errors.New("nothing is listening on that port")
	}
	c := ctr("ng", "fireblade004/nightingale-server", "192.168.12.130", r.now.Add(time.Minute))
	c.Config.Cmd = []string{"-statusPort=7778"}
	r.w.servers["ng"] = c

	// Not up yet: the first look around finds nothing.
	r.step(2 * time.Minute)
	if st := r.s.Status("ng", true); st.Status == StatusOnline {
		t.Fatalf("online too soon: %+v", st)
	}
	// It comes up on 8888: the next poll, well before the next look around.
	up = true
	r.step(pollStarting)
	st := r.s.Status("ng", true)
	if st.Status != StatusOnline || st.Method == nil || st.Method.Port != 8888 {
		t.Fatalf("on the image's port: %+v (asked %q)", st, r.w.asked[len(r.w.asked)-3:])
	}
}
