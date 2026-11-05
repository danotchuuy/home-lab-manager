import { useEffect, useState } from "react";
import { api } from "../api";
import type { DockerHealthcheck, HealthCheck, HealthCheckKind, HealthSettings, ServerHealth } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

/** A container's check, in words. */
function describeCheck(hc?: DockerHealthcheck): string {
  const test = hc?.Test;
  if (!test || test.length === 0) return "none";
  if (test[0] === "NONE") return "turned off";
  if (test[0] === "CMD-SHELL") return test.slice(1).join(" ");
  if (test[0] === "CMD") return test.slice(1).join(" ");
  return test.join(" ");
}

const KINDS: { kind: HealthCheckKind; label: string }[] = [
  { kind: "", label: "The image's own check, if it has one" },
  { kind: "tcp", label: "A TCP port accepts connections" },
  { kind: "http", label: "A web page answers (HTTP 2xx or 3xx)" },
  { kind: "command", label: "A command succeeds" },
  { kind: "none", label: "No health check (turn off the image's)" },
];

function NumberInput({
  value,
  onChange,
  min,
  max,
  placeholder,
  zero,
}: {
  value?: number;
  onChange: (v: number | undefined) => void;
  min: number;
  max: number;
  placeholder?: number;
  /** 0 is a value of its own, not "the default". */
  zero?: boolean;
}) {
  return (
    <input
      type="number"
      min={min}
      max={max}
      value={zero ? (value ?? "") : value || ""}
      placeholder={placeholder !== undefined ? String(placeholder) : undefined}
      onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
    />
  );
}

/** A server's health check, crash-loop limit and unhealthy restarts, on its Settings tab. */
export function ServerHealthCard({
  server,
  running,
  canManage,
}: {
  server: string;
  running: boolean;
  canManage: boolean;
}) {
  const [data, setData] = useState<ServerHealth>();
  const [form, setForm] = useState<HealthSettings>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [test, setTest] = useState<{ healthy: boolean; exitCode: number; output: string }>();
  const [testing, setTesting] = useState(false);

  useEffect(() => {
    api.serverHealth(server).then(
      (d) => {
        setData(d);
        setForm(d.settings);
      },
      (e) => setError((e as Error).message),
    );
  }, [server]);

  if (!data || !form) return error ? <div className="alert error small">{error}</div> : <Spinner />;

  const d = data.defaults;
  const set = (x: Partial<HealthSettings>) => {
    setForm({ ...form, ...x });
    setNotice(undefined);
  };
  const setCheck = (x: Partial<HealthCheck>) => {
    set({ check: { ...form.check, ...x } });
    setTest(undefined);
  };
  const setLoop = (x: Partial<HealthSettings["crashLoop"]>) => set({ crashLoop: { ...form.crashLoop, ...x } });
  const k = form.check;
  const custom = k.kind === "tcp" || k.kind === "http" || k.kind === "command";
  const changed = JSON.stringify(form) !== JSON.stringify(data.settings);

  async function save() {
    if (!form) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const saved = await api.setServerHealth(server, form);
      setData(saved);
      setForm(saved.settings);
      setNotice(
        saved.applied
          ? "Saved."
          : "Saved. The health check takes effect when the server is next recreated: Update image, or Save & recreate below.",
      );
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function runTest() {
    setTesting(true);
    setError(undefined);
    setTest(undefined);
    try {
      setTest(await api.testServerHealth(server, k.kind ? k : undefined));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setTesting(false);
    }
  }

  const canTest = running && canManage && k.kind !== "none" && (custom || !!data.currentCheck?.Test?.length);

  return (
    <div className="health-settings">
      <h2>Health and crash loops</h2>
      <ul className="muted small health-facts">
        <li>
          Health check: {data.health ? <strong>{data.health}</strong> : running ? "none" : "not running"}
          {data.unhealthySince && <> · failing since {timeAgo(data.unhealthySince)}</>}
          {data.custom && data.applied ? (
            <> · hlm&apos;s check, as set below</>
          ) : (
            data.currentCheck !== undefined && (
              <>
                {" "}
                · {data.custom ? "hlm's earlier check" : "the container's"}:{" "}
                <code>{describeCheck(data.currentCheck)}</code>
              </>
            )
          )}
          {!data.currentCheck && !data.custom && running && canManage && <> · the image has none</>}
        </li>
        <li>
          Unexpected exits in the last {form.crashLoop.minutes || d.minutes} minutes: {data.recentExits}
        </li>
      </ul>
      {!data.applied && (
        <div className="alert info small">
          The saved health check isn&apos;t on the container yet: it&apos;s set when the server is next recreated
          (Update image, or Save &amp; recreate below).
        </div>
      )}
      <fieldset className="plain-fieldset config-form" disabled={!canManage}>
        <div className="field">
          <span className="field-label">Crash-loop protection</span>
          <label className="radio small">
            <input
              type="checkbox"
              checked={!form.crashLoop.off}
              onChange={(e) => setLoop({ off: !e.target.checked })}
            />
            Stop the server if it exits on its own
            <span className="inline-input">
              <NumberInput
                value={form.crashLoop.exits}
                placeholder={d.exits}
                min={2}
                max={100}
                onChange={(v) => setLoop({ exits: v })}
              />
            </span>
            times within
            <span className="inline-input">
              <NumberInput
                value={form.crashLoop.minutes}
                placeholder={d.minutes}
                min={1}
                max={1440}
                onChange={(v) => setLoop({ minutes: v })}
              />
            </span>
            minutes
          </label>
          <span className="field-help">
            With Start on boot, Docker restarts a server every time it exits, so one that can&apos;t start (a bad
            setting, a corrupt save) would restart forever. Stops by hlm, people or backups don&apos;t count, nor do
            exits during an update. hlm notifies you (Crash loop) and the server stays stopped until someone starts it.
          </span>
        </div>

        <div className="field">
          <span className="field-label">Health check</span>
          {KINDS.map((o) => (
            <label key={o.kind} className="radio small">
              <input
                type="radio"
                checked={k.kind === o.kind}
                onChange={() => setCheck({ kind: o.kind, port: k.port, path: k.path, command: k.command })}
              />
              {o.label}
            </label>
          ))}
          <span className="field-help">
            Checks run inside the container, as Docker health checks. The TCP check needs <code>nc</code> or{" "}
            <code>bash</code> in the image, the web check <code>curl</code> or <code>wget</code>; use Test to see if it
            works. Game ports are often UDP, which these can&apos;t check: the game&apos;s status query (below) can be
            the signal instead.
          </span>
        </div>
        {(k.kind === "tcp" || k.kind === "http") && (
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Port (in the container)</span>
              <NumberInput value={k.port} min={1} max={65535} onChange={(v) => setCheck({ port: v })} />
            </label>
            {k.kind === "http" && (
              <label className="field">
                <span className="field-label">Path</span>
                <input
                  value={k.path ?? ""}
                  placeholder="/"
                  onChange={(e) => setCheck({ path: e.target.value })}
                  spellCheck={false}
                />
              </label>
            )}
          </div>
        )}
        {k.kind === "command" && (
          <label className="field">
            <span className="field-label">Command</span>
            <input
              value={k.command ?? ""}
              onChange={(e) => setCheck({ command: e.target.value })}
              placeholder="pgrep -f valheim_server"
              maxLength={1000}
              spellCheck={false}
            />
            <span className="field-help">Run with /bin/sh in the container; exit code 0 is healthy.</span>
          </label>
        )}
        {custom && (
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Check every (seconds)</span>
              <NumberInput
                value={k.interval}
                placeholder={d.interval}
                min={5}
                max={3600}
                onChange={(v) => setCheck({ interval: v })}
              />
            </label>
            <label className="field">
              <span className="field-label">Time limit (seconds)</span>
              <NumberInput
                value={k.timeout}
                placeholder={d.timeout}
                min={1}
                max={300}
                onChange={(v) => setCheck({ timeout: v })}
              />
            </label>
            <label className="field">
              <span className="field-label">Unhealthy after (failures in a row)</span>
              <NumberInput
                value={k.retries}
                placeholder={d.retries}
                min={1}
                max={20}
                onChange={(v) => setCheck({ retries: v })}
              />
            </label>
            <label className="field">
              <span className="field-label">Start-up time (minutes)</span>
              <NumberInput
                value={k.startPeriod}
                placeholder={d.startPeriod}
                min={0}
                max={120}
                zero
                onChange={(v) => setCheck({ startPeriod: v })}
              />
              <span className="field-help">
                Failures don&apos;t count this long after a start, while the world loads.
              </span>
            </label>
          </div>
        )}
        {canTest && (
          <div className="btn-row">
            <button type="button" className="btn small" disabled={testing} onClick={runTest}>
              {testing ? "Testing…" : "Test now"}
            </button>
            {test && (
              <span className={test.healthy ? "run-ok small" : "run-failed small"}>
                {test.healthy ? "Healthy" : `Unhealthy (exit code ${test.exitCode})`}
              </span>
            )}
          </div>
        )}
        {test?.output && <pre className="op-output">{test.output}</pre>}

        <div className="field">
          <span className="field-label">When its health fails</span>
          <label className="radio small">
            <input
              type="checkbox"
              checked={!!form.restartAfter}
              onChange={(e) => set({ restartAfter: e.target.checked ? 10 : undefined })}
            />
            Restart the server once it has been failing for
            <span className="inline-input">
              <NumberInput value={form.restartAfter} min={1} max={1440} onChange={(v) => set({ restartAfter: v })} />
            </span>
            minutes
          </label>
          {!!form.restartAfter && (
            <>
              <label className="radio small">
                <input type="radio" checked={!form.signal} onChange={() => set({ signal: "" })} />
                Failing means Docker&apos;s health check says unhealthy
              </label>
              <label className="radio small">
                <input
                  type="radio"
                  checked={form.signal === "query"}
                  disabled={!data.queryAvailable && form.signal !== "query"}
                  onChange={() => set({ signal: "query" })}
                />
                Failing means the game&apos;s status query stopped answering
                {!data.queryAvailable && <span className="muted"> (not available: set up its status query first)</span>}
              </label>
            </>
          )}
          <span className="field-help">
            Off by default. Each restart counts towards the crash-loop limit, so a server that never recovers is stopped
            instead of restarted forever.
          </span>
        </div>
      </fieldset>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      {canManage && (
        <div className="btn-row">
          <button className="btn small primary" onClick={save} disabled={busy || !changed}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      )}
    </div>
  );
}
