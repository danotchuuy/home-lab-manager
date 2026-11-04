# 10 — Clone, export and import servers

**Priority:** Medium · **Depends on:** nothing (uses existing backup/restore)

## Why

- Making a test copy of a world (try a mod) means installing again and copying files by hand.
- Restoring a backup always overwrites the original server; there's no "restore into a new server".
- Moving a server to another hlm (new machine) means re-entering every setting.

## What it does

### 1. Export settings

**Settings → Export** downloads `<server>.hlm.json`:

```json
{
  "hlmExport": 1,
  "exported": "2026-11-04T12:00:00Z",
  "hlmVersion": "v0.0.30",
  "template": { "id": "...", "source": "ca|custom", "custom": { /* full template if Custom */ } },
  "request": { /* deploy.Request: name, values, extra, extraParams, postArgs, privileged, network, ... */ },
  "autostart": true,
  "backupPlan": { "folders": [...], "intervalHours": 24, "keep": 7, "stopServer": true },
  "tasks": [ /* 03 */ ], "updatePolicy": { /* 04 */ }, "query": { /* 05 */ },
  "internetPorts": { /* forwarding/proxy settings for this server */ },
  "hostnames": [ /* tunnel routes for this server */ ]
}
```

- Secret-looking values are **left out by default** (`deploy/secrets.go` detection), with a tick box "Include
  passwords" (Manager+, requires confirmation).
- Paths under `HLM_APPDATA_ROOT` are written relative (`${APPDATA}/valheim/...`) so they map to the target's root.

### 2. Import settings

**Catalog → Import a server** takes the JSON, shows the install form pre-filled (so the user can change name, ports,
paths), warns about port conflicts (`POST /api/servers/check-ports` exists) and missing passwords, then installs.

### 3. Clone

**Clone** on a server = export + import on the same hlm, with:
- a new name (`valheim-copy`), paths moved to `<appdata>/<new-name>/...`, host ports shifted to free ones;
- **Copy data**: off / copy data folders now (stop source while copying, optional) / from a backup (choose one).
- Hostnames, port forwarding and tunnel routes are **not** cloned (they'd collide).

Copying data: for bind mounts on a host hlm can see, copy with a short-lived `busybox` container (`cp -a` from the
source mount to the new mount), like deleted-server cleanup does; for volumes, mount both into a helper. Streams
progress as a job.

### 4. Restore into a new server

On the Backups tab, a backup's menu gets **Restore as new server…**: runs clone (settings only) and then the existing
restore into the new server. Uses the same archive format (paths by container path), so it maps directly.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/export?secrets=0|1` | Operator (secrets: Manager + confirmed) | |
| `POST /api/servers/import/preview` | admin | Validate, return install-form values + warnings |
| `POST /api/servers/{id}/clone` | admin | `{name, copyData: "none"|"now"|"backup", backupKey?}` → job stream |

Admin for import/clone because installing is admin-only (except deployer keys).

## Edge cases

- Template no longer in the feed: embed the template in the export (it's small) and import it as Custom.
- Static IP on a custom network: clear it on clone, keep it on import to another host with a warning.
- Exports from a newer hlm version: refuse if `hlmExport` is higher than supported.
- Unraid templates with absolute non-appdata paths: keep them, show a warning on import.

## Testing

- Round trip: export → import preview gives the same `deploy.Request` (minus secrets).
- Path rewriting relative/absolute; port shifting avoids used ports.
- Clone copy via opt-in Docker test with busybox.
- Permission tests (Operator can't export secrets).

## Tasks

- [ ] Export format + builder from manager config, backup plan and other per-server settings
- [ ] Import preview + Install page "from file" mode
- [ ] Clone: name/path/port rewrite, data copy helper as job
- [ ] Restore-as-new on Backups tab
- [ ] API + UI
- [ ] README section
