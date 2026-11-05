# 11 — Backup improvements

**Priority:** Medium · **Depends on:** [01](01-notifications.md) for alerts, [03](03-scheduled-tasks.md) time helpers
(optional)

> **Status: done** (branch `feature/11-backup-improvements`). See the README's
> [Backups to S3](../README.md#backups-to-s3-seaweedfs-minio-). Where the result differs from this plan:
>
> - **Checksum:** stored beside each backup as `<backup>.sha256` (in `sha256sum`'s format), not as object metadata. S3
>   metadata is set when an upload starts, and the checksum is only known once it ends; setting it afterwards needs a
>   server-side copy of the whole object. The sidecar also lets anyone check a download by hand. Pruning and deleting
>   remove it too.
> - **Verify modes:** under 5 GB (default), always, or off. "Only the newest weekly" was left out: **Verify** on any
>   listed backup covers checking older ones. A backup that fails its check counts as failed.
> - **Schedule at a time:** a time of day and weekdays (`At`), in hlm's time zone. Not 03's `When` type: `schedule`
>   imports `backup`, so `backup` can't import it, and cron or "every N minutes" add nothing for backups. Failed
>   backups are retried after an hour, as before.
> - **Copy to another target:** always streamed from the primary through hlm (decrypting and re-encrypting as each
>   target says), rather than a server-side copy when the endpoint is the same. That's simpler, works between any two
>   targets, and keys differ per target anyway. One copy target per plan.
> - **Targets:** the existing settings endpoints still manage the primary (now `targets[0]`, migrated from the old
>   single settings on first save). New `/api/integrations/backup/targets` endpoints manage the rest. Adding or
>   changing a target needs a confirmed session (it holds credentials); reading a key does too.
> - **Encryption:** chunked AES-256-GCM from Go's standard library (no age dependency), with a final-chunk flag
>   against truncation. The checksum is of the stored (encrypted) bytes. Download, restore and verify decrypt;
>   `hlm decrypt-backup` decrypts without hlm. Configuration backups go to the primary and are encrypted if it is.
> - **Exclude patterns:** applied while archiving, from the host or through Docker's archive API. The rules are in
>   `files/exclude.go`.
> - **Hooks:** several commands per hook (one per line). They're sent only while the server runs, and a failure is a
>   warning, not a failed backup.
> - **Stale backups:** no setting. A scheduled plan is stale after twice its schedule's longest gap (at least 36
>   hours); checked hourly, and cleared by the next success.

Builds on `internal/backup` (plans keyed by server name, minute scheduler, multipart upload to S3, `prune` keeping the
newest N). Backup storage is S3-compatible, typically SeaweedFS; keep path-style addressing the default.

## 1. Verify after upload

Today a backup is "OK" when the upload completes. Add an optional **Verify** step:

- After upload, `HEAD` the object (size matches the bytes streamed) and stream it back through gzip + tar reader,
  checking every header and that the entry count matches what was written. No disk staging (same as upload).
- Record `verified: true/false` and duration on the `Run`; show a tick on the Backups tab.
- Plan option: verify every backup / only the newest weekly / off (default: every backup under 5 GB).
- Also a **Verify** button on any listed backup.
- Store a SHA-256 of the archive as object metadata (`x-amz-meta-sha256`) computed while streaming; verify compares it.

## 2. Smarter retention (GFS)

Replace the single "keep N" with an optional policy:

| Keep | Default |
| --- | --- |
| Last N | 3 |
| Daily (one per day) | 7 |
| Weekly | 4 |
| Monthly | 6 |

A backup is kept if any rule keeps it. `prune` becomes "compute keep set from timestamps in keys, delete the rest".
"Keep N" stays as the simple mode. Also apply to config backups.

## 3. Schedule at a time

`IntervalHours` drifts from when the plan was saved. Add "daily at 04:00" / weekdays (reuse 03's schedule type). Keep
interval mode.

## 4. Second destination (copy)

- **System → Backup storage** can hold more than one target (`Settings` becomes a list with an ID and name; the
  existing one becomes "primary", migration on load).
- Per plan: **Also copy to** another target. After a successful (and verified) upload, server-side copy if same
  endpoint, else stream from primary to secondary.
- Retention applied per target (can differ: keep 7 locally, 30 off-site).
- Restore/download let you pick the target.

## 5. Encryption at rest (client-side)

Optional per target: encrypt archives with age-style (X25519 + ChaCha20-Poly1305 streaming) or AES-GCM chunked
encryption before upload, key stored in the secrets store (and shown once for safekeeping). Useful for off-site targets
(B2, R2, AWS). The config backup's readme should explain the key.

## 6. Exclude patterns

Per plan glob excludes (`*.log`, `cache/`, `steamapps/` for SteamCMD game files that are re-downloadable). Applied in
the tar writer and the Docker archive path (filter entries as they stream).

## 7. Pre/post hooks

Per plan: console command before (e.g. `save-off` + `save-all flush`) and after (`save-on`), using [02](02-interactive-console.md).
Gives consistent Minecraft backups without stopping the server.

## 8. Notifications

`backup.failed`, `backup.verify_failed`, and "no successful backup in X days" (stale) via 01.

## API changes

- `Plan`: `verify`, `retention {last,daily,weekly,monthly}`, `schedule`, `copyTo`, `exclude[]`, `preCommand`, `postCommand`.
- `POST /api/servers/{id}/backups/verify?key=` (Operator).
- `GET/POST/PUT/DELETE /api/system/backup/targets` (admin/confirmed) — migrate the single settings endpoint.

## Testing

- GFS keep-set tables (edge: several backups per day, gaps, time zones).
- Verify against SeaweedFS in the opt-in Docker test (small `-master.volumeSizeLimitMB` so it doesn't preallocate GBs);
  corrupt an object → verify fails.
- Exclude patterns over tar streams.
- Settings migration single → list.

## Tasks

- [x] Streaming SHA-256 + metadata; verify reader; Verify button
- [x] GFS retention + migration of `Keep`
- [x] Time-of-day schedules
- [x] Multiple targets + copy-to
- [x] Client-side encryption option
- [x] Exclude patterns
- [x] Pre/post console hooks
- [x] Notifications incl. stale backups
- [x] UI changes on Backups tab and System → Backup storage
- [x] README updates
