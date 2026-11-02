package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// ExecResult is the output of a command run in a container.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool // output went past the limit and the rest was dropped
}

// Exec runs cmd in a running container as user and waits for it, keeping
// at most maxOutput bytes of each output stream. Unlike the archive API,
// exec doesn't lock the container, so other calls aren't held up.
func (c *Client) Exec(ctx context.Context, id, user string, cmd []string, maxOutput int) (ExecResult, error) {
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": cmd, "User": user}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, body, &created); err != nil {
		return ExecResult{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return ExecResult{}, err
	}
	res, err := demuxLimited(resp.Body, maxOutput)
	resp.Body.Close()
	if err != nil {
		return res, err
	}
	var info struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, nil, &info); err != nil {
		return res, err
	}
	res.ExitCode = info.ExitCode
	if info.Running {
		res.ExitCode = -1 // still running because we stopped reading early
	}
	return res, nil
}

// demuxLimited splits a multiplexed stdout/stderr stream, keeping up to max
// bytes of each. It stops reading once both are full.
func demuxLimited(r io.Reader, max int) (ExecResult, error) {
	var res ExecResult
	var out, errb bytes.Buffer
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return res, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		dst := &out
		if hdr[0] == 2 {
			dst = &errb
		}
		keep := min(size, int64(max-dst.Len()))
		if keep > 0 {
			if _, err := io.CopyN(dst, r, keep); err != nil {
				break
			}
		}
		if size > keep {
			res.Truncated = true
			if _, err := io.CopyN(io.Discard, r, size-keep); err != nil {
				break
			}
		}
		if out.Len() >= max && errb.Len() >= max {
			break
		}
	}
	res.Stdout, res.Stderr = out.Bytes(), errb.Bytes()
	return res, nil
}
