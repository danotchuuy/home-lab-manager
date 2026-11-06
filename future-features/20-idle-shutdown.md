# 20 — Stop idle servers, wake them on demand

**Priority:** High · **Depends on:** [05](05-game-query.md) (player counts), [17](17-public-status-page.md) (wake from
the status page, optional)

## Why

Most home-lab game servers are empty most of the day, but a running Palworld or ARK server holds 8–16 GB of memory
and some CPU whether anyone's on or not. hlm already knows how many players each server has. It can stop a server that
has been empty for a while and start it again when someone wants to play, so several servers can share a host that
could only run one or two at a time.

## What it does

Per server, on a new **Idle** section of its **Schedule** tab (Manager):

- **Stop when empty for** N minutes (default 30; off by default). Only counts while the status query is *answering*
  with a player count; a server hlm can't ask (no query method, or TCP check only) can't be stopped this way, and the
  section says so.
- **Wake on connect** (optional): while it's stopped this way, hlm listens on its published game port; the first
  connection or packet starts it.
- **Wake from the status page** (optional): a **Start** button for that server on the public status page, for friends
  who see it's asleep.
- A server stopped this way shows **Asleep (no players for 30 min)** instead of **Stopped**, in the server list, its
  page and the status page; starting it by hand clears that.

## Design

### Going to sleep

- `internal/idle`: `Service` with settings per server (`"idle"` in the secrets store: `{minutes, wakeOnConnect,
  wakeFromStatusPage}`), forgotten when a server is deleted.
- A loop (`RunScheduler` pattern, every minute) reads `gamequery.Summaries`. A server is idle when its status is
  `online`, `hasPlayers` is true and `players == 0`; the time it became idle is kept in memory and reset by any
  reading with players, by a query failure (unknown isn't empty), and by a start.
- At the limit: send the profile's `say` ("Stopping in 1 minute: no players"), wait a minute, read the count again,
  then stop through `manager.Stop`, which already saves first for games that need it (`UseBeforeStop`). Record `asleep[server] = since` (persisted,
  so a restart of hlm keeps the server asleep and its listener up).
- Never while a job runs on the server (install, update, backup, restore: `manager` jobs) or one of its scheduled
  tasks is running; try again next minute.
- Notification `server.asleep` (info, off by default in targets) and an audit entry by `hlm (idle)`.

### Waking on connect

- For each asleep server with `wakeOnConnect`, hlm binds the server's published host ports (from its last inspect:
  `HostConfig.PortBindings`) itself, TCP and UDP, on the same addresses. Docker released them when it stopped.
- **TCP:** accept, start the server, close the connection (the client retries). **Minecraft (Java)** is special-cased
  using the profile's `query.protocol`: answer a status ping with an MOTD "Asleep: join to wake it" and the max player
  count, and on a login attempt reply with a disconnect message "Starting, try again in a minute". That's the part
  players notice, so it's worth the few dozen lines of protocol hlm already has in `gamequery`.
- **UDP:** the first datagram starts the server; nothing is answered. Steam's A2S query from the server browser counts
  as a connection, so a browser refresh wakes it. That's acceptable and documented (and rate-limited, below).
- Before starting: close the listeners (so Docker can bind), then `manager.Start`. If the start fails, the listeners
  come back and a `server.wake_failed` notification goes out.
- **Abuse:** port scans would wake servers. Limit wakes to 1 per server per 10 minutes, and only count a TCP connection
  that sends at least one byte (a SYN scan doesn't), or for Minecraft a handshake with the right protocol. The
  setting's help says that open internet ports mean anyone can wake it.
- **Other hosts:** listeners must run on the host that owns the port. For the first version, wake on connect is only
  offered for servers on the local host; servers elsewhere can use the status page. A later version can run the
  listener in the agent (`hlm agent` already holds a connection to hlm).
- **Ports forwarded by hlm** (UniFi, the proxy over WireGuard, Cloudflare) all end at the host port, so they keep
  working: the listener sees the same traffic Docker would.

### Waking from the status page

- `POST /api/status-page/{slug}/wake` (public, only for servers with `wakeFromStatusPage`, the site's view password
  if it has one, the same per-address limiter plus 1 wake per server per 10 minutes). Returns the status, never
  anything else.
- The status page shows **Asleep** with a **Start** button and then polls until it's online.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/idle` | serverRole(Viewer) | Settings, asleep since, idle since |
| `PUT /api/servers/{id}/idle` | serverRole(Manager) | |
| `POST /api/status-page/{slug}/wake` | public (opt-in per server) | |

Servers' JSON gains `asleep` (`{since, minutes}`), and game summaries gain `idleSince`.

## Security

- Waking only ever starts a server that was put to sleep by hlm and is set to allow it; it can't start anything else,
  and it never stops anything.
- Listeners only bind ports the server itself published, and are closed before every start and when the setting is
  turned off, the server is started by hand, or deleted.
- The status page endpoint discloses nothing new: it's a button on a server already shown.

## Testing

- Idle detection with a fake clock and fake summaries: players, zero, unknown, failure, start resets.
- The stop sequence: warning, re-check, stop; skipped while busy.
- Listener: TCP with and without a byte, UDP, Minecraft status ping and login answers (against the existing Minecraft
  client in `gamequery`), the wake rate limit, ports released before the start, listeners back after a failed start.
- Persisted sleep across a restart of the service.
- API: policies, status page wake only for opted-in servers, password and rate limit.
- Manual: a real Minecraft server sleeping and waking from a client, and a Valheim server woken from the status page.

## Tasks

- [ ] `internal/idle` settings, idle detection and the stop sequence
- [ ] Persisted sleep state; "Asleep" in the list, server page and status page
- [ ] TCP/UDP wake listeners for local servers, with limits
- [ ] Minecraft sleeping MOTD and login message
- [ ] Status page wake button
- [ ] Notifications `server.asleep`, `server.wake_failed`; audit
- [ ] Schedule tab → Idle UI
- [ ] README section and API table
