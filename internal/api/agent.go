package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/hosts"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/statuspage"
)

// --- Hosts' agents ---

// enrolLimiter slows down guessing enrolment tokens (which are long and
// random anyway): ten tries, then one a minute per address.
var enrolLimiter = &statuspage.Limiter{Rate: 1.0 / 60, Burst: 10}

// createEnrolment makes a single-use token for a new host's agent; the UI
// turns it into the command to run there.
func (s *server) createEnrolment(w http.ResponseWriter, r *http.Request) {
	in, ok := readHost(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "host", in.ID)
	e, err := s.Hosts.CreateEnrolment(in.Host)
	if err != nil {
		writeHostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": e.Token, "host": e.Host, "expires": e.Expires, "version": s.Version})
}

// installAgent installs hlm's agent on a host over SSH, with a new
// enrolment token, and waits for it to connect, streaming the output.
func (s *server) installAgent(w http.ResponseWriter, r *http.Request) {
	var in hosts.AgentInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "host", in.ID)
	if in.SSH != nil {
		audit.Add(r.Context(), "ssh", in.SSH.User+"@"+in.SSH.Host)
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		h, err := s.Hosts.InstallAgent(ctx, in, func(line string) {
			emit(manager.Event{Stage: "log", Message: line})
		})
		if err != nil {
			return err
		}
		emit(manager.Event{Stage: "done", Message: "The agent on " + h.Name + " is connected, and the host is added."})
		return nil
	})
}

// enrolAgent trades an enrolment token for the agent's credential. It's
// called by "hlm agent", not a signed-in user: the token is the proof.
func (s *server) enrolAgent(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil || s.Agents == nil {
		writeError(w, http.StatusNotFound, errors.New("agents can't connect to this hlm"))
		return
	}
	if !enrolLimiter.Allow(remoteHost(r), time.Now()) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many tries; wait a minute"))
		return
	}
	var in agent.EnrolRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || in.Token == "" {
		writeError(w, http.StatusBadRequest, errors.New("send the enrolment token"))
		return
	}
	h, cred, err := s.Hosts.Enrol(in.Token)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, hosts.ErrBadToken) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err)
		return
	}
	audit.Add(r.Context(), "host", h.ID)
	audit.Add(r.Context(), "version", in.Version)
	writeJSON(w, http.StatusOK, agent.EnrolResponse{Host: h.ID, Credential: cred})
}

// agentConnect takes an agent's connection (a WebSocket, signed in with
// its credential).
func (s *server) agentConnect(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		writeError(w, http.StatusNotFound, errors.New("agents can't connect to this hlm"))
		return
	}
	s.Agents.ServeHTTP(w, r)
}

// moveAgents tells every agent hlm's new address (for agents that
// connect to it there): {"server": "https://hlm.example.com"}. Each checks
// it can reach hlm there before switching; ones not connected are told when
// they next connect.
func (s *server) moveAgents(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	server, err := hosts.CheckAgentServer(in.Server)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "server", server)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	moves := s.Hosts.MoveAgents(ctx, func(string) (string, bool) { return server, true })
	if moves == nil {
		moves = []hosts.AgentMove{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"moves": moves})
}

// agentCheck answers an agent checking an address is its hlm's, before
// it moves there.
func (s *server) agentCheck(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		writeError(w, http.StatusNotFound, errors.New("agents can't connect to this hlm"))
		return
	}
	s.Agents.ServeCheck(w, r)
}

// updateAgent updates a host's agent to hlm's version, streaming the
// progress.
func (s *server) updateAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	audit.Add(r.Context(), "host", id)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		err := s.Hosts.UpdateAgent(ctx, id, func(line string) {
			emit(manager.Event{Stage: "log", Message: line})
		})
		if err != nil {
			return err
		}
		emit(manager.Event{Stage: "done", Message: "The agent runs hlm " + s.Version + "."})
		return nil
	})
}

// setAgentAutoUpdate turns automatic agent updates on or off.
func (s *server) setAgentAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "on", fmt.Sprint(in.On))
	if err := s.Hosts.SetAgentAutoUpdate(in.On); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"on": in.On})
}
