package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/schedule"
)

// --- Scheduled tasks ---

// taskServer resolves the {id} in a task route to the server's name.
func (s *server) taskServer(w http.ResponseWriter, r *http.Request) (string, bool) {
	srv, err := s.Manager.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return "", false
	}
	return srv.Name, true
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, schedule.ErrBusy):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	name, ok := s.taskServer(w, r)
	if !ok {
		return
	}
	// Viewers don't see console commands, which can carry passwords.
	st, err := s.Schedule.Tasks(name, s.access(r).Can(name, rbac.Operator))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) saveTask(w http.ResponseWriter, r *http.Request) {
	name, ok := s.taskServer(w, r)
	if !ok {
		return
	}
	var t schedule.Task
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&t); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t.ID = r.PathValue("task") // empty when adding
	t.Runs, t.Next = nil, nil
	saved, err := s.Schedule.Save(name, t)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	status := http.StatusOK
	if r.PathValue("task") == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
}

func (s *server) deleteTask(w http.ResponseWriter, r *http.Request) {
	name, ok := s.taskServer(w, r)
	if !ok {
		return
	}
	if err := s.Schedule.Delete(name, r.PathValue("task")); err != nil {
		writeScheduleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) runTask(w http.ResponseWriter, r *http.Request) {
	name, ok := s.taskServer(w, r)
	if !ok {
		return
	}
	if err := s.Schedule.RunNow(name, r.PathValue("task")); err != nil {
		writeScheduleError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// upcomingTasks lists the next task runs on the servers the caller can see.
func (s *server) upcomingTasks(w http.ResponseWriter, r *http.Request) {
	access := s.access(r)
	list, err := s.Schedule.Upcoming(func(server string) bool { return access.Can(server, rbac.Viewer) }, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) timezone(w http.ResponseWriter, r *http.Request) {
	configured, system := s.Schedule.Timezone()
	writeJSON(w, http.StatusOK, map[string]string{
		"timezone": configured, "system": system, "effective": s.Schedule.ZoneName(),
	})
}

func (s *server) setTimezone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Timezone string `json:"timezone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Schedule.SetTimezone(in.Timezone); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.timezone(w, r)
}
