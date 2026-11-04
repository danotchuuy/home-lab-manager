# 03 — Scheduled tasks per server

**Priority:** High · **Depends on:** [02](02-interactive-console.md) for in-game commands · **Unblocks:** [04](04-auto-update-policy.md)

> **Status: done** (branch `feature/03-scheduled-tasks`). See the README's
> [Scheduled tasks](../README.md#scheduled-tasks). Where the result differs from this plan:
>
> - The "only if players online / none online" step is left for [05](05-game-query.md), which provides player counts.
>   Instead, a task can **skip when the server is stopped** (on by default), so a nightly restart doesn't start a
>   server someone stopped.
> - Task runs aren't manager jobs: a running task would block its own Update step, which is a job. The schedule
>   service tracks running tasks itself (shown on the Schedule tab) and waits for jobs and backups, as planned.
> - Restarts and stops are told apart from crashes by Docker's kill event (from 01), so nothing else was needed.
> - Servers can't be renamed (reconfiguring keeps the name), so there's nothing to move.
> - The time zone setting also drives notifications' quiet hours, and `time/tzdata` is built into the binary, since
>   release binaries weren't built with it.
> - Presets: nightly restart with warnings, save every 30 minutes, weekly update, nightly backup, start every evening,
>   stop every night, using the game's own say/save commands when the console knows the game.

## Why

Only backups can be scheduled. Game servers commonly need a nightly restart (memory leaks, world cleanup), a warning
broadcast before it, a periodic `save`, or a planned start/stop (only run the server in the evenings).

## What it does

A **Schedule** tab on each server holding a list of tasks. Each task has a **when** and a list of **steps**.

**When:**
- Daily at a time, on chosen weekdays (most people want this, and it's easy in a form).
- Every N minutes/hours.
- Advanced: a cron expression (5 fields).
- Time zone: hlm's configured zone (new System setting, default the host's `TZ`), shown next to the time.

**Steps** (run in order; a failed step stops the task unless "continue on error" is ticked):

| Step | Notes |
| --- | --- |
| Start / Stop / Restart | Through the manager, so it's tracked as an expected stop (no crash alert) |
| Update (if available) | Same as the **Update** button; skipped when no update |
| Backup now | Runs the server's backup plan, waits for it |
| Send command | Via the console (02): stdin, RCON or exec |
| Wait | 1 s – 60 min |
| Only if players online / none online | Condition using [05](05-game-query.md); stops the task if false |

Presets: "Nightly restart with warnings" = command `say Restarting in 5 minutes` → wait 4 m → `say 1 minute` → wait
1 m → command `save` → restart.

## Design

### Package `internal/schedule`

```go
type Task struct {
    ID       string
    Name     string
    Enabled  bool
    When     When   // {Kind: "daily"|"every"|"cron", Time "04:00", Weekdays []int, Every Duration, Cron string}
    Steps    []Step // {Kind, Command, Wait, ContinueOnError}
    Last     *Run   // started, finished, per-step results, error
    Next     time.Time // computed, not stored
}
type Service struct { store *secrets.Store /* or a JSON file */, servers Actions, console Console, backups Backups, ... }
func (s *Service) Run(ctx context.Context) // minute ticker, like backup.RunScheduler
```

- Stored keyed by server name (survives recreate). Plain JSON in `HLM_DATA_DIR/schedules.json` is fine (commands
  could contain passwords, e.g. `rcon-cli` args, so use the secrets store to be safe — it's what the rest uses).
- Cron parsing: a small in-repo parser (5 fields, ranges, steps, lists, names) instead of a dependency — the project
  uses only the standard library plus a few; keep that. Reuse it for daily/every by translating to the same "next
  time" function.
- **Missed runs:** if hlm was down at the scheduled time, run once on start only if less than 15 minutes late;
  otherwise skip and record "missed".
- **Overlap:** one task per server at a time; a manager job (install/update/reconfigure) or a backup in progress makes
  the task wait up to 10 minutes then fail.
- Each run becomes a manager `Job` of kind `task` so it shows on the server list with progress.
- The update checker and backup scheduler stay as they are; later they can be expressed as tasks.

### Interfaces it calls

`manager.Start/Stop/Restart/Update`, `backup.Service.Start` + wait for completion (add a `Wait(name)` or a completion
channel), `console.Send` (02), `query.Players` (05).

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/servers/{id}/tasks` | Viewer | Tasks, next run, last result (commands hidden from Viewer) |
| `POST /api/servers/{id}/tasks` | Manager | Create |
| `PUT /api/servers/{id}/tasks/{task}` | Manager | Edit / enable / disable |
| `DELETE /api/servers/{id}/tasks/{task}` | Manager | Delete |
| `POST /api/servers/{id}/tasks/{task}/run` | Operator | Run now |
| `GET /api/schedule/upcoming` | signedIn | Next 20 runs across servers the caller can see |
| `GET/PUT /api/system/timezone` | admin | Time zone |

Manager because tasks run unattended with the manager's authority; it matches who can edit the backup plan.

## UI

- Server **Schedule** tab: task cards ("Nightly restart — daily 04:00 — next in 6 h — last OK"), editor with a
  step list (add/reorder/remove), presets menu, **Run now**.
- Servers page: a small clock badge with the next task time.
- System: time zone picker; an "Upcoming" panel.

## Edge cases

- DST: daily 02:30 on a day without 02:30 runs at 03:00; on a day with two, runs once.
- Server deleted: forget its tasks (hook into the delete path like `rbac.ForgetServer`).
- Server renamed via reconfigure (name change): move the tasks.
- Console not configured: "Send command" step fails with a clear message; the editor warns up front.

## Testing

- Cron/next-time table tests incl. DST transitions (use `time.LoadLocation("Europe/London")` etc.).
- Service with a fake clock and fake actions: order of steps, wait, continue-on-error, overlap with a running job,
  missed runs.
- API permission tests (Viewer can't see commands).

## Tasks

- [ ] Cron parser + next-time function + tests
- [ ] `internal/schedule` model, store, runner with fake clock
- [ ] `backup.Service` completion wait
- [ ] Task runs as manager jobs (kind `task`)
- [ ] Time zone setting
- [ ] API routes
- [ ] Schedule tab, presets, badge, upcoming panel
- [ ] Forget/move tasks on delete/rename
- [ ] README section
