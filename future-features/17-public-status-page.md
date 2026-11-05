# 17 — Public status page

**Priority:** Low · **Depends on:** [05](05-game-query.md) (players/online), [08](08-metrics-history-prometheus.md)
(uptime history, optional)

> **Status: done** (branch `feature/17-public-status-page`). See the README's
> [Public status page](../README.md#public-status-page). Where the result differs from this plan:
>
> - **States:** online, starting, *up* (running, but hlm can't ask the game) and offline (stopped, or not answering).
> - **Uptime** needed history that didn't exist: the metrics sampler now records an `up` series per server (1 while
>   running and not known to be failing to answer), and the page shows its 7- and 30-day average once there's an hour
>   of it.
> - **Addresses** are suggested (proxy host for relayed ports, UniFi WAN address for forwarded ones) in the settings and
>   saved; the public page never looks anything up. A Cloudflare hostname isn't suggested: tunnels carry HTTP, not
>   game ports.
> - **Badge names** are slugs of the display names (`viking-world`), not server names, which stay internal.
> - **Rate limit:** a token bucket per address (2/s, bursts of 60) on top of the 15-second cache. Addresses are the
>   connection's (forwarded headers aren't trusted), so behind a tunnel everyone shares one.
> - **The SPA route** is handled before the app (`main.tsx`), so `/status` loads without the header, sign-in or any
>   admin API call.

## Why

Friends ask "is the server up?", "what's the address?", "who's on?". Giving them hlm accounts is overkill (and
Viewer still shows more than needed). A read-only public page answers that.

## What it does

An admin turns on **System → Status page** and picks which servers appear. The page at `/status` (or a separate
hostname through the Cloudflare Tunnel) shows for each chosen server:

- Display name and game icon (from the template)
- Status: Online / Starting / Offline (from the game query, falling back to container state)
- Players `3 / 10`, optionally names (per-server opt-in)
- **How to connect**: the public address (hostname or WAN IP + port, from the tunnel/proxy/port-forward settings), with
  a copy button. Optionally a password hint text the admin types (never the real password).
- Version / map (optional)
- Uptime over 7/30 days (if 08 exists)
- An optional message banner ("Maintenance Saturday 10:00")

No settings, logs, files, resource usage, or internal addresses.

## Design

- Separate route that is `public` in the route table: `GET /api/status-page` returns only the allow-listed fields for
  the chosen servers. The SPA has a `/status` route that renders without the app shell and without needing sign-in.
- Cache the response for 15 s to make it cheap to hit.
- Settings in the secrets store under `"statuspage"`: enabled, title, message, servers `[{name, displayName, showPlayers,
  showNames, address, hint}]`. Address is auto-filled (Cloudflare hostname, proxy address, or WAN IP from the UniFi
  integration) but editable.
- **Separate hostname option:** a tunnel route to hlm limited to `/status` and `/api/status-page` and static assets.
  Implement as a path-restricting handler wrapper selected by `Host` header, so the rest of hlm isn't reachable through
  that hostname even if someone tries `/api/servers`.
- Rate limit per IP (reuse the login limiter's style).
- Optional JSON/badge endpoints: `/api/status-page/{server}.json`, `/api/status-page/{server}.svg` (shields-style
  badge "valheim | 3/10 online") for Discord/README embeds.
- `allow-from` (reuse `auth/allowfrom.go`) to restrict it to e.g. a VPN range if wanted.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/status-page` | public (only when enabled) | |
| `GET /api/status-page/{server}.json` / `.svg` | public | |
| `GET/PUT /api/system/status-page` | admin | Settings |

## Privacy and security

- Server *names* are internal; show `displayName` (defaults to the template name, editable).
- Player names off by default.
- Disabled → endpoints return 404.
- Test that nothing in the response comes from fields not allow-listed (marshal from a dedicated struct, never from
  `manager.Server`).

## Testing

- Response contains only allow-listed fields; hidden servers absent; disabled → 404.
- Host-restricted wrapper blocks other paths.
- Badge SVG rendering snapshot.

## Tasks

- [x] Settings model + admin API
- [x] Public endpoint with dedicated response struct + cache + rate limit
- [x] Address auto-fill from proxy/forwarding (not the tunnel: HTTP only)
- [x] `/status` SPA route without app shell
- [x] Optional dedicated hostname with path restriction
- [x] JSON and SVG badges
- [x] System → Status page UI (badge previews; opens the page)
- [x] README section
