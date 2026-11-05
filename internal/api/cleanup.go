package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/cleanup"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// --- disk cleanup ---

// cleanupCandidates lists what can be deleted on a host (?host=), without
// deleting anything.
func (s *server) cleanupCandidates(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	plan, err := s.Manager.CleanupCandidates(ctx, host)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// runCleanup deletes the chosen items on a host.
func (s *server) runCleanup(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	var req manager.CleanupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "host", host)
	audit.Add(r.Context(), "images", strconv.Itoa(len(req.Images)))
	audit.Add(r.Context(), "containers", strconv.Itoa(len(req.Containers)))
	audit.Add(r.Context(), "volumes", strconv.Itoa(len(req.Volumes)))
	if req.BuildCache {
		audit.Add(r.Context(), "buildCache", "yes")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.Manager.Cleanup(ctx, host, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	audit.Add(r.Context(), "freed", strconv.FormatInt(res.Freed, 10))
	writeJSON(w, http.StatusOK, res)
}

func (s *server) cleanupSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.Cleanup.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *server) saveCleanupSettings(w http.ResponseWriter, r *http.Request) {
	var in cleanup.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	set, err := s.Cleanup.Save(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}
