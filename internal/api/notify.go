package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/notify"
)

// --- Notifications ---

func (s *server) notifications(w http.ResponseWriter, r *http.Request) {
	pub, err := s.Notify.Public()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) saveNotificationSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseURL     string `json:"baseUrl"`
		DiskPercent int    `json:"diskPercent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Notify.SaveGeneral(in.BaseURL, in.DiskPercent); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.notifications(w, r)
}

func decodeTarget(w http.ResponseWriter, r *http.Request) (notify.Target, bool) {
	var t notify.Target
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return t, false
	}
	return t, true
}

func writeNotifyError(w http.ResponseWriter, err error) {
	if errors.Is(err, notify.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *server) addNotificationTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	pub, err := s.Notify.AddTarget(t)
	if err != nil {
		writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pub)
}

func (s *server) updateNotificationTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	pub, err := s.Notify.UpdateTarget(r.PathValue("id"), t)
	if err != nil {
		writeNotifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *server) deleteNotificationTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.Notify.DeleteTarget(r.PathValue("id")); err != nil {
		writeNotifyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testNotificationTarget sends a test message to a target as entered in the
// form; secrets left blank come from the saved target with its ID.
func (s *server) testNotificationTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Notify.Test(ctx, t); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) notificationHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Notify.History())
}
