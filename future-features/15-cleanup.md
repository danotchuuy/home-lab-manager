# 15 — Image and disk cleanup

**Priority:** Low · **Depends on:** nothing

> **Status: done** (branch `feature/15-cleanup`). See the README's [Disk cleanup](../README.md#disk-cleanup). Where the
> result differs from this plan:
>
> - **Every Docker host:** cleanup (`GET/POST /api/host/cleanup`) takes `?host=` (feature 14 came first), with a host
>   picker in the panel; the weekly run covers every host. Deleted servers' volumes are only tracked on the local host,
>   so that's the only one where volumes are held back for them.
> - **"Old images"** are all untagged images no container uses (as `docker image prune` does), not only those a
>   managed server used before: Docker doesn't record what used an image.
> - **Leftover `-old-` containers** are only candidates when the server they were renamed from exists again: one left
>   by a failed restore *is* the server.
> - **Remove after update** only deletes an image with no tags left, so an image the user still has a tag on stays;
>   it applies to any successful recreate that changes the image, not only updates. It's on unless
>   `keepOldImages` is set (the setting is stored inverted, so existing installs get the default).
> - **Settings** are in the Host page's Clean up card (`/api/system/cleanup`), not the System page, beside what they
>   affect. Stored in the secrets store under `"cleanup"`, with the last weekly run.
> - **No dry-run flag:** `GET` is the dry run; `POST` re-checks every item against fresh candidates and deletes only
>   those still eligible.

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

- [x] Docker client calls
- [x] Candidate computation + safety rules
- [x] Cleanup endpoint with dry run
- [x] Remove-old-image-after-update option
- [x] Weekly scheduled cleanup
- [x] Host page UI
- [x] README section
