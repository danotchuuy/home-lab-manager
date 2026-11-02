package proxy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/remote"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeRelay records the configurations applied to it.
type fakeRelay struct {
	configs []string
	fail    bool
}

func (f *fakeRelay) run(_ context.Context, script string, stdin []byte, _ func(string)) (remote.Result, error) {
	if f.fail {
		return remote.Result{Stdout: "nginx: [emerg] bad\nhlm:testfailed\n", ExitCode: 4}, nil
	}
	if strings.Contains(script, "hlm:applied") {
		f.configs = append(f.configs, string(stdin))
		return remote.Result{Stdout: "hlm:applied\n"}, nil
	}
	return remote.Result{}, nil
}

func TestWANWatcherUpdatesAppliedProxy(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{"a": server(map[string]string{"2456/udp": "2456"})}, slog.New(slog.DiscardHandler))
	relay := &fakeRelay{}
	s.run = relay.run
	wan, wanErr := "203.0.113.7", error(nil)
	var asked string
	s.UseWANLookup(func(_ context.Context, w string) (string, error) { asked = w; return wan, wanErr })
	ctx := context.Background()

	saved, err := s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "ignored", UpstreamSource: "unifi", Servers: []string{"a"}})
	if err != nil || saved.Upstream != "" || saved.UnifiWAN != "wan" {
		t.Fatalf("save: %+v, %v", saved, err)
	}

	// Not applied yet: a check records the address but changes nothing.
	st, err := s.CheckWAN(ctx)
	if err != nil || st.Current != wan || st.Applied != "" || len(relay.configs) != 0 || asked != "wan" {
		t.Fatalf("check before apply: %+v, %v, %d applies", st, err, len(relay.configs))
	}

	if _, err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(relay.configs[0], "proxy_pass 203.0.113.7:2456;") {
		t.Errorf("applied config:\n%s", relay.configs[0])
	}
	if st, _ := s.State(); st.Applied != wan || st.AppliedAt.IsZero() {
		t.Errorf("state after apply: %+v", st)
	}

	// Same address: nothing to do.
	if _, err := s.CheckWAN(ctx); err != nil || len(relay.configs) != 1 {
		t.Errorf("unchanged address re-applied: %v, %d applies", err, len(relay.configs))
	}

	// The address changes: the proxy follows.
	wan = "198.51.100.20"
	st, err = s.CheckWAN(ctx)
	if err != nil || st.Applied != wan || len(relay.configs) != 2 || !strings.Contains(relay.configs[1], "proxy_pass 198.51.100.20:2456;") {
		t.Fatalf("after change: %+v, %v, %d applies", st, err, len(relay.configs))
	}

	// UniFi fails: the error is recorded and the relay left alone.
	wanErr = errors.New("unreachable")
	st, err = s.CheckWAN(ctx)
	if err == nil || !strings.Contains(st.Error, "unreachable") || st.Current != "198.51.100.20" || len(relay.configs) != 2 {
		t.Errorf("lookup failure: %+v, %v", st, err)
	}
	wanErr = nil

	// nginx rejects the update: the error is recorded and the old address kept.
	wan, relay.fail = "192.0.2.50", true
	st, err = s.CheckWAN(ctx)
	if err == nil || !strings.Contains(st.Error, "192.0.2.50") || st.Applied != "198.51.100.20" {
		t.Errorf("apply failure: %+v, %v", st, err)
	}
	// Once it works again, the next check catches up and clears the error.
	relay.fail = false
	if st, err = s.CheckWAN(ctx); err != nil || st.Applied != wan || st.Error != "" {
		t.Errorf("recovery: %+v, %v", st, err)
	}

	// After removing the proxies, a change doesn't apply them again.
	if _, err := s.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	wan = "192.0.2.99"
	n := len(relay.configs)
	if _, err := s.CheckWAN(ctx); err != nil || len(relay.configs) != n {
		t.Errorf("re-applied after remove: %v", err)
	}
}

func TestWANCheckIgnoresManualUpstream(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, fakeServers{}, slog.New(slog.DiscardHandler))
	called := false
	s.UseWANLookup(func(context.Context, string) (string, error) { called = true; return "203.0.113.7", nil })
	if _, err := s.Save(Settings{Target: remote.Target{Host: "vps", User: "root"}, Upstream: "home.example.net"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWAN(context.Background()); err != nil || called {
		t.Errorf("manual upstream: %v, looked up %v", err, called)
	}
	if st, _, _ := s.Settings(); st.UpstreamSource != "manual" {
		t.Errorf("source = %q", st.UpstreamSource)
	}
	bad := Settings{Target: remote.Target{Host: "vps", User: "root"}, UpstreamSource: "unifi", UnifiWAN: "wan3"}
	if err := Validate(&bad); err == nil {
		t.Error("wan3 accepted")
	}
}
