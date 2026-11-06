package players

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// readList reads one of a server's player lists from its file.
func (s *Service) readList(ctx context.Context, info docker.ContainerJSON, f gameprofile.ListFile) List {
	l := List{File: f.File, Format: f.Format, Players: []Player{}}
	l.Editable = f.Format == gameprofile.FormatLines && !info.State.Running
	p, err := s.findFile(ctx, info, f)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	l.Path = p
	file, err := s.opts.Servers.ReadFile(ctx, info.ID, p)
	if err != nil {
		l.Error = err.Error()
		return l
	}
	players, err := parseList(f.Format, file.Content)
	if err != nil {
		l.Error = fmt.Sprintf("%s: %v", f.File, err)
		return l
	}
	l.Players = players
	return l
}

// parseList reads a list file's players.
func parseList(format, content string) ([]Player, error) {
	out := []Player{}
	switch format {
	case gameprofile.FormatMinecraftJSON:
		if strings.TrimSpace(content) == "" {
			return out, nil
		}
		var entries []struct {
			Name   string `json:"name"`
			UUID   string `json:"uuid"`
			XUID   string `json:"xuid"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(content), &entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			out = append(out, Player{Name: e.Name, ID: cmpOr(e.UUID, e.XUID), Reason: e.Reason})
		}
	case gameprofile.FormatLines:
		for _, l := range strings.Split(content, "\n") {
			if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "//") {
				continue
			}
			out = append(out, Player{ID: l})
		}
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
	return out, nil
}

// editLines adds an entry to a lines file, or takes it off, keeping its
// comments and everything else as it was.
func (s *Service) editLines(ctx context.Context, info docker.ContainerJSON, f gameprofile.ListFile, entry string, remove bool) error {
	p, err := s.findFile(ctx, info, f)
	if err != nil {
		return err
	}
	file, err := s.opts.Servers.ReadFile(ctx, info.ID, p)
	if err != nil {
		return err
	}
	content, changed := editLines(file.Content, entry, remove)
	if !changed {
		if remove {
			return fmt.Errorf("%s isn't on the list", entry)
		}
		return fmt.Errorf("%s is already on the list", entry)
	}
	_, err = s.opts.Servers.WriteFile(ctx, info.ID, p, content, file.Mtime)
	return err
}

// editLines changes a lines file's content: the entry's lines removed, or
// it added at the end.
func editLines(content, entry string, remove bool) (string, bool) {
	crlf := strings.Contains(content, "\r\n")
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	found := false
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == entry {
			found = true
			if remove {
				continue
			}
		}
		out = append(out, l)
	}
	if remove != found {
		return content, false
	}
	if !remove {
		// After the last line, keeping a final newline.
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		out = append(out, entry, "")
	}
	res := strings.Join(out, "\n")
	if crlf {
		res = strings.ReplaceAll(res, "\n", "\r\n")
	}
	return res, true
}
