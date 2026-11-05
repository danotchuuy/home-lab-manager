# 09 — Health checks and crash-loop protection

**Priority:** Medium · **Depends on:** Docker events from [01](01-notifications.md); alerts via 01; game query from
[05](05-game-query.md) (optional)

> **Status: done** (branch `feature/09-health-crash-loop`). See the README's
> [Health checks and crash loops](../README.md#health-checks-and-crash-loops). Where the result differs from this plan:
>
> - **Code layout:** counting exits and stopping crash loops is in `internal/manager/health.go` (it needs the event
>   stream); the per-server settings and the unhealthy-restart loop are a new package, `internal/health`, which the
>   manager uses through a small `HealthPolicy` interface.
> - **What counts as an exit:** any `die` nobody asked for, including exit code 0, since Docker restarts those too.
>   Exits during jobs are ignored as planned; backup pauses already go through `docker stop`, so they never counted.
> - **Persisted state:** servers stopped for a crash loop are kept in `crash-loops.json` in the data directory (runtime
>   state, so not in configuration backups); settings are in the secrets store under `"health"` as planned. A crash
>   loop is also forgotten when the server is started outside hlm (e.g. `docker start`, or Docker after a reboot).
> - **Log excerpt:** one excerpt for every target: the last 50 lines, cut to their last 3,000 characters, which fits
>   Discord's 4,000-character embed, rather than a shorter one just for Discord.
> - **Health signal:** `signal` chooses Docker's health check or the game query (`query`); the query option is offered
>   only when the server's query is chosen or was found.
> - **Added:** `POST /api/servers/{id}/health/test` (manager) runs a check once in the running server and returns its
>   exit code and output, so you can tell whether the image has `nc`/`bash` or `curl`/`wget`. TCP and HTTP checks pick
>   their tool up front, so a closed port reports exit 1 rather than "command not found".
> - **Start period:** can be set to 0 (none); it defaults to 10 minutes.
> - **Unhealthy restarts:** the monitor checks every 30 seconds, so a restart comes up to 30 seconds after the limit.
>   Restarts wait while a job or backup runs. Each is audited and sends `server.unhealthy`.

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

- [x] Health state in `manager.Server`
- [x] Health flags in extra params
- [x] Crash-loop window + stop + persisted state
- [x] Custom health config in settings → applied on recreate
- [x] Unhealthy auto-restart option
- [x] Notifications with log excerpt
- [x] UI badges, settings section
- [x] README section
