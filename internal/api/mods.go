package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/mods"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// --- Mods: from Modrinth, as transactions ---

func writeModsError(w http.ResponseWriter, err error) {
	var apiErr *mods.APIError
	switch {
	case errors.Is(err, manager.ErrNotFound), errors.Is(err, manager.ErrNotManaged):
		writeManagerError(w, err)
	case errors.As(err, &apiErr), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusBadGateway, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// modsOverview answers for servers whose mods hlm can't manage with why.
func (s *server) modsOverview(w http.ResponseWriter, r *http.Request, get func(context.Context, string) (mods.Overview, error)) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	ov, err := get(r.Context(), name)
	var u *mods.ErrUnsupported
	if errors.As(err, &u) {
		writeJSON(w, http.StatusOK, map[string]any{"unsupported": u.Why})
		return
	}
	if err != nil {
		writeModsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *server) serverMods(w http.ResponseWriter, r *http.Request) {
	s.modsOverview(w, r, s.Mods.Overview)
}

func (s *server) checkModUpdates(w http.ResponseWriter, r *http.Request) {
	s.modsOverview(w, r, s.Mods.CheckUpdates)
}

func (s *server) searchMods(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	hits, err := s.Mods.Search(r.Context(), name, r.URL.Query().Get("q"))
	if err != nil {
		writeModsError(w, err)
		return
	}
	if hits == nil {
		hits = []mods.SearchHit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func decodeModChanges(w http.ResponseWriter, r *http.Request) ([]mods.Change, bool) {
	var in struct {
		Changes []mods.Change `json:"changes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, false
	}
	return in.Changes, true
}

func (s *server) planMods(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	changes, ok := decodeModChanges(w, r)
	if !ok {
		return
	}
	p, err := s.Mods.Plan(r.Context(), name, changes)
	if err != nil {
		writeModsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// applyMods makes the change, streaming its progress. The audit log gets
// each file installed, with its version and hash.
func (s *server) applyMods(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	changes, ok := decodeModChanges(w, r)
	if !ok {
		return
	}
	p, _ := r.Context().Value(principalKey{}).(rbac.Principal)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		res, err := s.Mods.Apply(ctx, name, p.Name, changes, emit)
		if res.Plan != nil {
			audit.Add(ctx, "change", mods.Summary(res.Plan))
			var hashes []string
			for _, st := range res.Plan.Steps {
				if st.New != nil {
					hashes = append(hashes, fmt.Sprintf("%s sha512:%s", st.New.File, st.New.SHA512))
				}
			}
			audit.Add(ctx, "files", strings.Join(hashes, "; "))
		}
		if res.RolledBack {
			audit.Add(ctx, "rolled_back", "yes")
		}
		if err == nil {
			emit(manager.Event{Stage: "done", Message: cmpOr(res.Warning, "Done: the server is up with the change.")})
		}
		return err
	})
}
