# 22 — Mod management

**Priority:** Medium · **Depends on:** [11](11-backup-improvements.md) (backups), [19](19-game-profiles.md)
(profiles), [05](05-game-query.md) (to know it came back)

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

- [ ] Profile `mods` section and validation
- [ ] `internal/mods` with the Modrinth source and `modrinthtest`
- [ ] `mods.json`, plans, the apply job with rollback
- [ ] Mods tab (installed, search, plan, progress)
- [ ] Update checks and notification
- [ ] Workshop source (variable editing and Steam details)
- [ ] Built-in profiles: Minecraft (itzg), ARK, Conan, Project Zomboid
- [ ] README section and API table
