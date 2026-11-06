package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/idle"
)

// --- Stopping idle servers and waking them ---

func (s *server) serverIdle(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	st, err := s.Idle.Status(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) setServerIdle(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in idle.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.Idle.Save(r.Context(), name, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// errNoWake is the answer for anything that can't be woken from the status
// page, whether it's not on it, not asleep, or not allowed to: they look
// the same from outside.
var errNoWake = errors.New("that server can't be started from here")

// wakeFromStatusPage starts a sleeping server whose status page entry
// allows it. It says only whether it worked.
func (s *server) wakeFromStatusPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.statusViewGate(w, r); !ok {
		return
	}
	// As for the rest of the API: other sites can't send this header.
	if r.Header.Get(csrfHeader) == "" {
		writeError(w, http.StatusForbidden, errors.New("send the "+csrfHeader+" header"))
		return
	}
	if s.Idle == nil {
		writeError(w, http.StatusNotFound, errNoWake)
		return
	}
	if !wakeLimiter.Allow(remoteHost(r), time.Now()) {
		w.Header().Set("Retry-After", "600")
		writeError(w, http.StatusTooManyRequests, errors.New("too many starts from here; wait a few minutes"))
		return
	}
	name, err := s.StatusPage.ServerName(r.Context(), r.Host, r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusNotFound, errNoWake)
		return
	}
	if asleep, allowed := s.Idle.AsleepInfo(name); !asleep || !allowed {
		writeError(w, http.StatusNotFound, errNoWake)
		return
	}
	if err := s.Idle.Wake(r.Context(), name, "the status page"); err != nil {
		if errors.Is(err, idle.ErrNotAsleep) {
			writeError(w, http.StatusNotFound, errNoWake)
			return
		}
		s.Log.Warn("wake from the status page", "server", name, "err", err)
		writeError(w, http.StatusBadGateway, errors.New("the server couldn't be started; the owner has been told"))
		return
	}
	s.StatusPage.Invalidate()
	writeJSON(w, http.StatusOK, map[string]string{"state": "starting"})
}
