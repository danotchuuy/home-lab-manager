package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
)

// --- Containers hlm didn't make, and adopting them ---

// unmanagedContainers lists a host's containers that aren't hlm's.
func (s *server) unmanagedContainers(w http.ResponseWriter, r *http.Request) {
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	list, err := s.Manager.Unmanaged(ctx, host)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": list})
}

// adoptPlan says how hlm would adopt a container, changing nothing.
func (s *server) adoptPlan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	plan, err := s.Manager.AdoptPlan(ctx, r.PathValue("id"))
	if err != nil {
		writeAdoptError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// adoptContainer recreates a container as one of hlm's servers, streaming
// the progress.
func (s *server) adoptContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	audit.Add(r.Context(), "server", id)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		// Starting the new container reports "done".
		_, err := s.Manager.Adopt(ctx, id, emit)
		return err
	})
}

func writeAdoptError(w http.ResponseWriter, err error) {
	if errors.Is(err, manager.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}
