package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/console"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/rcon"
)

// --- Console: sending servers commands ---

func writeConsoleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, manager.ErrNotManaged):
		writeManagerError(w, err)
	case errors.Is(err, console.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, err)
	case errors.Is(err, rcon.ErrAuth):
		writeError(w, http.StatusBadGateway, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) consoleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Console.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func decodeConsoleInput(w http.ResponseWriter, r *http.Request) (console.Input, bool) {
	var in console.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	return in, true
}

func (s *server) saveConsole(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeConsoleInput(w, r)
	if !ok {
		return
	}
	st, err := s.Console.Save(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) testConsole(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeConsoleInput(w, r)
	if !ok {
		return
	}
	msg, err := s.Console.Test(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

func (s *server) consoleCommand(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	audit.Add(r.Context(), "command", in.Command)
	res, err := s.Console.Send(r.Context(), r.PathValue("id"), p.Name, in.Command)
	if err != nil {
		writeConsoleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
