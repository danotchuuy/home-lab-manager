package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/agent"
	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/hosts"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// --- Docker hosts ---

// hostView is a host as the API shows it. Everyone signed in sees which
// hosts there are (for the install form and server badges); admins see how
// they're reached. Client keys are never sent.
type hostView struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Local       bool                `json:"local,omitempty"`
	Status      *manager.HostStatus `json:"status,omitempty"`
	Kind        string              `json:"kind,omitempty"`
	URL         string              `json:"url,omitempty"`
	CA          string              `json:"ca,omitempty"`
	Cert        string              `json:"cert,omitempty"`
	HasKey      bool                `json:"hasKey,omitempty"`
	SSH         *remote.Target      `json:"ssh,omitempty"`
	Socket      string              `json:"socket,omitempty"`
	AppdataRoot string              `json:"appdataRoot,omitempty"`
	Address     string              `json:"address,omitempty"`
	// Agent is the connection of the host's agent.
	Agent *agent.Status `json:"agent,omitempty"`
}

func (s *server) listHosts(w http.ResponseWriter, r *http.Request) {
	admin := s.access(r).Admin()
	out := []hostView{{ID: manager.LocalHost, Name: "This host", Local: true}}
	if admin {
		out[0].AppdataRoot = s.Manager.AppdataRoot()
		if s.Docker != nil {
			out[0].URL = s.Docker.Host()
		}
	}
	if s.Hosts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
		return
	}
	list, err := s.Hosts.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	statuses := s.Manager.HostStatuses()
	for _, h := range list {
		// Where its servers' ports are: what players connect to.
		v := hostView{ID: h.ID, Name: h.Name, Address: h.Address}
		if st, ok := statuses[h.ID]; ok {
			if !admin {
				st.Error = "" // may say how the host is reached
			}
			v.Status = &st
		}
		if admin {
			v.Kind, v.URL, v.CA, v.Cert, v.HasKey = h.Kind, h.URL, h.CA, h.Cert, h.HasKey
			v.SSH, v.Socket, v.AppdataRoot = h.SSH, h.Socket, h.AppdataRoot
			if h.Kind == hosts.KindAgent {
				v.Agent = s.Hosts.AgentStatus(h.ID)
			}
		}
		out = append(out, v)
	}
	res := map[string]any{"hosts": out}
	if admin && s.Proxy != nil {
		// Hosts reached over SSH sign in with hlm's key.
		if k, err := s.Proxy.Key(); err == nil && k != nil {
			res["sshKey"] = k
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func readHost(w http.ResponseWriter, r *http.Request) (hosts.Input, bool) {
	var in hosts.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return in, false
	}
	if id := r.PathValue("id"); id != "" {
		in.ID = id
	}
	return in, true
}

// writeHostError reports a failed host operation; an SSH host key that
// isn't trusted yet comes with the key, for the user to confirm.
func writeHostError(w http.ResponseWriter, err error) {
	var hk *remote.HostKeyError
	switch {
	case errors.As(err, &hk):
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "hostKey": hk.Key, "hostKeyFingerprint": hk.Fingerprint, "hostKeyType": hk.Type,
		})
	case errors.Is(err, hosts.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) testHost(w http.ResponseWriter, r *http.Request) {
	in, ok := readHost(w, r)
	if !ok {
		return
	}
	res, err := s.Hosts.Test(r.Context(), in)
	if err != nil {
		writeHostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) createHost(w http.ResponseWriter, r *http.Request) {
	s.saveHost(w, r, true)
}

func (s *server) updateHost(w http.ResponseWriter, r *http.Request) {
	s.saveHost(w, r, false)
}

func (s *server) saveHost(w http.ResponseWriter, r *http.Request, create bool) {
	in, ok := readHost(w, r)
	if !ok {
		return
	}
	h, err := s.Hosts.Save(r.Context(), in, create)
	if err != nil {
		writeHostError(w, err)
		return
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	writeJSON(w, status, h)
}

func (s *server) deleteHost(w http.ResponseWriter, r *http.Request) {
	if err := s.Hosts.Delete(r.PathValue("id")); err != nil {
		writeHostError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkHost tests a saved host's connection.
func (s *server) checkHost(w http.ResponseWriter, r *http.Request) {
	res, err := s.Hosts.Check(r.Context(), r.PathValue("id"))
	if err != nil {
		writeHostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// inspectHost signs in to an SSH host and says whether Docker is there,
// and how hlm would set it up if not.
func (s *server) inspectHost(w http.ResponseWriter, r *http.Request) {
	in, ok := readHost(w, r)
	if !ok {
		return
	}
	res, err := s.Hosts.Inspect(r.Context(), in)
	if err != nil {
		writeHostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// setUpDocker installs Docker CE on an SSH host (or starts it, or lets the
// user use it), streaming the output.
func (s *server) setUpDocker(w http.ResponseWriter, r *http.Request) {
	in, ok := readHost(w, r)
	if !ok {
		return
	}
	if in.SSH != nil {
		audit.Add(r.Context(), "host", in.SSH.User+"@"+in.SSH.Host)
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		res, err := s.Hosts.SetUpDocker(ctx, in, func(line string) {
			emit(manager.Event{Stage: "log", Message: line})
		})
		if err != nil {
			return err
		}
		emit(manager.Event{Stage: "done", Message: "Docker " + res.DockerVersion + " is running, and " + res.User + " can use it."})
		return nil
	})
}

// setUpTLS makes certificates for a host and sets Docker's API over TCP
// with TLS up there over SSH, then saves the host, streaming the output.
func (s *server) setUpTLS(w http.ResponseWriter, r *http.Request) {
	var in hosts.TLSInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_, err := s.Hosts.Get(in.ID)
	create := errors.Is(err, hosts.ErrNotFound)
	if err != nil && !create {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	audit.Add(r.Context(), "host", in.ID)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		h, err := s.Hosts.SetUpTLS(ctx, in, create, func(line string) {
			emit(manager.Event{Stage: "log", Message: line})
		})
		if err != nil {
			return err
		}
		emit(manager.Event{Stage: "done", Message: "Docker on " + h.Name + " now takes TLS connections at " + h.URL + ", and the host is saved."})
		return nil
	})
}

// sshKeyForHosts makes hlm's SSH key, if there isn't one, for hosts
// reached over SSH.
func (s *server) sshKeyForHosts(w http.ResponseWriter, r *http.Request) {
	key, err := s.Proxy.EnsureKey(false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

// hostParam is the ?host= a request is about (the local host if none),
// after checking hlm knows it.
func (s *server) hostParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	h := r.URL.Query().Get("host")
	if h == "" {
		return manager.LocalHost, true
	}
	if !s.Manager.KnownHost(h) {
		writeError(w, http.StatusNotFound, errors.New("there is no Docker host "+h))
		return "", false
	}
	return h, true
}
