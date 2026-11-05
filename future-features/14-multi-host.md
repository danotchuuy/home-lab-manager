# 14 — Multiple Docker hosts

**Priority:** Low (large) · **Depends on:** nothing, but touches almost everything

> **Status: done** (phases 1–3 on branch `feature/14-multi-host`; the agent on `feature/14-agent`). See the README's
> [More Docker hosts](../README.md#more-docker-hosts). Where the result differs from this plan:
>
> - **IDs are `name@host`, not `host/name`,** and servers on the local host keep their plain names. Docker doesn't allow
>   `@` in container names, so the two can't be confused, and unlike `/` it's safe in URL path segments, file names
>   and S3 keys. Because local names don't change, **no migration** was needed: per-server settings (backup plans,
>   groups, tasks, …) of existing servers keep their keys, and those of other hosts' servers are keyed by `name@host`.
> - **One manager, not one per host:** `manager.Manager` routes Docker calls by the server ID's host (`internal/manager/hosts.go`)
>   and lists every host's containers together (a host that can't be reached is left out, with its status kept for
>   the UI). Everything built on the manager (backups, schedule, health, console, game query, transfer) works for any
>   host unchanged.
> - **Connections (A):** SSH and TCP with TLS (`internal/hosts`). SSH forwards to Docker's socket over hlm's existing
>   SSH key (shared with the proxy), with the host key pinned on first use; where the SSH server refuses forwarding it
>   falls back to `docker system dial-stdio`, as the docker CLI does. TCP always verifies the daemon's certificate;
>   plain TCP isn't accepted. Hosts are kept in the secrets store (`"hosts"`, client keys apart in `"host-tls-key-<id>"`).
> - **Per-host networking (3):** a host's LAN address is used for Web UI links, game queries and console connections,
>   UniFi port forwards and Cloudflare Tunnel routes from the local `cloudflared`. A `cloudflared` or WireGuard per host
>   wasn't built.
> - **Clone between hosts:** the clone page has a host picker; data is copied through hlm (archive from one host,
>   restore on the other), ports stay when they're free on the target.
> - **Added: setting Docker up on an SSH host.** "Check for Docker" reports the host's OS and Docker state; hlm can
>   install Docker CE following Docker's install pages (Ubuntu, Debian, Raspberry Pi OS, Fedora, CentOS Stream, RHEL),
>   start it on boot and add the user to the `docker` group, as root (root or passwordless sudo).
> - **Added: TLS set up over SSH.** For a TCP host, hlm can make a per-host CA (key discarded after signing), a server
>   and a client certificate, install the server's, and start dockerd with `--tlsverify` on port 2376 through a
>   systemd drop-in (rolled back if Docker doesn't start), then save the host with its client certificate.
> - **Agent mode (B), built later:** `hlm agent` (the same binary and image) dials out to hlm over a WebSocket
>   (`/api/agent/connect`), not mTLS: hlm is often behind a reverse proxy or Cloudflare Tunnel that ends TLS before it,
>   and those pass WebSockets on. Streams are multiplexed with yamux (`github.com/hashicorp/yamux`, the one new
>   dependency); hlm's Docker client dials a stream per connection, so everything (attach, logs, events, archives)
>   works unchanged. The agent authenticates with a credential from a single-use, one-hour enrolment token (made by a
>   confirmed admin; only hashes are stored), checks hlm's certificate (plain HTTP only with `--allow-http`), and passes
>   on only an allow-list of the Docker calls hlm makes. Hosts gain `kind: "agent"`. The agent also reports its host's
>   metrics (the same collector as hlm's, from `/host/proc` and `/host/rootfs`): hlm polls them every 15 s into the
>   host's history (`hosts/<id>`), the Host page has a host picker, and `/metrics` labels `hlm_host_*` (and
>   `hlm_server_*`) with `host`. Host folder reads through the agent weren't needed: files and backups go through
>   Docker.
> - **Built later (branch `feature/14-remaining`):** host metrics for SSH hosts (a script over SSH) and TLS hosts (the
>   same script in a throwaway busybox container with `/proc` and `/` mounted read-only), through a source interface
>   in `hostmetrics`; disk usage by server and deleted servers' data per host (folders measured by one helper
>   container per calculation); Compose stacks on any host (a stack's host is fixed when it's created); and whole hosts
>   in server groups (`Group.Hosts`), which also fixed servers on other hosts being refused in groups (`@` wasn't
>   allowed in a group's server names).
> - **Not built:** hlm's WireGuard and Cloudflare Tunnel containers per host.

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

- [x] Design review of ID scheme and migration (`name@host`; no migration needed)
- [x] Host registry
- [x] Aggregating manager + API IDs
- [x] Extra hosts over TLS/SSH
- [x] UI host picker and badges
- [x] Per-host network features (links, queries, UniFi, Cloudflare Tunnel; not a cloudflared/WireGuard per host)
- [x] Agent mode + enrolment
- [x] README section
