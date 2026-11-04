package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/danotchuuy/home-lab-manager/internal/auth"
	"github.com/danotchuuy/home-lab-manager/internal/gamequery"
	"github.com/danotchuuy/home-lab-manager/internal/manager"
	"github.com/danotchuuy/home-lab-manager/internal/metrics"
	"github.com/danotchuuy/home-lab-manager/internal/metricstore"
	"github.com/danotchuuy/home-lab-manager/internal/rbac"
)

// historyRange reads ?range=, defaulting to a day.
func historyRange(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := cmp.Or(r.URL.Query().Get("range"), "24h")
	if _, ok := metricstore.Ranges[name]; !ok {
		writeError(w, http.StatusBadRequest, errors.New("range is one of 1h, 6h, 24h, 7d or 30d"))
		return "", false
	}
	return name, true
}

func (s *server) hostHistory(w http.ResponseWriter, r *http.Request) {
	rng, ok := historyRange(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.History.Query(metrics.HostGroup, metricstore.Ranges[rng]))
}

func (s *server) serverHistory(w http.ResponseWriter, r *http.Request) {
	rng, ok := historyRange(w, r)
	if !ok {
		return
	}
	info, err := s.Manager.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	name := strings.TrimPrefix(info.Name, "/")
	writeJSON(w, http.StatusOK, s.History.Query(metrics.ServerGroup(name), metricstore.Ranges[rng]))
}

func (s *server) metricsSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.MetricsConfig.Get())
}

func (s *server) setMetricsSettings(w http.ResponseWriter, r *http.Request) {
	var in metrics.Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.MetricsConfig.Set(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mayScrape reports whether a request may read /metrics: from an address
// allowed without a key, or with a metrics or admin key (or anyone, when
// hlm doesn't require signing in).
func (s *server) mayScrape(r *http.Request) bool {
	if auth.AllowedFrom(s.MetricsConfig.Get().AllowFrom, r.RemoteAddr) {
		return true
	}
	p, _, ok := s.identify(r)
	return ok && (p.Admin() || p.Role == rbac.Metrics)
}

// prometheus serves GET /metrics in Prometheus' text format.
func (s *server) prometheus(w http.ResponseWriter, r *http.Request) {
	if !s.mayScrape(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hlm"`)
		writeError(w, http.StatusUnauthorized, errors.New("send an API key with the metrics role as a bearer token"))
		return
	}
	var e metrics.Exposition
	e.Gauge("hlm_build_info", "hlm's version.", 1, "version", s.Version)

	// Servers, as last sampled.
	var latest map[string]metrics.ServerSample
	if s.Sampler != nil {
		latest = s.Sampler.Latest()
	}
	if len(latest) == 0 && s.Manager != nil {
		if list, err := s.Manager.List(r.Context()); err == nil {
			latest = map[string]metrics.ServerSample{}
			for _, srv := range list {
				latest[srv.Name] = metrics.ServerSample{Server: srv}
			}
		}
	}
	var query map[string]gamequery.Summary
	if s.Query != nil {
		query = s.Query.Summaries(func(string) bool { return true })
	}
	names := make([]string, 0, len(latest))
	for n := range latest {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, name := range names {
		smp := latest[name]
		l := []string{"server", name, "app", smp.Server.App, "image", smp.Server.Image}
		e.Gauge("hlm_server_up", "1 if the server's container is running.", b2f(smp.Server.State == "running"), l...)
		update := 0.0
		if smp.Server.Update != nil && smp.Server.Update.State == manager.UpdateAvailable {
			update = 1
		}
		e.Gauge("hlm_server_update_available", "1 if a newer image is available.", update, l...)
		e.Counter("hlm_server_restarts_total", "Times Docker restarted the container under its restart policy since it was started.", float64(smp.Restarts), l...)
		if st := smp.Stats; st != nil {
			e.Gauge("hlm_server_cpu_ratio", "CPU used, in cores (1 = one full core).", st.CPUPercent/100, l...)
			e.Gauge("hlm_server_memory_bytes", "Memory used.", float64(st.MemoryUsage), l...)
			e.Gauge("hlm_server_memory_limit_bytes", "Memory the container may use.", float64(st.MemoryLimit), l...)
			e.Counter("hlm_server_network_receive_bytes_total", "Network bytes received since the container started.", float64(st.NetworkRx), l...)
			e.Counter("hlm_server_network_transmit_bytes_total", "Network bytes sent since the container started.", float64(st.NetworkTx), l...)
			e.Counter("hlm_server_block_read_bytes_total", "Bytes read from disk since the container started.", float64(st.BlockRead), l...)
			e.Counter("hlm_server_block_write_bytes_total", "Bytes written to disk since the container started.", float64(st.BlockWrite), l...)
		}
		if q, ok := query[name]; ok {
			e.Gauge("hlm_server_query_up", "1 if the game answers queries.", b2f(q.Status == gamequery.StatusOnline), l...)
			if q.HasPlayers {
				e.Gauge("hlm_server_players", "Players online.", float64(q.Players), l...)
				if q.MaxPlayers > 0 {
					e.Gauge("hlm_server_players_max", "Most players the server takes.", float64(q.MaxPlayers), l...)
				}
			}
		}
	}

	// Backups.
	if s.Backup != nil {
		if plans, err := s.Backup.Plans(); err == nil {
			for _, name := range slices.Sorted(maps.Keys(plans)) {
				ok := plans[name].LastOK
				if ok == nil && plans[name].Last != nil && plans[name].Last.Error == "" {
					ok = plans[name].Last // from before LastOK was kept
				}
				if ok == nil {
					continue
				}
				e.Gauge("hlm_backup_last_success_timestamp_seconds", "When the server's last successful backup finished.", float64(ok.Finished.Unix()), "server", name)
				e.Gauge("hlm_backup_last_size_bytes", "Size of the last successful backup, compressed.", float64(ok.Size), "server", name)
				e.Gauge("hlm_backup_last_duration_seconds", "How long the last successful backup took.", ok.Finished.Sub(ok.Started).Seconds(), "server", name)
			}
		}
	}

	// The host.
	if s.Host != nil {
		if h := s.Host.Snapshot(); h.Available {
			e.Gauge("hlm_host_cpu_ratio", "Host CPU used, as a fraction of all cores.", h.CPU.Percent/100)
			e.Gauge("hlm_host_cpu_cores", "Host CPU cores.", float64(h.CPU.Cores))
			e.Gauge("hlm_host_load1", "Host load average over 1 minute.", h.CPU.Load1)
			e.Gauge("hlm_host_memory_used_bytes", "Host memory used.", float64(h.Memory.Used))
			e.Gauge("hlm_host_memory_total_bytes", "Host memory.", float64(h.Memory.Total))
			for _, fs := range h.Disks {
				e.Gauge("hlm_host_filesystem_size_bytes", "Filesystem size.", float64(fs.Total), "mount", fs.Mount, "device", fs.Device)
				e.Gauge("hlm_host_filesystem_avail_bytes", "Filesystem space available.", float64(fs.Free), "mount", fs.Mount, "device", fs.Device)
			}
			for _, i := range h.Network {
				e.Counter("hlm_host_network_receive_bytes_total", "Bytes received on a host interface.", float64(i.RxTotal), "interface", i.Name)
				e.Counter("hlm_host_network_transmit_bytes_total", "Bytes sent on a host interface.", float64(i.TxTotal), "interface", i.Name)
			}
		}
	}
	if s.HTTPS != nil {
		if st := s.HTTPS.Status(); !st.NotAfter.IsZero() {
			e.Gauge("hlm_https_cert_expiry_timestamp_seconds", "When hlm's HTTPS certificate expires.", float64(st.NotAfter.Unix()))
		}
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	e.WriteTo(w)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
