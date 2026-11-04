# Future features

Plans for features hlm doesn't have yet. Each file is one feature: why it's worth doing, how it fits the existing code,
the API and UI it needs, who may use it, how to test it, and a task list. They're numbered by suggested priority.

| # | Feature | Priority | Depends on | Status |
| --- | --- | --- | --- | --- |
| 01 | [Notifications and alerts](01-notifications.md) | High | — | Done |
| 02 | [Interactive console (stdin and RCON)](02-interactive-console.md) | High | — | Done |
| 03 | [Scheduled tasks per server](03-scheduled-tasks.md) | High | 02 for in-game commands | Done |
| 04 | [Auto-update policy](04-auto-update-policy.md) | High | 03 (shared scheduler), 05 (player-aware) | Done |
| 05 | [Game query: players and status](05-game-query.md) | High | — | Done |
| 06 | [Audit log](06-audit-log.md) | High | — | Done |
| 07 | [Private registry credentials](07-registry-credentials.md) | Medium | — | Done |
| 08 | [Metrics history and Prometheus](08-metrics-history-prometheus.md) | Medium | — | Planned |
| 09 | [Health checks and crash-loop protection](09-health-crash-loop.md) | Medium | 01 for alerts | Planned |
| 10 | [Clone, export and import servers](10-clone-export-import.md) | Medium | — | Planned |
| 11 | [Backup improvements](11-backup-improvements.md) | Medium | 01 for alerts | Planned |
| 12 | [Two-factor sign-in](12-two-factor.md) | Medium | — | Planned |
| 13 | [Docker Compose stacks](13-compose-stacks.md) | Low | — | Planned |
| 14 | [Multiple Docker hosts](14-multi-host.md) | Low | — | Planned |
| 15 | [Image and disk cleanup](15-cleanup.md) | Low | — | Planned |
| 16 | [UI improvements](16-ui-improvements.md) | Low | — | Planned |
| 17 | [Public status page](17-public-status-page.md) | Low | 05 | Planned |
| 18 | [Settings files: pinned files and a form editor](18-settings-files.md) | Requested | — | Done |

## Workflow: one feature at a time

Each feature is built on its own branch, committed, and merged into `main` through a pull request before work on the
next one starts:

1. Start from an up-to-date `main` and create a branch for the feature, named after its file, e.g.
   `feature/01-notifications`.
2. Implement the plan on that branch, with tests and the README/API docs, and commit the work there.
3. Push the branch and open a pull request into `main` with full details (see below).
4. Merge the pull request once tests pass, and tick the feature off (or remove its plan) in the same pull request. Don't
   merge feature branches into `main` locally without a pull request.
5. Only then start the next feature, from the updated `main`.

A feature's pull request description includes:

- **Summary:** what the feature does and a link to its plan file.
- **Changes:** the packages, endpoints (with their access policy), settings, stored data and UI pages added or changed.
- **Deviations from the plan:** anything done differently or left out, and why; follow-ups that remain.
- **Security and permissions:** who can use the feature, what it can reach, and how secrets are kept out of responses,
  logs and notifications.
- **Migrations and compatibility:** changes to stored data or settings, and how existing installs carry on.
- **Testing:** the tests added, the commands run and their results (including opt-in Docker tests, if run), and any
  manual checks, with screenshots for UI changes.
- **Docs:** the README sections and API table entries updated.

Don't mix two features on one branch or start a feature while another is unmerged. If a plan turns out to need part of
another feature, build that part first as its own branch.

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
