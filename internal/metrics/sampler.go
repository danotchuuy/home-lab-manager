// Package metrics samples servers' resource use into the history
// (internal/metricstore), and writes hlm's state in Prometheus' text format.
package metrics

import (
	"context"
	"log/slog"
	"math"
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

// HostGroupOf is the history group of a host's series: HostGroup for
// this one (manager.LocalHost, or ""), hosts/<id> for another.
func HostGroupOf(id string) string {
	if id == "" || id == manager.LocalHost {
		return HostGroup
	}
	return "hosts/" + id
}

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
	// Up is 1 while a server is up (running, and answering if its game can
	// be asked), else 0: its average over a range is the uptime.
	Up       = "up"
	fsPrefix = "fs:" // + mount: percent used
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
	// others are other hosts' latest samples (from their agents), by ID.
	others map[string]hostmetrics.Snapshot
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
		latest: map[string]ServerSample{}, prev: map[string]counters{}, others: map[string]hostmetrics.Snapshot{}}
}

// ObserveHost records a host sample; pass it to hostmetrics' Observe.
func (s *Sampler) ObserveHost(snap hostmetrics.Snapshot) {
	s.mu.Lock()
	s.host = snap
	s.mu.Unlock()
	s.record(HostGroup, snap)
}

// ObserveHostOn records a sample of another host (from its agent). A
// sample that isn't available forgets the host's latest one.
func (s *Sampler) ObserveHostOn(id string, snap hostmetrics.Snapshot) {
	if id == "" || id == manager.LocalHost {
		s.ObserveHost(snap)
		return
	}
	snap.History = nil
	s.mu.Lock()
	if !snap.Available {
		delete(s.others, id)
		s.mu.Unlock()
		return
	}
	s.others[id] = snap
	s.mu.Unlock()
	s.record(HostGroupOf(id), snap)
}

// OtherHosts are the other hosts' latest samples, by ID.
func (s *Sampler) OtherHosts() map[string]hostmetrics.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]hostmetrics.Snapshot, len(s.others))
	for id, snap := range s.others {
		out[id] = snap
	}
	return out
}

func (s *Sampler) record(group string, snap hostmetrics.Snapshot) {
	t := snap.SampledAt
	add := func(name string, v float64) { s.store.Add(group, name, t, v) }
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
		s.store.Add(g, Up, now, s.up(srv))
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

// up is a server's Up sample: running, and not known to be failing to answer.
func (s *Sampler) up(srv manager.Server) float64 {
	if srv.State != "running" {
		return 0
	}
	if r, ok := s.players.(interface{ Responding(string) (bool, bool) }); ok {
		if responding, known := r.Responding(srv.Name); known && !responding {
			return 0
		}
	}
	return 1
}

// Uptime is the share of rng a server was up (0–1), from its Up series;
// false without enough history (an hour).
func Uptime(store *metricstore.Store, server string, rng time.Duration) (float64, bool) {
	h := store.Query(ServerGroup(server), rng)
	v, ok := h.Series[Up]
	if !ok {
		return 0, false
	}
	var sum float64
	n := 0
	for _, a := range v.Avg {
		if f := float64(a); !math.IsNaN(f) {
			sum += f
			n++
		}
	}
	if n == 0 || time.Duration(int64(n)*h.Step)*time.Second < time.Hour {
		return 0, false
	}
	return sum / float64(n), true
}
