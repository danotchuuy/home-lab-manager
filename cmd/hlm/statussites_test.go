package main

import (
	"net/http"
	"net/http/httptest"
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
}
