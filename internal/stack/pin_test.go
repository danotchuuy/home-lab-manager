package stack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/imagetag"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

// fakeVersions is a registry: each tag's digest.
type fakeVersions map[string]string // "repo:tag" → digest

func (f fakeVersions) Digest(_ context.Context, image string) (string, error) {
	if d, ok := f[image]; ok {
		return d, nil
	}
	return "", fmt.Errorf("no %s", image)
}

func (f fakeVersions) VersionOf(_ context.Context, image, digest string) (string, error) {
	ref, _ := imagetag.Parse(image)
	var tags []string
	for k, d := range f {
		if name, tag, _ := strings.Cut(k, ":"); name == ref.Name && d == digest {
			tags = append(tags, tag)
		}
	}
	if b := imagetag.Best(tags); b != "" {
		return b, nil
	}
	return "", imagetag.ErrNoVersion
}

const latestFile = `services:
  homebridge:
    image: homebridge/homebridge:latest # the bridge
    network_mode: host
  web:
    image: "nginx"
  db:
    image: postgres:16
  cache:
    image: redis:latest
`

func TestAdoptPinsLatest(t *testing.T) {
	store, _ := secrets.Open(t.TempDir(), "")
	pi := newMemDocker()
	pi.files["/home/ubuntu/hb/docker-compose.yaml"] = latestFile
	composeContainer(pi, "hb", "homebridge", "/home/ubuntu/hb/docker-compose.yaml")
	composeContainer(pi, "hb", "cache", "/home/ubuntu/hb/docker-compose.yaml")
	// homebridge runs an older image than latest; web isn't running.
	pi.images = map[string]docker.Image{
		"img-homebridge": {ID: "sha256:hb1", RepoDigests: []string{"homebridge/homebridge@sha256:old"}},
		"img-cache":      {ID: "sha256:r", RepoDigests: []string{"redis@sha256:built-here"}},
	}
	reg := fakeVersions{
		"homebridge/homebridge:latest": "sha256:new", "homebridge/homebridge:2026-11-09": "sha256:new",
		"homebridge/homebridge:2026-11-02": "sha256:old", "homebridge/homebridge:ubuntu": "sha256:old",
		"nginx:latest": "sha256:n", "nginx:1.31": "sha256:n", "nginx:1.31.8": "sha256:n",
	}
	mgr := &hostsManager{hosts: map[string]*memDocker{"pi": pi}}
	s := New(store, func(host string) (Docker, error) {
		if d, ok := mgr.hosts[host]; ok {
			return d, nil
		}
		return nil, fmt.Errorf("there's no Docker host %q", host)
	}, mgr, slog.New(slog.DiscardHandler))
	s.UseVersions(reg)
	ctx := context.Background()

	ap, err := s.AdoptPreview(ctx, "pi", "/home/ubuntu/hb")
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]string{}
	for _, sp := range ap.Plan.Services {
		if sp.Pinned != nil {
			pinned[sp.Service] = sp.Pinned.From + " → " + sp.Pinned.To
		}
	}
	// The version it runs, not the newest; nginx (not running) gets the
	// version latest is; redis has no version with its digest.
	want := map[string]string{"homebridge": "homebridge/homebridge:latest → homebridge/homebridge:2026-11-02", "web": "nginx → nginx:1.31.8"}
	if fmt.Sprint(pinned) != fmt.Sprint(want) {
		t.Errorf("pinned %v", pinned)
	}
	if w := strings.Join(ap.Plan.Warnings, "\n"); !strings.Contains(w, "service cache: couldn't find which version redis:latest is") {
		t.Errorf("warnings %s", w)
	}

	if err := s.Adopt(ctx, "pi", "/home/ubuntu/hb", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Definition("hb")
	wantYAML := strings.NewReplacer("homebridge/homebridge:latest", "homebridge/homebridge:2026-11-02", `"nginx"`, `"nginx:1.31.8"`).Replace(latestFile)
	if d.YAML != wantYAML || fmt.Sprint(d.Track) != "map[homebridge:{homebridge/homebridge latest 2026-11-02} web:{nginx latest 1.31.8}]" {
		t.Fatalf("definition %q %v", d.YAML, d.Track)
	}
	if s.Tracks("hb", "homebridge") != "latest" || s.Tracks("hb", "db") != "" {
		t.Error("Tracks")
	}
	images := func() map[string]bool {
		out := map[string]bool{}
		for _, c := range pi.containers {
			out[c.Image] = true
		}
		return out
	}
	if im := images(); !im["homebridge/homebridge:2026-11-02"] || !im["nginx:1.31.8"] {
		t.Errorf("containers' images %v", im)
	}

	// Editing the file keeps what's followed.
	if err := s.Apply(ctx, "hb", strings.Replace(d.YAML, "postgres:16", "postgres:17", 1), "", true, "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	// Latest moves on: updating changes the file to the new version.
	var msgs []string
	if err := s.Update(ctx, "hb", func(e Event) { msgs = append(msgs, e.Message) }); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Definition("hb")
	if !strings.Contains(d.YAML, "image: homebridge/homebridge:2026-11-09 # the bridge") || !strings.Contains(d.YAML, `"nginx:1.31.8"`) ||
		!strings.Contains(d.YAML, "postgres:17") {
		t.Errorf("after the update:\n%s", d.YAML)
	}
	if !strings.Contains(strings.Join(msgs, "\n"), "homebridge: homebridge/homebridge:latest is now version 2026-11-09 (was 2026-11-02)") {
		t.Errorf("events %v", msgs)
	}
	if im := images(); !im["homebridge/homebridge:2026-11-09"] {
		t.Errorf("not recreated with the new version: %v", im)
	}
	if f := d.Track["homebridge"]; f.Version != "2026-11-09" {
		t.Errorf("follow after the update: %+v", f)
	}

	// Setting a version by hand stops following latest.
	if err := s.Apply(ctx, "hb", strings.Replace(d.YAML, "homebridge/homebridge:2026-11-09", "homebridge/homebridge:2026-11-02", 1), "", true, "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if s.Tracks("hb", "homebridge") != "" || s.Tracks("hb", "web") != "latest" {
		t.Errorf("after a hand edit: %q %q", s.Tracks("hb", "homebridge"), s.Tracks("hb", "web"))
	}
	if err := s.Update(ctx, "hb", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Definition("hb"); !strings.Contains(d.YAML, "homebridge/homebridge:2026-11-02") {
		t.Errorf("moved after a hand edit:\n%s", d.YAML)
	}
}
