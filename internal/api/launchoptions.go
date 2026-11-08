package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// --- A server's game's launch options ---

func (s *server) launchOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := s.Manager.LaunchOptions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": opts})
}

// setLaunchOption turns one of a server's game's launch options on or off,
// recreating the server, and keeps the option's port to the LAN. Its
// arguments come from the game's profile (which only admins write), so
// the server's managers may turn it on, as they may change its settings.
func (s *server) setLaunchOption(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, option := r.PathValue("id"), r.PathValue("option")
	audit.Add(r.Context(), "option", option)
	audit.Add(r.Context(), "on", fmt.Sprint(in.On))
	admin := s.access(r).Admin()
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		srv, port, err := s.Manager.SetLaunchOption(ctx, id, option, in.On, admin, emit)
		if err != nil {
			return err
		}
		if port != "" && in.On && s.Forwarding != nil {
			if err := s.Forwarding.KeepLocal(strings.TrimPrefix(srv.Name, "/"), port); err != nil {
				emit(manager.Event{Stage: "warning", Message: "Couldn't keep port " + port + " off the internet: " + err.Error()})
			} else {
				emit(manager.Event{Stage: "warning", Message: "Port " + port + " is only for your network: it's never forwarded or relayed to the internet."})
			}
		}
		return nil
	})
}
