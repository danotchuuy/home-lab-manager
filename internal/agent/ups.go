package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/nut"
)

// UPSPath is where the agent reads a UPS from a NUT server for hlm (POST,
// a nut.Query; it answers with a nut.Status). The agent asks, not hlm, so
// a NUT server on the host's network works even when hlm can't reach it.
// It only speaks NUT's protocol, and hlm could reach the host's network
// through Docker anyway.
const UPSPath = "/_hlm/ups"

// UPSListPath is where the agent lists a NUT server's UPSes (POST, a
// nut.Query without a UPS; it answers with []nut.UPS).
const UPSListPath = "/_hlm/ups/list"

// upsHandler serves UPSPath and UPSListPath.
func upsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var q nut.Query
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&q); err != nil {
			writeAgentError(w, http.StatusBadRequest, err)
			return
		}
		var out any
		var err error
		if r.URL.Path == UPSListPath {
			out, err = nut.List(r.Context(), q)
		} else {
			out, err = nut.Fetch(r.Context(), q)
		}
		if err != nil {
			writeAgentError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// UPS asks a host's agent to read a UPS.
func (h *Hub) UPS(ctx context.Context, host string, q nut.Query) (nut.Status, error) {
	var st nut.Status
	err := h.post(ctx, host, UPSPath, q, &st)
	return st, err
}

// UPSList asks a host's agent to list a NUT server's UPSes.
func (h *Hub) UPSList(ctx context.Context, host string, q nut.Query) ([]nut.UPS, error) {
	var out []nut.UPS
	err := h.post(ctx, host, UPSListPath, q, &out)
	return out, err
}

// errOldAgent is the answer of an agent from before a path was added: it
// passes the path to Docker's allow-list, which refuses it.
var errOldAgent = errors.New("the host's agent is too old for this: update it (Hosts page, or run its command again with the newer image)")

// post sends in to an agent's path as JSON and decodes its answer to out.
func (h *Hub) post(ctx context.Context, host, path string, in, out any) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return h.Dial(ctx, host) },
		DisableKeepAlives: true,
	}}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrNotConnected) {
			return ErrNotConnected
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"` // Docker's allow-list
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Error == "" {
			return errOldAgent
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
