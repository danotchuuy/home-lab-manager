package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/profileshare"
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

// --- Sharing profiles: links, indexes, updates and export ---

func writeShareError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, profileshare.ErrChanged):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

type profileLinkBody struct {
	URL    string `json:"url"`
	Index  string `json:"index"`
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

func decodeProfileLink(w http.ResponseWriter, r *http.Request) (profileLinkBody, bool) {
	var in profileLinkBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	return in, true
}

// fetchGameProfile fetches a profile from a link, or from an index, and
// shows it with what it would replace; nothing is saved.
func (s *server) fetchGameProfile(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeProfileLink(w, r)
	if !ok {
		return
	}
	var (
		pv  profileshare.Preview
		err error
	)
	if in.Index != "" {
		pv, err = s.ProfileShare.PreviewIndexed(r.Context(), in.Index, in.ID)
	} else {
		pv, err = s.ProfileShare.Preview(r.Context(), in.URL)
	}
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// importGameProfileLink saves a profile fetched from a link or an index,
// if it's still what was looked at.
func (s *server) importGameProfileLink(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeProfileLink(w, r)
	if !ok {
		return
	}
	var (
		p   *gameprofile.Profile
		err error
	)
	if in.Index != "" {
		audit.Add(r.Context(), "index", in.Index)
		audit.Add(r.Context(), "id", in.ID)
		p, err = s.ProfileShare.ImportIndexed(r.Context(), in.Index, in.ID, in.SHA256)
	} else {
		audit.Add(r.Context(), "url", in.URL)
		p, err = s.ProfileShare.Import(r.Context(), in.URL, in.SHA256)
	}
	if err != nil {
		writeShareError(w, err)
		return
	}
	audit.Add(r.Context(), "sha256", in.SHA256)
	writeJSON(w, http.StatusOK, p)
}

// checkGameProfiles fetches every linked profile again.
func (s *server) checkGameProfiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.ProfileShare.Check(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.GameProfiles.State())
}

func (s *server) exportGameProfiles(w http.ResponseWriter, r *http.Request) {
	data, err := s.ProfileShare.Export()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="game-profiles.zip"`)
	w.Write(data)
}

func (s *server) gameProfileIndexes(w http.ResponseWriter, r *http.Request) {
	links, err := s.ProfileShare.Indexes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": links})
}

func (s *server) setGameProfileIndexes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Indexes []string `json:"indexes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	links, err := s.ProfileShare.SetIndexes(in.Indexes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": links})
}

// browseGameProfileIndexes lists the indexes' profiles.
func (s *server) browseGameProfileIndexes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	ls, err := s.ProfileShare.Browse(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ls)
}
