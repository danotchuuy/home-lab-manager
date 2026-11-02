package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.10.0", "v1.9.0", true},
		{"1.2.0", "v1.2.0", false},
		{"v2.0.0", "v2.0.0-rc.1", true},
		{"v2.0.0-rc.2", "v2.0.0-rc.1", true},
		{"v2.0.0-rc.1", "v2.0.0", false},
		{"v1.0.0", "dev", false},
		{"v1.0.0+build.5", "v0.9.0", true},
		{"latest", "v1.0.0", false},
	}
	for _, tt := range tests {
		if got := newer(tt.a, tt.b); got != tt.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestContainerIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	mountinfo := "" +
		"1234 1200 0:52 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/x\n" +
		"1240 1234 252:0 /var/lib/docker/volumes/data/_data /data rw - ext4 /dev/sda1 rw\n" +
		"1241 1234 252:0 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n" +
		"1242 1234 252:0 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n"
	if got := containerIDFromMountinfo(mountinfo); got != id {
		t.Errorf("got %q, want %q", got, id)
	}
	if got := containerIDFromMountinfo("25 1 8:1 / / rw - ext4 /dev/sda1 rw\n"); got != "" {
		t.Errorf("host mountinfo: got %q", got)
	}
}

// fakeGitHub serves a latest release with a binary and checksums.
func fakeGitHub(t *testing.T, version string, binary []byte, checksum string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/me/hlm/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": version, "name": "Release " + version, "body": "Fixes things.",
			"html_url": "https://example.com/rel", "published_at": time.Now(),
			"assets": []map[string]string{
				{"name": AssetName(), "browser_download_url": srv.URL + "/dl/bin"},
				{"name": ChecksumsAsset, "browser_download_url": srv.URL + "/dl/sums"},
			},
		})
	})
	mux.HandleFunc("GET /dl/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(binary) })
	mux.HandleFunc("GET /dl/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  other_file\n%s  %s\n", strings.Repeat("0", 64), checksum, AssetName())
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func binaryUpdater(t *testing.T, api string, restart func(string) error) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "hlm")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := &Updater{
		cfg: Config{
			Version: "v1.0.0", Repo: "me/hlm", APIBase: api, DataDir: dir,
			Log: slog.New(slog.DiscardHandler), Restart: restart,
		},
		http: http.DefaultClient,
		mode: ModeBinary, exe: exe, target: exe,
	}
	return u, exe
}

func TestBinaryUpdate(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(newBin)
	gh := fakeGitHub(t, "v1.1.0", newBin, hex.EncodeToString(sum[:]))

	var mu sync.Mutex
	restarted := ""
	u, exe := binaryUpdater(t, gh.URL, func(path string) error {
		mu.Lock()
		restarted = path
		mu.Unlock()
		return nil
	})

	st := u.Check(context.Background())
	if !st.Available || st.Latest.Version != "v1.1.0" || st.Latest.Notes != "Fixes things." {
		t.Fatalf("status = %+v", st)
	}
	var stages []string
	if err := u.Apply(context.Background(), func(e Event) { stages = append(stages, e.Stage) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newBin) {
		t.Errorf("binary not replaced: %q", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); string(got) != "old binary" {
		t.Errorf("backup = %q", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm()&0o100 == 0 {
		t.Error("new binary isn't executable")
	}
	time.Sleep(700 * time.Millisecond)
	mu.Lock()
	if restarted != exe {
		t.Errorf("restarted %q, want %q", restarted, exe)
	}
	mu.Unlock()
	if strings.Join(stages, ",") != "download,download,verify,install,restart" {
		t.Errorf("stages = %v", stages)
	}

	// The outcome survives the restart.
	u2, _ := binaryUpdater(t, gh.URL, nil)
	u2.cfg.DataDir = u.cfg.DataDir
	u2.loadResult()
	if r := u2.Status().LastResult; r == nil || !r.OK || r.To != "v1.1.0" {
		t.Errorf("last result = %+v", r)
	}
}

func TestBinaryUpdateRejectsBadChecksum(t *testing.T) {
	gh := fakeGitHub(t, "v1.1.0", []byte("tampered"), strings.Repeat("a", 64))
	u, exe := binaryUpdater(t, gh.URL, func(string) error { t.Error("restarted after a bad download"); return nil })
	u.Check(context.Background())
	err := u.Apply(context.Background(), func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Errorf("binary changed despite bad checksum: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".hlm-update-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	if u.Status().Updating {
		t.Error("still marked as updating after a failure")
	}
}

func TestApplyPreconditions(t *testing.T) {
	gh := fakeGitHub(t, "v1.0.0", nil, "")
	u, _ := binaryUpdater(t, gh.URL, nil)
	if err := u.Apply(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "check for updates first") {
		t.Errorf("before check: %v", err)
	}
	u.Check(context.Background())
	if err := u.Apply(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "already running the latest") {
		t.Errorf("same version: %v", err)
	}

	gh2 := fakeGitHub(t, "v2.0.0", nil, "")
	u2, _ := binaryUpdater(t, gh2.URL, nil)
	u2.cfg.Busy = func() string { return "wait for the install of valheim to finish first" }
	u2.Check(context.Background())
	if err := u2.Apply(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "valheim") {
		t.Errorf("busy: %v", err)
	}

	u2.reason = "this is a development build (dev)"
	if err := u2.Apply(context.Background(), func(Event) {}); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Errorf("dev build: %v", err)
	}
}

func TestNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	u, _ := binaryUpdater(t, srv.URL, nil)
	st := u.Check(context.Background())
	if st.CheckError != ErrNoReleases.Error() || st.Available {
		t.Errorf("status = %+v", st)
	}
}

func TestCloneConfig(t *testing.T) {
	id := strings.Repeat("c", 64)
	var raw map[string]any
	json.Unmarshal([]byte(`{
		"Id": "`+id+`",
		"Name": "/home-lab-manager",
		"Image": "sha256:old",
		"Config": {
			"Hostname": "`+id[:12]+`",
			"Image": "ghcr.io/me/hlm:v1.0.0",
			"Env": ["HLM_TOKEN=secret", "PATH=/usr/bin", "HLM_WEB_DIR=/app/web"],
			"Cmd": null,
			"Entrypoint": ["/usr/local/bin/hlm"],
			"Labels": {"com.docker.compose.service": "hlm", "org.opencontainers.image.version": "v1.0.0"},
			"ExposedPorts": {"8080/tcp": {}}
		},
		"HostConfig": {"NetworkMode": "hlm_default", "Binds": ["/var/run/docker.sock:/var/run/docker.sock"],
			"PortBindings": {"8080/tcp": [{"HostPort": "8080"}]}, "RestartPolicy": {"Name": "unless-stopped"}},
		"NetworkSettings": {"Networks": {
			"hlm_default": {"Aliases": ["hlm", "`+id[:12]+`"], "IPAMConfig": null, "IPAddress": "172.20.0.2"},
			"monitoring": {"Aliases": null, "IPAMConfig": {"IPv4Address": "10.9.0.5"}}
		}}
	}`), &raw)
	var oldImg docker.Image
	oldImg.Config.Env = []string{"PATH=/usr/bin", "HLM_WEB_DIR=/app/web"}
	oldImg.Config.Entrypoint = []string{"/usr/local/bin/hlm"}
	oldImg.Config.Labels = map[string]string{"org.opencontainers.image.version": "v1.0.0"}

	body, extra := cloneConfig(raw, oldImg, "ghcr.io/me/hlm:v1.1.0", docker.Image{})
	if body["Image"] != "ghcr.io/me/hlm:v1.1.0" {
		t.Errorf("image = %v", body["Image"])
	}
	if _, ok := body["Hostname"]; ok {
		t.Error("default hostname (the old ID) was copied")
	}
	if env := fmt.Sprint(body["Env"]); env != "[HLM_TOKEN=secret]" {
		t.Errorf("env = %s; only settings the user added should be kept", env)
	}
	if _, ok := body["Entrypoint"]; ok {
		t.Error("entrypoint from the old image was copied")
	}
	labels := body["Labels"].(map[string]any)
	if _, ok := labels["org.opencontainers.image.version"]; ok || labels["com.docker.compose.service"] != "hlm" {
		t.Errorf("labels = %v", labels)
	}
	hc := body["HostConfig"].(map[string]any)
	if hc["NetworkMode"] != "hlm_default" || hc["PortBindings"] == nil {
		t.Errorf("host config = %v", hc)
	}
	eps := body["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	primary := eps["hlm_default"].(map[string]any)
	if fmt.Sprint(primary["Aliases"]) != "[hlm]" {
		t.Errorf("aliases = %v; the old ID alias should be dropped", primary["Aliases"])
	}
	if mon, ok := extra["monitoring"].(map[string]any); !ok || mon["IPAMConfig"] == nil {
		t.Errorf("extra networks = %v", extra)
	}
	// The original must be untouched.
	if raw["Config"].(map[string]any)["Image"] != "ghcr.io/me/hlm:v1.0.0" {
		t.Error("cloneConfig modified its input")
	}
}

func TestImageFor(t *testing.T) {
	rel := &Release{Version: "v1.2.0"}
	tests := map[string]string{
		"ghcr.io/me/hlm:latest":    "ghcr.io/me/hlm:latest", // follows the moving tag
		"ghcr.io/me/hlm":           "ghcr.io/me/hlm",
		"ghcr.io/me/hlm:v1.1.0":    "ghcr.io/me/hlm:v1.2.0", // pinned: move the pin
		"home-lab-manager-hlm":     "ghcr.io/me/hlm:v1.2.0", // built locally
		"ghcr.io/me/hlm@sha256:ab": "ghcr.io/me/hlm:v1.2.0",
	}
	for current, want := range tests {
		u := &Updater{cfg: Config{Image: "ghcr.io/me/hlm"}, image: current}
		if got := u.imageFor(rel); got != want {
			t.Errorf("imageFor(%s) = %s, want %s", current, got, want)
		}
	}
}

func TestImageForDockerHub(t *testing.T) {
	rel := &Release{Version: "v1.2.0"}
	tests := map[string]string{
		"danotchuuy/home-lab-manager:latest":                 "danotchuuy/home-lab-manager:latest",
		"docker.io/danotchuuy/home-lab-manager:latest":       "docker.io/danotchuuy/home-lab-manager:latest",
		"index.docker.io/danotchuuy/home-lab-manager:v1.1.0": "danotchuuy/home-lab-manager:v1.2.0",
		"danotchuuy/home-lab-manager:v1.1.0":                 "danotchuuy/home-lab-manager:v1.2.0",
		"ghcr.io/danotchuuy/home-lab-manager:latest":         "danotchuuy/home-lab-manager:v1.2.0", // another registry: move to Docker Hub's release tag
		// The image's name before the project was renamed: move to the new one.
		"danotchuuy/game-server-hoster:latest": "danotchuuy/home-lab-manager:v1.2.0",
		"danotchuuy/game-server-hoster:v1.1.0": "danotchuuy/home-lab-manager:v1.2.0",
	}
	for current, want := range tests {
		u := &Updater{cfg: Config{Image: "danotchuuy/home-lab-manager"}, image: current}
		if got := u.imageFor(rel); got != want {
			t.Errorf("imageFor(%s) = %s, want %s", current, got, want)
		}
	}
	if !sameRepo("nginx", "docker.io/library/nginx") || sameRepo("a/b", "c/b") {
		t.Error("sameRepo is wrong")
	}
}
