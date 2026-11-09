package api

import (
	"slices"
	"testing"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// A profile's game backup folder is looked for under each data folder,
// and carried on from one mounted partway down it.
func TestGameBackupCandidates(t *testing.T) {
	var info docker.ContainerJSON
	info.Mounts = []docker.MountPoint{
		{Type: "bind", Source: "/mnt/pal", Destination: "/serverdata/serverfiles"},
		{Type: "bind", Source: "/mnt/saved", Destination: "/palworld/Pal/Saved"},
	}
	got := gameBackupCandidates(info, "Pal/Saved/SaveGames/0/*/backup/world")
	for _, want := range []string{"/serverdata/serverfiles/Pal/Saved/SaveGames/0/*/backup/world", "/palworld/Pal/Saved/SaveGames/0/*/backup/world"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s not in %v", want, got)
		}
	}
	if p := gameprofile.Match("ghcr.io/ich777/steamcmd:palworld"); p == nil || len(p.GameBackups) == 0 {
		t.Error("Palworld's profile has no game backup folders")
	}
}
