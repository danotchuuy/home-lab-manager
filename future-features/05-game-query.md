# 05 — Game query: players and status

**Priority:** High · **Depends on:** nothing · **Unblocks:** 03 (conditions), 04 (only when empty), 17 (status page)

## Why

"Container running" doesn't mean players can join: SteamCMD images spend minutes downloading the game, and servers can
hang. Players also want to know who's on. And automation needs to know if anyone is playing before restarting.

## What it does

hlm asks each running server the game's own status query and shows:

- **Status:** Starting (container up, query not answering yet), **Online**, Not responding (answered before, stopped
  answering), Unsupported/Off.
- **Players:** `3 / 10`, and names where the protocol gives them.
- **Server name, map, version**, password-protected flag.
- History: player count over the last 24 h (sparkline), stored in the same way as metrics history ([08](08-metrics-history-prometheus.md)).

## Protocols (first version)

| Protocol | Games |
| --- | --- |
| Steam A2S (`A2S_INFO`, `A2S_PLAYER`, with challenge) over UDP | Valheim, ARK, Rust, CS2, Garry's Mod, 7 Days, Project Zomboid, Conan, Enshrouded, V Rising, Satisfactory (older), Palworld (partial)… |
| Minecraft Java Server List Ping (TCP, VarInt framing, JSON) | Minecraft Java, modded |
| Minecraft Bedrock (RakNet unconnected ping, UDP) | Bedrock |
| RCON command fallback (02) | e.g. Palworld `ShowPlayers` |
| TCP connect only | Anything else: "port open" |

Later: GameSpy4 / Unreal (Terraria via TShock REST), Factorio, Satisfactory HTTPS API.

## Design

### `internal/gamequery`

```go
type Result struct {
    OK        bool
    Name, Map, Version, Game string
    Players, MaxPlayers int
    PlayerNames []string
    Password  bool
    Latency   time.Duration
    Error     string
    At        time.Time
}
type Prober interface { Probe(ctx context.Context, addr string) (Result, error) }
```

- **Choosing protocol and port:** auto-detect from the template: known images/categories map to a protocol; A2S query
  port is usually game port +1 (Valheim 2457 for 2456) or a port named "Query"/"Steam Query" in the template. Store an
  override per server (protocol, port) on the Settings tab with **Test**.
- **Where to query from:** container IP on its Docker network if hlm can reach it (same network or hlm on the host),
  else `127.0.0.1:<host port>` / Docker host address. Never query through the internet address (that's what the port
  check already does).
- **Poller:** every 30 s per running server (15 s while "Starting"), with a 2 s timeout, concurrency limit 8. Results in
  memory; count history to [08](08-metrics-history-prometheus.md).
- **Transitions** feed [01](01-notifications.md): `server.online` (after start, optional), `server.not_responding`
  (3 failed probes after having been online).

### Player names and privacy

Player names are visible to Viewers? Names are mildly personal. Default: Viewer sees counts only; Operator sees names.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers` (existing) | — | add `query: {status, players, maxPlayers}` |
| `GET /api/servers/{id}/query` | Viewer | Full result (names if Operator), 24 h counts |
| `GET/PUT /api/servers/{id}/query/settings` | Viewer / Manager | Protocol/port override |
| `POST /api/servers/{id}/query/test` | Manager | Probe with given settings |

## UI

- Server cards: a status dot (Starting / Online / Not responding) next to the container state, `3/10` players.
- Server page Overview: name, map, version, player list, 24 h sparkline.
- Settings tab: Query section (auto / protocol / port, Test).

## Testing

- Each protocol against an in-process fake (UDP/TCP listeners in tests) incl. A2S challenge flow and split packets.
- Detection table tests over feed templates (`internal/catalog/testdata/feed.json`).
- Poller with fake probers: state transitions, backoff.

## Tasks

- [ ] `internal/gamequery`: A2S, Minecraft Java, Bedrock, TCP probe
- [ ] Detection from templates + known-image table
- [ ] Settings override store + test endpoint
- [ ] Poller + transitions + notify hook
- [ ] Add to server list/detail APIs
- [ ] UI: status dot, players, details, settings
- [ ] Player history (with 08)
- [ ] README section
