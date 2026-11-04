package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- Game status queries ---

// gameStatuses gives each running server the caller can see: whether it's
// answering its game's status query, and how many players are on.
func (s *server) gameStatuses(w http.ResponseWriter, r *http.Request) {
	access := s.access(r)
	writeJSON(w, http.StatusOK, s.Query.Summaries(func(name string) bool { return access.Can(name, rbac.Viewer) }))
}

func (s *server) gameStatus(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	// Player names are left out for viewers.
	writeJSON(w, http.StatusOK, s.Query.Status(name, s.access(r).Can(name, rbac.Operator)))
}

func decodeQuerySettings(w http.ResponseWriter, r *http.Request) (gamequery.Settings, bool) {
	var set gamequery.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&set); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return set, false
	}
	return set, true
}

func (s *server) setQuerySettings(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	set, ok := decodeQuerySettings(w, r)
	if !ok {
		return
	}
	if _, err := s.Query.SetSettings(name, set); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Query.Status(name, true))
}

// testQuery queries a server with the given settings without saving them;
// automatic settings say what was found.
func (s *server) testQuery(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	set, ok := decodeQuerySettings(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	found, res, err := s.Query.Test(ctx, name, set)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": found, "result": res})
}
