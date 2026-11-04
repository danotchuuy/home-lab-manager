package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/audit"
	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/registry"
)

// registryBody is a registry login as the System → Registries form sends it.
type registryBody struct {
	Host      string `json:"host"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Deployers bool   `json:"deployers"`
	// Image, for a test, is an image to look up with the login.
	Image string `json:"image"`
}

func decodeRegistry(w http.ResponseWriter, r *http.Request) (registryBody, bool) {
	var b registryBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return b, false
	}
	return b, true
}

func writeRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, registry.ErrExists):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func (s *server) registryList(w http.ResponseWriter, r *http.Request) {
	list, err := s.Registries.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) addRegistry(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeRegistry(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "host", b.Host)
	audit.Add(r.Context(), "username", b.Username)
	p, err := s.Registries.Add(registry.Entry{Host: b.Host, Username: b.Username, Password: b.Password, Deployers: b.Deployers})
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) updateRegistry(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeRegistry(w, r)
	if !ok {
		return
	}
	audit.Add(r.Context(), "username", b.Username)
	if b.Password != "" {
		audit.Add(r.Context(), "password", "changed")
	}
	if b.Host != "" {
		audit.Add(r.Context(), "host", b.Host)
	}
	p, err := s.Registries.Update(r.PathValue("host"), registry.Entry{Host: b.Host, Username: b.Username, Password: b.Password, Deployers: b.Deployers})
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	if err := s.Registries.Delete(r.PathValue("host")); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testRegistry signs in to a registry through the Docker daemon, and looks
// up an image with the login if one is given. A blank password tests the
// saved one.
func (s *server) testRegistry(w http.ResponseWriter, r *http.Request) {
	b, ok := decodeRegistry(w, r)
	if !ok {
		return
	}
	host, err := registry.NormalizeHost(b.Host)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	audit.Add(r.Context(), "host", host)
	if b.Password == "" {
		saved, err := s.Registries.Get(host)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("enter the password or access token to test"))
			return
		}
		b.Password = saved.Password
		if strings.TrimSpace(b.Username) == "" {
			b.Username = saved.Username
		}
	}
	login := docker.RegistryAuth{Username: strings.TrimSpace(b.Username), Password: b.Password, ServerAddress: registry.ServerAddress(host)}
	image := strings.TrimSpace(b.Image)
	if image != "" && registry.HostOf(image) != host {
		writeError(w, http.StatusBadRequest, errors.New(image+" isn't on "+host))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	status, err := s.Docker.Login(ctx, login)
	if err != nil {
		// Not 401: that would look like hlm's own session ending.
		writeError(w, http.StatusBadRequest, errors.New("sign-in failed: "+dockerMessage(err)))
		return
	}
	out := map[string]string{"status": status}
	if image != "" {
		digest, err := s.Docker.RemoteDigestAs(ctx, image, login)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("signed in, but couldn't look up "+image+": "+dockerMessage(err)))
			return
		}
		out["digest"] = digest
	}
	writeJSON(w, http.StatusOK, out)
}

// dockerMessage is the daemon's message, without "docker: … (status n)".
func dockerMessage(err error) string {
	var apiErr *docker.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return err.Error()
}
