package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/s3"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// configService is a service whose data directory holds a secrets store
// with a key file, and a templates file.
func configService(t *testing.T, keyFromEnv bool) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	envKey := ""
	if keyFromEnv {
		envKey = "from-env"
	}
	store, err := secrets.Open(filepath.Join(dir, "secrets"), envKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("unifi", map[string]string{"url": "https://192.168.1.1"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "templates.json"), []byte(`{"templates":[]}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "cache"), 0o755)
	os.WriteFile(filepath.Join(dir, "cache", "applicationFeed.json"), []byte("{}"), 0o644)
	s := New(store, &fakeServers{files: map[string]string{}}, slog.New(slog.DiscardHandler))
	s.UseConfig(ConfigSource{DataDir: dir, KeyFromEnv: keyFromEnv, Servers: func(context.Context) (any, error) {
		return []map[string]string{{"name": "valheim", "templateId": "valheim-1"}}, nil
	}})
	return s, dir
}

func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[hdr.Name] = string(b)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestConfigArchive(t *testing.T) {
	s, dir := configService(t, false)
	ctx := context.Background()

	var run Run
	data, err := s.configArchive(ctx, false, &run)
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, data)
	if want := []string{"README.txt", "secrets/secrets.enc", "servers.json", "templates.json"}; !slices.Equal(keys(got), want) {
		t.Errorf("without the key: %v, want %v", keys(got), want)
	}
	enc, _ := os.ReadFile(filepath.Join(dir, "secrets", "secrets.enc"))
	if got["secrets/secrets.enc"] != string(enc) || !strings.Contains(got["servers.json"], `"valheim-1"`) {
		t.Errorf("contents: %v", got)
	}
	if !strings.Contains(got["README.txt"], "isn't in this backup") || run.Files != 4 {
		t.Errorf("README = %q, run = %+v", got["README.txt"], run)
	}

	// With the key, the backup alone is enough to read the secrets.
	data, err = s.configArchive(ctx, true, &Run{})
	if err != nil {
		t.Fatal(err)
	}
	got = untar(t, data)
	restored := t.TempDir()
	for name, body := range got {
		p := filepath.Join(restored, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o600)
	}
	store, err := secrets.Open(filepath.Join(restored, "secrets"), "")
	if err != nil {
		t.Fatal(err)
	}
	var unifi map[string]string
	if ok, err := store.Get("unifi", &unifi); !ok || err != nil || unifi["url"] != "https://192.168.1.1" {
		t.Errorf("secrets from the restored backup: %v %v %v", unifi, ok, err)
	}
}

// With HLM_SECRET_KEY there's no key file to include, even when asked.
func TestConfigArchiveEnvKey(t *testing.T) {
	s, _ := configService(t, true)
	data, err := s.configArchive(context.Background(), true, &Run{})
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, data)
	if _, ok := got["secrets/secret.key"]; ok {
		t.Error("included a key file")
	}
	if !strings.Contains(got["README.txt"], "HLM_SECRET_KEY") {
		t.Errorf("README = %q", got["README.txt"])
	}
}

func TestConfigPlanDefaults(t *testing.T) {
	s, _ := configService(t, false)
	o, err := s.ConfigOverview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if o.Configured || o.Plan.IntervalHours != 24 || o.Plan.Keep != 14 || o.Plan.IncludeKey {
		t.Errorf("defaults = %+v", o)
	}
	if !dueEvery(o.Plan.IntervalHours, nil, time.Now()) {
		t.Error("a first configuration backup isn't due")
	}
	if err := s.StartConfig(); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("without storage: err = %v", err)
	}
	if p, err := s.SetConfigPlan(ConfigPlan{IntervalHours: 0, Keep: 3, IncludeKey: true}); err != nil || p.IntervalHours != 0 || !p.IncludeKey {
		t.Errorf("saved plan = %+v, %v", p, err)
	}
	if o, _ := s.ConfigOverview(context.Background()); o.Plan.IntervalHours != 0 || o.Plan.Keep != 3 {
		t.Errorf("after save = %+v", o.Plan)
	}
}

func TestSeaweedFSConfig(t *testing.T) {
	endpoint := seaweed(t)
	s, _ := configService(t, false)
	ctx := context.Background()
	if _, err := s.Save(ctx, Settings{Settings: s3.Settings{Endpoint: endpoint, Bucket: "games", AccessKey: "hlmkey", SecretKey: "hlmsecret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConfigPlan(ConfigPlan{IntervalHours: 24, Keep: 2}); err != nil {
		t.Fatal(err)
	}
	var o ConfigOverview
	for i := 0; i < 3; i++ {
		if i > 0 {
			nextSecond()
		}
		if err := s.StartConfig(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			var err error
			if o, err = s.ConfigOverview(ctx); err != nil {
				t.Fatal(err)
			}
			if o.Running == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("didn't finish")
			}
		}
		if o.Plan.Last == nil || o.Plan.Last.Error != "" {
			t.Fatalf("last = %+v", o.Plan.Last)
		}
	}
	if len(o.Backups) != 2 || o.Backups[0].Key != o.Plan.Last.Key || !strings.HasPrefix(o.Plan.Last.Key, "hlm/_hlm/hlm-config-") {
		t.Fatalf("backups = %+v, last = %s", o.Backups, o.Plan.Last.Key)
	}
	rc, _, err := s.DownloadConfig(ctx, o.Backups[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if got := untar(t, data); got["templates.json"] != `{"templates":[]}` {
		t.Errorf("downloaded: %v", keys(got))
	}
	// Config backups and server backups can't be reached through each other.
	if _, _, err := s.DownloadConfig(ctx, "hlm/srv/srv-20260101-000000.tar.gz"); !errors.Is(err, ErrBadKey) {
		t.Errorf("server key through config: %v", err)
	}
	if err := s.DeleteConfigBackup(ctx, o.Backups[1].Key); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.ConfigOverview(ctx); len(o.Backups) != 1 {
		t.Errorf("after delete: %d", len(o.Backups))
	}
}

func TestConfigArchiveAudit(t *testing.T) {
	s, dir := configService(t, false)
	os.MkdirAll(filepath.Join(dir, "audit"), 0o700)
	os.WriteFile(filepath.Join(dir, "audit", "audit-2026-10.jsonl"), []byte(`{"action":"a"}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "audit", "audit-2026-11.jsonl"), []byte(`{"action":"b"}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "audit", "notes.txt"), []byte("not part of the log"), 0o600)
	data, err := s.configArchive(context.Background(), false, &Run{})
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, data)
	if got["audit/audit-2026-10.jsonl"] != `{"action":"a"}`+"\n" || got["audit/audit-2026-11.jsonl"] != `{"action":"b"}`+"\n" {
		t.Errorf("audit files: %v", keys(got))
	}
	if _, ok := got["audit/notes.txt"]; ok {
		t.Error("included a file that isn't part of the log")
	}
	if !strings.Contains(got["README.txt"], "audit/") {
		t.Error("README doesn't mention the audit log")
	}
}
