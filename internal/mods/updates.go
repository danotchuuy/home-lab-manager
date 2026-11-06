package mods

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// CheckUpdates looks for newer versions of a server's mods. A newer
// version that needs another game version is shown as blocked.
func (s *Service) CheckUpdates(ctx context.Context, server string) (Overview, error) {
	st, err := s.load(ctx, server, true)
	if err != nil {
		return Overview{}, err
	}
	if st.ov.FitError != "" {
		return Overview{}, errors.New(st.ov.FitError)
	}
	var hashes []string
	for _, m := range st.ov.Mods {
		if m.Project != "" && m.sha512 != "" {
			hashes = append(hashes, m.sha512)
		}
	}
	fitting, err := s.opts.Modrinth.Latest(ctx, hashes, st.ov.Fit)
	if err != nil {
		return Overview{}, err
	}
	anyGame, err := s.opts.Modrinth.Latest(ctx, hashes, Fit{Loaders: st.ov.Fit.Loaders})
	if err != nil {
		return Overview{}, err
	}
	ups := map[string]Update{}
	var fresh []string
	s.mu.Lock()
	old := s.updates[server]
	for _, m := range st.ov.Mods {
		if m.Project == "" || m.sha512 == "" {
			continue
		}
		var u *Update
		v, ok := fitting[m.sha512]
		if ok && v.VersionType != "" && v.VersionType != "release" {
			// The newest is a beta: offer the newest release, as adding
			// a mod would.
			if vs, err := s.opts.Modrinth.Versions(ctx, m.Project, st.ov.Fit); err == nil {
				v, ok = Best(slices.DeleteFunc(vs, func(v Version) bool { return !fits(v, st.ov.Fit) }))
			}
		}
		if ok && v.ID != m.Version && v.ProjectID == m.Project {
			u = &Update{Version: v.ID, VersionNumber: v.VersionNumber}
		} else if v, ok := anyGame[m.sha512]; ok && v.ID != m.Version && v.ProjectID == m.Project && !slices.Contains(v.GameVersions, st.ov.Fit.GameVersion) {
			u = &Update{Version: v.ID, VersionNumber: v.VersionNumber,
				Blocked: fmt.Sprintf("needs %s %s", st.profile.Name, newestGame(v.GameVersions))}
		}
		if u == nil {
			continue
		}
		ups[m.File] = *u
		if prev, ok := old[m.File]; u.Blocked == "" && (!ok || prev.Version != u.Version) {
			fresh = append(fresh, fmt.Sprintf("%s %s", cmpOr(m.Title, m.File), u.VersionNumber))
		}
	}
	s.updates[server] = ups
	s.checked[server] = s.opts.Now()
	s.mu.Unlock()
	if len(fresh) > 0 && s.opts.Notifier != nil {
		noun := st.kind()
		if len(fresh) == 1 {
			noun = strings.TrimSuffix(noun, "s")
		}
		s.opts.Notifier.Notify(notify.Event{Kind: notify.KindModUpdates, Severity: notify.Info,
			Title:   fmt.Sprintf("%s has %d %s to update", server, len(fresh), noun),
			Message: "Newer versions: " + strings.Join(fresh, ", ") + ".",
			Server:  server, Link: "/servers/" + server + "?tab=mods",
			Key: server + "|" + strings.Join(fresh, ",")})
	}
	s.describe(ctx, st)
	return st.ov, nil
}

// newestGame is the last of a version's game versions.
func newestGame(vs []string) string {
	if len(vs) == 0 {
		return "a different version"
	}
	return vs[len(vs)-1]
}

// Lister lists servers, for the daily check.
type Lister interface {
	List(ctx context.Context) ([]manager.Server, error)
}

// Run checks every server's mods for updates once a day until ctx ends,
// starting after first.
func (s *Service) Run(ctx context.Context, servers Lister, first time.Duration) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := servers.List(ctx)
		if err == nil {
			for _, srv := range list {
				if ctx.Err() != nil {
					return
				}
				s.checkQuietly(ctx, srv.Name)
			}
		}
		t.Reset(24 * time.Hour)
	}
}

// checkQuietly checks one server, skipping those without mods.
func (s *Service) checkQuietly(ctx context.Context, server string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	st, err := s.load(ctx, server, false)
	if err != nil || len(st.ov.Mods) == 0 || st.ov.FitError != "" {
		return
	}
	if _, err := s.CheckUpdates(ctx, server); err != nil {
		s.opts.Log.Warn("mod update check failed", "server", server, "err", err)
	}
}

// ForgetServer drops what's cached about a deleted server.
func (s *Service) ForgetServer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.updates, name)
	delete(s.checked, name)
	for k := range s.hashes {
		if strings.HasPrefix(k, name+"|") {
			delete(s.hashes, k)
		}
	}
}
