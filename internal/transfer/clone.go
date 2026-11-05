package transfer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/backup"
	"github.com/danotchuuy/home-lab-manager/internal/catalog"
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// Ways a clone gets its data.
const (
	CopyNone   = "none"   // settings only: the clone starts empty
	CopyNow    = "now"    // the original's data folders as they are
	CopyBackup = "backup" // one of the original's backups
)

// CloneOptions say how to clone a server.
type CloneOptions struct {
	Name string `json:"name"`
	// Host is the Docker host the clone goes on; the original's if empty.
	Host string `json:"host,omitempty"`
	// Request is the clone's settings, from the clone form (starting from
	// PreviewClone's); nil uses PreviewClone's.
	Request  *deploy.Request `json:"request,omitempty"`
	CopyData string          `json:"copyData"`
	// BackupKey is the backup to copy with CopyBackup.
	BackupKey string `json:"backupKey,omitempty"`
	// StopSource stops a running original while its data is copied, so
	// its saves aren't copied half-written, and starts it again after.
	StopSource bool `json:"stopSource"`
	// Start starts the clone once it's ready.
	Start bool `json:"start"`
}

// PreviewClone works out a clone's settings: the original's, under a new
// name, with its data folders moved to the new name's, its host ports moved
// to free ones and any fixed address cleared.
func (s *Service) PreviewClone(ctx context.Context, id, name string) (Preview, error) {
	src, err := s.opts.Manager.Inspect(ctx, id)
	if err != nil {
		return Preview{}, err
	}
	// The clone may go on another host (its name ends in @host).
	_, old := manager.SplitID(strings.TrimPrefix(src.Name, "/"))
	host, bare := manager.SplitID(name)
	if err := s.checkNewName(ctx, name); err != nil {
		return Preview{}, err
	}
	e, err := s.Export(ctx, id, true)
	if err != nil {
		return Preview{}, err
	}
	app, _, err := s.template(e, host)
	if err != nil {
		return Preview{}, err
	}
	root := strings.TrimRight(s.opts.Manager.AppdataRootOf(host), "/")
	req := localPaths(e.Request, root)
	req.Name = name
	p := Preview{App: app, Warnings: []string{}, Settings: e.Settings.Summary()}
	if p.Settings == nil {
		p.Settings = []string{}
	}
	if req.IPAddress != "" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("The original's fixed address (%s) was cleared: Docker picks one, or enter a free one.", req.IPAddress))
		req.IPAddress = ""
	}
	items, err := deploy.Items(app, req)
	if err != nil {
		return Preview{}, err
	}
	req.Values = maps.Clone(req.Values)
	if req.Values == nil {
		req.Values = map[string]string{}
	}
	req.Extra = slices.Clone(req.Extra)
	// Data folders: the clone gets its own.
	for _, it := range items {
		if it.Type != catalog.TypePath || it.Disabled || it.Effective == "" {
			continue
		}
		moved, ok := clonePath(it.Effective, root, old, bare)
		if !ok && host != manager.HostOf(src.ID) {
			continue // the same path, on the other host
		}
		if !ok {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s (%s) is outside the appdata folder and isn't named after the server, so the copy shares it with the original. Change it if they shouldn't share it.",
				it.Name, it.Effective))
			continue
		}
		setValue(&req, it.ID, moved)
	}
	// Host ports: moved together, so ports that go together (a game port
	// and the query port after it) stay together.
	if err := s.shiftPorts(ctx, app, &req, &p); err != nil {
		return Preview{}, err
	}
	p.Request = req
	p.Copies, err = s.copies(ctx, src, app, req)
	if err != nil {
		return Preview{}, err
	}
	for _, c := range p.Copies {
		if c.From == "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is a Docker volume of the original's that isn't named after it, so it can't be copied: the copy gets its own, empty.", c.Target))
		}
	}
	p.Copies = slices.DeleteFunc(p.Copies, func(c Copy) bool { return c.From == "" })
	return p, nil
}

func (s *Service) checkNewName(ctx context.Context, name string) error {
	if !validServer(name) {
		return errors.New("a server name is letters, digits, dots, dashes and underscores, starting with a letter or digit")
	}
	if !s.opts.Manager.KnownHost(manager.HostOf(name)) {
		return fmt.Errorf("there is no Docker host %s", manager.HostOf(name))
	}
	if _, err := s.opts.Manager.Inspect(ctx, name); err == nil || !errors.Is(err, manager.ErrNotFound) {
		if err == nil || errors.Is(err, manager.ErrNotManaged) {
			return fmt.Errorf("a container named %s already exists", name)
		}
		return err
	}
	return nil
}

// CloneName is a clone's full name: on host, or on the original's host.
func CloneName(srcID, host, name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	if host == "" {
		host = manager.HostOf(srcID)
	}
	return manager.JoinID(host, name)
}

// clonePath moves a data folder to the clone's: under the appdata root, the
// folder named after the server (or holding its name) is renamed, or else
// the path goes into a folder of the new server's; elsewhere, a folder
// named after the server is renamed. ok is false when the path stays.
func clonePath(p, root, old, name string) (string, bool) {
	if rel, ok := under(p, root); ok && root != "" {
		parts := strings.Split(strings.TrimPrefix(rel, "/"), "/")
		if parts[0] == "" {
			return root + "/" + name, true
		}
		if strings.Contains(parts[0], old) {
			parts[0] = strings.Replace(parts[0], old, name, 1)
		} else {
			parts = append([]string{name}, parts...)
		}
		return root + "/" + strings.Join(parts, "/"), true
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == old {
			parts[i] = name
			return strings.Join(parts, "/"), true
		}
	}
	return p, false
}

// shiftPorts moves a request's host ports up by the smallest step that
// makes them all free.
func (s *Service) shiftPorts(ctx context.Context, app *catalog.App, req *deploy.Request, p *Preview) error {
	network := req.Network
	if network == "" {
		network = app.Network
	}
	if network == "host" {
		p.Warnings = append(p.Warnings, "The original uses the host's network, so the copy listens on the same ports: change the game's ports in its settings files before starting both.")
		return nil
	}
	host := manager.HostOf(req.Name)
	if nets, err := s.opts.Manager.Networks(ctx, host); err == nil {
		for _, n := range nets {
			if n.Name == network && n.DirectIP {
				return nil // its own address on the LAN: no ports to clash
			}
		}
	}
	items, err := deploy.Items(app, *req)
	if err != nil {
		return err
	}
	type port struct {
		id    string
		r     deploy.HostPortRange
		proto string
	}
	var ports []port
	for _, it := range items {
		if it.Type != catalog.TypePort || it.Disabled || it.Effective == "" {
			continue
		}
		r, err := deploy.ParseHostPorts(it.Effective)
		if err != nil {
			continue // Build reports it
		}
		proto := strings.ToLower(it.Mode)
		if proto != "udp" {
			proto = "tcp"
		}
		ports = append(ports, port{it.ID, r, proto})
	}
	if len(ports) == 0 {
		return nil
	}
	used, err := s.opts.Manager.HostPortsInUse(ctx, host)
	if err != nil {
		return err
	}
	// The original's own ports count as used on its host, so there the
	// ports always move; on another host they may stay as they are.
	for step := 0; step <= 2000; step++ {
		free := true
		for _, pt := range ports {
			for n := pt.r.Start + step; n <= pt.r.End+step && free; n++ {
				free = n <= 65535 && !used[fmt.Sprintf("%d/%s", n, pt.proto)]
			}
		}
		if !free {
			continue
		}
		if step == 0 {
			return nil
		}
		var moved []string
		for _, pt := range ports {
			v, err := pt.r.Shift(step)
			if err != nil {
				return err
			}
			setValue(req, pt.id, v)
			old, _ := pt.r.Shift(0)
			moved = append(moved, old+" → "+v)
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf("Host ports moved up by %d to free ones (%s). Players connect to the copy on these.",
			step, strings.Join(moved, ", ")))
		return nil
	}
	p.Warnings = append(p.Warnings, "No free host ports were found nearby: choose the copy's ports by hand.")
	return nil
}

// copies lists the data folders to copy: the original's mounts that the
// clone has somewhere else, and its anonymous Docker volumes. A volume that
// can't be copied has an empty From.
func (s *Service) copies(_ context.Context, src docker.ContainerJSON, app *catalog.App, req deploy.Request) ([]Copy, error) {
	// On another host, the same path is another folder.
	otherHost := manager.HostOf(req.Name) != manager.HostOf(src.ID)
	items, err := deploy.Items(app, req)
	if err != nil {
		return nil, err
	}
	to := map[string]string{} // container path → the clone's host path
	for _, it := range items {
		if it.Type == catalog.TypePath && !it.Disabled && it.Effective != "" {
			to[path.Clean(it.Target)] = it.Effective
		}
	}
	var out []Copy
	for _, m := range src.Mounts {
		dest := path.Clean(m.Destination)
		switch m.Type {
		case "bind":
			if t, ok := to[dest]; ok && (t != m.Source || otherHost) {
				out = append(out, Copy{Target: dest, From: m.Source, To: t})
			}
		case "volume":
			if _, bound := to[dest]; bound {
				continue
			}
			if anonymousVolume.MatchString(m.Name) {
				out = append(out, Copy{Target: dest, From: "Docker volume", To: "a new Docker volume"})
			} else {
				out = append(out, Copy{Target: dest})
			}
		}
	}
	slices.SortFunc(out, func(a, b Copy) int { return strings.Compare(a.Target, b.Target) })
	return out, nil
}

// anonymousVolume matches the names Docker gives volumes it makes for an
// image's VOLUME paths.
var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Clone makes a copy of a server: settings, other settings and, as opts
// say, its data. On failure, the half-made copy is removed (its data
// folders are kept, and listed on the Host page).
func (s *Service) Clone(ctx context.Context, id string, opts CloneOptions, emit func(manager.Event)) (manager.Server, error) {
	src, err := s.opts.Manager.Inspect(ctx, id)
	if err != nil {
		return manager.Server{}, err
	}
	srcName := strings.TrimPrefix(src.Name, "/")
	switch opts.CopyData {
	case "", CopyNone:
	case CopyNow:
	case CopyBackup:
		if opts.BackupKey == "" || s.opts.Backups == nil {
			return manager.Server{}, errors.New("choose the backup to copy")
		}
		if !s.opts.Backups.IsBackupOf(srcName, opts.BackupKey) {
			return manager.Server{}, backup.ErrBadKey
		}
	default:
		return manager.Server{}, fmt.Errorf("unknown way to copy data %q", opts.CopyData)
	}
	opts.Name = CloneName(src.ID, opts.Host, opts.Name)
	p, err := s.PreviewClone(ctx, id, opts.Name)
	if err != nil {
		return manager.Server{}, err
	}
	req := p.Request
	if opts.Request != nil {
		req = *opts.Request
		req.TemplateID = p.Request.TemplateID
		req.Name = opts.Name
		if p.Copies, err = s.copies(ctx, src, p.App, req); err != nil {
			return manager.Server{}, err
		}
		p.Copies = slices.DeleteFunc(p.Copies, func(c Copy) bool { return c.From == "" })
	}
	e, err := s.Export(ctx, id, true)
	if err != nil {
		return manager.Server{}, err
	}

	srv, err := s.opts.Manager.Create(ctx, req, emit)
	if err != nil {
		return srv, err
	}
	fail := func(err error) (manager.Server, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if rerr := s.opts.Manager.Remove(rctx, srv.ID); rerr != nil {
			s.opts.Log.Warn("remove failed clone", "server", srv.Name, "err", rerr)
		}
		return manager.Server{}, err
	}
	s.apply(ctx, srv.Name, e.Settings, emit)

	folders := make([]string, 0, len(p.Copies))
	for _, c := range p.Copies {
		folders = append(folders, c.Target)
	}
	progress := progressEmitter(emit)
	switch opts.CopyData {
	case CopyNow:
		if len(folders) == 0 {
			emit(manager.Event{Stage: "copy", Message: "There are no data folders to copy"})
			break
		}
		resume := func() {}
		if opts.StopSource && src.State.Running {
			emit(manager.Event{Stage: "copy", Message: "Stopping " + srcName + " while its data is copied"})
			if err := s.opts.Manager.Stop(ctx, src.ID); err != nil {
				return fail(fmt.Errorf("stop %s: %w", srcName, err))
			}
			resume = func() {
				emit(manager.Event{Stage: "copy", Message: "Starting " + srcName + " again"})
				if err := s.opts.Manager.Start(context.WithoutCancel(ctx), src.ID); err != nil {
					emit(manager.Event{Stage: "warning", Message: fmt.Sprintf("Couldn't start %s again: %v", srcName, err)})
				}
			}
		}
		emit(manager.Event{Stage: "copy", Message: "Copying " + strings.Join(folders, ", ")})
		st, err := s.opts.Manager.CopyData(ctx, src.ID, srv.ID, folders, progress)
		resume() // the original is back as soon as its data is copied
		if err != nil {
			return fail(fmt.Errorf("copy the data: %w", err))
		}
		reportCopy(emit, st.Files, st.Bytes, st.Warnings)
	case CopyBackup:
		emit(manager.Event{Stage: "copy", Message: "Restoring the backup into " + srv.Name})
		run, err := s.opts.Backups.RestoreInto(ctx, srcName, opts.BackupKey, srv.ID, progress)
		if err != nil {
			return fail(fmt.Errorf("restore the backup: %w", err))
		}
		reportCopy(emit, run.Files, run.Bytes, run.Warnings)
	}

	if opts.Start {
		emit(manager.Event{Stage: "start", Message: "Starting " + srv.Name})
		if err := s.opts.Manager.Start(ctx, srv.ID); err != nil {
			return srv, fmt.Errorf("the copy is ready, but starting it failed: %w", err)
		}
	}
	emit(manager.Event{Stage: "done", Message: srv.Name + " is ready", Server: &srv})
	return srv, nil
}

func reportCopy(emit func(manager.Event), n int, bytes int64, warnings []string) {
	emit(manager.Event{Stage: "copy", Message: fmt.Sprintf("Copied %d files (%s)", n, formatBytes(bytes))})
	for _, w := range warnings {
		emit(manager.Event{Stage: "warning", Message: w})
	}
}

// progressEmitter reports a copy's progress every couple of seconds.
func progressEmitter(emit func(manager.Event)) func(files.ArchiveStats) {
	var mu sync.Mutex
	var last time.Time
	return func(st files.ArchiveStats) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		emit(manager.Event{Stage: "copy", Message: fmt.Sprintf("Copied %d files (%s) so far", st.Files, formatBytes(st.Bytes))})
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
