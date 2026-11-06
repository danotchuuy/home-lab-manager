# 22 — Mod management

**Priority:** Medium · **Depends on:** [11](11-backup-improvements.md) (backups), [19](19-game-profiles.md)
(profiles), [05](05-game-query.md) (to know it came back)

> **Status: done** (branches `feature/22-mods` and `feature/22-workshop`). See the README's
> [Mods and plugins](../README.md#mods-and-plugins). Where the result differs from this plan:
>
> - **Steam Workshop came second** (branch `feature/22-workshop`), as a list on the Mods tab for images that take
>   item IDs in a variable: `hermsi/ark-server` (`GAME_MOD_IDS`), `alinmear/docker-conanexiles` (`CONANEXILES_MODS`;
>   the Conan profile also matches `conanexiles` now) and `renegademaster/zomboid-dedicated-server`
>   (`MOD_WORKSHOP_IDS` with `MOD_NAMES`), each checked against the image's README. The profile field is
>   `mods.workshop` with `app_id` and per-image `env`, `separator` and `names_env`. Searching the Workshop needs a Steam
>   API key, so items are added by link or ID, with a link to the game's Workshop; names, sizes and update times come
>   from Steam's keyless `GetPublishedFileDetails`. A change recreates the server; the rollback recreates it with the
>   old list and starts it. ARK: Survival Ascended has no profile yet, so its images aren't covered. CurseForge and
>   Thunderstore stay "maybe".
> - **A changed server must keep running after it answers** (10 seconds) to count as up, for Modrinth changes too:
>   a published TCP port can accept a connection, through Docker's proxy, just before a server crashes.
> - **A snapshot of the mods folder, not a backup.** It's taken before every change and is what the rollback restores;
>   a full backup first would make each change take as long as a backup. Changes are refused while a backup or restore
>   of the server runs.
> - **No `.mrpack` import and no automatic mod updates** in this version: updates are found daily and notified
>   (`server.mod_updates`), and installed with **Update** or **Update all**.
> - **Profile fields:** `mods.modrinth` has `loader_env`, `loader_default`, `version_env`, `loaders` (each a value of
>   the loader variable, with its Modrinth loaders, folder, and whether they're plugins) and `warn_env`, and
>   `mods.start_timeout` replaces the profile's start time. A `LATEST` version comes from the server's status query,
>   not Mojang's manifest.
> - **The record is `.hlm-mods.json`** in the mods folder. Turning a mod off renames it to `….jar.disabled`.
> - **Plans are recomputed when applied:** the plan's `changes` (versions fixed, dependencies added) are what the UI
>   sends, so the apply does exactly what was shown.
> - **A stopped server** is changed and left stopped: there's nothing to wait for.
> - **The opt-in Docker test** covers the helper-container file operations (`MakeDir`, `RemoveFiles`); the full
>   Fabric install, update, turn-off and rollback were checked by hand against a real `itzg/minecraft-server` and
>   Modrinth, and `TestModrinthLive` (`HLM_LIVE_MODRINTH=1`) checks the client against the real API.

## Why

Mods are the main reason people open a server panel after the first install: add a mod pack, update one, take out
the one that broke the server. Doing it by hand means finding the right file for the game version and loader,
copying it into the right folder, and restarting, with no easy way back when the server stops starting.

## What it does

A **Mods** tab on servers whose profile says how they take mods:

- **Installed**: each mod's name, version, and whether an update is available; **Update**, **Disable** (kept but not
  loaded), **Remove**.
- **Add a mod**: search the source, pick a version that fits the server (game version and loader filled in from the
  server), and install it with its required dependencies.
- **Every change is a transaction**: hlm takes a backup (or a snapshot of the mods folder for a quick change), applies
  the change, restarts the server, and waits for it to answer its status query. If it doesn't within the profile's
  start time (default 5 minutes), hlm puts the previous mods back, restarts again, and says which change failed.
- Optional: **Check for mod updates** daily with the image update check, shown as a badge and a notification.

## Sources, one at a time

1. **Modrinth** (first): an open API (`api.modrinth.com/v2`) with project search, versions filtered by game version and
   loader, file hashes (SHA-512) and dependencies. Covers Minecraft (Fabric, Quilt, Forge, NeoForge, plugins for
   Paper/Purpur) and mod packs (`.mrpack`).
2. **Steam Workshop** (second): most dedicated server images already download Workshop items themselves, given a list
   of IDs in a variable (ARK's `MODS`, Conan's, Project Zomboid's `WORKSHOP_IDS` and `MOD_IDS`, Arma). hlm only edits
   that variable (through the server's settings, which recreate the container) and shows names and update dates from
   Steam's public `GetPublishedFileDetails`. No Steam login, no downloading by hlm.
3. **CurseForge** (maybe): needs an API key from the user and its terms limit automated downloads of some mods; only
   if asked for.
4. **Thunderstore** (maybe): Valheim (BepInEx) and others; open API.

## Design

### Profiles

```yaml
mods:
  modrinth:
    dir: /data/mods              # container path; plugins: /data/plugins
    loader_env: TYPE             # itzg's TYPE=FABRIC → fabric
    loader_map: { FABRIC: fabric, FORGE: forge, NEOFORGE: neoforge, PAPER: paper }
    version_env: VERSION         # the game version; LATEST → ask the server or Mojang's manifest
  workshop:
    env: MODS                    # the variable with the IDs
    separator: ","
    app_id: 346110               # the game's Steam app, to search its Workshop
```

### Applying a change

- `internal/mods`: a `Source` interface (`Search`, `Versions`, `Resolve(deps)`, `Download`) with `modrinth` and
  `workshop` implementations, and a `Plan` (files to add, replace and remove; variable changes) shown before applying.
- Files are downloaded by hlm, checked against the source's hash, size-limited, and written into the server's data
  folder through the same file layer the Files tab uses (local, and other hosts through the agent or helper
  containers). Names are checked: no path separators, `.jar`/`.zip` only for Modrinth.
- hlm keeps `mods.json` beside the mods (in the folder, hidden from the game): each mod's project, version, file and
  hash, so it knows what it installed, what was there before, and what changed by hand (shown as "added outside hlm").
- The change runs as a `manager.Job` with progress events: backup or snapshot, download, apply, restart, wait for the
  status query, then success or rollback. It refuses to overlap with other jobs on the server.

### Updates

- Checked against the source for the server's game version and loader; a mod whose new version needs a newer game
  version is shown as blocked, with why.
- Auto-update of mods is off by default and, if on, uses the existing update policy's window and player check (04).

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/mods` | serverRole(Viewer) | Installed, updates |
| `GET /api/servers/{id}/mods/search?q=` | serverRole(Manager) | Through the source, fitted to the server |
| `POST /api/servers/{id}/mods/plan` | serverRole(Manager) | What a change would do |
| `POST /api/servers/{id}/mods/apply` | serverRole(Manager) | Job stream |

## Security

- Mods are code that runs in the game server's container: installing one is like changing the image, so it's
  Manager-only and audited with the mod, version and hash.
- Downloads only from the source's own CDN hosts, over HTTPS, hash-checked; nothing from user-entered URLs in the first
  version.
- No credentials for Modrinth or Workshop. A CurseForge key, if added later, goes in the secrets store.

## Testing

- A fake Modrinth server (`modrinthtest`) and fake Steam details: search, version fitting, dependencies, hashes,
  `.mrpack` import.
- Plans: add, replace, remove; files changed by hand; refusing bad names.
- The job: rollback when the fake server never answers; no rollback when it does; backup failure stops the change.
- Opt-in Docker test with `itzg/minecraft-server` (Fabric) installing a small mod and rolling back a broken one.

## Tasks

- [x] Profile `mods` section and validation
- [x] `internal/mods` with the Modrinth source and `modrinthtest`
- [x] `mods.json`, plans, the apply job with rollback
- [x] Mods tab (installed, search, plan, progress)
- [x] Update checks and notification
- [x] Workshop source (variable editing and Steam details)
- [x] Built-in profiles: Minecraft (itzg), ARK, Conan Exiles, Project Zomboid
- [x] README section and API table
