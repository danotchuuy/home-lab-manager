package agent

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The agent reads a server's bind-mounted folders from the host's disk and
// sends them as tar, never calling Docker's archive API (which would lock
// the container); it refuses containers that aren't hlm's; an older agent's
// refusal is told apart, so hlm can go through Docker instead.
func TestAgentArchive(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	root := t.TempDir()
	data := filepath.Join(root, "mnt", "games", "pal")
	if err := os.MkdirAll(filepath.Join(data, "Saved", "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "Saved", "world.sav"), []byte("the world"), 0o644)
	os.WriteFile(filepath.Join(data, "Saved", "logs", "a.log"), []byte("noise"), 0o644)

	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var archiveCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.47")
		io.WriteString(w, "OK")
	})
	inspect := func(managed bool) map[string]any {
		labels := map[string]string{}
		if managed {
			labels["io.hlm.managed"] = "true"
		}
		return map[string]any{
			"Id": "abc", "Name": "/pal", "Config": map[string]any{"Labels": labels},
			"Mounts": []map[string]any{{"Type": "bind", "Source": "/mnt/games/pal", "Destination": "/palworld", "RW": true}},
		}
	}
	mux.HandleFunc("GET /v1.47/containers/pal/json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(inspect(true)) })
	mux.HandleFunc("GET /v1.47/containers/other/json", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(inspect(false)) })
	mux.HandleFunc("/v1.47/containers/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		archiveCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container"}`)
	})
	dsrv := &http.Server{Handler: mux}
	go dsrv.Serve(ln)
	defer dsrv.Close()

	hub := NewHub(log)
	hub.Authenticate = func(host, secret string) bool { return host == "node" && secret == "s" }
	hmux := http.NewServeMux()
	hmux.Handle("GET /api/agent/connect", hub)
	hmux.HandleFunc("POST /api/agent/enrol", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(EnrolResponse{Host: "node", Credential: Credential("node", "s")})
	})
	srv := httptest.NewServer(hmux)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Config{Server: srv.URL, Token: "t", DataDir: t.TempDir(), Socket: sock, AllowHTTP: true, RootPath: root, Log: log})
	waitFor(t, "the agent to connect", func() bool { return hub.Status("node").Connected })

	a, err := hub.Archive(ctx, "node", ArchiveRequest{ID: "pal", Folders: []string{"/palworld/Saved"}, Exclude: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr := tar.NewReader(a)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
	io.Copy(io.Discard, a)
	a.Close()
	st, err := a.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got["palworld/Saved/world.sav"] != "the world" || len(got) != 2 {
		t.Errorf("entries %v", got)
	}
	if _, ok := got["palworld/Saved/logs/a.log"]; ok {
		t.Error("excluded file sent")
	}
	if st.Files != 1 || st.Bytes != int64(len("the world")) || st.Excluded != 1 {
		t.Errorf("stats %+v", st)
	}
	mu.Lock()
	if archiveCalls != 0 {
		t.Errorf("Docker's archive API called %d times", archiveCalls)
	}
	mu.Unlock()

	// A folder that isn't there: the error comes with the stream's end.
	a, err = hub.Archive(ctx, "node", ArchiveRequest{ID: "pal", Folders: []string{"/palworld/Nope"}})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, a)
	a.Close()
	if _, err := a.Result(); err == nil || !strings.Contains(err.Error(), "/palworld/Nope") {
		t.Errorf("missing folder: %v", err)
	}

	// Not one of hlm's servers, or not there at all.
	if _, err := hub.Archive(ctx, "node", ArchiveRequest{ID: "other", Folders: []string{"/palworld"}}); err == nil || !strings.Contains(err.Error(), "isn't one of hlm's") {
		t.Errorf("unmanaged: %v", err)
	}
	if _, err := hub.Archive(ctx, "node", ArchiveRequest{ID: "gone", Folders: []string{"/x"}}); err == nil || errors.Is(err, ErrArchiveUnsupported) {
		t.Errorf("missing container: %v", err)
	}
	if Allowed("POST", ArchivePath) {
		t.Error("archive path in the Docker allow-list")
	}

	// An older agent: its Docker allow-list answers.
	old := httptest.NewServer(Handler(DockerProxy(sock, log), nil, nil, nil))
	defer old.Close()
	resp, err := http.Post(old.URL+ArchivePath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]string
	json.NewDecoder(resp.Body).Decode(&msg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || msg["message"] == "" || msg["error"] != "" {
		t.Errorf("older agent: %d %v", resp.StatusCode, msg)
	}
}
