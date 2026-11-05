// Package cleanup keeps the disk cleanup settings and runs the weekly
// cleanup: old images left by updates and hlm's leftover containers, on
// every Docker host. What's safe to delete is worked out by the manager.
package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/secrets"
)

const secretName = "cleanup"

// Week is how often the scheduled cleanup runs.
const Week = 7 * 24 * time.Hour

// Settings are the cleanup settings, with the last scheduled run.
type Settings struct {
	// KeepOldImages keeps a server's previous image after an update; by
	// default it's deleted once the update has worked.
	KeepOldImages bool `json:"keepOldImages"`
	// Weekly deletes old images and hlm's leftover containers once a week.
	Weekly bool `json:"weekly"`

	LastRun   *time.Time `json:"lastRun,omitempty"`
	LastFreed int64      `json:"lastFreed"`
	LastError string     `json:"lastError,omitempty"`
}

// Manager is what the service needs from the server manager.
type Manager interface {
	HostIDs() []string
	CleanupLeftovers(ctx context.Context, host string) (manager.CleanupResult, error)
}

// Service keeps the settings and runs the weekly cleanup.
type Service struct {
	store *secrets.Store
	mgr   Manager
	audit audit.Recorder
	log   *slog.Logger
	mu    sync.Mutex
}

// New creates the service.
func New(store *secrets.Store, mgr Manager, rec audit.Recorder, log *slog.Logger) *Service {
	return &Service{store: store, mgr: mgr, audit: rec, log: log}
}

// Settings returns the settings.
func (s *Service) Settings() (Settings, error) {
	var set Settings
	_, err := s.store.Get(secretName, &set)
	return set, err
}

// Save changes the settings, keeping the last run.
func (s *Service) Save(in Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Settings()
	if err != nil {
		return Settings{}, err
	}
	cur.KeepOldImages, cur.Weekly = in.KeepOldImages, in.Weekly
	return cur, s.store.Set(secretName, cur)
}

// RemoveOldImages says whether a server's old image goes after an update.
func (s *Service) RemoveOldImages() bool {
	set, err := s.Settings()
	return err == nil && !set.KeepOldImages
}

// Run checks every hour whether the weekly cleanup is due, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.RunIfDue(ctx, time.Now())
	}
}

// RunIfDue runs the weekly cleanup if it's on and a week has passed since
// the last run (or there hasn't been one).
func (s *Service) RunIfDue(ctx context.Context, now time.Time) bool {
	set, err := s.Settings()
	if err != nil || !set.Weekly || (set.LastRun != nil && now.Sub(*set.LastRun) < Week) {
		return false
	}
	var freed int64
	var errs []error
	for _, host := range s.mgr.HostIDs() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		res, err := s.mgr.CleanupLeftovers(cctx, host)
		cancel()
		if err != nil {
			errs = append(errs, errors.New(host+": "+err.Error()))
			continue
		}
		freed += res.Freed
		for _, f := range res.Failed {
			errs = append(errs, errors.New(host+": "+f.Name+": "+f.Error))
		}
		if len(res.Removed)+len(res.Failed) > 0 && s.audit != nil {
			e := audit.Entry{Actor: "hlm", ActorKind: audit.ActorHLM, Action: "host.cleanup.weekly", Target: host, Result: audit.ResultOK,
				Details: map[string]string{"removed": strconv.Itoa(len(res.Removed)), "freed": strconv.FormatInt(res.Freed, 10)}}
			if len(res.Failed) > 0 {
				e.Result, e.Error = audit.ResultError, res.Failed[0].Name+": "+res.Failed[0].Error
			}
			s.audit.Record(e)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Settings()
	if err != nil {
		return true
	}
	cur.LastRun, cur.LastFreed, cur.LastError = &now, freed, ""
	if err := errors.Join(errs...); err != nil {
		cur.LastError = err.Error()
	}
	if err := s.store.Set(secretName, cur); err != nil {
		s.log.Warn("save cleanup result", "err", err)
	}
	s.log.Info("weekly cleanup", "freed", freed, "errors", len(errs))
	return true
}
