package mods

import (
	"archive/tar"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// Plan works out what changes to a server's mods would do.
func (s *Service) Plan(ctx context.Context, server string, changes []Change) (*Plan, error) {
	st, err := s.load(ctx, server, true)
	if err != nil {
		return nil, err
	}
	p, err := s.plan(ctx, st, changes)
	if err != nil {
		return nil, err
	}
	p.Warnings = append(st.ov.Warnings, p.Warnings...)
	return p, nil
}

// Result is how an applied change went.
type Result struct {
	Plan *Plan `json:"plan"`
	// RolledBack says the server didn't come back, so the mods were put
	// back as they were.
	RolledBack bool `json:"rolledBack,omitempty"`
	// Warning is something to know about a change that worked.
	Warning string `json:"warning,omitempty"`
}

// Apply makes changes to a server's mods as one transaction: snapshot the
// folder, download, stop the server, change the files, start it and wait
// for it to answer its status query. If it doesn't, the folder goes back
// to the snapshot and the server is started again. A stopped server is
// changed and left stopped. who is recorded beside the mods.
func (s *Service) Apply(ctx context.Context, server, who string, changes []Change, emit func(manager.Event)) (res Result, err error) {
	st, err := s.load(ctx, server, true)
	if err != nil {
		return res, err
	}
	if why := s.opts.Busy(server); why != "" {
		return res, errors.New(why)
	}
	plan, err := s.plan(ctx, st, changes)
	if err != nil {
		return res, err
	}
	res.Plan = plan
	emit, finish, err := s.opts.Servers.StartJob(manager.Job{Kind: "mods", Server: server, Image: st.info.Config.Image}, emit)
	if err != nil {
		return res, err
	}
	defer func() { finish(err) }()
	say := func(stage, format string, a ...any) {
		emit(manager.Event{Stage: stage, Message: fmt.Sprintf(format, a...)})
	}

	work, err := os.MkdirTemp(s.opts.Dir, "mods-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(work)

	if !st.exists {
		say("folder", "Making the %s folder, %s", st.kind(), st.loader.Dir)
		if err := s.opts.Servers.MakeDir(ctx, server, st.loader.Dir); err != nil {
			return res, err
		}
		st.exists = true
	}
	say("snapshot", "Saving a copy of %s", st.loader.Dir)
	snap, err := s.snapshot(ctx, st, work)
	if err != nil {
		return res, fmt.Errorf("save a copy of the %s first: %w", st.kind(), err)
	}

	downloads := map[string]string{} // file → downloaded copy
	for _, step := range plan.Steps {
		if step.New == nil {
			continue
		}
		say("download", "Downloading %s %s", cmpOr(step.Title, step.New.File), step.New.VersionNumber)
		p, err := s.opts.Modrinth.Download(ctx, step.New.vfile, work)
		if err != nil {
			return res, err
		}
		downloads[step.New.File] = p
	}

	running := st.info.State.Running
	if running {
		say("stop", "Stopping %s", server)
		if err := s.opts.Servers.Stop(ctx, server); err != nil {
			return res, fmt.Errorf("stop the server: %w", err)
		}
	}
	say("apply", "Changing the %s", st.kind())
	cause := s.write(ctx, st, plan, snap, downloads, who)
	if cause == nil && running {
		say("start", "Starting %s", server)
		if cause = s.opts.Servers.Start(ctx, server); cause == nil {
			say("wait", "Waiting for %s to answer (up to %s)", server, st.ov.Timeout)
			res.Warning, cause = s.waitUp(ctx, server, st.ov.Timeout)
		}
	}
	if cause == nil {
		if !running {
			res.Warning = fmt.Sprintf("The server is stopped, so the %s load when it next starts; hlm couldn't check them.", st.kind())
		}
		return res, nil
	}

	// Put everything back.
	s.opts.Log.Warn("mod change failed; putting the mods back", "server", server, "err", cause)
	say("rollback", "That didn't work (%v); putting the %s back", cause, st.kind())
	res.RolledBack = true
	err = fmt.Errorf("the server didn't come back after the change (%v), so hlm put the %s back as they were", cause, st.kind())
	if back := s.rollback(ctx, st, plan, snap, running); back != nil {
		err = fmt.Errorf("the server didn't come back after the change (%v), and putting the %s back failed: %v", cause, st.kind(), back)
	} else if running {
		say("wait", "Waiting for %s to answer again", server)
		if _, again := s.waitUp(ctx, server, st.ov.Timeout); again != nil {
			err = fmt.Errorf("the server didn't come back after the change (%v). hlm put the %s back as they were, but it doesn't start with them either (%v), so the problem may be something else",
				cause, st.kind(), again)
		}
	}
	if s.opts.Notifier != nil {
		s.opts.Notifier.Notify(notify.Event{Kind: notify.KindModsFailed, Severity: notify.Error,
			Title:   "A mod change on " + server + " was undone",
			Message: err.Error(), Server: server, Link: "/servers/" + server + "?tab=mods",
			Fields: map[string]string{"Change": Summary(plan)}})
	}
	return res, err
}

// Summary describes a plan in a line.
func Summary(p *Plan) string {
	var out []string
	for _, st := range p.Steps {
		name := cmpOr(st.Title, st.File)
		switch st.Op {
		case OpAdd:
			out = append(out, fmt.Sprintf("add %s %s", name, st.New.VersionNumber))
		case OpReplace:
			out = append(out, fmt.Sprintf("update %s to %s", name, st.New.VersionNumber))
		default:
			out = append(out, st.Op+" "+name)
		}
	}
	return strings.Join(out, ", ")
}

// snapshot is a copy of the mods folder, as a tar file, and the folder's
// owner.
type snapshot struct {
	path     string
	uid, gid int
	// files are the files in it, by container path.
	files map[string]bool
}

func (s *Service) snapshot(ctx context.Context, st *state, dir string) (*snapshot, error) {
	snap := &snapshot{files: map[string]bool{}}
	f, err := os.CreateTemp(dir, "snapshot-*.tar")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	stats, err := s.opts.Servers.ArchiveFolders(ctx, st.name, []string{st.loader.Dir}, nil, tw, nil)
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return nil, err
	}
	if len(stats.Warnings) > 0 {
		return nil, fmt.Errorf("some files couldn't be read: %s", strings.Join(stats.Warnings, "; "))
	}
	snap.path = f.Name()
	// The folder's owner, and what's in it.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		full := "/" + strings.TrimSuffix(hdr.Name, "/")
		if full == st.loader.Dir {
			snap.uid, snap.gid = hdr.Uid, hdr.Gid
		} else if hdr.Typeflag == tar.TypeReg {
			snap.files[full] = true
		}
	}
	return snap, nil
}

// write changes the folder as the plan says.
func (s *Service) write(ctx context.Context, st *state, plan *Plan, snap *snapshot, downloads map[string]string, who string) error {
	dir := st.loader.Dir
	rec := Record{Mods: map[string]RecordEntry{}}
	for k, v := range st.record.Mods {
		rec.Mods[k] = v
	}
	var remove []string
	type rename struct{ from, to string }
	var renames []rename
	for _, step := range plan.Steps {
		base := strings.TrimSuffix(step.File, disabledSuffix)
		switch step.Op {
		case OpAdd, OpReplace:
			if step.Op == OpReplace {
				delete(rec.Mods, base)
				if step.File != step.New.File {
					remove = append(remove, path.Join(dir, step.File))
				}
			}
			rec.Mods[step.New.File] = RecordEntry{Project: step.New.Project, Version: step.New.Version,
				VersionNumber: step.New.VersionNumber, SHA512: step.New.SHA512, Size: step.New.Size,
				Installed: s.opts.Now().UTC(), By: who}
		case OpRemove:
			delete(rec.Mods, base)
			remove = append(remove, path.Join(dir, step.File))
		case OpDisable:
			renames = append(renames, rename{step.File, step.File + disabledSuffix})
		case OpEnable:
			renames = append(renames, rename{step.File, base})
		}
	}
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	built := make(chan error, 1)
	go func() {
		err := func() error {
			tw := tar.NewWriter(pw)
			now := time.Now()
			hdr := func(name string, size int64) *tar.Header {
				return &tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(path.Join(dir, name), "/"),
					Mode: 0o644, Uid: snap.uid, Gid: snap.gid, ModTime: now, Size: size}
			}
			for _, step := range plan.Steps {
				if step.New == nil {
					continue
				}
				if err := copyFile(tw, hdr(step.New.File, step.New.Size), downloads[step.New.File]); err != nil {
					return err
				}
			}
			for _, r := range renames {
				if err := copyFromSnapshot(tw, snap.path, path.Join(dir, r.from), hdr(r.to, 0)); err != nil {
					return err
				}
			}
			if err := tw.WriteHeader(hdr(RecordFile, int64(len(recJSON)))); err != nil {
				return err
			}
			if _, err := tw.Write(recJSON); err != nil {
				return err
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
		built <- err
	}()
	stats, err := s.opts.Servers.RestoreArchive(ctx, st.name, pr, nil)
	pr.CloseWithError(errors.New("stopped"))
	if berr := <-built; berr != nil && err == nil {
		err = berr
	}
	if err == nil && len(stats.Warnings) > 0 {
		err = fmt.Errorf("couldn't write: %s", strings.Join(stats.Warnings, "; "))
	}
	if err != nil {
		return fmt.Errorf("write the %s: %w", st.kind(), err)
	}
	for _, r := range renames {
		remove = append(remove, path.Join(dir, r.from))
	}
	if len(remove) > 0 {
		if err := s.opts.Servers.RemoveFiles(ctx, st.name, remove); err != nil {
			return err
		}
	}
	return nil
}

// copyFile adds a file to an archive.
func copyFile(tw *tar.Writer, hdr *tar.Header, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, hdr.Size)
	return err
}

// copyFromSnapshot adds a file from the snapshot to an archive under hdr's
// name, keeping its owner and mode.
func copyFromSnapshot(tw *tar.Writer, snapPath, full string, hdr *tar.Header) error {
	f, err := os.Open(snapPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s isn't in the copy of the folder", full)
		}
		if err != nil {
			return err
		}
		if "/"+h.Name != full || h.Typeflag != tar.TypeReg {
			continue
		}
		hdr.Size, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = h.Size, h.Mode, h.Uid, h.Gid, h.Uname, h.Gname
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.CopyN(tw, tr, h.Size)
		return err
	}
}

// rollback puts the folder back as the snapshot has it, and starts the
// server again if it was running.
func (s *Service) rollback(ctx context.Context, st *state, plan *Plan, snap *snapshot, running bool) error {
	// It may be running (it came up, then crashed), or not.
	ctx = context.WithoutCancel(ctx)
	if info, err := s.opts.Servers.Inspect(ctx, st.name); err == nil && info.State.Running {
		if err := s.opts.Servers.Stop(ctx, st.name); err != nil {
			return fmt.Errorf("stop the server: %w", err)
		}
	}
	// Files the change added go; the snapshot brings back the rest.
	var added []string
	dir := st.loader.Dir
	for _, step := range plan.Steps {
		var p string
		switch step.Op {
		case OpAdd, OpReplace:
			p = path.Join(dir, step.New.File)
		case OpDisable:
			p = path.Join(dir, step.File+disabledSuffix)
		case OpEnable:
			p = path.Join(dir, strings.TrimSuffix(step.File, disabledSuffix))
		}
		if p != "" && !snap.files[p] {
			added = append(added, p)
		}
	}
	if rec := path.Join(dir, RecordFile); !snap.files[rec] {
		added = append(added, rec)
	}
	if err := s.opts.Servers.RemoveFiles(ctx, st.name, added); err != nil {
		return err
	}
	f, err := os.Open(snap.path)
	if err != nil {
		return err
	}
	stats, err := s.opts.Servers.RestoreArchive(ctx, st.name, f, nil)
	f.Close()
	if err == nil && len(stats.Warnings) > 0 {
		err = errors.New(strings.Join(stats.Warnings, "; "))
	}
	if err != nil {
		return fmt.Errorf("put the copy back: %w", err)
	}
	if running {
		return s.opts.Servers.Start(ctx, st.name)
	}
	return nil
}

// waitUp waits for a started server to answer its status query. It fails
// as soon as the server stops or Docker restarts it. A server that can't
// be queried counts as up once it has kept running for a while; warning
// says so.
func (s *Service) waitUp(ctx context.Context, server string, timeout time.Duration) (warning string, err error) {
	begin := time.Now()
	first, err := s.opts.Servers.Inspect(ctx, server)
	if err != nil {
		return "", err
	}
	for {
		info, err := s.opts.Servers.Inspect(ctx, server)
		if err != nil {
			return "", err
		}
		switch {
		case !info.State.Running && !info.State.Restarting:
			return "", fmt.Errorf("it stopped (exit code %d)", info.State.ExitCode)
		case info.State.Restarting || info.RestartCount > first.RestartCount || info.State.StartedAt != first.State.StartedAt:
			return "", errors.New("it crashed, and Docker restarted it")
		}
		up, known := s.opts.Ready(ctx, server)
		if known && up {
			// One answer isn't enough: a server can answer (or a published
			// port can accept a connection) just before it crashes. It has
			// to still be the same running container a little later.
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(s.opts.Settle):
			}
			after, err := s.opts.Servers.Inspect(ctx, server)
			if err != nil {
				return "", err
			}
			switch {
			case !after.State.Running && !after.State.Restarting:
				return "", fmt.Errorf("it answered, then stopped (exit code %d)", after.State.ExitCode)
			case after.State.Restarting || after.RestartCount > info.RestartCount || after.State.StartedAt != info.State.StartedAt:
				return "", errors.New("it answered, then crashed, and Docker restarted it")
			}
			return "", nil
		}
		if !known && time.Since(begin) >= s.opts.Unqueryable {
			return fmt.Sprintf("hlm can't ask this server whether it's up (its status query is off), so it only checked that it kept running for %s.",
				s.opts.Unqueryable), nil
		}
		if time.Since(begin) >= timeout {
			return "", fmt.Errorf("it didn't answer its status query within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.opts.Poll):
		}
	}
}

// hashFiles works out the SHA-512 of mods in the folder, through a cache
// keyed by name, size and time.
func (s *Service) hashFiles(ctx context.Context, st *state, idx []int) error {
	key := func(m Installed) string {
		return fmt.Sprintf("%s|%s|%s|%d|%d", st.name, st.loader.Dir, m.File, m.Size, m.Modified.UnixNano())
	}
	want := map[string]int{}
	s.mu.Lock()
	for _, i := range idx {
		m := &st.ov.Mods[i]
		if h, ok := s.hashes[key(*m)]; ok {
			m.sha512 = h
		} else {
			want[path.Join(st.loader.Dir, m.File)] = i
		}
	}
	s.mu.Unlock()
	if len(want) == 0 {
		return nil
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		_, err := s.opts.Servers.ArchiveFolders(ctx, st.name, []string{st.loader.Dir}, nil, tw, nil)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	tr := tar.NewReader(pr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if errors.Is(err, files.ErrNotFound) {
				return nil
			}
			return err
		}
		i, ok := want["/"+hdr.Name]
		if !ok || hdr.Typeflag != tar.TypeReg {
			continue
		}
		h := sha512.New()
		if _, err := io.Copy(h, tr); err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		s.mu.Lock()
		st.ov.Mods[i].sha512 = sum
		s.hashes[key(st.ov.Mods[i])] = sum
		s.mu.Unlock()
	}
}
