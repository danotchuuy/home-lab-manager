package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WriteStdin writes data to a running container's standard input, through
// the attach API, as `docker attach` does when you type. The container must
// have been created with stdin open (-i): Docker accepts writes to other
// containers too, and drops them. With StdinOnce unset, detaching afterwards
// leaves the container's stdin open for the next write.
func (c *Client) WriteStdin(ctx context.Context, id string, data []byte) error {
	q := url.Values{"stream": {"1"}, "stdin": {"1"}}
	version := c.negotiate(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/v"+version+"/containers/"+url.PathEscape(id)+"/attach?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	// Ask for the connection to be handed over, so it can be written to.
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &APIError{Status: resp.StatusCode, Message: apiMessage(data)}
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return errors.New("the Docker daemon didn't hand over the connection for writing")
	}
	if d, ok := ctx.Deadline(); ok {
		if dc, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			_ = dc.SetDeadline(d)
		}
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write to the container's input: %w", err)
	}
	// Half-close where possible, so the daemon sees the end of what was
	// sent and passes it all on before the connection goes.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		drain := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(conn, 1<<20))
			close(drain)
		}()
		select {
		case <-drain:
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
	return conn.Close()
}
