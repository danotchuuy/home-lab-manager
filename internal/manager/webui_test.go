package manager

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestFillWebUI(t *testing.T) {
	dir := t.TempDir()
	feed := `{"apiVersion":1,"applist":[{"Name":"Authelia","Repository":"authelia/authelia","WebUI":"http://[IP]:[PORT:9091]/"}]}`
	if err := os.WriteFile(filepath.Join(dir, "applicationFeed.json"), []byte(feed), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := catalog.New("", dir, slog.New(slog.DiscardHandler))
	if err := cat.LoadCache(); err != nil {
		t.Fatal(err)
	}
	published := []docker.PortSummary{{PrivatePort: 9091, PublicPort: 19091, Type: "tcp"}}

	tests := []struct {
		name   string
		srv    Server
		labels map[string]string
		remote string
		want   string
	}{
		{
			name: "image without a template uses the catalog and published ports",
			srv:  Server{Image: "ghcr.io/authelia/authelia:4.39", Ports: published},
			want: "http://[IP]:19091/",
		},
		{
			name: "saved link is kept",
			srv:  Server{WebUI: "http://[IP]:9000/", Image: "authelia/authelia", Ports: published},
			want: "http://[IP]:9000/",
		},
		{
			name: "unknown image has no link",
			srv:  Server{Image: "example/game-server:latest", Ports: published},
		},
		{
			name:   "direct IP fills in the address",
			srv:    Server{Image: "authelia/authelia", IPAddress: "192.168.1.50"},
			labels: map[string]string{deploy.LabelDirectIP: "true"},
			want:   "http://192.168.1.50:9091/",
		},
		{
			name:   "remote host fills in the address",
			srv:    Server{Image: "authelia/authelia", Ports: published},
			remote: "10.0.0.5",
			want:   "http://10.0.0.5:19091/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{catalog: cat, remoteHost: tt.remote}
			srv := tt.srv
			m.fillWebUI(&srv, tt.labels)
			if srv.WebUI != tt.want {
				t.Errorf("WebUI = %q, want %q", srv.WebUI, tt.want)
			}
		})
	}
}
