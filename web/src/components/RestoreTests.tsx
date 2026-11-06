import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { DeployEvent, RestoreTestResult, RestoreTests as State, RestoreTestSettings } from "../types";
import { Ago, Spinner } from "./common";
import { DeployProgress } from "./DeployProgress";

const OUTCOME: Record<RestoreTestResult["outcome"], { label: string; cls: string }> = {
  passed: { label: "✓ Restores and starts", cls: "state-running" },
  failed: { label: "✗ Failed", cls: "state-dead" },
  skipped: { label: "Skipped", cls: "" },
};

const baseName = (key: string) => key.split("/").pop() ?? key;

function took(seconds?: number) {
  if (seconds === undefined) return "";
  if (seconds < 90) return `${seconds} s`;
  return `${Math.round(seconds / 60)} min`;
}

/**
 * A server's restore tests: a backup restored into an isolated, throwaway
 * copy of the server, started, and checked to answer its status query.
 */
export function RestoreTests({
  server,
  backups,
  canManage,
}: {
  server: string;
  /** The backups' keys, newest first. */
  backups: string[];
  canManage: boolean;
}) {
  const [state, setState] = useState<State>();
  const [form, setForm] = useState<RestoreTestSettings>();
  const [key, setKey] = useState("");
  const [events, setEvents] = useState<DeployEvent[]>();
  const [running, setRunning] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [open, setOpen] = useState<number>();

  const load = useCallback(
    () =>
      api.restoreTests(server).then(
        (s) => {
          setState(s);
          setForm((f) => f ?? s.settings);
        },
        (e) => setError((e as Error).message),
      ),
    [server],
  );
  useEffect(() => {
    load();
  }, [load]);

  if (!state || !form)
    return <div className="card">{error ? <div className="alert error">{error}</div> : <Spinner />}</div>;

  async function save() {
    if (!form) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      setForm(await api.setRestoreTests(server, form));
      setNotice("Saved.");
      load();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  }

  async function testNow() {
    setRunning(true);
    setError(undefined);
    setNotice(undefined);
    const got: DeployEvent[] = [];
    setEvents([]);
    try {
      await api.runRestoreTest(server, key, (e) => {
        got.push(e);
        setEvents([...got]);
      });
    } catch (e) {
      got.push({ stage: "error", message: (e as Error).message });
      setEvents([...got]);
    }
    setRunning(false);
    load();
  }

  const s = state.settings;
  const schedule =
    s.every === "week"
      ? "about once a week, after a new backup"
      : s.every === "backups"
        ? `after every ${s.backups === 1 ? "" : `${s.backups}th `}backup`
        : "";
  const dirty = JSON.stringify(form) !== JSON.stringify(s);

  return (
    <div className="card restore-tests">
      <h2>Restore tests</h2>
      <p className="muted small">
        A restore test restores a backup into a throwaway copy of this server and starts it, to check the game really
        loads it. The copy runs the same image and settings with its own, empty data folders, no published ports, and a
        network of its own that reaches neither the internet nor your LAN. hlm waits for its game to answer its status
        query, then removes the copy and its data, whatever happened. This server keeps running throughout. Since the
        copy can't download anything, a backup has to hold everything the server needs to start: a game that downloads
        its files when it first starts fails the test if they aren't in the backup.
      </p>
      {schedule && <p className="small">Tested {schedule}.</p>}

      {state.results.length === 0 ? (
        <p className="muted small">No tests yet.</p>
      ) : (
        <div className="restore-test-results">
          {state.results.map((r, i) => (
            <div key={i} className="restore-test-result">
              <div className="restore-test-row">
                <span className={`badge ${OUTCOME[r.outcome].cls}`}>{OUTCOME[r.outcome].label}</span>
                <code className="file-name" title={r.key}>
                  {baseName(r.key)}
                </code>
                <span className="muted small">
                  <Ago iso={r.finished} /> · {r.trigger === "schedule" ? "scheduled" : "by hand"}
                  {r.outcome === "passed" && r.seconds !== undefined && <> · answered after {took(r.seconds)}</>}
                </span>
              </div>
              {r.answer && <div className="small muted">The copy said: {r.answer}</div>}
              {r.error && <div className="small run-failed">{r.error}</div>}
              {r.note && <div className="small muted">{r.note}</div>}
              {r.log && (
                <>
                  <button className="btn ghost small" onClick={() => setOpen(open === i ? undefined : i)}>
                    {open === i ? "Hide the copy's log" : "The copy's log"}
                  </button>
                  {open === i && <pre className="op-output">{r.log}</pre>}
                </>
              )}
            </div>
          ))}
        </div>
      )}

      {canManage && (
        <>
          <h3>Test a restore now</h3>
          <div className="btn-row">
            <select value={key} onChange={(e) => setKey(e.target.value)} aria-label="Backup to test" disabled={running}>
              <option value="">The newest backup</option>
              {backups.map((k) => (
                <option key={k} value={k}>
                  {baseName(k)}
                </option>
              ))}
            </select>
            <button className="btn small primary" onClick={testNow} disabled={running || backups.length === 0}>
              {running ? "Testing…" : "Test a restore"}
            </button>
          </div>
          {events && (
            <div className="restore-test-progress">
              <DeployProgress events={events} />
              {!running && (
                <button className="btn small" onClick={() => setEvents(undefined)}>
                  Close
                </button>
              )}
            </div>
          )}

          <h3>Test on a schedule</h3>
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Test a backup</span>
              <select
                value={form.every}
                onChange={(e) =>
                  setForm({
                    ...form,
                    every: e.target.value as RestoreTestSettings["every"],
                    backups: e.target.value === "backups" ? form.backups || 1 : undefined,
                  })
                }
              >
                <option value="">Never (the default)</option>
                <option value="week">Once a week</option>
                <option value="backups">After every few backups</option>
              </select>
            </label>
            {form.every === "backups" && (
              <label className="field">
                <span className="field-label">After every … backups</span>
                <input
                  type="number"
                  min={1}
                  max={100}
                  value={form.backups ?? 1}
                  onChange={(e) => setForm({ ...form, backups: Number(e.target.value) })}
                />
              </label>
            )}
            <label className="field">
              <span className="field-label">Minutes it gets to answer</span>
              <input
                type="number"
                min={1}
                max={120}
                value={form.timeoutMinutes || 10}
                onChange={(e) => setForm({ ...form, timeoutMinutes: Number(e.target.value) })}
              />
            </label>
          </div>
          <p className="muted small">
            A test takes a few minutes of CPU, memory and disk: as much as the server needs to start. It&apos;s skipped
            when the host has less free memory than the server uses. A failed test sends the{" "}
            <code>backup.restore_test_failed</code> notification.
          </p>
          {error && <div className="alert error small">{error}</div>}
          {notice && <div className="alert info small">{notice}</div>}
          <div className="btn-row">
            <button className="btn small" onClick={save} disabled={busy || !dirty}>
              {busy ? "Saving…" : "Save"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}
