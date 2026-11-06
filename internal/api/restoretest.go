package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/restoretest"
)

// --- Restore tests: a backup restored into an isolated copy and started ---

func (s *server) restoreTests(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	ov, err := s.RestoreTests.Get(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *server) setRestoreTests(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in restoretest.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	set, err := s.RestoreTests.SetSettings(name, in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// runRestoreTest tests one backup now (the newest if no key), streaming
// its progress; it ends with a result event.
func (s *server) runRestoreTest(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "key", in.Key)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		res, err := s.RestoreTests.Test(ctx, name, in.Key, "manual", emit)
		if err != nil {
			return err
		}
		audit.Add(ctx, "key", res.Key)
		audit.Add(ctx, "outcome", res.Outcome)
		if res.Outcome == restoretest.Failed {
			return errors.New(res.Error)
		}
		msg := "Skipped: " + res.Note
		if res.Outcome == restoretest.Passed {
			msg = "The backup restores and the copy started: " + cmpOr(res.Answer, res.Note) + "."
		}
		emit(manager.Event{Stage: "done", Message: msg})
		return nil
	})
}
