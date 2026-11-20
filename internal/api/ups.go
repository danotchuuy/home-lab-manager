package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danotchuuy/home-lab-manager/internal/ups"
)

var errNoUPS = errors.New("UPS monitoring isn't available")

// upsStatus returns each watched host's UPS.
func (s *server) upsStatus(w http.ResponseWriter, r *http.Request) {
	if s.UPS == nil {
		writeJSON(w, http.StatusOK, []ups.HostUPS{})
		return
	}
	writeJSON(w, http.StatusOK, s.UPS.Status())
}

func (s *server) upsSettings(w http.ResponseWriter, r *http.Request) {
	if s.UPS == nil {
		writeError(w, http.StatusNotFound, errNoUPS)
		return
	}
	writeJSON(w, http.StatusOK, s.UPS.Settings())
}

func (s *server) setUPSSettings(w http.ResponseWriter, r *http.Request) {
	if s.UPS == nil {
		writeError(w, http.StatusNotFound, errNoUPS)
		return
	}
	var in ups.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.UPS.SetSettings(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// upsListRequest is a NUT server to list, as a host sees it.
type upsListRequest struct {
	ups.Settings
	Host string `json:"host"`
}

// listUPS lists a NUT server's UPSes, asked from a host (through its
// agent, if it has one), with settings that may not be saved yet.
func (s *server) listUPS(w http.ResponseWriter, r *http.Request) {
	if s.UPS == nil {
		writeError(w, http.StatusNotFound, errNoUPS)
		return
	}
	var in upsListRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list, err := s.UPS.List(r.Context(), in.Host, in.Settings)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// forgetUPSStopped stops hlm from starting a host's servers again when its
// power comes back.
func (s *server) forgetUPSStopped(w http.ResponseWriter, r *http.Request) {
	if s.UPS == nil {
		writeError(w, http.StatusNotFound, errNoUPS)
		return
	}
	s.UPS.Forget(r.PathValue("host"))
	w.WriteHeader(http.StatusNoContent)
}
