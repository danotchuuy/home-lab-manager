# 15 — Image and disk cleanup

**Priority:** Low · **Depends on:** nothing

## Why

Every update leaves the old image behind (game images are often several GB). Imports, failed installs and removed
servers leave unused images, stopped helper containers and build cache. The Host page already shows Docker's disk
usage, but nothing can reclaim it.

## What it does

**Host → Docker disk usage** gets a **Clean up** panel listing what can be freed, each with its size and a tick box:

| Item | Source | Default |
| --- | --- | --- |
| Old images left by updates (untagged/dangling, previously used by a managed server) | `GET /images/json?filters={"dangling":["true"]}` | ✓ |
| Unused images (not used by any container, managed or not) | images minus those referenced by containers | ✗ (shows names) |
| Stopped hlm helper containers (busybox, update helpers, leftovers from failed recreates `*-old-*`) | label/name filter | ✓ |
| Build cache | `/system/df` BuildCache | ✗ |
| Unused volumes not tracked as a deleted server's | `/volumes?filters=dangling=true` minus `deleted-servers.json` | ✗, separate confirmation |

**Clean up** shows the total, asks to confirm, runs the prunes and reports what was freed.

Options:
- **Remove the old image after a successful update** (System setting, default on): after `manager.Update` succeeds,
  if no container uses the previous image ID, delete it. Keep it if the update had to roll back.
- **Scheduled cleanup**: weekly, dangling images + hlm leftovers only.

## Design

- `internal/docker`: add `ImagesList(filters)`, `RemoveImage(id, force=false)`, `PruneImages(filters)`,
  `PruneBuildCache`, `ListVolumes(filters)`, `RemoveVolume`, `SystemDF` (if not already there for the Host page).
- `internal/manager/cleanup.go`: compute the candidate list (never touches images used by any container; never touches
  volumes recorded as deleted servers' data — those are handled by the existing **Deleted servers** UI).
- Never use `force` on images; a "conflict" (in use) is reported, not overridden.
- Show what *would* be deleted first (dry run is the default response).

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/host/cleanup` | admin | Candidates with sizes |
| `POST /api/host/cleanup` | admin | `{images: [...ids], helpers: true, buildCache: false, volumes: [...]}` → result |
| `GET/PUT /api/system/cleanup` | admin | Remove-after-update, schedule |

## Testing

- Candidate computation with a fake Docker: images used by stopped containers kept; deleted-server volumes kept.
- Remove-after-update: rolled-back update keeps the image.
- Opt-in Docker test: pull busybox two tags, simulate update, check removal.

## Tasks

- [ ] Docker client calls
- [ ] Candidate computation + safety rules
- [ ] Cleanup endpoint with dry run
- [ ] Remove-old-image-after-update option
- [ ] Weekly scheduled cleanup
- [ ] Host page UI
- [ ] README section
