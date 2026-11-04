package configfiles

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

type fakeServers struct {
	info     docker.ContainerJSON
	files    []string // every file in the container
	searches int
}

func (f *fakeServers) Inspect(context.Context, string) (docker.ContainerJSON, error) {
	return f.info, nil
}
func (f *fakeServers) SearchFiles(_ context.Context, _, dir, q string) ([]files.Match, bool, error) {
	f.searches++
	var out []files.Match
	for _, p := range f.files {
		if strings.HasPrefix(p, dir+"/") && strings.Contains(strings.ToLower(p[strings.LastIndex(p, "/")+1:]), strings.ToLower(q)) {
			out = append(out, files.Match{Path: p})
		}
	}
	return out, false, nil
}

func setup(t *testing.T, image string, fs []string) (*Service, *fakeServers) {
	t.Helper()
	store, err := secrets.Open(t.TempDir(), "k")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServers{files: fs}
	f.info.ID, f.info.Name = "c1", "/palworld"
	f.info.Config.Image = image
	f.info.Mounts = []docker.MountPoint{{Type: "bind", Destination: "/serverdata/serverfiles"}, {Type: "tmpfs", Destination: "/tmp"}}
	return New(store, f), f
}

func TestSuggestions(t *testing.T) {
	s, f := setup(t, "ghcr.io/ich777/steamcmd:palworld", []string{
		"/serverdata/serverfiles/DefaultPalWorldSettings.ini",
		"/serverdata/serverfiles/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini",
		"/serverdata/serverfiles/Pal/Content/Old/PalWorldSettings.ini",
	})
	l, err := s.List(context.Background(), "palworld")
	if err != nil {
		t.Fatal(err)
	}
	want := []File{{Path: "/serverdata/serverfiles/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"}}
	if !slices.Equal(l.Suggested, want) || len(l.Pinned) != 0 {
		t.Errorf("list = %+v", l)
	}
	if f.searches != 1 {
		t.Errorf("searched %d times (tmpfs should be skipped)", f.searches)
	}
	// Cached for a while.
	s.List(context.Background(), "palworld")
	if f.searches != 1 {
		t.Errorf("searched again straight away")
	}
	s.now = func() time.Time { return time.Now().Add(suggestFor + time.Minute) }
	s.List(context.Background(), "palworld")
	if f.searches != 2 {
		t.Errorf("didn't look again after %v", suggestFor)
	}
	// Pinning it takes it out of the suggestions.
	if _, err := s.SetPinned("palworld", want); err != nil {
		t.Fatal(err)
	}
	l, _ = s.List(context.Background(), "palworld")
	if len(l.Suggested) != 0 || !slices.Equal(l.Pinned, want) {
		t.Errorf("after pinning: %+v", l)
	}
	// Unknown games get no suggestions, and aren't searched.
	s2, f2 := setup(t, "plexinc/pms-docker", nil)
	l, _ = s2.List(context.Background(), "plex")
	if len(l.Suggested) != 0 || f2.searches != 0 {
		t.Errorf("plex: %+v, %d searches", l, f2.searches)
	}
}

func TestPinned(t *testing.T) {
	s, _ := setup(t, "x", nil)
	for _, bad := range [][]File{
		{{Path: "relative/file.ini"}},
		{{Path: "/a.ini", Label: strings.Repeat("x", 65)}},
		{{Path: "/a\nb"}},
	} {
		if _, err := s.SetPinned("srv", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	got, err := s.SetPinned("srv", []File{
		{Path: " /data/../data/server.properties ", Label: "server.properties"},
		{Path: "/data/server.properties", Label: "dupe"},
		{Path: "/data/ops.json", Label: " Operators "},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []File{{Path: "/data/server.properties"}, {Path: "/data/ops.json", Label: "Operators"}}
	if !slices.Equal(got, want) {
		t.Errorf("pinned = %+v", got)
	}
	var many []File
	for i := range maxPinned + 1 {
		many = append(many, File{Path: "/f" + string(rune('a'+i))})
	}
	if _, err := s.SetPinned("srv", many); err == nil {
		t.Error("accepted too many")
	}
	if err := s.ForgetServer("srv"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pinned("srv"); len(p) != 0 {
		t.Errorf("after forgetting: %+v", p)
	}
}
