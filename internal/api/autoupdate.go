package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/autoupdate"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- Automatic updates ---

func (s *server) updatePolicy(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	st, err := s.AutoUpdate.Status(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The warning is a console command; viewers don't see those.
	if !s.access(r).Can(name, rbac.Operator) {
		st.Policy.Warn, st.Effective.Warn, st.Default.Warn = "", "", ""
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) setUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var p autoupdate.Policy
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.AutoUpdate.SetServerPolicy(name, p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.updatePolicy(w, r)
}

func (s *server) systemUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	set, err := s.AutoUpdate.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{
		"default": set.Default, "checkHours": set.CheckHours, "self": set.Self,
		"defaultCheckHours": autoupdate.DefaultCheckHours, "checkEnv": s.UpdateCheckEnv,
	}
	if res, ok := set.Results[autoupdate.SelfKey]; ok {
		out["selfLast"] = res
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) setSystemUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	var g autoupdate.General
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&g); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.AutoUpdate.SetGeneral(g); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.systemUpdatePolicy(w, r)
}

// autoUpdates says which servers the caller can see update automatically,
// and what's happening with their updates, for the server list.
func (s *server) autoUpdates(w http.ResponseWriter, r *http.Request) {
	set, err := s.AutoUpdate.Settings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	access := s.access(r)
	visible := func(name string) bool { return access.Can(name, rbac.Viewer) }
	modes := map[string]string{}
	for name, p := range set.Servers {
		if visible(name) {
			modes[name] = p.Mode
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default": set.Default.Mode, "servers": modes,
		"states": autoupdate.Visible(s.AutoUpdate.States(), visible),
	})
}
