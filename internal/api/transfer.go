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
	"github.com/danotchuuy/home-lab-manager/internal/deploy"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
	"github.com/danotchuuy/home-lab-manager/internal/transfer"
)

// --- Export, import and clone ---

// maxExport is the largest export accepted (templates are a few KB).
const maxExport = 1 << 20

func (s *server) exportServer(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	secrets := r.URL.Query().Get("secrets") == "1"
	if secrets {
		// Passwords are for the server's managers, at a browser, who've
		// just confirmed it's them.
		if !s.access(r).Can(name, rbac.Manager) {
			writeError(w, http.StatusForbidden, errors.New("only managers of this server can export its passwords"))
			return
		}
		if !s.stepUp(w, r) {
			return
		}
	}
	audit.Add(r.Context(), "secrets", fmt.Sprint(secrets))
	exp, err := s.Transfer.Export(r.Context(), name, secrets)
	if err != nil {
		writeManagerError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.hlm.json"`, name))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, exp)
}

func readExport(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxExport)).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("that isn't a server exported from hlm: %w", err))
		return false
	}
	return true
}

func (s *server) previewImport(w http.ResponseWriter, r *http.Request) {
	var exp transfer.Export
	if !readExport(w, r, &exp) {
		return
	}
	host, ok := s.hostParam(w, r)
	if !ok {
		return
	}
	p, err := s.Transfer.PreviewImport(r.Context(), exp, host)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) importServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Export  transfer.Export `json:"export"`
		Request deploy.Request  `json:"request"`
		// Host is the Docker host the server goes on; the local one if empty.
		Host string `json:"host"`
	}
	if !readExport(w, r, &body) {
		return
	}
	if strings.Contains(body.Request.Name, "@") {
		writeError(w, http.StatusBadRequest, errors.New("choose the host with \"host\", not in the name"))
		return
	}
	body.Request.Name = manager.JoinID(body.Host, body.Request.Name)
	audit.SetServer(r.Context(), body.Request.Name)
	audit.Add(r.Context(), "template", body.Export.Request.TemplateID)
	audit.Add(r.Context(), "from", body.Export.Server)
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		_, err := s.Transfer.Import(ctx, body.Export, body.Request, emit)
		return err
	})
}

func (s *server) previewClone(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Host string `json:"host"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.Transfer.PreviewClone(r.Context(), name, transfer.CloneName(name, body.Host, body.Name))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) cloneServer(w http.ResponseWriter, r *http.Request) {
	name, ok := s.namedServer(w, r)
	if !ok {
		return
	}
	var opts transfer.CloneOptions
	if !readExport(w, r, &opts) {
		return
	}
	audit.Add(r.Context(), "name", opts.Name)
	audit.Add(r.Context(), "copyData", opts.CopyData)
	if opts.BackupKey != "" {
		audit.Add(r.Context(), "key", opts.BackupKey)
	}
	s.stream(w, r, func(ctx context.Context, emit func(manager.Event)) error {
		_, err := s.Transfer.Clone(ctx, name, opts, emit)
		return err
	})
}
