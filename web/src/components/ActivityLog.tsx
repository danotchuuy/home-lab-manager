import { Fragment, useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { AuditActorKind, AuditEntry, AuditFilter, AuditPage, AuditSettings } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

const KIND_LABEL: Record<AuditActorKind, string> = {
  local: "account",
  sso: "single sign-on",
  key: "API key",
  ssh: "SSH key",
  token: "HLM_TOKEN",
  anonymous: "no sign-in",
  hlm: "hlm",
  host: "on the host",
  discord: "via Discord",
  "": "",
};

const RESULT_LABEL = { ok: "OK", denied: "Denied", error: "Failed" } as const;

function when(t: string): string {
  return new Date(t).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** Entries as a table; a row opens to show the request's details. */
export function ActivityTable({ entries, showServer = true }: { entries: AuditEntry[]; showServer?: boolean }) {
  const [open, setOpen] = useState<number>();
  if (entries.length === 0) return <p className="muted small">Nothing recorded yet.</p>;
  return (
    <div className="table-scroll">
      <table className="data-table activity-table">
        <thead>
          <tr>
            <th>When</th>
            <th>Who</th>
            <th>Did</th>
            {showServer && <th>To</th>}
            <th>Result</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e, i) => {
            const isOpen = open === i;
            return (
              <Fragment key={i}>
                <tr
                  className={`activity-row${isOpen ? " open" : ""}`}
                  onClick={() => setOpen(isOpen ? undefined : i)}
                >
                  <td className="nowrap" title={timeAgo(e.time)}>
                    {/* The row opens with a click anywhere; this is its keyboard and screen reader control. */}
                    <button
                      type="button"
                      className="row-toggle"
                      aria-expanded={isOpen}
                      onClick={(ev) => {
                        ev.stopPropagation();
                        setOpen(isOpen ? undefined : i);
                      }}
                    >
                      {when(e.time)}
                    </button>
                  </td>
                  <td>
                    <strong>{e.actor}</strong>
                    {e.actorKind && e.actorKind !== "local" && <span className="muted small"> · {KIND_LABEL[e.actorKind]}</span>}
                  </td>
                  <td>
                    <code>{e.action}</code>
                  </td>
                  {showServer && <td>{e.target}</td>}
                  <td>
                    <span className={`badge audit-${e.result}`}>{RESULT_LABEL[e.result] ?? e.result}</span>
                  </td>
                </tr>
                {isOpen && (
                  <tr className="activity-details">
                    <td colSpan={showServer ? 5 : 4}>
                      <dl className="facts">
                        <dt>Time</dt>
                        <dd>{new Date(e.time).toLocaleString()}</dd>
                        {e.ip && (
                          <>
                            <dt>From</dt>
                            <dd>{e.ip}</dd>
                          </>
                        )}
                        {e.server && (
                          <>
                            <dt>Server</dt>
                            <dd>{e.server}</dd>
                          </>
                        )}
                        {e.target && (
                          <>
                            <dt>Target</dt>
                            <dd>{e.target}</dd>
                          </>
                        )}
                        {e.status ? (
                          <>
                            <dt>Status</dt>
                            <dd>{e.status}</dd>
                          </>
                        ) : null}
                        {e.error && (
                          <>
                            <dt>Error</dt>
                            <dd className="form-error">{e.error}</dd>
                          </>
                        )}
                        {Object.entries(e.details ?? {}).map(([k, v]) => (
                          <Fragment key={k}>
                            <dt>{k}</dt>
                            <dd>
                              <code>{v}</code>
                            </dd>
                          </Fragment>
                        ))}
                      </dl>
                    </td>
                  </tr>
                )}
              </Fragment>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const AREAS: { label: string; prefix: string }[] = [
  { label: "Everything", prefix: "" },
  { label: "Servers", prefix: "servers." },
  { label: "Sign-ins", prefix: "auth.login" },
  { label: "Accounts, keys and sign-in settings", prefix: "auth." },
  { label: "Groups", prefix: "groups." },
  { label: "Scheduled tasks run", prefix: "tasks." },
  { label: "Notifications", prefix: "notifications." },
  { label: "Proxy and tunnel", prefix: "proxy." },
  { label: "System", prefix: "system." },
];

/** System → Activity: the whole audit log, filtered, a page at a time, with export and settings. */
export function SystemActivity() {
  const [filter, setFilter] = useState<AuditFilter>({});
  const [text, setText] = useState("");
  const [entries, setEntries] = useState<AuditEntry[]>();
  const [next, setNext] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const load = useCallback(async (f: AuditFilter, more?: string) => {
    setBusy(true);
    try {
      const page: AuditPage = await api.audit({ ...f, before: more, limit: 50 });
      setEntries((prev) => (more ? [...(prev ?? []), ...page.entries] : page.entries));
      setNext(page.next);
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => {
    load(filter);
  }, [filter, load]);

  // Typing filters after a pause.
  useEffect(() => {
    const t = setTimeout(() => setFilter((f) => (f.text === text ? f : { ...f, text })), 350);
    return () => clearTimeout(t);
  }, [text]);

  const exportAs = (format: "csv" | "jsonl") => api.exportAudit(filter, format).catch((e) => setError((e as Error).message));

  return (
    <div className="activity">
      <p className="muted small no-margin">
        Every change made in hlm, by whom and from where, and every sign-in: kept as one file a month in hlm&apos;s data
        folder, and included in its configuration backups. Entries can&apos;t be changed or deleted here. Click a row for
        details.
      </p>
      <div className="activity-filters">
        <input type="search" placeholder="Search who, what, server, address…" value={text} onChange={(e) => setText(e.target.value)} aria-label="Search" />
        <select value={filter.action ?? ""} onChange={(e) => setFilter({ ...filter, action: e.target.value })} aria-label="Area">
          {AREAS.map((a) => (
            <option key={a.label} value={a.prefix}>
              {a.label}
            </option>
          ))}
        </select>
        <select value={filter.result ?? ""} onChange={(e) => setFilter({ ...filter, result: e.target.value })} aria-label="Result">
          <option value="">Any result</option>
          <option value="ok">OK</option>
          <option value="denied">Denied</option>
          <option value="error">Failed</option>
        </select>
        <select
          value={filter.since ?? ""}
          onChange={(e) => setFilter({ ...filter, since: e.target.value })}
          aria-label="Since"
        >
          <option value="">Any time</option>
          <option value={new Date(Date.now() - 86400e3).toISOString()}>Last 24 hours</option>
          <option value={new Date(Date.now() - 7 * 86400e3).toISOString()}>Last 7 days</option>
          <option value={new Date(Date.now() - 30 * 86400e3).toISOString()}>Last 30 days</option>
        </select>
        <span className="spacer" />
        <button className="btn small" onClick={() => exportAs("csv")}>
          Export CSV
        </button>
        <button className="btn small ghost" onClick={() => exportAs("jsonl")}>
          JSON Lines
        </button>
      </div>
      {error && <div className="alert error small">{error}</div>}
      {!entries ? <Spinner /> : <ActivityTable entries={entries} />}
      {next && (
        <div className="btn-row">
          <button className="btn small" onClick={() => load(filter, next)} disabled={busy}>
            {busy ? "Loading…" : "Show older"}
          </button>
        </div>
      )}
      <AuditSettingsForm />
    </div>
  );
}

function AuditSettingsForm() {
  const [set, setSet] = useState<AuditSettings>();
  const [months, setMonths] = useState(12);
  const [mirror, setMirror] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  useEffect(() => {
    api.auditSettings().then((s) => {
      setSet(s);
      setMonths(s.retentionMonths);
      setMirror(s.mirror);
    }, () => {});
  }, []);
  if (!set) return null;

  async function save() {
    if (months < set!.retentionMonths && !confirm(`Keep only ${months} months? Older entries are deleted for good.`)) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const s = await api.setAuditSettings(months, mirror);
      setSet(s);
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="activity-settings">
      <h3>Keeping the log</h3>
      <div className="inline-input">
        Keep
        <input type="number" min={1} max={120} value={months} onChange={(e) => setMonths(Number(e.target.value))} aria-label="Months to keep" />
        months of entries
      </div>
      <label className="radio small">
        <input type="checkbox" checked={mirror} onChange={(e) => setMirror(e.target.checked)} />
        Also write each entry to hlm&apos;s own log (for journald, Loki and the like)
      </label>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button className="btn small primary" onClick={save} disabled={busy || (months === set.retentionMonths && mirror === set.mirror)}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}

/** What was done to one server, on its Settings tab. */
export function ServerActivity({ server }: { server: string }) {
  const [page, setPage] = useState<AuditPage>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    api.serverActivity(server).then(setPage, (e) => setError((e as Error).message));
  }, [server]);
  return (
    <div className="server-activity">
      <h2>Activity</h2>
      <p className="muted small">The last changes to this server, and what hlm did to it by itself.</p>
      {error && <div className="alert error small">{error}</div>}
      {!page && !error && <Spinner />}
      {page && <ActivityTable entries={page.entries} showServer={false} />}
    </div>
  );
}

/** Your recent sign-ins, under your account. */
export function MySignIns() {
  const [page, setPage] = useState<AuditPage>();
  useEffect(() => {
    api.myActivity().then(setPage, () => {});
  }, []);
  if (!page) return null;
  return (
    <div className="my-sign-ins">
      <h3>Your recent sign-ins</h3>
      {page.entries.length === 0 ? (
        <p className="muted small">None recorded yet.</p>
      ) : (
        <ul className="small">
          {page.entries.map((e, i) => (
            <li key={i}>
              <span className={`badge audit-${e.result}`}>{RESULT_LABEL[e.result] ?? e.result}</span> {e.action.replace(/^auth\./, "").replace("_", " ")}{" "}
              <span className="muted">
                {when(e.time)}
                {e.ip && ` from ${e.ip}`}
                {e.error && ` · ${e.error}`}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
