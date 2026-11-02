package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// PathStat describes a path inside a container.
type PathStat struct {
	Name       string      `json:"name"`
	Size       int64       `json:"size"`
	Mode       os.FileMode `json:"mode"`
	Mtime      time.Time   `json:"mtime"`
	LinkTarget string      `json:"linkTarget"`
}

func parsePathStat(h http.Header) (PathStat, error) {
	var st PathStat
	raw := h.Get("X-Docker-Container-Path-Stat")
	if raw == "" {
		return st, nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

// StatPath returns information about a path in a container, which may be
// stopped.
func (c *Client) StatPath(ctx context.Context, id, path string) (PathStat, error) {
	resp, err := c.do(ctx, http.MethodHead, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return PathStat{}, err
	}
	resp.Body.Close()
	return parsePathStat(resp.Header)
}

// CopyFrom returns a tar stream of path (a file or a whole directory tree)
// in a container, which may be stopped. Entries are named relative to the
// parent of path. The caller must close the stream.
func (c *Client) CopyFrom(ctx context.Context, id, path string) (io.ReadCloser, PathStat, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, PathStat{}, err
	}
	st, err := parsePathStat(resp.Header)
	if err != nil {
		resp.Body.Close()
		return nil, PathStat{}, err
	}
	return resp.Body, st, nil
}

// CopyTo extracts the tar stream into dir in a container. Ownership and
// modes come from the tar headers.
func (c *Client) CopyTo(ctx context.Context, id, dir string, tarStream io.Reader) error {
	q := url.Values{"path": {dir}, "noOverwriteDirNonDir": {"1"}}
	resp, err := c.doRaw(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive", q, tarStream, "application/x-tar")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
