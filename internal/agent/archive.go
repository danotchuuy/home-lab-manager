package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/files"
)

// ArchivePath is where the agent answers with a tar of a server's folders,
// read from the host's disk where they're bind mounts or volumes, rather
// than through Docker's archive API. Docker locks a container for as long
// as an archive copy runs, so a long backup would leave every other call
// about it (an inspect, say) waiting.
const ArchivePath = "/_hlm/archive"

// ArchiveRequest asks for a server's folders (container paths).
type ArchiveRequest struct {
	ID      string   `json:"id"`
	Folders []string `json:"folders"`
	Exclude []string `json:"exclude,omitempty"`
}

// Trailers sent after an archive: its stats (JSON files.ArchiveStats), and
// the error that cut it short, if one did.
const (
	ArchiveStatsTrailer = "X-Hlm-Archive-Stats"
	ArchiveErrorTrailer = "X-Hlm-Archive-Error"
)

// managedLabel marks hlm's servers (deploy.LabelManaged); the agent only
// archives those.
const managedLabel = "io.hlm.managed"

// ListPath is where the agent answers with a listing of a folder in one of
// hlm's servers, read from the host's disk where it's in a bind mount or
// volume. Through Docker, a stopped container's folder can only be listed
// by copying it out (locking the container, and cut short for big ones).
const ListPath = "/_hlm/list"

// ListRequest asks for a folder (a container path) in a server.
type ListRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// server finds one of hlm's servers for a request, answering with an
// error (and false) if it can't.
func server(w http.ResponseWriter, r *http.Request, dc *docker.Client, id string) (docker.ContainerJSON, bool) {
	if id == "" || strings.ContainsAny(id, "/?#") {
		writeAgentError(w, http.StatusBadRequest, errors.New("name the container"))
		return docker.ContainerJSON{}, false
	}
	info, err := dc.InspectContainer(r.Context(), id)
	if err != nil {
		status := http.StatusBadGateway
		if docker.IsNotFound(err) {
			status = http.StatusNotFound
		}
		writeAgentError(w, status, err)
		return info, false
	}
	// Only hlm's servers: the agent passes on hlm's Docker calls, not
	// reads of any file on the host.
	if info.Config.Labels[managedLabel] != "true" {
		writeAgentError(w, http.StatusForbidden, errors.New("that container isn't one of hlm's servers"))
		return info, false
	}
	return info, true
}

// listHandler serves ListPath.
func listHandler(b *files.Browser, dc *docker.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ListRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
			writeAgentError(w, http.StatusBadRequest, err)
			return
		}
		info, ok := server(w, r, dc, in.ID)
		if !ok {
			return
		}
		l, err := b.List(r.Context(), info, in.Path)
		switch {
		case errors.Is(err, files.ErrNotFound):
			writeAgentError(w, http.StatusNotFound, err)
		case errors.Is(err, files.ErrNotDir):
			writeAgentError(w, http.StatusConflict, err)
		case err != nil:
			writeAgentError(w, http.StatusBadGateway, err)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(l)
		}
	}
}

// browser reads servers' files from the host's disk where it can (rootPath
// is where the host's root filesystem is mounted), and through Docker
// otherwise.
func browser(dc *docker.Client, rootPath string) *files.Browser {
	var hostPath func(string) string
	if rootPath != "" {
		hostPath = func(p string) string { return path.Join(rootPath, p) }
	}
	return files.New(dc, hostPath)
}

// archiveHandler serves ArchivePath. rootPath is where the host's root
// filesystem is mounted; without it, folders come through Docker as before.
func archiveHandler(b *files.Browser, dc *docker.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ArchiveRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			writeAgentError(w, http.StatusBadRequest, err)
			return
		}
		info, ok := server(w, r, dc, in.ID)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Trailer", ArchiveStatsTrailer+", "+ArchiveErrorTrailer)
		w.WriteHeader(http.StatusOK)
		tw := tar.NewWriter(w)
		st, err := b.ArchiveExcluding(r.Context(), info, in.Folders, in.Exclude, tw, nil)
		if err == nil {
			err = tw.Close()
		}
		if data, jerr := json.Marshal(st); jerr == nil {
			w.Header().Set(ArchiveStatsTrailer, string(data))
		}
		if err != nil {
			w.Header().Set(ArchiveErrorTrailer, oneLine(err.Error()))
		}
	}
}

// oneLine keeps a message to what a header can carry.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}

// ErrArchiveUnsupported is returned by Hub.Archive and Hub.List for an
// agent from before their paths: its Docker allow-list refuses them.
var ErrArchiveUnsupported = errors.New("the host's agent can't read folders from the host's disk: update it")

// Archive is a tar stream of a server's folders from its host's agent. Its
// stats and error come once the stream has been read to the end.
type Archive struct {
	io.ReadCloser
	resp *http.Response
}

// Result is the agent's stats for the archive, and the error that cut it
// short, if any. Call it after reading the stream to EOF.
func (a *Archive) Result() (files.ArchiveStats, error) {
	var st files.ArchiveStats
	if v := a.resp.Trailer.Get(ArchiveStatsTrailer); v != "" {
		_ = json.Unmarshal([]byte(v), &st)
	}
	if v := a.resp.Trailer.Get(ArchiveErrorTrailer); v != "" {
		return st, errors.New(v)
	}
	if a.resp.Trailer.Get(ArchiveStatsTrailer) == "" {
		return st, errors.New("the agent's archive stopped before the end")
	}
	return st, nil
}

// Archive asks a host's agent for a tar of a server's folders (container
// is the container's ID or name on that host).
func (h *Hub) Archive(ctx context.Context, host string, in ArchiveRequest) (*Archive, error) {
	resp, err := h.agentPost(ctx, host, ArchivePath, in)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return &Archive{ReadCloser: resp.Body, resp: resp}, nil
	}
	defer resp.Body.Close()
	return nil, agentError(resp)
}

// List asks a host's agent for a listing of a folder in a server (container
// is its ID or name on that host).
func (h *Hub) List(ctx context.Context, host string, in ListRequest) (files.Listing, error) {
	resp, err := h.agentPost(ctx, host, ListPath, in)
	if err != nil {
		return files.Listing{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return files.Listing{}, agentError(resp)
	}
	var l files.Listing
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&l); err != nil {
		return files.Listing{}, err
	}
	return l, nil
}

// agentPost sends a JSON request to one of the agent's own paths.
func (h *Hub) agentPost(ctx context.Context, host, p string, in any) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return h.Dial(ctx, host) },
		DisableKeepAlives: true,
	}}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+p, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil && errors.Is(err, ErrNotConnected) {
		return nil, ErrNotConnected
	}
	return resp, err
}

// agentError is the error in an agent's answer that isn't OK. One answered
// as Docker does comes from an older agent's allow-list, which doesn't
// know the path: ErrArchiveUnsupported.
func agentError(resp *http.Response) error {
	var msg struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&msg)
	switch {
	case msg.Error == "":
		return ErrArchiveUnsupported
	case resp.StatusCode == http.StatusNotFound && strings.Contains(msg.Error, "No such container"):
		return &docker.APIError{Status: http.StatusNotFound, Message: msg.Error}
	case resp.StatusCode == http.StatusNotFound:
		return files.ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		return files.ErrNotDir
	}
	return errors.New(msg.Error)
}
