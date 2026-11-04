# 09 — Health checks and crash-loop protection

**Priority:** Medium · **Depends on:** Docker events from [01](01-notifications.md); alerts via 01; game query from
[05](05-game-query.md) (optional)

## Why

With **Start on boot** the restart policy is `always`, so a server with a bad config or a corrupt save restarts forever:
burning CPU, filling logs, possibly corrupting more data, and showing as "running" most of the time.

## What it does

### Crash-loop protection

- hlm counts unexpected exits per server (Docker `die` events not caused by hlm/a user — see 01's expected-stop
  tracking).
- If a server exits **N times within M minutes** (default 5 in 10), hlm **stops it**, marks it **Crash loop**, and
  notifies (`server.restart_loop`) with the last 50 log lines attached (for webhooks/email; Discord gets a short
  excerpt).
- The server card shows a red "Crash loop — stopped by hlm" badge with **View logs** and **Start anyway**.
- Per-server override: thresholds, or off.
- Stopping uses `manager.Stop` (keeps the restart policy as is; Docker doesn't restart a stopped container). Starting
  again clears the state.

### Health status

- Show Docker's own healthcheck state (`State.Health.Status`: starting / healthy / unhealthy) on cards when the image
  defines one. Add it to `manager.Server` from inspect data.
- **Custom health check** on the Settings tab for images without one (written as `HealthConfig` on the container at
  the next recreate):
  - TCP port open, HTTP GET path returns 2xx/3xx, or a command (`CMD-SHELL`).
  - Interval, timeout, retries, start period (game servers need long start periods: default 10 min).
  - Add `--health-cmd`, `--health-interval`, `--health-retries`, `--health-start-period` and `--health-timeout` to
    `deploy/extraparams.go`, so templates' `ExtraParams` work too. `--no-healthcheck` is currently ignored there;
    make it set `Healthcheck.Test = ["NONE"]`.
- Optional action on **unhealthy for X minutes**: restart the server (off by default; when on, it counts toward the
  crash-loop limit).
- If the game query (05) is set up, "query not responding" can be the health signal instead of a Docker healthcheck.

## Design

- `internal/manager/health.go`: per-server sliding window of exit times; state in memory plus a small persisted
  "stopped for crash loop" set (so the badge survives an hlm restart). Settings per server name in the secrets store
  (`"health"`).
- Ignore exits during an hlm job (install/update/reconfigure) and during backup pauses.
- Docker's restart policy backs off exponentially itself; our count uses real `die` events so it's independent of that.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers` (existing) | — | add `health`, `crashLoop` |
| `GET/PUT /api/servers/{id}/health` | Viewer / Manager | Thresholds, custom check, auto-restart |
| `POST /api/servers/{id}/start` (existing) | Operator | Clears crash-loop state |

Custom health check *commands* run inside the container: Manager can already edit files and run console commands, so
Manager is right; but `CMD-SHELL` changes container config → treat like Docker options? It only affects that container,
so Manager is acceptable. Document it.

## Testing

- Window logic with fake clock and synthetic events: 5 exits in 10 min → stop + notify once; expected stops ignored;
  start clears.
- Extra params parsing for health flags.
- Opt-in Docker test: busybox `exit 1` with restart always → stopped after N.

## Tasks

- [ ] Health state in `manager.Server`
- [ ] Health flags in extra params
- [ ] Crash-loop window + stop + persisted state
- [ ] Custom health config in settings → applied on recreate
- [ ] Unhealthy auto-restart option
- [ ] Notifications with log excerpt
- [ ] UI badges, settings section
- [ ] README section
