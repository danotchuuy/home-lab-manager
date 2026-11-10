package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// Update states.
const (
	UpdateCurrent   = "current"   // the server runs the newest image for its tag
	UpdateAvailable = "available" // a newer image exists
	UpdateUnknown   = "unknown"   // can't tell, e.g. a locally built image
	UpdateError     = "error"     // the check failed
)

// UpdateStatus is the result of checking a server's image for updates.
type UpdateStatus struct {
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	// Version is the version tag to update to, for a stack's service that
	// follows latest.
	Version string `json:"version,omitempty"`
}

// UpdateCheckInfo describes the update checker's progress.
type UpdateCheckInfo struct {
	Checking  bool       `json:"checking"`
	LastCheck *time.Time `json:"lastCheck,omitempty"`
	Available int        `json:"available"`
	// Done and Total are how many of the check's images have been looked
	// up, while it's checking.
	Done  int `json:"done,omitempty"`
	Total int `json:"total,omitempty"`
}

// updateTracker holds update check results in memory, keyed by container ID.
type updateTracker struct {
	mu        sync.Mutex
	byID      map[string]UpdateStatus
	lastCheck time.Time
	checking  bool
	// queued counts checks started in the background that are waiting
	// for the one running.
	queued      int
	done, total int
	run         sync.Mutex // serializes checks
}

// checkConcurrency is how many images are looked up at once.
const checkConcurrency = 8

func newUpdateTracker() *updateTracker {
	return &updateTracker{byID: map[string]UpdateStatus{}}
}

func (t *updateTracker) get(id string) (UpdateStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.byID[id]
	return st, ok
}

func (t *updateTracker) set(id string, st UpdateStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[id] = st
}

func (t *updateTracker) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byID, id)
}

// UpdateInfo reports when updates were last checked and how many servers
// and connection containers have one available.
func (m *Manager) UpdateInfo() UpdateCheckInfo {
	t := m.updates
	t.mu.Lock()
	defer t.mu.Unlock()
	info := UpdateCheckInfo{Checking: t.checking || t.queued > 0}
	if t.checking {
		info.Done, info.Total = t.done, t.total
	}
	if !t.lastCheck.IsZero() {
		last := t.lastCheck
		info.LastCheck = &last
	}
	for _, st := range t.byID {
		if st.State == UpdateAvailable {
			info.Available++
		}
	}
	return info
}

// StartCheckUpdates checks for updates in the background, returning at
// once; UpdateInfo shows its progress, and each server's status is filled in
// as its image is looked up. If a check is already running or waiting, it's
// the one.
func (m *Manager) StartCheckUpdates() {
	t := m.updates
	t.mu.Lock()
	if t.queued > 0 {
		t.mu.Unlock()
		return
	}
	t.queued++
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := m.checkUpdates(ctx, true); err != nil {
			m.log.Warn("check for updates", "err", err)
		}
	}()
}

// CheckUpdates checks every server's and connection container's image
// against its registry. Each image is looked up once per host, and each
// registry once per image, however many containers and hosts use it.
func (m *Manager) CheckUpdates(ctx context.Context) error {
	return m.checkUpdates(ctx, false)
}

func (m *Manager) checkUpdates(ctx context.Context, queued bool) error {
	t := m.updates
	t.run.Lock()
	defer t.run.Unlock()
	t.mu.Lock()
	if queued {
		t.queued--
	}
	t.checking, t.done, t.total = true, 0, 0
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.checking, t.done, t.total = false, 0, 0
		t.mu.Unlock()
	}()

	servers, err := m.List(ctx)
	if err != nil {
		return err
	}
	// Connection containers are checked the same way.
	conns, err := m.Connections(ctx)
	if err != nil {
		return err
	}
	for _, c := range conns {
		servers = append(servers, Server{ID: c.ID, Image: c.Image, imageID: c.imageID})
	}

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, checkConcurrency)
		// Each host has its own copy of an image, but the registry's
		// answer is the same for all of them.
		byKey   = map[string][]Server{}
		keys    []string
		lookups = newDigestLookups(m.docker.RemoteDigest)
	)
	key := func(srv Server) string { return HostOf(srv.ID) + " " + srv.Image }
	for _, srv := range servers {
		k := key(srv)
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], srv)
	}
	t.mu.Lock()
	t.total = len(keys)
	t.mu.Unlock()
	for _, k := range keys {
		wg.Add(1)
		go func(list []Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			host, ref := HostOf(list[0].ID), list[0].Image
			localID, st := m.checkImageWith(cctx, host, ref, lookups.get)
			st.CheckedAt = time.Now()
			// Each server's status as soon as its image is looked up.
			for _, srv := range list {
				st := m.checkTracked(cctx, srv, withLocalImage(srv, localID, st), lookups.get)
				t.set(srv.ID, st)
				if st.State == UpdateAvailable && srv.Name != "" {
					m.notifyUpdateAvailable(srv)
				}
			}
			t.mu.Lock()
			t.done++
			t.mu.Unlock()
		}(byKey[k])
	}
	wg.Wait()

	live := map[string]bool{}
	for _, srv := range servers {
		live[srv.ID] = true
	}

	t.mu.Lock()
	for id := range t.byID {
		if !live[id] {
			delete(t.byID, id) // container no longer exists
		}
	}
	t.lastCheck = time.Now()
	t.mu.Unlock()
	return nil
}

// withLocalImage is a server's status from its image's: the registry may
// match the local image while the container still runs an older one, e.g.
// after a manual `docker pull`.
func withLocalImage(srv Server, localID string, st UpdateStatus) UpdateStatus {
	if st.State == UpdateCurrent && localID != "" && srv.imageID != "" && localID != srv.imageID {
		return UpdateStatus{
			State:     UpdateAvailable,
			Reason:    "a newer image has already been downloaded; update to recreate the server with it",
			CheckedAt: st.CheckedAt,
		}
	}
	return st
}

// digestLookups asks the registry about each image once per check,
// whichever host asks first, sharing the answer with the others.
type digestLookups struct {
	lookup func(ctx context.Context, host, ref string) (string, error)
	mu     sync.Mutex
	calls  map[string]*digestCall
}

type digestCall struct {
	done   chan struct{}
	digest string
	err    error
}

func newDigestLookups(lookup func(ctx context.Context, host, ref string) (string, error)) *digestLookups {
	return &digestLookups{lookup: lookup, calls: map[string]*digestCall{}}
}

func (d *digestLookups) get(ctx context.Context, host, ref string) (string, error) {
	d.mu.Lock()
	c, ok := d.calls[ref]
	if !ok {
		c = &digestCall{done: make(chan struct{})}
		d.calls[ref] = c
	}
	d.mu.Unlock()
	if ok {
		select {
		case <-c.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if c.err == nil {
			return c.digest, nil
		}
		// Its host couldn't ask (it may be down, or not logged in): ask from
		// this one.
		return d.lookup(ctx, host, ref)
	}
	c.digest, c.err = d.lookup(ctx, host, ref)
	close(c.done)
	return c.digest, c.err
}

// checkImage compares the local image for ref with the registry. It returns
// the local image ID (if the image is present) and the status.
func (m *Manager) checkImage(ctx context.Context, host, ref string) (string, UpdateStatus) {
	return m.checkImageWith(ctx, host, ref, m.docker.RemoteDigest)
}

func (m *Manager) checkImageWith(ctx context.Context, host, ref string, remoteDigest func(ctx context.Context, host, ref string) (string, error)) (string, UpdateStatus) {
	if strings.Contains(ref, "@") {
		return "", UpdateStatus{State: UpdateCurrent, Reason: "pinned to a digest"}
	}
	if strings.HasPrefix(ref, "sha256:") {
		return "", UpdateStatus{State: UpdateUnknown, Reason: "the server was created from an image ID, not a tag"}
	}

	img, err := m.docker.InspectImage(ctx, host, ref)
	if docker.IsNotFound(err) {
		return "", UpdateStatus{State: UpdateAvailable, Reason: "the image is no longer present locally"}
	}
	if err != nil {
		return "", UpdateStatus{State: UpdateError, Reason: err.Error()}
	}
	if len(img.RepoDigests) == 0 {
		return img.ID, UpdateStatus{State: UpdateUnknown, Reason: "the image wasn't pulled from a registry"}
	}

	remote, err := remoteDigest(ctx, host, ref)
	if err != nil {
		var apiErr *docker.APIError
		if errors.As(err, &apiErr) {
			err = errors.New(apiErr.Message)
		}
		reason := "registry lookup failed: " + err.Error()
		if docker.IsAuthError(err) {
			reason += ". " + LoginHint
		}
		return img.ID, UpdateStatus{State: UpdateError, Reason: reason}
	}
	for _, d := range img.RepoDigests {
		if _, digest, ok := strings.Cut(d, "@"); ok && digest == remote {
			return img.ID, UpdateStatus{State: UpdateCurrent}
		}
	}
	return img.ID, UpdateStatus{State: UpdateAvailable, Reason: "a newer image is available in the registry"}
}

// RunUpdateChecker checks for updates every interval() until ctx is done,
// starting shortly after startup. The interval is asked for again after
// each check, so a new setting applies from the next one; 0 pauses checking.
func (m *Manager) RunUpdateChecker(ctx context.Context, interval func() time.Duration) {
	wait := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if interval() <= 0 {
			wait = time.Minute // checking is off; look at the setting again soon
			continue
		}
		if err := m.CheckUpdates(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("check for updates", "err", err)
		} else {
			m.log.Info("checked for updates", "available", m.UpdateInfo().Available)
		}
		wait = interval()
		if wait <= 0 {
			wait = time.Minute
		}
	}
}

// UpdateAll updates every server that has an update available and that keep
// accepts, one at a time. Events about a server carry its name in Target.
// Failures don't stop the remaining updates.
func (m *Manager) UpdateAll(ctx context.Context, keep func(Server) bool, emit func(Event)) error {
	servers, err := m.List(ctx)
	if err != nil {
		return err
	}
	var todo []Server
	for _, srv := range servers {
		if srv.Update != nil && srv.Update.State == UpdateAvailable && keep(srv) {
			todo = append(todo, srv)
		}
	}
	if len(todo) == 0 {
		emit(Event{Stage: "summary", Message: "No updates available"})
		return nil
	}

	var failed []string
	for i, srv := range todo {
		name := srv.Name
		emit(Event{Stage: "begin", Target: name, Message: fmt.Sprintf("Updating %s (%d of %d)", name, i+1, len(todo))})
		_, err := m.Update(ctx, srv.ID, func(e Event) {
			e.Target = name
			if e.Stage == "error" {
				e.Stage = "warning"
			}
			emit(e)
		})
		if err != nil {
			failed = append(failed, name)
			emit(Event{Stage: "failed", Target: name, Message: err.Error()})
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d updates failed: %s", len(failed), len(todo), strings.Join(failed, ", "))
	}
	emit(Event{Stage: "summary", Message: fmt.Sprintf("Updated %d servers", len(todo))})
	return nil
}

// notifyUpdateAvailable reports a server's update, once for each image the
// server runs.
func (m *Manager) notifyUpdateAvailable(srv Server) {
	m.notify(notify.Event{
		Kind:    notify.KindUpdateAvailable,
		Title:   "Update available for " + srv.Name,
		Message: fmt.Sprintf("A newer %s image is available for %s.", srv.Image, srv.Name),
		Server:  srv.Name,
		Link:    "/servers/" + srv.Name,
		Key:     notify.KindUpdateAvailable + "/" + srv.Name + "/" + cmp.Or(srv.imageID, srv.Image),
		Fields:  map[string]string{"Image": srv.Image},
	})
}

// CheckUpdate checks one server's image against its registry, and records
// the result like CheckUpdates does.
func (m *Manager) CheckUpdate(ctx context.Context, id string) (UpdateStatus, error) {
	srv, err := m.Get(ctx, id)
	if err != nil {
		return UpdateStatus{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	localID, st := m.checkImage(cctx, HostOf(srv.ID), srv.Image)
	st.CheckedAt = time.Now()
	st = m.checkTracked(cctx, srv, withLocalImage(srv, localID, st), m.docker.RemoteDigest)
	m.updates.set(srv.ID, st)
	return st, nil
}
