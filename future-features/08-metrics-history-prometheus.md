# 08 — Metrics history and Prometheus export

**Priority:** Medium · **Depends on:** nothing · **Used by:** 05 (player history), 17

> **Status: done** (branch `feature/08-metrics-history`). See the README's
> [Resource history](../README.md#resource-history) and [Prometheus](../README.md#prometheus). Where the result differs
> from this plan:
>
> - **Storage:** rather than rewriting every series' file each minute (around 10 MB a minute at 20 servers), finished
>   minutes are appended to `metrics/minutes.jsonl` and the whole store is written to `metrics/snapshot.gob` every 6
>   hours and on shutdown. Buckets keep sum, count and peak, so 15-minute averages are exact.
> - **Retention of deleted servers:** a group is dropped 30 days after its last sample, which covers deleted servers
>   without a hook.
> - **Ranges:** 1 h, 6 h, 24 h, 7 d and 30 d; the Host page keeps its live 5-second view as "Live". CPU charts also draw
>   each step's peak.
> - **Auth:** a `metrics` API key role (an API key can only have one role) rather than a scope; it's refused on every
>   `/api` route. Unauthenticated scraping from listed addresses is a System → Prometheus setting (confirmed admins).
> - **Metrics:** `hlm_server_restarts_total` is Docker's restart count for the current container (it starts again
>   when the server is recreated). Host metrics add CPU cores, load and per-interface network totals. Backups now keep
>   the last *successful* run (`lastOk`), so a failure doesn't hide when the last good backup was.

## Why

The Host page keeps 30 minutes of history in memory (`hostmetrics.Collector`, 5 s samples), and per-server stats are
live only. "Why was it laggy last night?" can't be answered. People with Grafana want to scrape hlm.

## Part A — Longer history in hlm

### Retention tiers

| Resolution | Kept for | Points per series |
| --- | --- | --- |
| 5 s (existing, in memory) | 30 min | 360 |
| 1 min (avg + max) | 24 h | 1,440 |
| 15 min (avg + max) | 30 days | 2,880 |

### Series

- Host: CPU %, memory used, network rx/tx rate, disk read/write rate, each filesystem's used %.
- Per server: CPU %, memory, network rx/tx rate, disk I/O rate, players online (05).

At ~20 servers that's ~150 series × 4,320 points × 16 B ≈ 10 MB on disk. Fine.

### Storage

- `internal/metricstore`: fixed-size ring buffers per (series, tier), persisted to `HLM_DATA_DIR/metrics/*.bin` every
  minute and on shutdown (simple binary format: header + float32 pairs + start time). No external database.
- Downsampling done in memory as 5 s samples arrive (running avg/max per minute bucket, then per 15 min).
- Per-server series are keyed by server name, so history survives updates. Deleted servers' files are removed after
  30 days.
- Per-server stats currently come from `manager.AllStats` on demand; add a background sampler (every 15 s) that feeds
  the store. It's the same Docker stats call the list page already makes.

### API and UI

- `GET /api/host/history?range=1h|24h|7d|30d` (admin) and `GET /api/servers/{id}/history?range=…` (Viewer).
- Range picker on the Host page charts and the server page resource charts (`components/charts.tsx`).

## Part B — Prometheus endpoint

`GET /metrics` in Prometheus text format.

- **Auth:** separate from the UI session: an API key with a new `metrics` scope (read-only), sent as bearer token
  (Prometheus `authorization` config). Optionally allow unauthenticated scraping from listed addresses (reuse
  `auth/allowfrom.go`). Off by default.
- Metrics (labels: `server`, `app`, `image`):
  - `hlm_server_up`, `hlm_server_cpu_ratio`, `hlm_server_memory_bytes`, `hlm_server_memory_limit_bytes`,
    `hlm_server_network_{receive,transmit}_bytes_total`, `hlm_server_block_{read,write}_bytes_total`,
    `hlm_server_restarts_total`, `hlm_server_update_available`, `hlm_server_players`, `hlm_server_players_max`,
    `hlm_server_query_up`
  - `hlm_backup_last_success_timestamp_seconds{server}`, `hlm_backup_last_size_bytes`, `hlm_backup_last_duration_seconds`
  - `hlm_host_*` (cpu, memory, filesystem size/avail) when host metrics are available
  - `hlm_build_info{version}`, `hlm_https_cert_expiry_timestamp_seconds`
- Hand-written exposition (simple text format); no `client_golang` dependency needed.
- README: example `prometheus.yml` scrape config and a starter Grafana dashboard JSON in `docs/`.

## Testing

- Ring buffer wrap/downsample/persist/reload; corrupted file is discarded, not fatal.
- `/metrics` output parses (check with a small parser in test) and respects auth.

## Tasks

- [ ] `internal/metricstore` rings, downsampling, persistence
- [ ] Feed from `hostmetrics` and a new per-server sampler
- [ ] History endpoints + range picker in charts
- [ ] `metrics` API key scope + `/metrics` handler
- [ ] Grafana dashboard example
- [ ] README sections
