package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
)

// --- Game profiles: what hlm knows about each game ---

func (s *server) gameProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.GameProfiles.State())
}

// gameProfileYAML is a profile in use as it was written, or with
// ?builtin=1 the built-in one, also when your own takes its place.
func (s *server) gameProfileYAML(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var (
		data []byte
		err  error
	)
	if r.URL.Query().Get("builtin") == "1" {
		data, err = gameprofile.Builtin(id)
	} else {
		data, err = s.GameProfiles.Source(id)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

// importGameProfile saves a profile sent as YAML.
func (s *server) importGameProfile(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, gameprofile.MaxSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.GameProfiles.Import(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) deleteGameProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.GameProfiles.Delete(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gameprofile.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, s.GameProfiles.State())
}

// reloadGameProfiles reads the profiles folder again, for files changed
// there by hand.
func (s *server) reloadGameProfiles(w http.ResponseWriter, r *http.Request) {
	if err := s.GameProfiles.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.GameProfiles.State())
}
