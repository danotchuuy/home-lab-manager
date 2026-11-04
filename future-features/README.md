# Future features

Plans for features hlm doesn't have yet. Each file is one feature: why it's worth doing, how it fits the existing code,
the API and UI it needs, who may use it, how to test it, and a task list. They're numbered by suggested priority.

| # | Feature | Priority | Depends on |
| --- | --- | --- | --- |
| 01 | [Notifications and alerts](01-notifications.md) | High | — |
| 02 | [Interactive console (stdin and RCON)](02-interactive-console.md) | High | — |
| 03 | [Scheduled tasks per server](03-scheduled-tasks.md) | High | 02 for in-game commands |
| 04 | [Auto-update policy](04-auto-update-policy.md) | High | 03 (shared scheduler), 05 (player-aware) |
| 05 | [Game query: players and status](05-game-query.md) | High | — |
| 06 | [Audit log](06-audit-log.md) | High | — |
| 07 | [Private registry credentials](07-registry-credentials.md) | Medium | — |
| 08 | [Metrics history and Prometheus](08-metrics-history-prometheus.md) | Medium | — |
| 09 | [Health checks and crash-loop protection](09-health-crash-loop.md) | Medium | 01 for alerts |
| 10 | [Clone, export and import servers](10-clone-export-import.md) | Medium | — |
| 11 | [Backup improvements](11-backup-improvements.md) | Medium | 01 for alerts |
| 12 | [Two-factor sign-in](12-two-factor.md) | Medium | — |
| 13 | [Docker Compose stacks](13-compose-stacks.md) | Low | — |
| 14 | [Multiple Docker hosts](14-multi-host.md) | Low | — |
| 15 | [Image and disk cleanup](15-cleanup.md) | Low | — |
| 16 | [UI improvements](16-ui-improvements.md) | Low | — |
| 17 | [Public status page](17-public-status-page.md) | Low | 05 |

## Conventions every plan follows

These are how the existing code does things, so new features should match:

- **Endpoints** go in the route table in `internal/api/routes.go` with a policy: `public`, `signedIn`, `admin`,
  `confirmed` (admin plus a fresh sign-in, for changes that grant access or hold credentials) or
  `serverRole(rbac.Viewer|Operator|Manager)` for per-server endpoints.
- **Settings and credentials** are kept encrypted in `internal/secrets` (`Store.Get/Set/Delete` under a name such as
  `"unifi"` or `"rbac"`). Anything stored there is included in the [configuration backup](../README.md#backing-up-hlms-own-configuration)
  automatically. Plain, non-secret data files (like `templates.json`) live in `HLM_DATA_DIR` and have to be added to
  `configFiles` in `internal/backup/config.go` to be backed up.
- **Per-server data** is keyed by server (container) name, like backup plans, so it survives updates and
  reconfiguration, which recreate the container. Deleting a server should forget it (see `rbac.Store.ForgetServer`).
- **Background loops** follow the `RunScheduler`/`RunUpdateChecker` pattern: a ticker, started from `run()` in
  `cmd/hlm/main.go`, stopped by the context.
- **Long-running actions** report progress through `manager.Job`/`Event` (NDJSON streams) and refuse to overlap with
  another job on the same server.
- **Secret-looking values** are hidden from operators (`internal/deploy/secrets.go`); new features that show settings or
  output must not leak them to viewers.
- **Tests** use fakes for external services (`cloudflaretest`, `linodetest`, `unifitest`) and Docker-backed tests are
  opt-in. Don't run NET_ADMIN or host-network containers in tests.
- **Docs**: each feature gets a README section and its endpoints in the API table.
