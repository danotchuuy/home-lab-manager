# 11 — Backup improvements

**Priority:** Medium · **Depends on:** [01](01-notifications.md) for alerts, [03](03-scheduled-tasks.md) time helpers
(optional)

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

- [ ] Streaming SHA-256 + metadata; verify reader; Verify button
- [ ] GFS retention + migration of `Keep`
- [ ] Time-of-day schedules
- [ ] Multiple targets + copy-to
- [ ] Client-side encryption option
- [ ] Exclude patterns
- [ ] Pre/post console hooks
- [ ] Notifications incl. stale backups
- [ ] UI changes on Backups tab and System → Backup storage
- [ ] README updates
