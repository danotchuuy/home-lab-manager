package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *Run { return &Run{Started: now.Add(-d)} }
	failed := func(d time.Duration) *Run { r := ago(d); r.Error = "boom"; return r }
	folders := []string{"/data"}
	for _, tc := range []struct {
		name string
		p    Plan
		want bool
	}{
		{"off", Plan{Folders: folders, Last: ago(100 * time.Hour)}, false},
		{"no folders", Plan{IntervalHours: 1}, false},
		{"never ran", Plan{Folders: folders, IntervalHours: 24}, true},
		{"not yet", Plan{Folders: folders, IntervalHours: 24, Last: ago(23 * time.Hour)}, false},
		{"due", Plan{Folders: folders, IntervalHours: 24, Last: ago(24 * time.Hour)}, true},
		{"failed retries after an hour", Plan{Folders: folders, IntervalHours: 24, Last: failed(time.Hour)}, true},
		{"failed, not yet", Plan{Folders: folders, IntervalHours: 24, Last: failed(59 * time.Minute)}, false},
	} {
		if got := due(tc.p, now, time.UTC); got != tc.want {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackupKeys(t *testing.T) {
	c := Settings{Prefix: "backups/games"}
	for key, want := range map[string]bool{
		"backups/games/valheim/valheim-20261026-120000.tar.gz":       true,
		"backups/games/valheim/other-20261026-120000.tar.gz":         false,
		"backups/games/valheim/sub/valheim-20261026-120000.tar.gz":   false,
		"backups/games/valheim2/valheim2-20261026-120000.tar.gz":     false,
		"backups/games/valheim/valheim-20261026-120000.tar.gz.extra": false,
	} {
		if got := serverSeries(c, "valheim").has(key); got != want {
			t.Errorf("%s: %v, want %v", key, got, want)
		}
	}
}

func TestLegacyBackupLocations(t *testing.T) {
	// Settings saved before the rename without a folder keep using gsh;
	// saving now stores the new default.
	if p := (Settings{}).prefix(); p != "gsh" {
		t.Errorf("prefix of old settings = %q", p)
	}
	store, _ := secrets.Open(t.TempDir(), "")
	s := New(store, &fakeServers{}, slog.New(slog.DiscardHandler))
	if c, err := s.merge(Settings{Prefix: " / "}); err != nil || c.Prefix != "hlm" {
		t.Errorf("merged prefix = %q, %v", c.Prefix, err)
	}
	r := configSeries(Settings{Prefix: "b"})
	for key, want := range map[string]bool{
		"b/_hlm/hlm-config-20261026-120000.tar.gz": true,
		"b/_gsh/gsh-config-20261026-120000.tar.gz": true,
		"b/_gsh/hlm-config-20261026-120000.tar.gz": false,
	} {
		if got := r.has(key); got != want {
			t.Errorf("%s: %v, want %v", key, got, want)
		}
	}
}

// fakeServers is one server, "srv", whose files (by archive name) are kept
// in a map.
type fakeServers struct {
	mu      sync.Mutex
	running bool
	files   map[string]string
	mtime   map[string]time.Time // by path, for ListFolder
	calls   []string
}

func (f *fakeServers) Inspect(ctx context.Context, id string) (docker.ContainerJSON, error) {
	var info docker.ContainerJSON
	if id != "srv" && id != "abc123" {
		return info, errors.New("server not found")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	info.Name, info.State.Running = "/srv", f.running
	return info, nil
}

func (f *fakeServers) ArchiveFolders(ctx context.Context, id string, folders, exclude []string, tw *tar.Writer, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "archive "+strings.Join(folders, ","))
	var st files.ArchiveStats
	names := make([]string, 0, len(f.files))
	for name := range f.files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		in := len(folders) == 0
		for _, d := range folders {
			d = strings.TrimPrefix(d, "/")
			in = in || name == d || strings.HasPrefix(name, d+"/")
		}
		if !in {
			continue
		}
		body := f.files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return st, err
		}
		io.WriteString(tw, body)
		st.Files++
		st.Bytes += int64(len(body))
		progress(st)
	}
	return st, nil
}

func (f *fakeServers) RestoreArchive(ctx context.Context, id string, r io.Reader, progress func(files.ArchiveStats)) (files.ArchiveStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "restore")
	var st files.ArchiveStats
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return st, nil
		}
		if err != nil {
			return st, err
		}
		body, _ := io.ReadAll(tr)
		f.files[hdr.Name] = string(body)
		st.Files++
		progress(st)
	}
}

func (f *fakeServers) setFile(name, body string) {
	f.mu.Lock()
	f.files[name] = body
	f.mu.Unlock()
}

func (f *fakeServers) file(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[name]
}

func (f *fakeServers) ListFolder(ctx context.Context, id, dir string) ([]files.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = strings.TrimPrefix(dir, "/")
	seen := map[string]files.Entry{}
	for name := range f.files {
		rest, ok := strings.CutPrefix(name, dir+"/")
		if dir == "" {
			rest, ok = name, true
		}
		if !ok || rest == "" {
			continue
		}
		first, more, _ := strings.Cut(rest, "/")
		e := files.Entry{Name: first, Type: "file", Mtime: f.mtime[path.Join(dir, first)]}
		if more != "" {
			e.Type = "dir"
		}
		seen[first] = e
	}
	if len(seen) == 0 {
		return nil, files.ErrNotFound
	}
	var out []files.Entry
	for _, e := range seen {
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeServers) Start(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.running = append(f.calls, "start"), true
	return nil
}

func (f *fakeServers) Stop(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.running = append(f.calls, "stop"), false
	return nil
}

// seaweed starts SeaweedFS with its S3 gateway on a free port and returns
// the endpoint. Enable with HLM_DOCKER_TESTS=1.
func seaweed(t *testing.T) string {
	t.Helper()
	return startSeaweed(t, "", "")
}

// startSeaweed starts SeaweedFS; with a TLS directory holding server.crt and
// server.key, its S3 gateway serves https, trusted through caPEM.
func startSeaweed(t *testing.T, tlsDir, caPEM string) string {
	t.Helper()
	if os.Getenv("HLM_DOCKER_TESTS") == "" {
		t.Skip("set HLM_DOCKER_TESTS=1 to run tests against Docker")
	}
	cfg := t.TempDir() + "/s3.json"
	os.WriteFile(cfg, []byte(`{"identities":[{"name":"hlm","credentials":[{"accessKey":"hlmkey","secretKey":"hlmsecret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}`), 0o644)
	os.Chmod(cfg, fs.FileMode(0o644))
	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::8333", "-v", cfg + ":/s3.json:ro"}
	// SeaweedFS preallocates 7 volumes of volumeSizeLimitMB each per bucket
	// (30 GB by default, 7 GB on disk); keep that small for tests.
	weed := []string{"server", "-s3", "-s3.config=/s3.json", "-dir=/data", "-master.volumeSizeLimitMB=16"}
	scheme := "http://"
	if tlsDir != "" {
		args = append(args, "-v", tlsDir+":/tls:ro")
		weed = append(weed, "-s3.cert.file=/tls/server.crt", "-s3.key.file=/tls/server.key")
		scheme = "https://"
	}
	args = append(append(args, "chrislusf/seaweedfs:3.80"), weed...)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("start seaweedfs: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	// -v also removes SeaweedFS's anonymous data volume.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", id).Run() })
	out, err = exec.Command("docker", "port", id, "8333/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := scheme + strings.TrimSpace(strings.Split(string(out), "\n")[0])

	// Wait for the gateway, then make the bucket with the weed shell.
	cl, err := s3.New(s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret", CACert: caPEM})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		mk := exec.Command("docker", "exec", id, "sh", "-c", "echo 's3.bucket.create -name games' | weed shell")
		mk.Run()
		_, err := cl.List(context.Background(), "")
		if err == nil {
			return endpoint
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("seaweedfs didn't come up: %v\n%s", err, logs)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestSeaweedFS(t *testing.T) {
	endpoint := seaweed(t)
	ctx := context.Background()
	store, err := secrets.Open(t.TempDir(), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	servers := &fakeServers{running: true, files: map[string]string{"data/server.cfg": "port=1", "data/world.sav": "day 1"}}
	s := New(store, servers, slog.New(slog.DiscardHandler))

	settings := Settings{Settings: s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "wrong"}, Prefix: "/homelab/"}
	if _, err := s.Save(ctx, settings); err == nil {
		t.Fatal("saved settings with a wrong secret key")
	}
	settings.SecretKey = "hlmsecret"
	pub, err := s.Save(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if pub.HasSecret != true || pub.Prefix != "homelab" {
		t.Errorf("public settings = %+v", pub)
	}
	// An empty secret keeps the saved one.
	settings.SecretKey = ""
	if _, err := s.Test(ctx, settings); err != nil {
		t.Errorf("test with the saved secret: %v", err)
	}

	if err := s.Start(ctx, "srv"); err == nil {
		t.Error("started a backup with no folders chosen")
	}
	if _, err := s.SetPlan(ctx, "abc123", Plan{Folders: []string{"/data/", "/data/saves"}, Keep: 2, StopServer: true}); err != nil {
		t.Fatal(err)
	}

	// Three backups, a second apart so their names differ; Keep is 2.
	for i := 0; i < 3; i++ {
		if i > 0 {
			nextSecond()
		}
		if err := s.Start(ctx, "srv"); err != nil {
			t.Fatal(err)
		}
		o := wait(t, s)
		if o.Plan.Last == nil || o.Plan.Last.Error != "" || o.Plan.Last.Files != 2 {
			t.Fatalf("last run = %+v", o.Plan.Last)
		}
	}
	o, _ := s.Overview(ctx, "srv")
	if len(o.Backups) != 2 || o.Error != "" || o.Backups[0].Key != o.Plan.Last.Key {
		t.Fatalf("backups = %+v (error %q), last = %s", o.Backups, o.Error, o.Plan.Last.Key)
	}
	if !strings.HasPrefix(o.Plan.Last.Key, "homelab/srv/srv-") {
		t.Errorf("key = %s", o.Plan.Last.Key)
	}
	// The list of every server's backups has the same ones.
	all, err := s.All(ctx, func(string) bool { return true }, true)
	if err != nil || !all.Configured || len(all.Servers) != 1 {
		t.Fatalf("all = %+v %v", all, err)
	}
	if sb := all.Servers[0]; sb.Server != "srv" || len(sb.Stored) != 2 || sb.Stored[0].Key != o.Plan.Last.Key || sb.StoredSize == 0 || sb.StoredError != "" {
		t.Errorf("all: %+v", sb)
	}
	servers.mu.Lock()
	if want := []string{"stop", "archive /data", "start"}; !slices.Equal(servers.calls[:3], want) {
		t.Errorf("calls = %v, want %v first", servers.calls, want)
	}
	servers.mu.Unlock()

	// Download and read the newest backup back.
	rc, _, err := s.Download(ctx, "srv", o.Backups[0].Key, "")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if string(body) != servers.file(hdr.Name) {
			t.Errorf("%s: %q", hdr.Name, body)
		}
		names = append(names, hdr.Name)
	}
	rc.Close()
	if !slices.Equal(names, []string{"data/server.cfg", "data/world.sav"}) {
		t.Errorf("archive entries = %v", names)
	}

	// Restore the newest backup over changed files, backing them up first.
	restored := o.Backups[0].Key
	servers.setFile("data/world.sav", "day 2")
	servers.mu.Lock()
	servers.calls = nil
	servers.mu.Unlock()
	nextSecond()
	if err := s.Restore(ctx, "srv", restored, "", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx, "srv"); !errors.Is(err, ErrRunning) {
		t.Errorf("backup during a restore: err = %v", err)
	}
	o = wait(t, s)
	r := o.Plan.LastRestore
	if r == nil || r.Error != "" || r.Key != restored || r.Files != 2 || r.Before == "" {
		t.Fatalf("last restore = %+v", r)
	}
	if got := servers.file("data/world.sav"); got != "day 1" {
		t.Errorf("after restore, world.sav = %q", got)
	}
	servers.mu.Lock()
	if want := []string{"stop", "archive /data", "restore", "start"}; !slices.Equal(servers.calls, want) {
		t.Errorf("calls = %v, want %v", servers.calls, want)
	}
	servers.mu.Unlock()
	// The backup made first holds what was replaced, and isn't pruned.
	if len(o.Backups) != 3 || o.Backups[0].Key != r.Before {
		t.Errorf("backups = %+v, before = %s", o.Backups, r.Before)
	}

	// The restore can be undone by restoring that backup, without one first.
	if err := s.Restore(ctx, "srv", r.Before, "", false); err != nil {
		t.Fatal(err)
	}
	o = wait(t, s)
	if got := servers.file("data/world.sav"); got != "day 2" || o.Plan.LastRestore.Before != "" {
		t.Errorf("after undo, world.sav = %q, restore = %+v", got, o.Plan.LastRestore)
	}
	if err := s.Restore(ctx, "srv", "homelab/srv/srv-nope.tar.gz", "", false); !errors.Is(err, ErrBadKey) {
		t.Errorf("bad key: err = %v", err)
	}

	if _, _, err := s.Download(ctx, "srv", "homelab/other/other-20260101-000000.tar.gz", ""); !errors.Is(err, ErrBadKey) {
		t.Errorf("another server's key: err = %v", err)
	}
	if err := s.DeleteBackup(ctx, "srv", o.Backups[1].Key, ""); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.Overview(ctx, "srv"); len(after.Backups) != len(o.Backups)-1 {
		t.Errorf("after delete: %d backups, want %d", len(after.Backups), len(o.Backups)-1)
	}
}

// wait waits for the running backup or restore to finish.
func wait(t *testing.T, s *Service) Overview {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		o, err := s.Overview(context.Background(), "srv")
		if err != nil {
			t.Fatal(err)
		}
		if o.Running == nil {
			return o
		}
		if time.Now().After(deadline) {
			t.Fatal("didn't finish")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// nextSecond waits for the next second, so a new backup's name differs.
func nextSecond() {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
}

// A large backup goes up in several parts.
func TestSeaweedFSMultipart(t *testing.T) {
	endpoint := seaweed(t)
	cl, err := s3.New(s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	size := int64(s3.PartSize*2 + 12345)
	n, err := cl.Upload(ctx, "big/file with spaces+plus.bin", io.LimitReader(zeroReader{}, size), "application/octet-stream", nil)
	if err != nil || n != size {
		t.Fatalf("upload: %d, %v", n, err)
	}
	objs, err := cl.List(ctx, "big/")
	if err != nil || len(objs) != 1 || objs[0].Size != size || objs[0].Key != "big/file with spaces+plus.bin" {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	rc, _, err := cl.Get(ctx, objs[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.Copy(io.Discard, rc)
	rc.Close()
	if got != size {
		t.Errorf("read back %d bytes, want %d", got, size)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Verifying, damage, and copies to encrypted storage, against SeaweedFS.
func TestSeaweedFSVerifyAndCopy(t *testing.T) {
	endpoint := seaweed(t)
	ctx := context.Background()
	store, _ := secrets.Open(t.TempDir(), "test-key")
	servers := &fakeServers{running: true, files: map[string]string{"data/world.sav": strings.Repeat("day 1 ", 100000)}}
	s := New(store, servers, slog.New(slog.DiscardHandler))
	set := func(prefix string) Settings {
		return Settings{Settings: s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret"}, Prefix: prefix}
	}
	if _, err := s.Save(ctx, set("main")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTarget(ctx, Target{Name: "Offsite", Settings: set("offsite"), Encrypt: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPlan(ctx, "srv", Plan{Folders: []string{"/data"}, CopyTo: &CopyPlan{Target: "offsite"}}); err != nil {
		t.Fatal(err)
	}
	run, err := s.BackupNow(ctx, "srv", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run.Verify == nil || !run.Verify.OK || !run.Verify.Checksum || run.Copy == nil || run.Copy.Error != "" {
		t.Fatalf("run = %+v, verify %+v, copy %+v", run, run.Verify, run.Copy)
	}
	if v, err := s.VerifyBackup(ctx, "srv", run.Copy.Key, "offsite"); err != nil || !v.OK || v.Files != 1 {
		t.Errorf("verify the encrypted copy = %+v, %v", v, err)
	}
	// Damage the stored backup: verifying it fails.
	cl, _ := s3.New(set("main").Settings)
	rc, _, _ := cl.Get(ctx, run.Key)
	data, _ := io.ReadAll(rc)
	rc.Close()
	data[len(data)/2] ^= 0xff
	if err := cl.Put(ctx, run.Key, data, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.VerifyBackup(ctx, "srv", run.Key, ""); err != nil || v.OK {
		t.Errorf("verify damaged = %+v, %v", v, err)
	}
}
