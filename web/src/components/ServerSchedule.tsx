import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { ConsoleStatus, ScheduledTask, ServerTasks, StepKind, TaskRun, TaskStep, TaskWhen } from "../types";
import { usePoll } from "../usePoll";
import { formatInZone, timeUntil } from "../util";
import { Ago, Spinner } from "./common";

const DAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

const STEP_LABEL: Record<StepKind, string> = {
  start: "Start the server",
  stop: "Stop the server",
  restart: "Restart the server",
  update: "Update, if there's an update",
  backup: "Back up (the server's backup plan)",
  command: "Send a console command",
  wait: "Wait",
  if_players: "Only carry on if…",
};

export function describeWhen(w: TaskWhen): string {
  switch (w.kind) {
    case "daily":
      return `${w.weekdays?.length ? w.weekdays.map((d) => DAYS[d]).join(", ") : "Every day"} at ${w.time}`;
    case "every":
      if (!w.every) return "";
      return w.every % 60 === 0 ? (w.every === 60 ? "Every hour" : `Every ${w.every / 60} hours`) : `Every ${w.every} minutes`;
    case "cron":
      return `Cron: ${w.cron}`;
  }
}

function describeStep(s: TaskStep): string {
  switch (s.kind) {
    case "command":
      return s.command ? `Send "${s.command}"` : "Send a console command";
    case "wait":
      return `Wait ${formatSeconds(s.seconds ?? 0)}`;
    case "if_players":
      return s.players === "some" ? "Only carry on if someone's playing" : "Only carry on if nobody's playing";
    default:
      return STEP_LABEL[s.kind];
  }
}

function formatSeconds(s: number): string {
  if (s % 60 === 0) return `${s / 60} min`;
  return s > 60 ? `${Math.floor(s / 60)} min ${s % 60} s` : `${s} s`;
}

/** Games' commands for warning players and saving, for the presets. */
/** Presets, with the game's own commands to warn players and save (from its profile), or the usual ones. */
function presets(game?: { say: string; save: string }): { label: string; task: ScheduledTask }[] {
  const g = game ?? { say: "say", save: "save" };
  const base = { id: "", enabled: true, onlyIfRunning: true };
  const warn = (text: string): TaskStep => ({ kind: "command", command: `${g.say} ${text}`, continueOnError: true });
  return [
    {
      label: "Nightly restart, warning players first",
      task: {
        ...base,
        name: "Nightly restart",
        when: { kind: "daily", time: "04:00" },
        steps: [
          warn("Restarting in 5 minutes"),
          { kind: "wait", seconds: 240 },
          warn("Restarting in 1 minute"),
          { kind: "wait", seconds: 60 },
          ...(g.save ? [{ kind: "command" as const, command: g.save, continueOnError: true }] : []),
          { kind: "restart" },
        ],
      },
    },
    {
      label: "Save every 30 minutes",
      task: { ...base, name: "Save", when: { kind: "every", every: 30 }, steps: [{ kind: "command", command: g.save || "save" }] },
    },
    {
      label: "Update weekly when there's an update",
      task: {
        ...base,
        name: "Weekly update",
        when: { kind: "daily", time: "05:00", weekdays: [1] },
        steps: [warn("Updating in 5 minutes"), { kind: "wait", seconds: 300 }, { kind: "update" }],
      },
    },
    {
      label: "Back up every night",
      task: { ...base, name: "Nightly backup", onlyIfRunning: false, when: { kind: "daily", time: "03:00" }, steps: [{ kind: "backup" }] },
    },
    {
      label: "Start every evening",
      task: { ...base, name: "Evening start", onlyIfRunning: false, when: { kind: "daily", time: "18:00" }, steps: [{ kind: "start" }] },
    },
    {
      label: "Stop every night, warning players first",
      task: {
        ...base,
        name: "Night stop",
        when: { kind: "daily", time: "23:00" },
        steps: [
          warn("Stopping in 5 minutes"),
          { kind: "wait", seconds: 300 },
          ...(g.save ? [{ kind: "command" as const, command: g.save, continueOnError: true }] : []),
          { kind: "stop" },
        ],
      },
    },
    {
      label: "Blank task",
      task: { ...base, name: "", when: { kind: "daily", time: "04:00" }, steps: [{ kind: "restart" }] },
    },
  ];
}

const RESULT_LABEL: Record<TaskRun["result"], string> = { ok: "OK", failed: "Failed", skipped: "Skipped", missed: "Missed" };

/** A server's scheduled tasks: when they run, how they went, and an editor for managers. */
export function ServerSchedule({ server, canRun, canManage }: { server: string; canRun: boolean; canManage: boolean }) {
  const [data, setData] = useState<ServerTasks>();
  const [consoleStatus, setConsoleStatus] = useState<ConsoleStatus>();
  const [editing, setEditing] = useState<ScheduledTask>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const [loadError, setLoadError] = useState<string>();
  const load = useCallback(async () => {
    try {
      setData(await api.tasks(server));
      setLoadError(undefined);
    } catch (e) {
      setLoadError((e as Error).message);
    }
  }, [server]);
  usePoll(load, data?.running ? 2000 : 15000, [load, !!data?.running]);
  useEffect(() => {
    // Operators can see how the console is set up; the presets use the game it knows.
    if (canRun) api.console(server).then(setConsoleStatus, () => {});
  }, [server, canRun]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  if (!data) return loadError ? <div className="alert error">{loadError}</div> : <Spinner />;
  const tz = data.timezone;

  if (editing) {
    return (
      <div className="card">
        <TaskEditor
          initial={editing}
          timezone={tz}
          consoleReady={consoleStatus ? consoleStatus.settings.mode !== "" : undefined}
          busy={busy}
          error={error}
          onCancel={() => {
            setEditing(undefined);
            setError(undefined);
          }}
          onSave={(t) =>
            run("save", async () => {
              await api.saveTask(server, t);
              setEditing(undefined);
              setNotice(`Saved ${t.name}.`);
              await load();
            })
          }
        />
      </div>
    );
  }

  const toggle = (t: ScheduledTask) =>
    run("toggle", async () => {
      await api.saveTask(server, { ...t, enabled: !t.enabled });
      await load();
    });
  const remove = (t: ScheduledTask) => {
    if (!confirm(`Delete the task ${t.name}?`)) return;
    run("delete", async () => {
      await api.deleteTask(server, t.id);
      await load();
    });
  };
  const runNow = (t: ScheduledTask) =>
    run("run", async () => {
      await api.runTask(server, t.id);
      setNotice(`Started ${t.name}.`);
      await load();
    });

  return (
    <div className="card schedule">
      <div className="settings-actions">
        <div>
          <h2>Scheduled tasks</h2>
          <p className="muted small">
            Restart, update, back up or send console commands at set times. Times are in hlm&apos;s time zone,{" "}
            <strong>{tz}</strong>.
          </p>
        </div>
        {canManage && (
          <select
            className="preset-select"
            value=""
            onChange={(e) => {
              const p = presets(consoleStatus?.schedule)[Number(e.target.value)];
              if (p) setEditing(structuredClone(p.task));
            }}
          >
            <option value="">Add a task…</option>
            {presets(consoleStatus?.schedule).map((p, i) => (
              <option key={p.label} value={i}>
                {p.label}
              </option>
            ))}
          </select>
        )}
      </div>

      {data.running && (
        <div className="alert info small">
          <Spinner /> Running <strong>{data.running.taskName}</strong>: step {data.running.step + 1} of {data.running.steps}
        </div>
      )}
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}

      {data.tasks.length === 0 ? (
        <p className="muted">No scheduled tasks.{canManage ? " Add one with the menu above." : ""}</p>
      ) : (
        <div className="task-list">
          {data.tasks.map((t) => {
            const last = t.runs?.[0];
            return (
              <div key={t.id} className={`task-card${t.enabled ? "" : " disabled"}`}>
                <div className="task-head">
                  <div>
                    <strong>{t.name}</strong>{" "}
                    {!t.enabled && <span className="badge">off</span>}
                    <div className="muted small">
                      {describeWhen(t.when)}
                      {t.onlyIfRunning && " · only while running"}
                    </div>
                  </div>
                  <div className="btn-row">
                    {canRun && (
                      <button className="btn small" onClick={() => runNow(t)} disabled={!!busy || !!data.running}>
                        Run now
                      </button>
                    )}
                    {canManage && (
                      <>
                        <button className="btn small" onClick={() => setEditing(structuredClone(t))} disabled={!!busy}>
                          Edit
                        </button>
                        <button className="btn small ghost" onClick={() => toggle(t)} disabled={!!busy}>
                          {t.enabled ? "Turn off" : "Turn on"}
                        </button>
                        <button className="btn small ghost danger" onClick={() => remove(t)} disabled={!!busy}>
                          Delete
                        </button>
                      </>
                    )}
                  </div>
                </div>
                <ol className="task-steps small">
                  {t.steps.map((s, i) => (
                    <li key={i}>
                      {describeStep(s)}
                      {s.continueOnError && <span className="muted"> (carry on if it fails)</span>}
                    </li>
                  ))}
                </ol>
                <div className="small task-times">
                  <span>
                    Next:{" "}
                    {t.next ? (
                      <>
                        {formatInZone(t.next, tz)} <span className="muted">({timeUntil(t.next)})</span>
                      </>
                    ) : (
                      <span className="muted">not scheduled</span>
                    )}
                  </span>
                  {last && (
                    <span>
                      Last: <span className={`run-${last.result}`}>{RESULT_LABEL[last.result]}</span>{" "}
                      <span className="muted" title={formatInZone(last.started, tz)}>
                        <Ago iso={last.started} />
                        {last.trigger === "manual" ? ", run by hand" : ""}
                      </span>
                    </span>
                  )}
                </div>
                {last?.error && <div className={`small ${last.result === "ok" ? "muted" : "form-error"}`}>{last.error}</div>}
                {t.runs && t.runs.length > 0 && <RunHistory runs={t.runs} tz={tz} />}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function RunHistory({ runs, tz }: { runs: TaskRun[]; tz: string }) {
  return (
    <details className="small run-history">
      <summary>Recent runs</summary>
      <ul>
        {runs.map((r, i) => (
          <li key={i}>
            <span className={`run-${r.result}`}>{RESULT_LABEL[r.result]}</span> {formatInZone(r.started, tz)}
            {r.trigger === "manual" && " (by hand)"}
            {r.error && <span className="muted"> · {r.error}</span>}
            {r.steps && r.steps.length > 0 && (
              <ol>
                {r.steps.map((s, j) => (
                  <li key={j}>
                    {s.ok ? "✓" : s.skipped ? "–" : "✗"} {STEP_LABEL[s.kind]}
                    {s.message && <span className="muted"> · {s.message}</span>}
                  </li>
                ))}
              </ol>
            )}
          </li>
        ))}
      </ul>
    </details>
  );
}

function TaskEditor({
  initial,
  timezone,
  consoleReady,
  busy,
  error,
  onSave,
  onCancel,
}: {
  initial: ScheduledTask;
  timezone: string;
  /** Whether the server's console is set up; undefined if unknown. */
  consoleReady?: boolean;
  busy?: string;
  error?: string;
  onSave: (t: ScheduledTask) => void;
  onCancel: () => void;
}) {
  const [t, setT] = useState<ScheduledTask>(initial);
  const [everyUnit, setEveryUnit] = useState<"minutes" | "hours">(
    initial.when.every && initial.when.every % 60 === 0 ? "hours" : "minutes",
  );
  const set = <K extends keyof ScheduledTask>(k: K, v: ScheduledTask[K]) => setT((old) => ({ ...old, [k]: v }));
  const setWhen = (w: Partial<TaskWhen>) => setT((old) => ({ ...old, when: { ...old.when, ...w } }));
  const setStep = (i: number, s: Partial<TaskStep>) =>
    setT((old) => ({ ...old, steps: old.steps.map((x, j) => (j === i ? { ...x, ...s } : x)) }));
  const move = (i: number, by: number) =>
    setT((old) => {
      const steps = [...old.steps];
      const [s] = steps.splice(i, 1);
      steps.splice(i + by, 0, s);
      return { ...old, steps };
    });
  const everyShown = t.when.every ? (everyUnit === "hours" ? t.when.every / 60 : t.when.every) : "";
  const usesConsole = t.steps.some((s) => s.kind === "command");

  return (
    <form
      className="config-form task-editor"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(t);
      }}
    >
      <h2>{t.id ? `Edit ${initial.name}` : "New task"}</h2>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Name</span>
          <input value={t.name} onChange={(e) => set("name", e.target.value)} maxLength={64} required placeholder="Nightly restart" />
        </label>
        <label className="field">
          <span className="field-label">Runs</span>
          <select value={t.when.kind} onChange={(e) => setWhen({ kind: e.target.value as TaskWhen["kind"] })}>
            <option value="daily">At a time of day</option>
            <option value="every">Every so often</option>
            <option value="cron">On a cron schedule</option>
          </select>
        </label>
        {t.when.kind === "daily" && (
          <label className="field">
            <span className="field-label">At ({timezone})</span>
            <input type="time" value={t.when.time ?? ""} onChange={(e) => setWhen({ time: e.target.value })} required />
          </label>
        )}
        {t.when.kind === "every" && (
          <label className="field">
            <span className="field-label">Every</span>
            <span className="inline-input">
              <input
                type="number"
                min={everyUnit === "hours" ? 1 : 5}
                value={everyShown}
                onChange={(e) => {
                  const n = Number(e.target.value);
                  setWhen({ every: n ? (everyUnit === "hours" ? n * 60 : n) : undefined });
                }}
                required
              />
              <select
                value={everyUnit}
                onChange={(e) => {
                  const unit = e.target.value as "minutes" | "hours";
                  setEveryUnit(unit);
                  if (t.when.every) setWhen({ every: unit === "hours" ? Math.max(1, Math.round(t.when.every / 60)) * 60 : t.when.every });
                }}
              >
                <option value="minutes">minutes</option>
                <option value="hours">hours</option>
              </select>
            </span>
            <span className="field-help">Counted from when the task is saved.</span>
          </label>
        )}
        {t.when.kind === "cron" && (
          <label className="field">
            <span className="field-label">Cron expression ({timezone})</span>
            <input value={t.when.cron ?? ""} onChange={(e) => setWhen({ cron: e.target.value })} placeholder="30 4 * * 1-5" required spellCheck={false} />
            <span className="field-help">minute hour day-of-month month day-of-week, e.g. 0 */6 * * * for every 6 hours.</span>
          </label>
        )}
      </div>
      {t.when.kind === "daily" && (
        <div className="field">
          <span className="field-label">On</span>
          <div className="weekday-picker">
            {DAYS.map((d, i) => (
              <label key={d} className="radio small">
                <input
                  type="checkbox"
                  checked={!t.when.weekdays?.length || t.when.weekdays.includes(i)}
                  onChange={(e) => {
                    const all = t.when.weekdays?.length ? t.when.weekdays : [0, 1, 2, 3, 4, 5, 6];
                    const next = e.target.checked ? [...all, i] : all.filter((x) => x !== i);
                    setWhen({ weekdays: next.length === 7 ? [] : next.sort() });
                  }}
                />
                {d}
              </label>
            ))}
          </div>
        </div>
      )}

      <fieldset className="config-section">
        <legend>Steps, in order</legend>
        {usesConsole && consoleReady === false && (
          <div className="alert warn small">
            This server&apos;s console isn&apos;t set up, so command steps will fail. Set it up under the Console tab first.
          </div>
        )}
        <ol className="step-editor">
          {t.steps.map((s, i) => (
            <li key={i}>
              <select value={s.kind} onChange={(e) => setStep(i, { kind: e.target.value as StepKind })}>
                {(Object.keys(STEP_LABEL) as StepKind[]).map((k) => (
                  <option key={k} value={k}>
                    {STEP_LABEL[k]}
                  </option>
                ))}
              </select>
              {s.kind === "command" && (
                <input
                  className="grow"
                  value={s.command ?? ""}
                  onChange={(e) => setStep(i, { command: e.target.value })}
                  placeholder="say Restarting soon"
                  maxLength={1024}
                  required
                  spellCheck={false}
                />
              )}
              {s.kind === "if_players" && (
                <select value={s.players ?? "none"} onChange={(e) => setStep(i, { players: e.target.value as TaskStep["players"] })}>
                  <option value="none">nobody&apos;s playing</option>
                  <option value="some">someone&apos;s playing</option>
                </select>
              )}
              {s.kind === "wait" && (
                <span className="inline-input">
                  <input
                    type="number"
                    min={1}
                    max={3600}
                    value={s.seconds ?? ""}
                    onChange={(e) => setStep(i, { seconds: Number(e.target.value) || undefined })}
                    required
                  />
                  seconds{s.seconds && s.seconds >= 60 ? ` (${formatSeconds(s.seconds)})` : ""}
                </span>
              )}
              {s.kind !== "wait" && s.kind !== "if_players" && (
                <label className="radio small">
                  <input
                    type="checkbox"
                    checked={!!s.continueOnError}
                    onChange={(e) => setStep(i, { continueOnError: e.target.checked })}
                  />
                  carry on if it fails
                </label>
              )}
              <span className="step-buttons">
                <button type="button" className="btn small ghost" onClick={() => move(i, -1)} disabled={i === 0} title="Move up">
                  ↑
                </button>
                <button type="button" className="btn small ghost" onClick={() => move(i, 1)} disabled={i === t.steps.length - 1} title="Move down">
                  ↓
                </button>
                <button
                  type="button"
                  className="btn small ghost danger"
                  onClick={() => set("steps", t.steps.filter((_, j) => j !== i))}
                  title="Remove"
                >
                  ✕
                </button>
              </span>
            </li>
          ))}
        </ol>
        <button
          type="button"
          className="link-button"
          onClick={() => set("steps", [...t.steps, { kind: "command", command: "" }])}
          disabled={t.steps.length >= 20}
        >
          Add a step
        </button>
      </fieldset>

      <label className="radio small">
        <input type="checkbox" checked={t.onlyIfRunning} onChange={(e) => set("onlyIfRunning", e.target.checked)} />
        Skip the task when the server is stopped (so a restart doesn&apos;t start a server someone stopped)
      </label>
      <label className="radio small">
        <input type="checkbox" checked={t.enabled} onChange={(e) => set("enabled", e.target.checked)} />
        Turned on
      </label>

      {error && <div className="alert error small">{error}</div>}
      <div className="btn-row">
        <button type="submit" className="btn small primary" disabled={!!busy || t.steps.length === 0}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button type="button" className="btn small ghost" onClick={onCancel} disabled={!!busy}>
          Cancel
        </button>
      </div>
    </form>
  );
}
