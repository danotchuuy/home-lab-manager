import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { DeployEvent, InstalledMod, ModChange, ModPlan, ModSearchHit, ModsOverview } from "../types";
import { formatBytes } from "../util";
import { Ago, Spinner } from "./common";
import { DeployProgress } from "./DeployProgress";

const SOURCE_NOTE: Record<InstalledMod["source"], string> = {
  hlm: "",
  modrinth: "added outside hlm",
  unknown: "not on Modrinth",
};

/** A server's Mods tab: what's installed, updates, and adding from Modrinth. */
export function ServerMods({ server, canCheck, canManage }: { server: string; canCheck: boolean; canManage: boolean }) {
  const [ov, setOv] = useState<ModsOverview>();
  const [loadError, setLoadError] = useState<string>();
  const [error, setError] = useState<string>();
  const [checking, setChecking] = useState(false);
  // The change being looked at, then applied.
  const [plan, setPlan] = useState<ModPlan>();
  const [planning, setPlanning] = useState(false);
  const [events, setEvents] = useState<DeployEvent[]>();
  const [applying, setApplying] = useState(false);

  const load = useCallback(
    () =>
      api.serverMods(server).then(
        (o) => {
          setOv(o);
          setLoadError(undefined);
        },
        (e) => setLoadError((e as Error).message),
      ),
    [server],
  );
  useEffect(() => {
    load();
  }, [load]);

  if (!ov)
    return <div className="card">{loadError ? <div className="alert error small">{loadError}</div> : <Spinner />}</div>;
  if (ov.unsupported) {
    return (
      <div className="card">
        <h2>Mods</h2>
        <p className="muted small">{ov.unsupported}.</p>
      </div>
    );
  }

  const noun = ov.plugins ? "plugin" : "mod";
  const Noun = ov.plugins ? "Plugin" : "Mod";
  const updates = ov.mods.filter((m) => m.update && !m.update.blocked);

  async function propose(changes: ModChange[]) {
    setPlanning(true);
    setError(undefined);
    setEvents(undefined);
    try {
      setPlan(await api.planMods(server, changes));
    } catch (e) {
      setError((e as Error).message);
    }
    setPlanning(false);
  }

  async function apply() {
    if (!plan) return;
    setApplying(true);
    setError(undefined);
    const got: DeployEvent[] = [];
    setEvents([]);
    try {
      await api.applyMods(server, plan.changes, (e) => {
        got.push(e);
        setEvents([...got]);
      });
    } catch (e) {
      got.push({ stage: "error", message: (e as Error).message });
      setEvents([...got]);
    }
    setApplying(false);
    setPlan(undefined);
    load();
  }

  async function check() {
    setChecking(true);
    setError(undefined);
    try {
      setOv(await api.checkModUpdates(server));
    } catch (e) {
      setError((e as Error).message);
    }
    setChecking(false);
  }

  const busy = planning || applying;

  return (
    <div className="card mods">
      <div className="settings-actions">
        <div>
          <h2>{ov.plugins ? "Plugins" : "Mods"}</h2>
          <p className="muted small">
            {ov.loader.charAt(0) + ov.loader.slice(1).toLowerCase()}
            {ov.fit.gameVersion && <> · game version {ov.fit.gameVersion}</>} · <code>{ov.dir}</code>
          </p>
        </div>
        <div className="btn-row">
          {canCheck && ov.mods.length > 0 && (
            <button className="btn small" onClick={check} disabled={checking || busy || !!ov.fitError}>
              {checking ? "Checking…" : "Check for updates"}
            </button>
          )}
          <button className="btn small" onClick={() => load()} disabled={busy}>
            Refresh
          </button>
        </div>
      </div>
      {loadError && <div className="alert error small">{loadError}</div>}
      {ov.fitError && <div className="alert warning small">{ov.fitError}.</div>}
      {ov.warnings?.map((w) => (
        <div key={w} className="alert warning small">
          {w}
        </div>
      ))}
      {error && <div className="alert error small">{error}</div>}

      {plan && (
        <PlanView
          plan={plan}
          noun={noun}
          timeoutNs={ov.timeoutNs}
          applying={applying}
          onApply={apply}
          onCancel={() => setPlan(undefined)}
        />
      )}
      {events && (
        <section className="mod-progress">
          <h3>Applying the change</h3>
          <DeployProgress events={events} />
          {!applying && (
            <button className="btn small" onClick={() => setEvents(undefined)}>
              Close
            </button>
          )}
        </section>
      )}

      <section>
        <div className="settings-actions">
          <h3>Installed</h3>
          {canManage && updates.length > 1 && (
            <button
              className="btn small primary"
              disabled={busy || !!plan}
              onClick={() => propose(updates.map((m) => ({ op: "update", file: m.file, version: m.update!.version })))}
            >
              Update all ({updates.length})
            </button>
          )}
        </div>
        {ov.checked && (
          <p className="muted small">
            Updates last checked <Ago iso={ov.checked} />.
          </p>
        )}
        {ov.mods.length === 0 ? (
          <p className="muted small">No {noun}s yet.</p>
        ) : (
          <table className="data-table">
            <tbody>
              {ov.mods.map((m) => (
                <tr key={m.file} className={m.disabled ? "mod-off" : ""}>
                  <td>
                    <div>
                      {m.slug ? (
                        <a
                          href={`https://modrinth.com/project/${encodeURIComponent(m.slug)}`}
                          target="_blank"
                          rel="noreferrer noopener"
                        >
                          {m.title || m.file}
                        </a>
                      ) : (
                        m.title || m.file
                      )}{" "}
                      {m.versionNumber && <span className="muted">{m.versionNumber}</span>}
                      {m.disabled && <span className="badge">off</span>}
                      {SOURCE_NOTE[m.source] && <span className="badge">{SOURCE_NOTE[m.source]}</span>}
                    </div>
                    <div className="muted small">
                      <code>{m.file}</code> · {formatBytes(m.size)}
                      {m.source === "hlm" && m.installed && (
                        <>
                          {" "}
                          · added <Ago iso={m.installed} />
                          {m.by && <> by {m.by}</>}
                        </>
                      )}
                    </div>
                    {m.update && (
                      <div className={`small ${m.update.blocked ? "muted" : "mod-update"}`}>
                        {m.update.blocked
                          ? `${m.update.versionNumber} is out, but it ${m.update.blocked}.`
                          : `${m.update.versionNumber} is available.`}
                      </div>
                    )}
                  </td>
                  {canManage && (
                    <td className="actions">
                      {m.update && !m.update.blocked && (
                        <button
                          className="btn small primary"
                          disabled={busy || !!plan}
                          onClick={() => propose([{ op: "update", file: m.file, version: m.update!.version }])}
                        >
                          Update
                        </button>
                      )}
                      <button
                        className="btn small"
                        disabled={busy || !!plan}
                        onClick={() => propose([{ op: m.disabled ? "enable" : "disable", file: m.file }])}
                      >
                        {m.disabled ? "Turn on" : "Turn off"}
                      </button>
                      <button
                        className="btn small danger"
                        disabled={busy || !!plan}
                        onClick={() => propose([{ op: "remove", file: m.file }])}
                      >
                        Remove
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {canManage && !ov.fitError && (
        <AddMod
          server={server}
          Noun={Noun}
          installed={ov.mods.map((m) => m.project).filter((p): p is string => !!p)}
          disabled={busy || !!plan}
          onAdd={(project) => propose([{ op: "add", project }])}
        />
      )}
    </div>
  );
}

/** What a change will do, before it's done. */
function PlanView({
  plan,
  noun,
  timeoutNs,
  applying,
  onApply,
  onCancel,
}: {
  plan: ModPlan;
  noun: string;
  timeoutNs: number;
  applying: boolean;
  onApply: () => void;
  onCancel: () => void;
}) {
  const minutes = Math.round(timeoutNs / 6e10);
  return (
    <section className="mod-plan">
      <h3>This change will</h3>
      <ul>
        {plan.steps.map((s, i) => {
          const name = s.title || s.file || s.new?.file;
          return (
            <li key={i}>
              {s.op === "add" && (
                <>
                  Add <strong>{name}</strong> {s.new?.versionNumber}{" "}
                  <span className="muted small">
                    ({s.new && formatBytes(s.new.size)}
                    {s.why && `, ${s.why}`})
                  </span>
                </>
              )}
              {s.op === "replace" && (
                <>
                  Update <strong>{name}</strong> from {s.fromVersion || "?"} to {s.new?.versionNumber}
                </>
              )}
              {s.op === "remove" && (
                <>
                  Remove <strong>{name}</strong>
                </>
              )}
              {s.op === "disable" && (
                <>
                  Turn off <strong>{name}</strong> (kept, but not loaded)
                </>
              )}
              {s.op === "enable" && (
                <>
                  Turn on <strong>{name}</strong>
                </>
              )}
            </li>
          );
        })}
      </ul>
      {plan.warnings?.map((w) => (
        <div key={w} className="alert warning small">
          {w}
        </div>
      ))}
      <p className="muted small">
        {plan.running
          ? `hlm saves a copy of the ${noun}s, stops the server, makes the change and starts it again. If it doesn't answer within ${minutes} minute${minutes === 1 ? "" : "s"}, hlm puts the ${noun}s back as they were and starts it again.`
          : `The server is stopped: hlm makes the change, and the ${noun}s load when it next starts.`}
      </p>
      <div className="btn-row">
        <button className="btn small primary" onClick={onApply} disabled={applying}>
          {applying ? "Applying…" : plan.running ? "Apply and restart" : "Apply"}
        </button>
        <button className="btn small" onClick={onCancel} disabled={applying}>
          Cancel
        </button>
      </div>
    </section>
  );
}

/** Searches Modrinth for mods that fit the server. */
function AddMod({
  server,
  Noun,
  installed,
  disabled,
  onAdd,
}: {
  server: string;
  Noun: string;
  installed: string[];
  disabled: boolean;
  onAdd: (project: string) => void;
}) {
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<ModSearchHit[]>();
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState<string>();

  async function search(e?: FormEvent) {
    e?.preventDefault();
    setSearching(true);
    setError(undefined);
    try {
      setHits(await api.searchMods(server, q.trim()));
    } catch (err) {
      setError((err as Error).message);
    }
    setSearching(false);
  }

  return (
    <section>
      <h3>Add a {Noun.toLowerCase()}</h3>
      <form className="mod-search" onSubmit={search}>
        <input
          type="search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Search Modrinth"
          maxLength={100}
          aria-label="Search Modrinth"
        />
        <button className="btn small" disabled={searching}>
          {searching ? "Searching…" : "Search"}
        </button>
      </form>
      <p className="muted small">Only versions for this server&apos;s loader and game version are shown.</p>
      {error && <div className="alert error small">{error}</div>}
      {hits && hits.length === 0 && <p className="muted small">Nothing found that runs on this server.</p>}
      {hits && hits.length > 0 && (
        <table className="data-table">
          <tbody>
            {hits.map((h) => (
              <tr key={h.project_id}>
                <td>
                  <div>
                    <a
                      href={`https://modrinth.com/project/${encodeURIComponent(h.slug)}`}
                      target="_blank"
                      rel="noreferrer noopener"
                    >
                      {h.title}
                    </a>{" "}
                    <span className="muted small">
                      by {h.author} · {h.downloads.toLocaleString()} downloads
                    </span>
                  </div>
                  <div className="muted small">{h.description}</div>
                </td>
                <td className="actions">
                  {installed.includes(h.project_id) ? (
                    <span className="muted small">Installed</span>
                  ) : (
                    <button className="btn small" disabled={disabled} onClick={() => onAdd(h.project_id)}>
                      Add…
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
