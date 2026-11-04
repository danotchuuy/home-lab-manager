# 14 — Multiple Docker hosts

**Priority:** Low (large) · **Depends on:** nothing, but touches almost everything

## Why

hlm manages one Docker daemon (local socket or one remote host over TLS via `DOCKER_HOST`). Home labs often have two or
three machines: a NAS, a mini-PC, a GPU box. Today that means running a separate hlm on each, with separate accounts,
backups and tunnels.

## Options

| Approach | How | Pros | Cons |
| --- | --- | --- | --- |
| **A. Direct Docker API per host** | hlm holds several `docker.Client`s (unix, TLS, or SSH-tunnelled) | No agent; reuses the client | Host metrics, file access from host, busybox helpers need per-host thinking; exposing Docker over TCP is risky |
| **B. hlm agent** | A small `hlm agent` mode on each extra host connects *out* to the main hlm over WebSocket/mTLS and relays Docker API + `/proc` | Works through NAT, no open Docker port, host metrics work | New protocol, more code |
| **C. Federation** | Each host runs full hlm; one "main" hlm aggregates via API keys | Least change to internals | Two sources of truth, duplicate config |

**Recommendation: B**, with A (TLS / `ssh://`) as a lighter alternative that ships first since the client already
does TLS.

## Design (A then B)

1. **Host abstraction.** Introduce `type Host struct { ID, Name string; Docker *docker.Client; Metrics *hostmetrics.Collector; AppdataRoot string }`
   and a `Hosts` registry. The default host is today's single host (`id: "local"`), so nothing changes for existing
   installs.
2. **Server identity.** Server IDs become `<host>/<name>` in the API; keep bare `<name>` meaning the local host for
   compatibility. Per-server data keyed by name (backup plans, groups, tasks…) becomes keyed by `host/name`, with a
   migration that prefixes existing keys with `local/`.
3. **Manager per host** (`manager.New` per host), with an aggregating layer for list/updates/jobs.
4. **Per-host features:** port forwarding and proxy target the host's LAN IP (needs a per-host address setting);
   Cloudflare Tunnel runs a `cloudflared` per host or routes to other hosts over the LAN; WireGuard per host.
5. **Agent (B):** `hlm agent --server https://hlm.example.com --token <enrol token>`. Enrolment creates a host record
   and a client certificate. The agent dials out, keeps a multiplexed connection (yamux-like over WebSocket), and
   serves: Docker API passthrough (allow-listed endpoints), `/proc` + filesystem reads for host metrics, host folder
   reads for Files/Backups.
6. **Install** gets a "Host" picker; Clone ([10](10-clone-export-import.md)) can move a server between hosts via backup
   → restore.

## API sketch

- `GET/POST/PUT/DELETE /api/hosts` (admin/confirmed), `POST /api/hosts/enrol-token` (confirmed).
- All `/api/servers/{id}` accept `host/name` IDs; list responses include `host`.
- `GET /api/host?host=` for metrics.

## Security

- An agent can run any container on its host; enrolment tokens are single-use, short-lived, confirmed.
- The main hlm should only send allow-listed Docker calls; the agent enforces the same list (defence in depth).
- RBAC: server groups work across hosts; add "host" as a grant target later.

## Phasing

1. Host registry + `local` host + ID migration (no UI change) — big refactor, ship alone.
2. Additional hosts via TLS/SSH Docker endpoints; host picker in Install; host badges in list.
3. Per-host networking features.
4. Agent mode.

## Testing

- Migration of keys `name` → `local/name` across secrets store entries.
- Manager aggregation with two fake Docker clients.
- Agent protocol with in-process client/server.

## Tasks

- [ ] Design review of ID scheme and migration
- [ ] Host registry + migration
- [ ] Aggregating manager + API IDs
- [ ] Extra hosts over TLS/SSH
- [ ] UI host picker and badges
- [ ] Per-host network features
- [ ] Agent mode + enrolment
- [ ] README section
