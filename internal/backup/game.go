package backup

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
	"github.com/danotchuuy/home-lab-manager/internal/s3"
)

// A game's own backups. Some games save copies of their world into a
// backup folder now and then (Palworld every half hour, say). hlm uploads
// each one it hasn't uploaded yet, instead of (or as well as) making its
// own archives: they're whole, consistent saves, taken while nothing was
// half written, and only new ones need uploading.
//
// Each entry of a game backup folder, a file or a folder, is one backup.
// It's stored as a backup of just that path, under
//
//	<prefix>/<server>/game/<folder>/<name>@<mtime>.tar.gz
//
// so it's uploaded again only when it's new or the game has written it
// again (games that keep a few backups under the same names, rotating
// them), and restoring it puts it back where the game looks for it.

// GameFolder is a folder a game keeps its backups in: a container path, in
// which * stands for any one folder's name (a world's ID, say). Match, if
// set, takes only the entries whose names match it (path.Match syntax), for
// games that keep their backups beside the live save.
type GameFolder struct {
	Path  string `json:"path"`
	Match string `json:"match,omitempty"`
}

// GameSync is how an upload of a game's backups went.
type GameSync struct {
	Trigger  string    `json:"trigger"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Found is how many backups the folders held, Uploaded how many of
	// them were new (Bytes their size, compressed, as stored).
	Found    int      `json:"found"`
	Uploaded int      `json:"uploaded"`
	Bytes    int64    `json:"bytes"`
	Deleted  int      `json:"deleted,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// gameEvery is how often a running server's game backup folders are
// checked for new backups.
var gameEvery = 10 * time.Minute

// gameSettle is how long a backup is left after it last changed before
// it's uploaded: the game may still be writing it.
var gameSettle = 2 * time.Minute

// gameSeries is where a server's game backups go: <prefix>/<server>/game/.
func gameSeries(c Settings, name string) gameKeys {
	return gameKeys{dir: c.prefix() + "/" + name + "/game/"}
}

type gameKeys struct{ dir string }

var gameKeyEnd = regexp.MustCompile(`@\d+\.tar\.gz$`)

// key names the backup of entry name (last changed at mtime) in folder.
func (g gameKeys) key(folder, name string, mtime time.Time) string {
	return g.dir + strings.TrimPrefix(path.Clean(folder), "/") + "/" + name + "@" + strconv.FormatInt(mtime.Unix(), 10) + ".tar.gz"
}

// has reports whether key is one of the series' game backups.
func (g gameKeys) has(key string) bool {
	rest, ok := strings.CutPrefix(key, g.dir)
	return ok && gameKeyEnd.MatchString(rest) && !strings.Contains(rest, "..") && !strings.HasPrefix(rest, "/")
}

// source is the container path a game backup's key was taken from.
func (g gameKeys) source(key string) string {
	rest := strings.TrimPrefix(key, g.dir)
	return "/" + rest[:strings.LastIndex(rest, "@")]
}

// changed is when a game backup's source last changed, from its key.
func gameKeyTime(key string) int64 {
	at := key[strings.LastIndex(key, "@")+1 : len(key)-len(".tar.gz")]
	n, _ := strconv.ParseInt(at, 10, 64)
	return n
}

// checkGameFolders cleans a plan's game backup folders.
func checkGameFolders(in []GameFolder) ([]GameFolder, error) {
	var out []GameFolder
	seen := map[string]bool{}
	for _, f := range in {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			continue
		}
		if !path.IsAbs(p) {
			return nil, fmt.Errorf("game backup folder %q: give the full path in the container, starting with /", p)
		}
		if slices.Contains(strings.Split(p, "/"), "..") {
			return nil, fmt.Errorf("game backup folder %q: give the path without ..", p)
		}
		p = path.Clean(p)
		if p == "/" {
			return nil, errors.New("a game backup folder can't be / itself")
		}
		for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
			if seg == ".." || (strings.Contains(seg, "*") && seg != "*") {
				return nil, fmt.Errorf("game backup folder %q: * stands for a whole folder name, as in /saves/*/backup", p)
			}
		}
		m := strings.TrimSpace(f.Match)
		if m != "" {
			if _, err := path.Match(m, ""); err != nil || strings.Contains(m, "/") {
				return nil, fmt.Errorf("game backup folder %q: %q isn't a name pattern like *_backup_*", p, m)
			}
		}
		if k := p + "\x00" + m; !seen[k] {
			seen[k] = true
			out = append(out, GameFolder{Path: p, Match: m})
		}
	}
	return out, nil
}

// expand finds the folders a game backup folder's * stand for. cut is set
// if a listing was cut short.
func (s *Service) expand(ctx context.Context, name, p string, cut *bool) ([]string, error) {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	dirs := []string{"/"}
	for i, seg := range segs {
		var next []string
		for _, d := range dirs {
			if seg != "*" {
				next = append(next, path.Join(d, seg))
				continue
			}
			entries, truncated, err := s.servers.ListFolder(ctx, name, d)
			if err != nil {
				if errors.Is(err, files.ErrNotFound) {
					continue
				}
				return nil, err
			}
			*cut = *cut || truncated
			for _, e := range entries {
				if e.Type == "dir" {
					next = append(next, path.Join(d, e.Name))
				}
			}
		}
		dirs = next
		if len(dirs) == 0 && i < len(segs)-1 {
			return nil, nil
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// gameEntry is one backup a game wrote.
type gameEntry struct {
	folder, path string
	mtime        time.Time
}

// gameEntries lists the backups in a plan's game backup folders. cut is
// set if a listing was cut short, so some may be missing.
func (s *Service) gameEntries(ctx context.Context, name string, folders []GameFolder, cut *bool, warn func(string)) ([]gameEntry, error) {
	var out []gameEntry
	for _, f := range folders {
		dirs, err := s.expand(ctx, name, f.Path, cut)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if len(dirs) == 0 {
			warn(f.Path + " isn't there (yet)")
		}
		for _, d := range dirs {
			entries, truncated, err := s.servers.ListFolder(ctx, name, d)
			if errors.Is(err, files.ErrNotFound) {
				warn(d + " isn't there (yet)")
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d, err)
			}
			if truncated {
				*cut = true
				warn(d + " has more backups than could be listed through Docker while the server is stopped; the rest are uploaded once it runs (or once the host's agent is updated)")
			}
			for _, e := range entries {
				if e.Type != "dir" && e.Type != "file" {
					continue
				}
				if f.Match != "" {
					if ok, _ := path.Match(f.Match, e.Name); !ok {
						continue
					}
				}
				out = append(out, gameEntry{folder: d, path: path.Join(d, e.Name), mtime: e.Mtime})
			}
		}
	}
	return out, nil
}

// SyncGame uploads a server's new game backups, in the background.
func (s *Service) SyncGame(ctx context.Context, id string) error {
	_, name, err := s.serverName(ctx, id)
	if err != nil {
		return err
	}
	return s.startGame(name, "manual")
}

func (s *Service) startGame(name, trigger string) error {
	if _, _, err := s.client(); err != nil {
		return err
	}
	plans, err := s.plans()
	if err != nil {
		return err
	}
	plan := plans[name]
	if len(plan.GameFolders) == 0 {
		return errors.New("choose the folders the game keeps its backups in first")
	}
	prog, err := s.claim(name, "game", trigger)
	if err != nil {
		return err
	}
	go func() {
		sync := s.syncGame(context.Background(), name, plan, prog)
		s.release(name)
		if sync.Error != "" {
			s.log.Warn("upload of the game's backups failed", "server", name, "err", sync.Error)
		} else if sync.Uploaded > 0 {
			s.log.Info("uploaded the game's backups", "server", name, "uploaded", sync.Uploaded)
		}
		var before *GameSync
		if err := s.updatePlan(name, func(p *Plan) { before, p.LastGame = p.LastGame, &sync }); err != nil {
			s.log.Warn("record the game's backups' upload", "server", name, "err", err)
		}
		s.notifyGame(name, sync, before)
	}()
	return nil
}

// syncGame uploads the game backups that aren't in the bucket yet, then
// deletes old ones past GameKeep that the game no longer has.
func (s *Service) syncGame(ctx context.Context, name string, plan Plan, prog *Progress) (sync GameSync) {
	sync = GameSync{Trigger: prog.Trigger, Started: prog.Started}
	fail := func(err error) GameSync {
		sync.Finished, sync.Error = time.Now(), err.Error()
		return sync
	}
	st, err := s.store("")
	if err != nil {
		return fail(err)
	}
	keys := gameSeries(st.t.Settings, s.seriesName(name))
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	stored, err := st.cl.List(lctx, keys.dir)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("list the bucket: %w", err))
	}
	have := map[string]bool{}
	for _, o := range stored {
		have[o.Key] = true
	}
	cut := false
	entries, err := s.gameEntries(ctx, name, plan.GameFolders, &cut, func(w string) { sync.Warnings = append(sync.Warnings, w) })
	if err != nil {
		return fail(err)
	}
	sync.Found = len(entries)
	present := map[string]bool{}
	now := time.Now()
	s.setStage(prog, "uploading")
	for _, e := range entries {
		key := keys.key(e.folder, path.Base(e.path), e.mtime)
		present[key] = true
		if have[key] || now.Sub(e.mtime) < gameSettle {
			continue
		}
		var run Run
		if err := s.uploadAs(ctx, st, name, key, []string{e.path}, nil, prog, &run); err != nil {
			if errors.Is(err, files.ErrNotFound) {
				continue // the game deleted it meanwhile
			}
			return fail(fmt.Errorf("%s: %w", e.path, err))
		}
		sync.Uploaded++
		sync.Bytes += run.Size
		sync.Warnings = append(sync.Warnings, run.Warnings...)
		have[key] = true
		stored = append(stored, s3.Object{Key: key, Size: run.Size, LastModified: time.Now()})
	}
	// A listing cut short doesn't show every backup the game still has:
	// cleaning up from it could delete those, only to upload them again.
	if plan.GameKeep > 0 && cut {
		sync.Warnings = append(sync.Warnings, "old game backups weren't cleaned up, as not every folder could be listed in full")
	} else if plan.GameKeep > 0 {
		s.setStage(prog, "cleaning")
		n, err := pruneGame(ctx, st, keys, stored, present, plan.GameKeep)
		sync.Deleted = n
		if err != nil {
			sync.Warnings = append(sync.Warnings, "couldn't delete old game backups: "+err.Error())
		}
	}
	if len(sync.Warnings) > maxGameWarnings {
		sync.Warnings = append(sync.Warnings[:maxGameWarnings], "and more")
	}
	sync.Finished = time.Now()
	return sync
}

const maxGameWarnings = 20

// pruneGame keeps the newest keep game backups of each folder, and every
// one the game still has (deleting those would only upload them again).
func pruneGame(ctx context.Context, st *store, keys gameKeys, stored []s3.Object, present map[string]bool, keep int) (int, error) {
	byFolder := map[string][]string{}
	for _, o := range stored {
		if keys.has(o.Key) {
			f := path.Dir(keys.source(o.Key))
			byFolder[f] = append(byFolder[f], o.Key)
		}
	}
	deleted := 0
	for _, list := range byFolder {
		sort.Slice(list, func(i, j int) bool { return gameKeyTime(list[i]) > gameKeyTime(list[j]) })
		for i, k := range list {
			if i < keep || present[k] {
				continue
			}
			if err := st.remove(ctx, k); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
	return deleted, nil
}

// gameDue reports whether a server's game backups should be checked now:
// every gameEvery while it runs, and once after it stops (for the backups
// it wrote last).
func gameDue(p Plan, running bool, stopped time.Time, now time.Time) bool {
	if len(p.GameFolders) == 0 {
		return false
	}
	if p.LastGame == nil {
		return true
	}
	if running {
		return now.Sub(p.LastGame.Started) >= gameEvery
	}
	// Since it stopped, after it had time to settle.
	return !stopped.IsZero() && p.LastGame.Started.Before(stopped.Add(gameSettle)) && now.Sub(stopped) >= gameSettle
}

// notifyGame reports an upload of a game's backups that failed, once until
// one works again.
func (s *Service) notifyGame(server string, sync GameSync, before *GameSync) {
	if s.notifier == nil {
		return
	}
	key := notify.DedupeKey(notify.KindBackupFailed, server+"#game")
	if sync.Error == "" {
		s.notifier.Clear(key)
		if sync.Uploaded > 0 {
			s.notifier.Clear(notify.DedupeKey(notify.KindBackupStale, server))
		}
		return
	}
	if before != nil && before.Error != "" {
		return // already said
	}
	s.notifier.Notify(notify.Event{
		Kind: notify.KindBackupFailed, Title: "Uploading " + server + "'s game backups failed", Message: sync.Error,
		Server: server, Link: "/servers/" + server + "?tab=backups", Key: key,
	})
}

// gameBackups lists a server's uploaded game backups, newest first.
func gameBackups(ctx context.Context, st *store, keys gameKeys) ([]s3.Object, error) {
	objs, err := st.cl.List(ctx, keys.dir)
	if err != nil {
		return nil, err
	}
	out := make([]s3.Object, 0, len(objs))
	for _, o := range objs {
		if keys.has(o.Key) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return gameKeyTime(out[i].Key) > gameKeyTime(out[j].Key) })
	return out, nil
}
