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
}

// UpdateCheckInfo describes the update checker's progress.
type UpdateCheckInfo struct {
	Checking  bool       `json:"checking"`
	LastCheck *time.Time `json:"lastCheck,omitempty"`
	Available int        `json:"available"`
}

// updateTracker holds update check results in memory, keyed by container ID.
type updateTracker struct {
	mu        sync.Mutex
	byID      map[string]UpdateStatus
	lastCheck time.Time
	checking  bool
	run       sync.Mutex // serializes checks
}

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
	info := UpdateCheckInfo{Checking: t.checking}
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

// CheckUpdates checks every server's and connection container's image
// against its registry. Each image is looked up once, however many
// containers use it.
func (m *Manager) CheckUpdates(ctx context.Context) error {
	t := m.updates
	t.run.Lock()
	defer t.run.Unlock()
	t.mu.Lock()
	t.checking = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.checking = false
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

	type result struct {
		localID string // ID of the local image the tag points to
		status  UpdateStatus
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = map[string]result{}
		started = map[string]bool{}
		sem     = make(chan struct{}, 4)
	)
	for _, srv := range servers {
		if started[srv.Image] {
			continue
		}
		started[srv.Image] = true
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			localID, st := m.checkImage(cctx, ref)
			st.CheckedAt = time.Now()
			mu.Lock()
			results[ref] = result{localID, st}
			mu.Unlock()
		}(srv.Image)
	}
	wg.Wait()

	live := map[string]bool{}
	for _, srv := range servers {
		live[srv.ID] = true
		r := results[srv.Image]
		st := r.status
		// The registry may match the local image while the container still
		// runs an older one, e.g. after a manual `docker pull`.
		if st.State == UpdateCurrent && r.localID != "" && srv.imageID != "" && r.localID != srv.imageID {
			st = UpdateStatus{
				State:     UpdateAvailable,
				Reason:    "a newer image has already been downloaded; update to recreate the server with it",
				CheckedAt: st.CheckedAt,
			}
		}
		t.set(srv.ID, st)
		if st.State == UpdateAvailable && srv.Name != "" {
			m.notifyUpdateAvailable(srv)
		}
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

// checkImage compares the local image for ref with the registry. It returns
// the local image ID (if the image is present) and the status.
func (m *Manager) checkImage(ctx context.Context, ref string) (string, UpdateStatus) {
	if strings.Contains(ref, "@") {
		return "", UpdateStatus{State: UpdateCurrent, Reason: "pinned to a digest"}
	}
	if strings.HasPrefix(ref, "sha256:") {
		return "", UpdateStatus{State: UpdateUnknown, Reason: "the server was created from an image ID, not a tag"}
	}

	img, err := m.docker.InspectImage(ctx, ref)
	if docker.IsNotFound(err) {
		return "", UpdateStatus{State: UpdateAvailable, Reason: "the image is no longer present locally"}
	}
	if err != nil {
		return "", UpdateStatus{State: UpdateError, Reason: err.Error()}
	}
	if len(img.RepoDigests) == 0 {
		return img.ID, UpdateStatus{State: UpdateUnknown, Reason: "the image wasn't pulled from a registry"}
	}

	remote, err := m.docker.RemoteDigest(ctx, ref)
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
	localID, st := m.checkImage(cctx, srv.Image)
	st.CheckedAt = time.Now()
	if st.State == UpdateCurrent && localID != "" && srv.imageID != "" && localID != srv.imageID {
		st = UpdateStatus{
			State:     UpdateAvailable,
			Reason:    "a newer image has already been downloaded; update to recreate the server with it",
			CheckedAt: st.CheckedAt,
		}
	}
	m.updates.set(srv.ID, st)
	return st, nil
}
