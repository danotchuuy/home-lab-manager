package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/configfiles"
)

// --- A server's settings files ---

// configFiles lists a server's pinned settings files, and the usual ones of
// its game that hlm found.
func (s *server) configFiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	l, err := s.ConfigFiles.List(ctx, r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *server) setConfigFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in struct {
		Pinned []configfiles.File `json:"pinned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.ConfigFiles.SetPinned(name, in.Pinned); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.configFiles(w, r)
}
