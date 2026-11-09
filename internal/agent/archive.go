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

// archiveHandler serves ArchivePath. rootPath is where the host's root
// filesystem is mounted; without it, folders come through Docker as before.
func archiveHandler(dc *docker.Client, rootPath string) http.HandlerFunc {
	var hostPath func(string) string
	if rootPath != "" {
		hostPath = func(p string) string { return path.Join(rootPath, p) }
	}
	b := files.New(dc, hostPath)
	return func(w http.ResponseWriter, r *http.Request) {
		var in ArchiveRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
			writeAgentError(w, http.StatusBadRequest, err)
			return
		}
		if in.ID == "" || strings.ContainsAny(in.ID, "/?#") {
			writeAgentError(w, http.StatusBadRequest, errors.New("name the container"))
			return
		}
		info, err := dc.InspectContainer(r.Context(), in.ID)
		if err != nil {
			status := http.StatusBadGateway
			if docker.IsNotFound(err) {
				status = http.StatusNotFound
			}
			writeAgentError(w, status, err)
			return
		}
		// Only hlm's servers: the agent passes on hlm's Docker calls, not
		// reads of any file on the host.
		if info.Config.Labels[managedLabel] != "true" {
			writeAgentError(w, http.StatusForbidden, errors.New("that container isn't one of hlm's servers"))
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

// ErrArchiveUnsupported is returned by Hub.Archive for an agent from before
// ArchivePath: its Docker allow-list refuses the path.
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
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return h.Dial(ctx, host) },
		DisableKeepAlives: true,
	}}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+ArchivePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrNotConnected) {
			return nil, ErrNotConnected
		}
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return &Archive{ReadCloser: resp.Body, resp: resp}, nil
	}
	defer resp.Body.Close()
	var msg struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&msg)
	if msg.Error == "" {
		// Answered as Docker does: an older agent's allow-list.
		return nil, ErrArchiveUnsupported
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &docker.APIError{Status: http.StatusNotFound, Message: msg.Error}
	}
	return nil, errors.New(msg.Error)
}
