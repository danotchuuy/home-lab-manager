// Package metrics samples servers' resource use into the history
// (internal/metricstore), and writes hlm's state in Prometheus' text format.
package metrics

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/danotchuuy/home-lab-manager/internal/docker"
	"github.com/danotchuuy/home-lab-manager/internal/hostmetrics"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/metricstore"
)

// Interval is how often servers are sampled.
const Interval = 15 * time.Second

// HostGroup is the history group of the host's series.
const HostGroup = "host"

// ServerGroup is the history group of a server's series.
func ServerGroup(name string) string { return "servers/" + name }

// Series names.
const (
	CPU       = "cpu"       // percent: of all cores for the host, of one core for a server
	Memory    = "mem"       // bytes used
	Rx        = "rx"        // network bytes received per second
	Tx        = "tx"        // sent
	DiskRead  = "diskRead"  // bytes per second
	DiskWrite = "diskWrite" //
	Players   = "players"   // players online
	fsPrefix  = "fs:"       // + mount: percent used
)

// FSSeries is the series of a filesystem's used percent.
func FSSeries(mount string) string { return fsPrefix + mount }

// Servers is what the sampler needs from the server manager.
type Servers interface {
	List(ctx context.Context) ([]manager.Server, error)
	StatsOf(ctx context.Context, servers []manager.Server) map[string]docker.Stats
	Inspect(ctx context.Context, id string) (docker.ContainerJSON, error)
}

// PlayerCounter says how many players are on a server, if that's known.
type PlayerCounter interface {
	Players(name string) (int, bool)
}

// ServerSample is a server's latest state, for /metrics.
type ServerSample struct {
	Server   manager.Server
	Stats    *docker.Stats // nil when it isn't running or couldn't be read
	Restarts int
	At       time.Time
}

// Sampler samples servers every Interval, and the host as its collector
// does, into the history.
type Sampler struct {
	store   *metricstore.Store
	servers Servers
	players PlayerCounter
	log     *slog.Logger

	mu     sync.Mutex
	latest map[string]ServerSample // by server name
	prev   map[string]counters     // by container ID
	host   hostmetrics.Snapshot
}

// counters are a container's totals at a sample, to make rates from.
type counters struct {
	at                  time.Time
	rx, tx, read, write uint64
}

// New returns a sampler writing to store.
func New(store *metricstore.Store, servers Servers, players PlayerCounter, log *slog.Logger) *Sampler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sampler{store: store, servers: servers, players: players, log: log,
		latest: map[string]ServerSample{}, prev: map[string]counters{}}
}

// ObserveHost records a host sample; pass it to hostmetrics' Observe.
func (s *Sampler) ObserveHost(snap hostmetrics.Snapshot) {
	s.mu.Lock()
	s.host = snap
	s.mu.Unlock()
	t := snap.SampledAt
	add := func(name string, v float64) { s.store.Add(HostGroup, name, t, v) }
	add(CPU, snap.CPU.Percent)
	add(Memory, float64(snap.Memory.Used))
	var rx, tx, read, write float64
	for _, i := range snap.Network {
		rx += i.RxRate
		tx += i.TxRate
	}
	for _, d := range snap.DiskIO {
		read += d.ReadRate
		write += d.WriteRate
	}
	add(Rx, rx)
	add(Tx, tx)
	add(DiskRead, read)
	add(DiskWrite, write)
	for _, fs := range snap.Disks {
		if fs.Total > 0 {
			add(FSSeries(fs.Mount), float64(fs.Used)/float64(fs.Total)*100)
		}
	}
}

// Host is the latest host sample, if any.
func (s *Sampler) Host() hostmetrics.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host
}

// Latest returns each server's latest sample, by name.
func (s *Sampler) Latest() map[string]ServerSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ServerSample, len(s.latest))
	for k, v := range s.latest {
		out[k] = v
	}
	return out
}

// Run samples servers until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		s.Sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sample takes one sample of every server.
func (s *Sampler) Sample(ctx context.Context) {
	list, err := s.servers.List(ctx)
	if err != nil {
		s.log.Debug("metrics: list servers", "err", err)
		return
	}
	stats := s.servers.StatsOf(ctx, list)
	now := time.Now()
	latest := make(map[string]ServerSample, len(list))
	seen := map[string]bool{}
	for _, srv := range list {
		sample := ServerSample{Server: srv, At: now}
		if info, err := s.servers.Inspect(ctx, srv.ID); err == nil {
			sample.Restarts = info.RestartCount
		}
		g := ServerGroup(srv.Name)
		if s.players != nil {
			if n, ok := s.players.Players(srv.Name); ok {
				s.store.Add(g, Players, now, float64(n))
			}
		}
		st, ok := stats[srv.ID]
		if !ok {
			latest[srv.Name] = sample
			continue
		}
		sample.Stats = &st
		latest[srv.Name] = sample
		seen[srv.ID] = true
		s.store.Add(g, CPU, now, st.CPUPercent)
		s.store.Add(g, Memory, now, float64(st.MemoryUsage))

		s.mu.Lock()
		prev, had := s.prev[srv.ID]
		s.prev[srv.ID] = counters{at: now, rx: st.NetworkRx, tx: st.NetworkTx, read: st.BlockRead, write: st.BlockWrite}
		s.mu.Unlock()
		if secs := now.Sub(prev.at).Seconds(); had && secs > 0 && secs < 10*Interval.Seconds() {
			for _, c := range []struct {
				name      string
				cur, prev uint64
			}{
				{Rx, st.NetworkRx, prev.rx}, {Tx, st.NetworkTx, prev.tx},
				{DiskRead, st.BlockRead, prev.read}, {DiskWrite, st.BlockWrite, prev.write},
			} {
				if c.cur >= c.prev { // a smaller total means the container restarted
					s.store.Add(g, c.name, now, float64(c.cur-c.prev)/secs)
				}
			}
		}
	}
	s.mu.Lock()
	s.latest = latest
	for id := range s.prev {
		if !seen[id] {
			delete(s.prev, id)
		}
	}
	s.mu.Unlock()
}

// ServerOf is the server name in a history group, if it's a server's.
func ServerOf(group string) (string, bool) {
	return strings.CutPrefix(group, "servers/")
}
