# 02 — Interactive console (stdin and RCON)

**Priority:** High · **Depends on:** nothing · **Unblocks:** in-game commands in 03, graceful updates in 04

## Why

The **Console** tab only shows output. Running game servers means typing commands: `save`, `say`, `kick`, `whitelist
add`, `stop`. Today that needs `docker attach` or an RCON tool on another machine.

## What it does

A command box under the Console output, in one of three modes per server:

1. **Stdin** — write the line to the container's stdin (needs `OpenStdin`/`-i`). Output appears in the existing log
   stream. Works for Valheim (some images), Terraria, Factorio, Minecraft (vanilla/itzg with `-i`), Satisfactory, many
   SteamCMD images that wrap the server in a shell.
2. **RCON** — connect to the game's RCON port and show the reply inline. Source RCON protocol covers Minecraft, Rust,
   ARK, Palworld, CS2, Conan, Squad, 7 Days to Die (telnet variant later).
3. **Exec** — run a configured command inside the container per line (e.g. `rcon-cli %s` in itzg/minecraft-server,
   `mc-send-to-console %s`). Uses the existing `docker.Client.Exec`.

The mode and its settings are chosen on the server's **Settings** tab, with **Detect** filling them in.

Extras: command history (↑/↓, kept per server in the browser), and per-template saved quick commands ("Save world",
"List players").

## Design

### Detection (`internal/console`)

- Stdin: container `Config.OpenStdin == true` → stdin available.
- RCON: a template variable named like `RCON_PORT`/`RCON_PASSWORD`/`RCON_PASS`/`ADMIN_PASSWORD`/`ENABLE_RCON`, or a
  port named "RCON" in the template; default ports per known image (25575 Minecraft, 28016 Rust, 25575 Palworld,
  27020 ARK). Password read from the container's env (never sent to the browser).
- Exec: known images (`itzg/minecraft-server` → `rcon-cli`).

Stored per server name in the secrets store under `"console"` (it may hold an RCON password override):

```go
type Settings struct {
    Mode     string // "", "stdin", "rcon", "exec"
    RCONHost string // default: container IP on its network, or 127.0.0.1 + host port
    RCONPort int
    Password string // optional override; default: from the container's env var PasswordVar
    PasswordVar string
    ExecCmd  []string // with %s for the command
}
```

### Stdin

Docker's attach API: `POST /containers/{id}/attach?stream=1&stdin=1` with a hijacked connection. Add
`docker.Client.AttachStdin(ctx, id) (io.WriteCloser, error)` using the same raw-connection approach as logs. Keep one
attached writer per server inside the manager, shared by all users, reconnecting when the container restarts. Write
`line + "\n"`. Containers without `OpenStdin` can be switched on with a **reconfigure**: `-i`/`--interactive` in extra
params already sets `OpenStdin` (`deploy/extraparams.go`), so the UI only needs an "Enable console input" toggle that
adds it.

### RCON

`internal/rcon`: Source RCON client (auth packet, exec, multi-packet replies via the empty-packet trick, 4 KB
limit handling, 10 s timeout). Connect from hlm to the container: if hlm and the container share a Docker network use
the container IP; otherwise the published host port on the Docker host. Keep a pooled connection per server, closed
after 5 minutes idle.

### API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/console` | Operator | Mode, whether usable, detected suggestions (no passwords) |
| `PUT /api/servers/{id}/console` | Manager | Save settings (password write-only) |
| `POST /api/servers/{id}/console/command` | Operator | `{"command":"save"}` → `{"output":"..."}` (RCON/exec) or 204 (stdin) |
| `POST /api/servers/{id}/console/detect` | Manager | Probe and suggest settings |

Operators can already restart the server and read logs; sending commands is the same level of trust. Mention in the
README's role table that console commands can do anything the game's admin can (including, for some games, running
scripts), so Operator is "can administer the game".

### UI

- Command input under the log viewer (`LogViewer.tsx`), disabled with a hint when no mode is set ("Set up the console
  on the Settings tab").
- RCON/exec replies are shown inline in the console, styled as a command echo plus reply, so the log stream and replies
  read as one transcript.
- Settings tab: "Console" section with mode, detect button, RCON fields.
- Quick commands: chips above the input, from the template (`internal/catalog` gains an optional per-app list in a
  small built-in table for popular games) plus user-added ones stored with the settings.

## Safety

- Limit command length (1 KB) and rate (5/s per server per user).
- Commands are written to the [audit log](06-audit-log.md) (who, server, command — not the output).
- Never return the RCON password; redact it if a game echoes it.
- A stdin line is raw bytes: strip control characters other than tab.

## Testing

- `rcon`: in-process fake RCON server (auth success/failure, multi-packet reply, timeout).
- Attach: opt-in Docker test with busybox `cat` (OpenStdin) — write a line, see it in logs.
- Detection: table tests over template variables and images.
- API: Viewer gets 403/404; Operator can send; Manager can configure.

## Tasks

- [ ] `docker.Client.AttachStdin` + test
- [ ] "Enable console input" toggle that adds `-i` and recreates
- [ ] `internal/rcon` client + fake server
- [ ] `internal/console`: settings store, detection, per-server sessions (stdin writer, RCON pool, exec)
- [ ] API routes
- [ ] Console input, history, inline replies, quick commands
- [ ] Settings tab section + detect
- [ ] Audit log hook (when 06 lands)
- [ ] README: Console section, role table note

## Open questions

- Telnet-style consoles (7 Days to Die) and WebSocket RCON (Rust's WebRCON) — v2.
- Should Viewer be able to see quick-command *output* like "list players"? Probably via [05](05-game-query.md) instead.
