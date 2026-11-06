package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/gameprofile"
	"github.com/danotchuuy/home-lab-manager/internal/players"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- Players: who's on, kicks, bans and lists ---

func (s *server) serverPlayers(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	ov, err := s.Players.Overview(r.Context(), name)
	if errors.Is(err, players.ErrNoPlayers) {
		writeJSON(w, http.StatusOK, map[string]any{"unsupported": err.Error(), "running": ov.Running})
		return
	}
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// playerAction kicks, bans or changes a list. Operators can kick; the rest
// takes a manager.
func (s *server) playerAction(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != gameprofile.ActionKick && !s.access(r).Can(name, rbac.Manager) {
		writeError(w, http.StatusForbidden, errors.New("only the server's managers can do that; operators can kick"))
		return
	}
	var in struct {
		Name   string `json:"name"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	out, err := s.Players.Act(r.Context(), name, p.Name, action, players.Player{Name: in.Name, ID: in.ID}, in.Reason)
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}
