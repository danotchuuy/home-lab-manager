package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// EventMessage is an event from the daemon, such as a container dying.
type EventMessage struct {
	Type   string     `json:"Type"`   // container, image, network, ...
	Action string     `json:"Action"` // die, kill, oom, health_status: unhealthy, ...
	Actor  EventActor `json:"Actor"`
	Time   int64      `json:"time"`
	// TimeNano is the time in nanoseconds since the epoch.
	TimeNano int64 `json:"timeNano"`
}

// EventActor is what an event is about. For containers, Attributes hold
// the container's name, image and labels, and for die events its
// exitCode.
type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// When returns the event's time.
func (e EventMessage) When() time.Time {
	if e.TimeNano != 0 {
		return time.Unix(0, e.TimeNano)
	}
	return time.Unix(e.Time, 0)
}

// Events follows the daemon's event stream, calling fn for each event that
// matches filters (as for `docker events --filter`, e.g.
// {"type": {"container"}, "event": {"die"}}), until ctx ends or the stream
// does. With since set, it starts with the events since then. It returns
// nil only when ctx ended.
func (c *Client) Events(ctx context.Context, since time.Time, filters map[string][]string, fn func(EventMessage)) error {
	q := url.Values{}
	if len(filters) > 0 {
		f, err := json.Marshal(filters)
		if err != nil {
			return err
		}
		q.Set("filters", string(f))
	}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	resp, err := c.do(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var e EventMessage
		if err := dec.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		addCurrentLabels(e.Actor.Attributes)
		fn(e)
	}
}
