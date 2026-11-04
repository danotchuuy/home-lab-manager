# 04 — Auto-update policy

**Priority:** High · **Depends on:** [03](03-scheduled-tasks.md) (runner and time windows), [05](05-game-query.md)
(player check, optional), [01](01-notifications.md) (notify-only mode)

> **Status: done** (branch `feature/04-auto-update-policy`). See the README's
> [Automatic updates](../README.md#automatic-updates). Where the result differs from this plan:
>
> - Two modes, not three: **manual** and **automatic**. "Notify" is already covered by 01: the
>   `server.update_available` notification is sent once per image to targets that choose it, so a policy mode for it
>   would duplicate that.
> - "Only when empty" is left for [05](05-game-query.md), which provides player counts.
> - After a failed automatic update, the next try waits a day rather than "never for this digest": the update tracker
>   doesn't keep remote digests, and a day is long enough not to loop on a broken image while still catching a fixed
>   one. The registry is checked again just before each update.
> - The policy lives on the server's Settings tab (with the Update button), and the System section is called
>   "Automatic updates".
> - hlm's own automatic update also waits while any server has an automatic update ready, so servers go first.

## Why

`manager.RunUpdateChecker` finds updates every 12 hours, but applying them is always a click. Many game images publish
updates often and clients refuse to connect to an out-of-date server, so a server that isn't updated is effectively
down for players.

## What it does

Each server gets an **Updates** setting:

| Policy | Behaviour |
| --- | --- |
| **Manual** (default; today's behaviour) | Show the badge |
| **Notify** | Send `server.update_available` (01) once per new digest |
| **Automatic** | Apply the update in the chosen window |

Automatic options:
- **Window:** any time, or a daily window (e.g. 03:00–06:00 local).
- **Only when empty:** wait until no players are online (05), up to the end of the window.
- **Warn players first:** send a console command N minutes before (02), e.g. `say Server updating in 5 minutes`.
- **Back up first:** run the backup plan before updating (on by default when a plan exists).
- **Pin:** don't auto-update across a tag change — only digests of the current tag (that's how updates already work).

Global default on System → Updates, overridable per server. Also: "Check for updates every" moves from `HLM_UPDATE_CHECK`
env to a UI setting (env still wins if set).

## Design

- Policy stored per server name (secrets store `"updatepolicy"` or alongside schedule tasks).
- After `CheckUpdates`, for each server with an update:
  - Notify: emit once per (server, digest).
  - Automatic: mark "pending auto-update" with the earliest start time = next window opening.
- A loop (or the schedule runner from 03) picks pending servers when the window is open and:
  1. if "only when empty" and players > 0 → retry in 5 minutes until the window closes;
  2. warn (console) → wait;
  3. backup (wait for it; a failed backup cancels the update and notifies);
  4. `manager.Update` (already rolls back if the new container doesn't start);
  5. notify success/failure.
- At most one auto-update at a time host-wide (pulls are big), queue the rest.
- After a failed auto-update for a digest, don't retry it automatically; show "Auto-update failed — update manually".
- The same option for hlm itself: System → "Update hlm automatically" in a window, using `selfupdate`, never while a
  server job runs (it already refuses).

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/update-policy` | Viewer | |
| `PUT /api/servers/{id}/update-policy` | Manager | |
| `GET/PUT /api/system/update-policy` | admin | Defaults, check interval, hlm self-update policy |
| `GET /api/updates` (existing) | — | Add `pending` and `policy` fields |

## UI

- Server **Settings** tab: "Updates" section.
- Servers list: update badge shows "auto at 03:00" / "waiting for players to leave".
- System → Updates: defaults, check interval, hlm auto-update.

## Testing

- Fake clock + fake manager/query/console: window open/closed, players present until window closes, backup failure
  cancels, notify once per digest, one-at-a-time queue.
- API permissions.

## Tasks

- [ ] Policy model + store + defaults
- [ ] Hook into `CheckUpdates` results (new digests only)
- [ ] Pending queue + window logic (reuse 03's time helpers)
- [ ] Steps: player wait, warn, backup, update, notify
- [ ] hlm self auto-update option
- [ ] Check-interval UI setting
- [ ] API + UI
- [ ] README: update section
