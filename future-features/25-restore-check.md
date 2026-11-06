# 25 — Backup restore check

**Priority:** Medium · **Depends on:** [11](11-backup-improvements.md) (verify, restore as new server),
[05](05-game-query.md) (status query)

## Why

hlm already checks that each backup can be read back whole (**Verify**). That catches a damaged archive, but not a
backup of the wrong folder, a world saved mid-write, a missing file the game needs, or a game version that can no
longer load it. The only real test of a backup is restoring it and starting the game. Doing that by hand is rare, so
it should happen on a schedule, on a copy, with an alert when it fails.

## What it does

Per server, in its backup plan (Manager): **Test a restore** — weekly by default, or after every Nth backup, or never
(the default, since it costs a few minutes of CPU, memory and disk).

A test:

1. Restores the latest backup into a **throwaway copy**: the same image and settings as the server, a new data folder,
   **no published ports**, its own isolated network, and a name like `hlm-restore-test-valheim`.
2. Starts it and waits for it to **answer its status query** inside the network (the same detection as the real
   server), up to the profile's start time (default 10 minutes, since some games take long to load a big world).
3. Records the result — **✓ starts** with how long it took, or **✗** with why (didn't start, exited, never answered)
   and the copy's last 50 log lines — on the backup and in the server's backup history.
4. Removes the copy and its data, whatever happened.

A failure sends `backup.restore_test_failed` (error). **Test a restore now** on any listed backup runs one on demand.

## Design

- Builds on **Restore as new server** (a clone that copies a backup), with a `test` mode:
  - no ports published, no hostnames, tunnel routes, port forwards or proxy relays (none of the server's connections);
  - an internal network made for the test (`docker network create --internal`), so the copy can't reach the internet
    or the LAN, and nothing can reach it except hlm's query;
  - resource limits (CPU and memory) at most the server's own, and the test is skipped if the host's free memory is
    below what the server is using (shown as "skipped: not enough free memory", not as a failure);
  - the image the server runs now (by ID, not tag), so the test doesn't pull.
- The status query runs from hlm against the copy's address on the internal network. For a server on another host,
  hlm queries through that host's agent or the helper container approach used for storage (a small container on the
  same network that runs the query).
- Runs as a `manager.Job` on the server (so it doesn't overlap with its backups or updates), but the server itself
  keeps running throughout.
- Copies are labelled (`io.hlm.restore-test=<server>`) and are cleaned up at start if hlm stopped mid-test; they never
  show in the server list.
- Scheduling follows the backup scheduler: after the backup that makes one due, at most one test running per host at a
  time.
- The game's own first-start work (downloading via SteamCMD in images that update on start) can make tests slow; the
  plan lets the profile or the server set a longer limit, and the result shows the time it took.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `PUT /api/servers/{id}/backups/plan` | serverRole(Manager) | (exists) gains `restoreTest: {every, weekday, timeoutMinutes}` |
| `POST /api/servers/{id}/backups/restore-test` | serverRole(Manager) | `{key}`, like verify; job stream |
| `GET /api/servers/{id}/backups` | serverRole(Viewer) | (exists) each backup gains `restoreTest` |

## Security

- The copy is isolated: no published ports, an internal-only network, no connections, and it's removed after. It runs
  the same image with the same settings, so it holds the same secrets as the server; they never leave the host.
- Logs in the failure notification go through the existing redaction.

## Testing

- With fake Docker: the copy's create request (no port bindings, internal network, labels, limits), cleanup after
  success, failure and timeout, cleanup at start of leftovers, skipping for low memory.
- Scheduling: weekly, every Nth backup, not overlapping, one per host.
- Opt-in Docker test: a small image that serves a TCP port only if a file from the backup is present — passes with a
  good backup, fails with one missing the file.

## Tasks

- [ ] Test mode for restore-as-new: isolated network, no ports or connections, limits, labels
- [ ] Waiting for the status query inside the network (local, then other hosts)
- [ ] Results on backups and in history; notification
- [ ] Scheduling in the backup plan; on-demand button
- [ ] Leftover cleanup at start
- [ ] UI in the Backups tab and backup plan
- [ ] README section and API table
