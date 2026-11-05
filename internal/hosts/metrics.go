package hosts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/remote"
)

// Host metrics of other hosts: the CPU, memory, network and disks of the
// machine itself. A host with hlm's agent reports them; on a host reached
// over SSH, hlm runs a short script there (reading /proc and running df);
// on one reached over TLS, the same script runs in a throwaway busybox
// container with the host's /proc and / mounted read-only.

// Intervals between samples: SSH costs one command, TLS a container.
const (
	sshMetricsEvery = 15 * time.Second
	tlsMetricsEvery = time.Minute
	// helperImage runs the script on TLS hosts, as hlm's other helpers do.
	helperImage = "busybox:1.37"
	// LabelHelper marks hlm's throwaway helper containers.
	LabelHelper = "io.hlm.helper"
)

// HostMetrics returns a host's metrics: from its agent, or the last sample
// hlm took over SSH or TLS (taking one if there's none yet).
func (r *Registry) HostMetrics(ctx context.Context, id string) (hostmetrics.Snapshot, error) {
	h, err := r.Get(id)
	if err != nil {
		return hostmetrics.Snapshot{}, err
	}
	if h.Kind == KindAgent {
		if r.opts.Agents == nil {
			return hostmetrics.Snapshot{}, agentUnavailable
		}
		return r.opts.Agents.HostMetrics(ctx, id)
	}
	col, err := r.collectorFor(id)
	if err != nil {
		return hostmetrics.Snapshot{}, err
	}
	snap := col.Snapshot()
	if !snap.Available && snap.Reason == notSampled {
		col.SampleNow(ctx)
		snap = col.Snapshot()
	}
	return snap, nil
}

var agentUnavailable = errors.New("agents can't connect to this hlm")

const notSampled = "not sampled yet"

// collectorFor is a SSH or TLS host's sampler, made on first use.
func (r *Registry) collectorFor(id string) (*hostmetrics.Collector, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[id]
	if !ok {
		return nil, ErrNotFound
	}
	if col, ok := r.collectors[id]; ok && col.conn == c {
		return col.c, nil
	}
	var fetch func(ctx context.Context) (*hostmetrics.Dump, error)
	every := tlsMetricsEvery
	if c.ssh != nil {
		every = sshMetricsEvery
		fetch = hostmetrics.FetchWith(sshFetcher(c.ssh), "/proc", "")
	} else {
		fetch = hostmetrics.FetchWith(helperFetcher(c.client), "/host/proc", "/host/rootfs")
	}
	col := hostmetrics.NewRemote(every, 360, fetch)
	r.collectors[id] = &hostCollector{c: col, conn: c, every: every}
	return col, nil
}

// hostCollector is a SSH or TLS host's sampler.
type hostCollector struct {
	c     *hostmetrics.Collector
	conn  *conn // the connection it was made for; a new one starts afresh
	every time.Duration
	last  time.Time
}

// sshFetcher runs the script over the host's SSH connection.
func sshFetcher(d *sshDialer) hostmetrics.Fetcher {
	return func(ctx context.Context, script string, args []string) ([]byte, error) {
		c, err := d.session(ctx)
		if err != nil {
			return nil, err
		}
		cmd := "sh -s --"
		for _, a := range args {
			cmd += " " + remote.Quote(a)
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res, err := c.Run(ctx, cmd, []byte(script))
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("reading the host's figures failed (exit status %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return []byte(res.Stdout), nil
	}
}

// helperFetcher runs the script in a throwaway container on the host,
// with its /proc and / mounted read-only and no network.
func helperFetcher(dc *docker.Client) hostmetrics.Fetcher {
	return func(ctx context.Context, script string, args []string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if _, err := dc.InspectImage(ctx, helperImage); err != nil {
			if err := dc.PullImage(ctx, helperImage, nil); err != nil {
				return nil, fmt.Errorf("pull %s: %w", helperImage, err)
			}
		}
		id, _, err := dc.CreateContainer(ctx, "", docker.CreateRequest{
			Image:  helperImage,
			Cmd:    append([]string{"sh", "-c", script, "sh"}, args...),
			Labels: map[string]string{LabelHelper: "metrics"},
			HostConfig: docker.HostConfig{
				Binds:       []string{"/proc:/host/proc:ro", "/:/host/rootfs:ro"},
				NetworkMode: "none",
			},
		})
		if err != nil {
			return nil, fmt.Errorf("create the metrics helper: %w", err)
		}
		defer dc.RemoveContainer(context.Background(), id, true)
		if err := dc.StartContainer(ctx, id); err != nil {
			return nil, fmt.Errorf("start the metrics helper: %w", err)
		}
		if _, err := dc.WaitContainer(ctx, id); err != nil {
			return nil, err
		}
		logs, err := dc.Logs(ctx, id, docker.LogsOptions{Tail: "all"})
		if err != nil {
			return nil, err
		}
		defer logs.Close()
		return io.ReadAll(io.LimitReader(logs, 4<<20))
	}
}

// RunHostMetrics samples the other hosts until ctx ends: agents' hosts
// every interval, SSH hosts every 15 seconds and TLS hosts every minute,
// passing each sample (or an unavailable one, saying why) to observe.
func (r *Registry) RunHostMetrics(ctx context.Context, every time.Duration, observe func(id string, snap hostmetrics.Snapshot)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := r.List()
		if err != nil {
			continue
		}
		for _, h := range list {
			if h.Kind == KindAgent {
				snap, err := r.HostMetrics(ctx, h.ID)
				if err != nil {
					snap = hostmetrics.Snapshot{Reason: err.Error()}
				}
				observe(h.ID, snap)
				continue
			}
			if _, err := r.collectorFor(h.ID); err != nil {
				continue
			}
			r.mu.Lock()
			hc := r.collectors[h.ID]
			due := hc != nil && time.Since(hc.last) >= hc.every-time.Second
			if due {
				hc.last = time.Now()
			}
			r.mu.Unlock()
			if !due {
				continue
			}
			hc.c.SampleNow(ctx)
			if snap := hc.c.Snapshot(); snap.Available || snap.Reason != notSampled {
				observe(h.ID, snap)
			}
		}
	}
}
