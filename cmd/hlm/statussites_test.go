package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/cftunnel"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

func TestHostPortOf(t *testing.T) {
	var bridge docker.ContainerJSON
	bridge.HostConfig.NetworkMode = "bridge"
	bridge.HostConfig.PortBindings = map[string][]docker.PortBinding{"8080/tcp": {{HostPort: "18080"}}}
	var host docker.ContainerJSON
	host.HostConfig.NetworkMode = "host"
	var none docker.ContainerJSON
	none.HostConfig.NetworkMode = "bridge"
	for _, tt := range []struct {
		so   cftunnel.SelfOrigin
		want int
	}{
		{cftunnel.SelfOrigin{Port: 8080}, 8080},
		{cftunnel.SelfOrigin{Port: 8080, Container: &bridge}, 18080},
		{cftunnel.SelfOrigin{Port: 8080, Container: &host}, 8080},
		{cftunnel.SelfOrigin{Port: 8080, Container: &none}, 0},
	} {
		got, err := hostPortOf(tt.so)
		if got != tt.want || (tt.want == 0) != (err != nil) {
			t.Errorf("%+v: %d, %v", tt.so, got, err)
		}
	}
}

// Status sites aren't redirected to hlm's own HTTPS address.
func TestStatusSitesFirst(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	sp := statuspage.New(statuspage.Options{Secrets: store})
	if _, err := sp.Save(statuspage.Settings{Enabled: true, Sites: []statuspage.Site{{Hostname: "friends.example.com"}}}, nil); err != nil {
		t.Fatal(err)
	}
	redirect := http.RedirectHandler("https://hlm.example.com/", http.StatusTemporaryRedirect)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := statusSitesFirst(sp, redirect, ok)
	for host, want := range map[string]int{"friends.example.com": 200, "hlm.lan:8080": 307} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", host, w.Code, want)
		}
	}
	// A Cloudflare Tunnel route to hlm that isn't a site's.
	sp.UseStatusHosts(func() []string { return []string{"status.example.com"} })
	for host, want := range map[string]int{"status.example.com": 200, "Status.Example.com.:443": 200, "other.example.com": 307} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("tunnel route %s: %d, want %d", host, w.Code, want)
		}
	}
	// The proxy's own address, marked by its nginx.
	r := httptest.NewRequest("GET", "/status", nil)
	r.Host = "203.0.113.7"
	r.Header.Set(statuspage.OnlyHeader, "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("marked: %d", w.Code)
	}
}

// hlm on a macvlan network has its own LAN address; on a bridge it doesn't.
func TestOwnLANAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/networks") {
			w.Write([]byte(`[{"Name":"br0","Driver":"macvlan"},{"Name":"games","Driver":"bridge"}]`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	dc, err := docker.NewClient(docker.Config{Host: "tcp://" + strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	on := func(network, ip string) cftunnel.SelfOrigin {
		var info docker.ContainerJSON
		info.HostConfig.NetworkMode = network
		info.NetworkSettings.Networks = map[string]docker.EndpointSettings{network: {IPAddress: ip}}
		return cftunnel.SelfOrigin{Port: 8080, Container: &info}
	}
	ctx := context.Background()
	if got := ownLANAddress(ctx, dc, on("br0", "192.168.11.5")); got != "192.168.11.5" {
		t.Errorf("br0: %q", got)
	}
	for _, n := range []string{"games", "bridge", "host"} {
		if got := ownLANAddress(ctx, dc, on(n, "172.18.0.5")); got != "" {
			t.Errorf("%s: %q", n, got)
		}
	}
	if got := ownLANAddress(ctx, dc, cftunnel.SelfOrigin{Port: 8080}); got != "" {
		t.Errorf("not in a container: %q", got)
	}
}
