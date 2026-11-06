# 23 — Discord commands

**Priority:** Low · **Depends on:** [01](01-notifications.md) (Discord already a target), [05](05-game-query.md),
[20](20-idle-shutdown.md) (waking, optional)

> **Status: done** (branch `feature/23-discord-commands`). See the README's
> [Discord commands](../README.md#discord-commands). Where the result differs from this plan:
>
> - **Its own small WebSocket client** (`internal/discord/ws.go`): the agent's is a byte stream over binary frames, and
>   the gateway needs text messages with their boundaries.
> - **Rules are the bot's own, not rbac grants**: each is a Discord role (or Everyone), actions, and server groups (or
>   every server on the page), checked with the same group membership (by name and by whole host). Audit entries are
>   `discord:<username>` with the new actor kind `discord`.
> - **Discord sees the status page's servers**, whether or not the page is on, through a new `statuspage.All`. Which
>   ones it may act on comes from the rules. Addresses and player names need the new **Show addresses and who's
>   playing** setting, the Discord version of the page's details password.
> - **Commands are registered in the one Discord server**, not globally, so they appear at once; `/stop` replies when
>   the server has stopped; refusals are private (ephemeral) and the same for unknown servers and forbidden ones.
> - **The token is checked with Discord when saved.** `GET …/discord/roles` lists the server's roles for the rule
>   editor.
> - **Not tested with a real bot in a real Discord server**: that needs a bot token of yours. The fake gateway
>   (`discordtest`) covers the protocol, and `TestGatewayLive` (`HLM_LIVE_DISCORD=1`) checks the WebSocket client
>   against Discord's real gateway (hello, identify, a refused token closing with 4004).

## Why

The people who play on a home-lab server are usually in a Discord server together. "Can you start Valheim?" goes to
whoever runs hlm. Letting trusted friends start a server, or check who's on, from Discord saves that, without giving
them hlm accounts.

## What it does

An admin connects a Discord bot on **System → Discord**, and maps Discord roles to what they may do. Then in Discord:

- `/status` — every server shown to the channel: online, players, address (from the status page settings).
- `/players valheim` — who's on.
- `/start valheim`, `/stop valheim`, `/restart valheim` — for roles allowed to.
- `/say valheim Back in 5` — the profile's `say` command, for roles allowed to.

Replies are short embeds; start and restart reply once the server answers its status query ("Valheim is up: 0/10
players"), or with the failure.

## Design

### Connection

- **Gateway, not an interactions URL.** Discord can deliver slash commands either to a public HTTPS endpoint or over
  the bot's outbound WebSocket gateway connection. A home lab often has no public hlm, so use the gateway: hlm
  connects out to Discord, nothing inbound is needed. hlm already has an RFC 6455 WebSocket client (`internal/agent`).
- `internal/discord`: gateway connection (identify, heartbeat, resume, reconnect with backoff), only the
  `INTERACTION_CREATE` event (no message content intent, no privileged intents), and the REST calls to register the
  global or per-guild commands and reply to interactions.
- The bot token goes in the secrets store (`"discord"`), shown once, like other credentials. The setup page links to
  the Discord developer portal with the steps and the invite URL (permissions: `applications.commands` and send
  messages only).

### Who may do what

- Settings: the guild (Discord server) ID, and rules `{discord role → hlm servers (by server group or name) →
  actions}`, where actions are `status`, `players`, `start`, `stop`, `restart`, `say`. Nothing is allowed by default
  but `status` for servers on the status page.
- Decisions use `rbac` underneath: each rule acts like a principal `discord:<role id>` with a role per server group, so
  the same checks as the API apply, and audit entries read `discord:<user name> (via Discord)`.
- Commands only from the configured guild; DMs refused. Per-user rate limit (5 commands a minute).
- Autocomplete for server names lists only the servers the caller's roles can act on.

### Output

- Only what the status page would show for status and players (display names, not internal names; player names only if
  that server shows them there). No logs, files, settings or internal addresses ever.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/integrations/discord` | admin | Settings (no token), connection state |
| `PUT /api/integrations/discord` | confirmed | Token and rules (grants access) |
| `POST /api/integrations/discord/test` | admin | Connects and registers the commands |
| `DELETE /api/integrations/discord` | confirmed | Disconnects and unregisters |

## Security

- The bot token is a credential: secrets store, never returned, never logged; the notify package's redaction covers
  it.
- Rules grant access, so saving them needs `confirmed`.
- Discord user and role IDs are trusted only from the gateway session hlm authenticated with its token.

## Testing

- A fake gateway (`discordtest`): hello, identify, heartbeats, resume after a drop, an interaction, replies.
- Rule evaluation: roles to servers and actions; default deny; guild check; rate limit.
- Output allow-listing, as for the status page: a dedicated struct, tests that nothing else gets through.
- Manual: a real bot in a test Discord server.

## Tasks

- [x] `internal/discord` gateway and REST client with `discordtest`
- [x] Settings and rules (the bot's own, checked against server groups)
- [x] Commands: status, players, start/stop/restart (with waiting), say; autocomplete
- [x] Audit and rate limit
- [x] System → Discord setup page with the developer portal steps
- [x] README section and API table
