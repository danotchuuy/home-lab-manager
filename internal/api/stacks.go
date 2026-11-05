package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/stack"
)

// --- Compose stacks ---

// maxStackFile is the largest Compose file (or .env) accepted.
const maxStackFile = 512 << 10

type stackInput struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
	Env  string `json:"env"`
}

func readStack(w http.ResponseWriter, r *http.Request) (stackInput, bool) {
	var in stackInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*maxStackFile)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	if len(in.YAML) > maxStackFile || len(in.Env) > maxStackFile {
		writeError(w, http.StatusBadRequest, errors.New("the Compose file or .env is too big"))
		return in, false
	}
	if n := r.PathValue("name"); n != "" {
		in.Name = n
	}
	return in, true
}

func writeStackError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, stack.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, stack.ErrBusy):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

// visibleStack trims a stack to the services the caller can see, and says
// whether there are any.
func (s *server) visibleStack(r *http.Request, st stack.Stack) (stack.Stack, bool) {
	access := s.access(r)
	if access.Admin() {
		return st, true
	}
	var keep []stack.Member
	for _, m := range st.Services {
		if access.Can(m.Container, rbac.Viewer) {
			keep = append(keep, m)
		}
	}
	st.Services = keep
	return st, len(keep) > 0
}

func (s *server) listStacks(w http.ResponseWriter, r *http.Request) {
	list, err := s.Stacks.List(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	out := []stack.Stack{}
	for _, st := range list {
		if v, ok := s.visibleStack(r, st); ok {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getStack(w http.ResponseWriter, r *http.Request) {
	st, err := s.Stacks.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStackError(w, err)
		return
	}
	st, ok := s.visibleStack(r, st)
	if !ok {
		writeStackError(w, stack.ErrNotFound)
		return
	}
	out := map[string]any{"stack": st}
	// The file and .env can hold passwords: admins only.
	if s.access(r).Admin() {
		d, err := s.Stacks.Definition(st.Name)
		if err == nil {
			out["yaml"], out["env"] = d.YAML, d.Env
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) previewStack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		stackInput
		Existing bool `json:"existing"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*maxStackFile)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan, err := s.Stacks.Preview(r.Context(), in.Name, in.YAML, in.Env, in.Existing)
	if err != nil {
		writeStackError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *server) createStack(w http.ResponseWriter, r *http.Request) {
	s.applyStack(w, r, false)
}

func (s *server) updateStack(w http.ResponseWriter, r *http.Request) {
	s.applyStack(w, r, true)
}

func (s *server) applyStack(w http.ResponseWriter, r *http.Request, existing bool) {
	in, ok := readStack(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "stack", in.Name)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		return s.Stacks.Apply(ctx, in.Name, in.YAML, in.Env, existing, emit)
	})
}

// stackAction starts, stops, restarts or updates a stack, for someone who
// may operate every one of its services.
func (s *server) stackAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	st, err := s.Stacks.Get(r.Context(), name)
	if err != nil {
		writeStackError(w, err)
		return
	}
	access := s.access(r)
	visible := false
	for _, m := range st.Services {
		if access.Can(m.Container, rbac.Viewer) {
			visible = true
		}
		if !access.Can(m.Container, rbac.Operator) {
			if visible {
				writeError(w, http.StatusForbidden, errors.New("that takes being an operator of every service in the stack"))
			} else {
				writeStackError(w, stack.ErrNotFound)
			}
			return
		}
	}
	switch action {
	case "start", "stop", "restart", "update":
	default:
		writeError(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		if action == "update" {
			return s.Stacks.Update(ctx, name, emit)
		}
		return s.Stacks.Act(ctx, name, action, emit)
	})
}

func (s *server) deleteStack(w http.ResponseWriter, r *http.Request) {
	if err := s.Stacks.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeStackError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
