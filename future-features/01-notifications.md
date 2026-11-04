# 01 — Notifications and alerts

**Priority:** High · **Depends on:** nothing · **Unblocks:** 04, 09, 11

## Why

hlm does a lot unattended: scheduled backups, update checks, certificate renewals, WAN-address checks, tunnel
health. When any of it fails, the only trace is a status line on a page nobody is looking at, or a log line. A
crashed game server is found when a friend says they can't connect.

## What it does

Admins add **notification targets** and choose which **events** go to each. hlm sends a short message when an event
happens, with a link back to the relevant page.

### Targets (first version)

| Kind | Config | Notes |
| --- | --- | --- |
| Discord webhook | URL | Embeds with colour by severity |
| ntfy | server URL, topic, optional token | Priority mapped from severity |
| Gotify | server URL, app token | |
| Generic webhook | URL, optional headers, method | JSON body (schema below); lets people wire up anything |
| Email (SMTP) | host, port, TLS mode, user, password, from, to | Plain text + HTML |

Later: Slack, Telegram, Pushover, Matrix. Each is a small `Sender` implementation, so they're cheap to add.

### Events

| Event | Severity | Source |
| --- | --- | --- |
| `server.crashed` — container exited non-zero (and not stopped by hlm or a user) | error | Docker events |
| `server.restart_loop` — see [09](09-health-crash-loop.md) | error | Docker events |
| `server.unhealthy` — Docker healthcheck went unhealthy | warning | Docker events |
| `server.update_available` | info | `manager.CheckUpdates` |
| `server.updated` / `server.update_failed` | info / error | update jobs |
| `backup.failed` (server or config) | error | `backup.Service` |
| `backup.succeeded` (off by default) | info | `backup.Service` |
| `restore.finished` / `restore.failed` | info / error | `backup.Service` |
| `hlm.update_available` | info | `selfupdate` |
| `https.renew_failed`, `https.expiring` (< 14 days) | error / warning | `tlscert` |
| `tunnel.down` (cloudflared or WireGuard container not running / check failing) | error | `cftunnel`, `proxy` |
| `wan.changed` — home address changed and the proxy was re-applied | info | `proxy/wan.go` |
| `disk.low` — a filesystem over a threshold (default 90%) | warning | `hostmetrics` |
| `auth.locked_out` — repeated failed sign-ins for an account | warning | `auth` |

Each target has: enabled events (default: all `error` and `warning`), optional server filter (all servers / chosen
servers / server groups), and quiet hours (hold non-errors until the window ends).

## Design

### New package `internal/notify`

```go
type Severity string // info, warning, error

type Event struct {
    Kind     string            // "backup.failed"
    Severity Severity
    Title    string            // "Backup of valheim failed"
    Message  string            // the error / details, already free of secrets
    Server   string            // optional, for filtering and links
    Link     string            // path in the UI, made absolute with the public URL
    Fields   map[string]string // extra key/values for webhooks
    Time     time.Time
}

type Sender interface {
    Kind() string
    Send(ctx context.Context, t Target, e Event) error
}

type Service struct { /* store, senders, queue, dedupe state, log */ }
func (s *Service) Notify(e Event)         // non-blocking: puts on a queue
func (s *Service) Test(ctx, Target) error // sends a test event
func (s *Service) Run(ctx context.Context) // drains the queue
```

- `Notify` never blocks or fails the caller. A buffered channel feeds a worker; when full, events are dropped and
  counted (logged once).
- **Retries:** 3 attempts with backoff per target; the last result per target is kept for the UI.
- **Dedupe / rate limit:** the same `(Kind, Server)` isn't sent again within 15 minutes (configurable), except when
  the state changes (e.g. `server.crashed` then `server.recovered`). This stops a flapping server spamming Discord.
- **Secrets:** targets are stored in the secrets store under `"notify"`. Webhook URLs (Discord's contains a token),
  tokens and SMTP passwords are write-only in the API, like other integrations' credentials.
- **Public URL** for links: use the HTTPS hostname if set (`tlscert`), else a new optional setting "hlm's address".

### Wiring the sources

Add an optional `Notifier` interface (`Notify(notify.Event)`) to the packages that produce events, set from
`cmd/hlm/main.go`, so packages don't import each other:

- `backup.Service`: at the end of `run`, `runConfig` and `restore`.
- `manager`: update check results (only *newly* available updates), update jobs finishing.
- `selfupdate`: new release found.
- `tlscert`: renew failure; a daily expiry check.
- `hostmetrics.Collector`: threshold check in `sample()` with hysteresis (alert at 90%, clear at 85%).
- **Docker events:** add `Events(ctx, filters)` to `internal/docker` (streaming `GET /events`, filtered to
  `type=container` and label `io.hlm.*`). A watcher in `manager` maps `die` with non-zero `exitCode`, `health_status`
  and `restart` to events. To tell a crash from a deliberate stop, the manager records "expected stops" (Stop, Restart,
  Update, backup pause) in a short-lived set keyed by container ID; a `die` without one is a crash.

### Webhook JSON

```json
{"kind":"backup.failed","severity":"error","title":"Backup of valheim failed",
 "message":"upload: connection refused","server":"valheim",
 "link":"https://hlm.example.com/servers/valheim?tab=backups","time":"2026-11-04T12:00:00Z","fields":{}}
```

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/notifications/targets` | admin | List (secrets masked), with last delivery result |
| `POST /api/notifications/targets` | confirmed | Add |
| `PUT /api/notifications/targets/{id}` | confirmed | Edit (blank secret = keep) |
| `DELETE /api/notifications/targets/{id}` | admin | Remove |
| `POST /api/notifications/targets/{id}/test` | admin | Send a test message |
| `GET /api/notifications/events` | admin | Event kinds and descriptions, for the UI |
| `GET /api/notifications/history` | admin | Last 200 events and where they went |

`confirmed` because a webhook URL receives server names and errors; adding one is close to granting read access.

## UI

- **System → Notifications**: list of targets with kind icon, status of last delivery, **Test** and **Edit**. The edit
  form has the kind-specific fields, an event checklist grouped by area, server filter, and quiet hours.
- A small "recent alerts" list under it (the history).
- Server page: a bell on the **Settings** tab showing which targets cover this server (read-only, links to System).

## Permissions

Admin only for now. Later: let members subscribe their own target to servers they can see (filtered by their access
at send time).

## Edge cases

- Target unreachable: don't block anything, record the failure, show it on the System page.
- hlm restarting: in-flight events are lost; acceptable. Don't send "crashed" for containers that died while hlm was
  down (only events seen live).
- hlm's own update restarts its container; don't alert on hlm's own container.
- Messages must not include secrets: errors from `deploy`/`s3` can echo URLs with credentials — run messages through
  a redactor (`user:pass@` in URLs, query tokens).

## Testing

- Unit: each sender against an `httptest.Server` (body shape, headers, retries); dedupe window; quiet hours; server
  filter including groups.
- SMTP: a tiny in-process SMTP server in the test.
- Docker events: fake event stream in `docker_test.go`; manager test that a `die` after `Stop` is not a crash, and one
  without is.
- Opt-in Docker test: run busybox `sh -c 'exit 1'`, expect `server.crashed`.

## Tasks

- [ ] `internal/notify`: types, store in secrets, queue worker, retries, dedupe, history ring
- [ ] Senders: Discord, ntfy, Gotify, webhook, SMTP
- [ ] `docker.Client.Events` streaming + tests
- [ ] Manager event watcher with expected-stop tracking
- [ ] Hook up backup, update check, update jobs, selfupdate, tlscert, hostmetrics disk threshold, tunnel checks
- [ ] Secret redaction helper for messages
- [ ] API routes + handlers
- [ ] System → Notifications UI, test button, history
- [ ] Add "hlm's address" setting if no HTTPS hostname
- [ ] README section and API table

## Open questions

- Should members get their own targets in v1, or admin-only?
- Do we want a digest mode (one daily summary of info events)?
