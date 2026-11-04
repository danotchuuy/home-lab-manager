# 06 — Audit log

**Priority:** High · **Depends on:** nothing

> **Status: done** (branch `feature/06-audit-log`). See the README's [Activity (audit log)](../README.md#activity-audit-log).
> Where the result differs from this plan:
>
> - Action names are made from each route's pattern (`PUT /api/servers/{id}/config` → `servers.config.set`), with
>   overrides in one map (`auditNames`) rather than a field on every route; that map also lists the reads that are
>   recorded (backup and configuration downloads, exports) and the routes left to the sign-in service.
> - Streamed operations (install, update, reconfigure, import) answer 200 before they finish; a failure partway through
>   is passed to the entry, so it's recorded as failed.
> - Requests without any credentials are refused before routing and aren't recorded; failed sign-ins are, by the
>   sign-in service, with the username tried.
> - Paging uses the time of the last entry as the cursor.
> - Not done: syslog forwarding (the "also write to hlm's own log" option covers collecting entries elsewhere).

## Why

hlm now has local accounts, SSO users, groups, API keys, deployer keys and SSH keys. When a server is deleted or a
setting changes, there's no record of who did it. For a tool that controls the Docker socket, "who did what" is
basic.

## What it records

Every **change** (not reads), with:

```go
type Entry struct {
    Time     time.Time
    Actor    string // "alice" (local), "sso:bob", "key:ci-deploy", "token", "anonymous", "hlm" (scheduler)
    ActorKind string
    IP       string
    Action   string // "server.stop", "server.config", "backup.restore", "auth.key.create", ...
    Target   string // server name, account, key id, ...
    Result   string // "ok" | "denied" | "error"
    Error    string `json:",omitempty"`
    Details  map[string]any `json:",omitempty"` // e.g. changed field names, never values of secrets
}
```

Also: sign-ins (success, failure, lockout), sign-outs, "confirm it's you", denied requests (403s), and actions taken by
hlm itself (scheduled backups, auto-updates, scheduled tasks) with actor `hlm`.

## Design

### Capture point

All endpoints pass through `authorize(pol, next)` in `internal/api/authz.go`. Wrap there:

- For non-GET methods, record after the handler returns using a status-capturing `ResponseWriter`.
- `Action` comes from the route table: add an optional `audit string` field to `route` (default derived from method +
  pattern, e.g. `POST /api/servers/{id}/{action}` → `server.<action>`). Making the route table say it keeps the
  "every endpoint has to declare" style.
- `Target` from path values (`r.PathValue("id")`, `username`, …).
- `Details`: handlers can add to a per-request audit context (`audit.Add(r, "fields", []string{"ports","env"})`) — e.g.
  reconfigure lists which settings changed, never their values.
- Sign-in events are recorded inside `internal/auth` (they don't go through `authorize` as a signed-in user).

### Storage

- `internal/audit`: append-only JSON Lines at `HLM_DATA_DIR/audit/audit-YYYY-MM.jsonl`, `fsync` every few seconds.
- Retention: keep 12 months (setting), delete older files on start and daily.
- Not in the secrets store (it's append-heavy), but **include in the config backup** (`configFiles`).
- Optional: also write each entry to stdout as a structured log line (`slog` with `audit=true`), so people with Loki/
  journald get it for free.
- Optional forwarding: syslog (RFC 5424 over UDP/TCP/TLS) — v2.

### Reading

Scan files newest-first with filters; at home-lab volumes (thousands of entries a month) a linear scan is fine. Keep
an in-memory index of the current month if needed.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/audit?actor=&action=&target=&result=&since=&until=&limit=` | admin | Entries, newest first, cursor paging |
| `GET /api/audit/export?…` | admin | CSV / JSONL download (via download ticket) |
| `GET /api/servers/{id}/activity` | Manager | Entries for that server only |
| `GET/PUT /api/system/audit` | admin / confirmed | Retention, stdout mirror |

## UI

- **System → Activity**: table with filters (actor, action, server, result, date range), expandable details, export.
- Server page **Activity** section on Settings tab (Manager+): last 50 changes to this server.
- My account: "Recent sign-ins to your account" (from the same log).

## Safety

- Never log request bodies. Details are allow-listed field names.
- Entries can't be edited or deleted through the API (only aged out by retention).
- Console commands (02) are logged with the command text, since that's the point; redact anything matching the secret
  patterns in `deploy/secrets.go`.

## Testing

- Middleware: a POST produces one entry with right actor/action/target/result; GET produces none; denied → `denied`.
- Each actor kind (local, SSO, API key, `HLM_TOKEN`, SSH key token, anonymous when sign-in isn't required).
- Retention deletes old files; reader filters and paging across month files.

## Tasks

- [ ] `internal/audit` writer, reader, retention
- [ ] `audit` field on routes + middleware in `authorize`
- [ ] Per-request details helper; use in reconfigure, ports, backups plan, auth config
- [ ] Sign-in / lockout events in `internal/auth`
- [ ] System actor for schedulers
- [ ] API + export
- [ ] System → Activity UI, server activity, my-account sign-ins
- [ ] Add audit files to config backup
- [ ] README section
