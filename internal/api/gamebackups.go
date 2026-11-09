package api

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// GameBackupSuggestion is a folder the server's game profile says the game
// keeps its own backups in, found in this server's container.
type GameBackupSuggestion struct {
	backup.GameFolder
	Note string `json:"note,omitempty"`
}

// gameBackupSuggestions finds where a server's game keeps its backups: its
// profile's folders, under each of the server's data folders. A data
// folder mounted partway down one (…/Pal/Saved for Pal/Saved/SaveGames/…)
// counts too. Only folders that are there are suggested.
func (s *server) gameBackupSuggestions(ctx context.Context, id string) []GameBackupSuggestion {
	info, err := s.Manager.Inspect(ctx, id)
	if err != nil {
		return nil
	}
	p := gameprofile.Match(info.Config.Image)
	if p == nil || len(p.GameBackups) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := []GameBackupSuggestion{}
	seen := map[string]bool{}
	for _, g := range p.GameBackups {
		for _, full := range gameBackupCandidates(info, g.Path) {
			if seen[full] || !s.folderThere(ctx, id, full) {
				continue
			}
			seen[full] = true
			out = append(out, GameBackupSuggestion{GameFolder: backup.GameFolder{Path: full, Match: g.Match}, Note: g.Note})
		}
	}
	return out
}

// gameBackupCandidates are where a profile's game backup folder could be
// in a container: under each data folder, or carrying on from one that is
// itself partway down it.
func gameBackupCandidates(info docker.ContainerJSON, rel string) []string {
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	var out []string
	for _, m := range files.Mounts(info) {
		mount := path.Clean(m.Path)
		for i := range segs {
			if i > 0 && !strings.HasSuffix(mount, "/"+strings.Join(segs[:i], "/")) {
				continue
			}
			out = append(out, path.Join(mount, strings.Join(segs[i:], "/")))
		}
	}
	return out
}

// folderThere reports whether a folder exists: the part of it before any *,
// as the rest is found when its backups are uploaded.
func (s *server) folderThere(ctx context.Context, id, p string) bool {
	if i := strings.Index(p, "/*"); i >= 0 {
		p = p[:i]
	}
	_, err := s.Manager.ListFolder(ctx, id, p)
	return err == nil
}
