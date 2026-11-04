import { useEffect, useState } from "react";
import { api } from "../api";
import type { AutoUpdateResult, AutoUpdateState, ServerUpdatePolicy, SystemUpdatePolicy, UpdatePolicy } from "../types";
import { formatInZone, timeAgo, timeUntil } from "../util";
import { Spinner } from "./common";

/** Describes a policy in words. */
export function describePolicy(p: UpdatePolicy): string {
  if (p.mode !== "auto") return "Updates wait for someone to click Update.";
  const when = p.windowStart ? `between ${p.windowStart} and ${p.windowEnd}` : "as soon as they're found";
  const extras = [
    p.onlyWhenEmpty ? "once nobody's playing" : "",
    p.warn ? `warning players ${p.warnMinutes ?? 0} min before` : "",
    p.backup ? "backing up first" : "",
  ].filter(Boolean);
  return `Updates are applied automatically ${when}${extras.length ? `, ${extras.join(" and ")}` : ""}.`;
}

export function describeState(s: AutoUpdateState, tz: string): string {
  const next = s.next ? ` (next try ${formatInZone(s.next, tz)}, ${timeUntil(s.next)})` : "";
  switch (s.state) {
    case "waiting":
      return `Update waiting for its window${next}.`;
    case "busy":
      return `Update waiting: ${s.message?.replace(/^waiting: /, "") ?? "the server is busy"}.`;
    case "queued":
      return `Update queued: ${s.message ?? ""}.`;
    case "updating":
      return `Updating: ${s.message ?? ""}…`;
    case "failed":
      return `${s.message ?? "The last automatic update failed"}${next}.`;
    case "players":
      return `Update ${s.message ?? "waiting for players to leave"}.`;
  }
}

function LastResult({ r, tz }: { r: AutoUpdateResult; tz: string }) {
  return (
    <div className="small">
      Last automatic update: <span className={r.ok ? "run-ok" : "run-failed"}>{r.ok ? "OK" : "Failed"}</span>{" "}
      <span className="muted" title={formatInZone(r.started, tz)}>
        {timeAgo(r.started)}
      </span>
      {r.steps && r.steps.length > 0 && <span className="muted"> · {r.steps.join(", ")}</span>}
      {r.error && <div className="form-error">{r.error}</div>}
    </div>
  );
}

/** The fields of an update policy. With followsDefault, there's a "Follow the default" choice. */
function PolicyFields({
  p,
  onChange,
  timezone,
  followsDefault,
  disabled,
}: {
  p: UpdatePolicy;
  onChange: (p: UpdatePolicy) => void;
  timezone: string;
  followsDefault?: string;
  disabled?: boolean;
}) {
  const set = (x: Partial<UpdatePolicy>) => onChange({ ...p, ...x });
  const anyTime = !p.windowStart && !p.windowEnd;
  return (
    <fieldset className="plain-fieldset config-form" disabled={disabled}>
      <div className="field">
        {followsDefault !== undefined && (
          <label className="radio small">
            <input type="radio" checked={p.mode === ""} onChange={() => set({ mode: "" })} />
            Follow the default ({followsDefault})
          </label>
        )}
        <label className="radio small">
          <input type="radio" checked={p.mode === "manual"} onChange={() => set({ mode: "manual" })} />
          Wait for someone to click Update
        </label>
        <label className="radio small">
          <input type="radio" checked={p.mode === "auto"} onChange={() => set({ mode: "auto" })} />
          Apply updates automatically
        </label>
      </div>
      {p.mode === "auto" && (
        <>
          <div className="field">
            <span className="field-label">When</span>
            <label className="radio small">
              <input type="radio" checked={anyTime} onChange={() => set({ windowStart: "", windowEnd: "" })} />
              As soon as an update is found
            </label>
            <label className="radio small">
              <input
                type="radio"
                checked={!anyTime}
                onChange={() => set({ windowStart: "03:00", windowEnd: "06:00" })}
              />
              Only starting between
              <input
                type="time"
                value={p.windowStart ?? ""}
                onChange={(e) => set({ windowStart: e.target.value })}
                disabled={anyTime}
              />
              and
              <input type="time" value={p.windowEnd ?? ""} onChange={(e) => set({ windowEnd: e.target.value })} disabled={anyTime} />
              <span className="muted">({timezone})</span>
            </label>
          </div>
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Warn players first (optional)</span>
              <input
                value={p.warn ?? ""}
                onChange={(e) => set({ warn: e.target.value, warnMinutes: p.warnMinutes || 5 })}
                placeholder="say Updating in 5 minutes"
                maxLength={1024}
                spellCheck={false}
              />
              <span className="field-help">A console command, sent if the server&apos;s console is set up and it&apos;s running.</span>
            </label>
            <label className="field">
              <span className="field-label">Then wait</span>
              <span className="inline-input">
                <input
                  type="number"
                  min={0}
                  max={30}
                  value={p.warn ? (p.warnMinutes ?? 0) : ""}
                  onChange={(e) => set({ warnMinutes: Number(e.target.value) })}
                  disabled={!p.warn}
                />
                minutes before updating
              </span>
            </label>
          </div>
          <label className="radio small">
            <input type="checkbox" checked={!!p.onlyWhenEmpty} onChange={(e) => set({ onlyWhenEmpty: e.target.checked })} />
            Wait while players are online (when the server&apos;s status shows how many are; otherwise this doesn&apos;t wait)
          </label>
          <label className="radio small">
            <input type="checkbox" checked={p.backup} onChange={(e) => set({ backup: e.target.checked })} />
            Back up first, if the server has a backup plan (if the backup fails, the update isn&apos;t applied)
          </label>
        </>
      )}
    </fieldset>
  );
}

/** A server's update policy, on its Settings tab. */
export function ServerUpdatePolicyCard({ server, canManage }: { server: string; canManage: boolean }) {
  const [data, setData] = useState<ServerUpdatePolicy>();
  const [form, setForm] = useState<UpdatePolicy>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  useEffect(() => {
    api.updatePolicy(server).then(
      (d) => {
        setData(d);
        setForm({ ...d.policy, backup: d.policy.mode ? d.policy.backup : true });
      },
      (e) => setError((e as Error).message),
    );
  }, [server]);

  if (!data || !form) return error ? <div className="alert error small">{error}</div> : <Spinner />;

  async function save() {
    if (!form) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const d = await api.setUpdatePolicy(server, form);
      setData(d);
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const changed = JSON.stringify(form) !== JSON.stringify({ ...data.policy, backup: data.policy.mode ? data.policy.backup : true });

  return (
    <div className="update-policy">
      <h2>Automatic updates</h2>
      <p className="muted small">
        {describePolicy(data.effective)}
        {data.policy.mode === "" && " (the default, set under System → Automatic updates)"}
      </p>
      {data.state && <div className="alert info small">{describeState(data.state, data.timezone)}</div>}
      {data.last && <LastResult r={data.last} tz={data.timezone} />}
      {canManage && (
        <>
          <PolicyFields
            p={form}
            onChange={setForm}
            timezone={data.timezone}
            followsDefault={data.default.mode === "auto" ? "automatic" : "wait for Update"}
          />
          {error && <div className="alert error small">{error}</div>}
          {notice && <div className="alert success small">{notice}</div>}
          <div className="btn-row">
            <button className="btn small primary" onClick={save} disabled={busy || !changed}>
              {busy ? "Saving…" : "Save"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}

/** System → Automatic updates: the default policy, how often to check, and hlm's own updates. */
export function SystemUpdateSettings({ timezone }: { timezone: string }) {
  const [data, setData] = useState<SystemUpdatePolicy>();
  const [def, setDef] = useState<UpdatePolicy>();
  const [hours, setHours] = useState(0);
  const [self, setSelf] = useState<SystemUpdatePolicy["self"]>({ auto: false });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const apply = (d: SystemUpdatePolicy) => {
    setData(d);
    setDef(d.default);
    setHours(d.checkHours || d.defaultCheckHours);
    setSelf(d.self);
  };
  useEffect(() => {
    api.systemUpdatePolicy().then(apply, (e) => setError((e as Error).message));
  }, []);

  if (!data || !def) return error ? <div className="alert error small">{error}</div> : <Spinner />;

  async function save() {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      apply(await api.setSystemUpdatePolicy({ default: def!, checkHours: hours, self }));
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const selfAnyTime = !self.windowStart && !self.windowEnd;

  return (
    <div className="system-updates">
      <h3>Servers, by default</h3>
      <p className="muted small no-margin">Each server can choose otherwise on its Settings tab.</p>
      <PolicyFields p={def} onChange={setDef} timezone={timezone} />

      <h3>Checking for updates</h3>
      {data.checkEnv ? (
        <p className="muted small">
          Every <code>{data.checkEnv}</code>, set by <code>HLM_UPDATE_CHECK</code> (which wins over a setting here).
        </p>
      ) : (
        <label className="field inline-field">
          <span className="inline-input">
            Check every
            <input type="number" min={1} max={168} value={hours} onChange={(e) => setHours(Number(e.target.value))} />
            hours
          </span>
        </label>
      )}

      <h3>Home Lab Manager itself</h3>
      <label className="radio small">
        <input type="checkbox" checked={self.auto} onChange={(e) => setSelf({ ...self, auto: e.target.checked })} />
        Install new versions of hlm automatically (never while a server is being installed or updated)
      </label>
      {self.auto && (
        <label className="radio small">
          <input
            type="checkbox"
            checked={!selfAnyTime}
            onChange={(e) => setSelf({ ...self, ...(e.target.checked ? { windowStart: "03:00", windowEnd: "05:00" } : { windowStart: "", windowEnd: "" }) })}
          />
          Only between
          <input type="time" value={self.windowStart ?? ""} onChange={(e) => setSelf({ ...self, windowStart: e.target.value })} disabled={selfAnyTime} />
          and
          <input type="time" value={self.windowEnd ?? ""} onChange={(e) => setSelf({ ...self, windowEnd: e.target.value })} disabled={selfAnyTime} />
          <span className="muted">({timezone})</span>
        </label>
      )}
      {data.selfLast && <LastResult r={data.selfLast} tz={timezone} />}

      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button className="btn small primary" onClick={save} disabled={busy}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
