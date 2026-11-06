# 21 — Player management

**Priority:** Medium · **Depends on:** [02](02-interactive-console.md) (console), [05](05-game-query.md) (player
list), [19](19-game-profiles.md) (profiles)

## Why

Kicking a griefer, banning them, adding a friend to the allow list and making someone an admin are the commonest
things people do with a game server, and today they mean remembering each game's command and typing it in the
console. hlm already reads who's online. With the commands in each game's profile, it can put buttons next to the
names.

## What it does

A **Players** tab on each server whose profile has player actions:

- **Online now**: the names (and IDs, where the game gives them) from the status query or console, with **Kick**,
  **Ban** and **Make admin** buttons, each with a confirmation and an optional reason.
- **Allow list**, **Bans** and **Admins**: hlm's own lists of what was done through it, with **Remove** (unban, take off
  the allow list, remove admin) and **Add a player…** for someone who isn't online.
- Where the game keeps these lists in files (Minecraft's `whitelist.json`, `banned-players.json`, `ops.json`;
  Valheim's `adminlist.txt`, `bannedlist.txt`, `permittedlist.txt`), the lists are **read from the files** instead, so
  changes made in-game show too. Changes still go through the console when the server is running (the game rewrites the
  file), and are written to the file when it's stopped.

## Design

### Profiles

A new `players.actions` section, plus optional lists:

```yaml
console:
  players:
    command: list
    format: summary
    pattern: …
    actions:
      kick: "kick {player} {reason}"
      ban: "ban {player} {reason}"
      unban: "pardon {player}"
      allow: "whitelist add {player}"
      disallow: "whitelist remove {player}"
      admin: "op {player}"
      unadmin: "deop {player}"
    lists:                        # optional: where the game keeps them
      allow: { file: whitelist.json, format: minecraft-json }
      ban: { file: banned-players.json, format: minecraft-json }
      admin: { file: ops.json, format: minecraft-json }
```

- `{player}` is the name, or the ID for games that need it (`id_field: steamid` in `players`, with csv's column or a
  pattern group `id`: Palworld's `KickPlayer` and `BanPlayer` take the Steam ID). `{reason}` is optional and dropped
  with the space before it when empty.
- List formats are built in, like the REST driver: `minecraft-json`, `lines` (one name or ID per line, `//` comments,
  Valheim and many others), more as profiles need them. Files are found the same way settings files are
  (`configfiles`), under the server's data folders.
- Built-in profiles get actions for the games whose commands are known: Minecraft (Java and Bedrock), Palworld, ARK,
  Rust, Valheim (lists only, no RCON), CS2, Factorio, 7 Days to Die, Terraria (tShock). Each is checked against the
  game's documentation and, where possible, a real server.

### Safety of what's sent

- Player names and IDs are checked before they go into a command: printable, no control characters, no newlines,
  no `;` or quotes, at most 64 characters, and for IDs only digits or the ID format the profile names. A name that
  fails is refused, not cleaned, so a crafted name can't add a second command.
- Reasons are limited the same way and to 200 characters.
- Commands go through the server's configured console (RCON, REST, exec, stdin) via `console.Service.Send`, so they're
  audited and limited like typed commands.

### Records

- hlm keeps a per-server log of actions (`"players"` in the secrets store or a data file: who, what, which player,
  reason, when), shown under the lists and in the audit log as `server.player.kick`, etc.
- Optional notification `server.player_banned`.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/players` | serverRole(Operator) | Online (names), lists, which actions the game has |
| `POST /api/servers/{id}/players/{action}` | serverRole(Operator) for kick; Manager for ban, allow, admin and their undo | `{player, reason}` |

Viewers keep seeing only the count, as today.

## Testing

- Profile validation: actions need `{player}`; list formats are known; `id_field` exists in the player list.
- Name and reason checks, including names with newlines, `;`, quotes and very long ones (refused).
- Each built-in profile's actions render the expected commands; list parsing for each format from sample files.
- API: role per action; reading lists from files; writing a list file while stopped (atomic, keeping the format).
- Manual: a real Minecraft server (kick, ban, whitelist, op) and Valheim's lists.

## Tasks

- [ ] Profile `actions`, `lists`, `id_field`; validation
- [ ] Built-in profiles' actions and lists, checked per game
- [ ] Name and reason checks; sending through the console
- [ ] List readers and writers (`minecraft-json`, `lines`)
- [ ] Action log, audit names, notification
- [ ] Players tab
- [ ] README section and API table
