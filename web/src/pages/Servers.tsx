import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { can, useAdmin } from "../access";
import { api } from "../api";
import { AppIcon, CONNECTION_GLYPH, Spinner, StateBadge } from "../components/common";
import { InstallCard, JobStatus } from "../components/JobProgress";
import { ResourceSummary } from "../components/Resources";
import { UpdateProgress } from "../components/UpdateProgress";
import type { Connection, DeployEvent, Job, Server, Stats, UpdateCheckInfo, UpdateStatus } from "../types";
import { usePoll } from "../usePoll";
import { formatNetwork, formatPorts, timeAgo, webUILink } from "../util";

export function ServersPage() {
  const admin = useAdmin();
  const [servers, setServers] = useState<Server[] | null>(null);
  const [connections, setConnections] = useState<Connection[]>([]);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState<string>();
  const [stats, setStats] = useState<Record<string, Stats>>({});
  const [updateInfo, setUpdateInfo] = useState<UpdateCheckInfo>();
  const [checking, setChecking] = useState(false);
  const [updateEvents, setUpdateEvents] = useState<DeployEvent[]>([]);
  const [updatingAll, setUpdatingAll] = useState(false);
  // Last progress message of a single-server update started from this page,
  // shown until the backend's job list catches up.
  const [updating, setUpdating] = useState<Record<string, string>>({});
  const [jobs, setJobs] = useState<Job[]>([]);
  const [group, setGroup] = useState("");

  const load = useCallback(async () => {
    try {
      const [list, info, conns] = await Promise.all([
        api.servers(),
        api.updateInfo(),
        // Connections are extra, and only for admins; the servers still show without them.
        admin ? api.connections().catch(() => [] as Connection[]) : ([] as Connection[]),
      ]);
      setServers(list);
      setUpdateInfo(info);
      setConnections(conns);
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [admin]);

  usePoll(load, 5000, [load]);

  // Stats take about a second to collect, so poll them on their own loop and
  // wait for each response before scheduling the next.
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const s = await api.allStats();
        if (!cancelled) setStats(s);
      } catch {
        // Usage is best-effort; the list still works without it.
      }
      if (!cancelled) timer = setTimeout(poll, 5000);
    };
    poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, []);

  // Follow installs and updates, including ones started elsewhere: every
  // second while something is running, every five seconds otherwise.
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    let wasActive = false;
    const poll = async () => {
      let active = wasActive;
      try {
        const list = await api.jobs();
        if (cancelled) return;
        setJobs(list);
        active = list.some((j) => !j.finished);
        // Something just finished: a new server may exist or one changed.
        if (wasActive && !active) load();
        wasActive = active;
      } catch {
        // Progress is best-effort.
      }
      if (!cancelled) timer = setTimeout(poll, active ? 1000 : 5000);
    };
    poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [load]);

  async function act(s: Server, action: "start" | "stop" | "restart") {
    setBusy(s.id);
    try {
      await api.action(s.id, action);
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function toggleAutostart(s: Server, enabled: boolean) {
    setBusy(s.id);
    try {
      const updated = await api.setAutostart(s.id, enabled);
      setServers((prev) => prev?.map((p) => (p.id === s.id ? updated : p)) ?? null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const updatable = servers?.filter((s) => s.update?.state === "available" && can(s, "operator")) ?? [];
  const groups = [...new Set(servers?.flatMap((s) => s.groups ?? []))].sort();
  const shown = servers?.filter((s) => !group || s.groups?.includes(group));

  async function checkUpdates() {
    setChecking(true);
    try {
      setUpdateInfo(await api.checkUpdates());
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }

  async function connectionAction(c: Connection, action: "start" | "stop" | "restart") {
    setBusy(c.id);
    try {
      if (c.self) await api.restartSelf(c.id);
      else setConnections(await api.connectionAction(c.id, action));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function updateOne(s: Server) {
    if (s.state === "running" && !confirm(`Update ${s.name}? It will restart, disconnecting any players.`)) return;
    await runUpdate(s, (cb) => api.update(s.id, cb));
  }

  async function updateConnection(c: Connection) {
    if (!confirm(`Update ${c.name}? It's recreated on the new image, so the ${c.title.toLowerCase()} drops for a few seconds.`))
      return;
    await runUpdate(c, (cb) => api.updateConnection(c.id, cb));
  }

  /** Runs one container's update, showing its progress on its card. */
  async function runUpdate(s: { id: string; name: string }, op: (onEvent: (e: DeployEvent) => void) => Promise<void>) {
    setUpdating((prev) => ({ ...prev, [s.id]: "Starting update…" }));
    let failed: string | undefined;
    try {
      await op((e) => {
        if (e.stage === "error") failed = e.message;
        if (e.message) setUpdating((prev) => ({ ...prev, [s.id]: e.message! }));
      });
    } catch (e) {
      failed = (e as Error).message;
    }
    setUpdating((prev) => {
      const next = { ...prev };
      delete next[s.id];
      return next;
    });
    if (failed) setError(`Updating ${s.name} failed: ${failed}`);
    await load();
  }

  async function updateAll() {
    const targets = updatable;
    const running = targets.filter((s) => s.state === "running").map((s) => s.name);
    const warning = running.length ? `\n\nThese will restart, disconnecting any players: ${running.join(", ")}.` : "";
    if (!confirm(`Update ${targets.length} server${targets.length === 1 ? "" : "s"}?${warning}`)) return;
    setUpdatingAll(true);
    setUpdateEvents([]);
    try {
      await api.updateAll((e) => setUpdateEvents((prev) => [...prev, e]));
    } catch (e) {
      setUpdateEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setUpdatingAll(false);
      await load();
    }
  }

  const available = updatable.length;
  const names = new Set(servers?.map((s) => s.name));
  // Installs whose server isn't in the list yet, plus failed ones for a minute.
  const installs = jobs.filter((j) => j.kind === "install" && (j.error || !names.has(j.server) || !j.finished));
  const jobFor = (name: string) => jobs.find((j) => j.server === name && j.kind !== "install" && !j.finished);
  const checkingNow = checking || !!updateInfo?.checking;

  return (
    <div className="page">
      <div className="page-header">
        <h1>Servers</h1>
        {groups.length > 0 && (
          <select aria-label="Server group" value={group} onChange={(e) => setGroup(e.target.value)}>
            <option value="">All groups</option>
            {groups.map((g) => (
              <option key={g} value={g}>
                {g}
              </option>
            ))}
          </select>
        )}
        {admin && (
          <Link to="/catalog" className="btn primary">
            + New server
          </Link>
        )}
      </div>

      {((servers && servers.length > 0) || connections.length > 0) && (
        <div className="update-bar">
          <span className="muted small">
            {checkingNow ? "Checking for updates…" : `Updates last checked ${timeAgo(updateInfo?.lastCheck)}`}
          </span>
          <div className="spacer" />
          {admin && (
            <button className="btn small" onClick={checkUpdates} disabled={checkingNow || updatingAll}>
              {checkingNow ? "Checking…" : "Check for updates"}
            </button>
          )}
          {available > 0 && (
            <button className="btn small primary" onClick={updateAll} disabled={updatingAll || checkingNow}>
              {updatingAll ? "Updating…" : `Update all (${available})`}
            </button>
          )}
        </div>
      )}

      {updateEvents.length > 0 && <UpdateProgress events={updateEvents} running={updatingAll} />}

      {installs.length > 0 && (
        <section className="downloads">
          <h2 className="section-title">Downloads in progress</h2>
          <div className="server-list">
            {installs.map((j) => (
              <InstallCard key={j.id} job={j} />
            ))}
          </div>
        </section>
      )}

      {error && <div className="alert error">{error}</div>}
      {servers === null && !error && <Spinner />}
      {servers?.length === 0 && installs.length === 0 && (
        <div className="empty">
          {admin ? (
            <>
              <p>No game servers yet.</p>
              <Link to="/catalog" className="btn primary">
                Browse the catalog
              </Link>
            </>
          ) : (
            <p>No servers have been shared with you yet. An admin can add you to a server group.</p>
          )}
        </div>
      )}

      <div className="server-list">
        {shown?.map((s) => {
          const running = s.state === "running" || s.state === "restarting";
          const link = webUILink(s.webui);
          const job = jobFor(s.name);
          const progress = job ? (job.message ?? "Working…") : updating[s.id];
          return (
            <div key={s.id} className="card server-card">
              <Link to={`/servers/${encodeURIComponent(s.name)}`} className="server-main">
                <AppIcon src={s.icon} />
                <div>
                  <div className="server-name">
                    {s.name} <UpdateBadge update={s.update} />
                  </div>
                  <div className="muted small">
                    {s.app} · <code>{s.image}</code>
                  </div>
                  <div className="muted small">
                    {formatNetwork(s.network, s.ipAddress)}
                    {s.ports.length > 0 && ` · ${formatPorts(s.ports)}`}
                  </div>
                  {job ? (
                    <JobStatus job={job} />
                  ) : progress ? (
                    <div className="small update-inline">
                      <Spinner /> {progress}
                    </div>
                  ) : (
                    running && <ResourceSummary stats={stats[s.id]} />
                  )}
                </div>
              </Link>
              <div className="server-side">
                <StateBadge state={s.state} />
                <span className="muted small">{s.status}</span>
                <label className="switch" title={`Restart policy: ${s.restartPolicy}`}>
                  <input
                    type="checkbox"
                    checked={s.autostart}
                    disabled={busy === s.id || !!progress || !can(s, "manager")}
                    onChange={(e) => toggleAutostart(s, e.target.checked)}
                  />
                  <span className="switch-track" aria-hidden="true" />
                  <span className="small">Start on boot</span>
                </label>
                <div className="btn-row">
                  {s.update?.state === "available" && can(s, "operator") && (
                    <button
                      className="btn small primary"
                      disabled={!!progress || updatingAll}
                      onClick={() => updateOne(s)}
                    >
                      Update
                    </button>
                  )}
                  {link && running && (
                    <a className="btn ghost small" href={link} target="_blank" rel="noreferrer">
                      Web UI
                    </a>
                  )}
                  {!can(s, "operator") ? null : running ? (
                    <>
                      <button className="btn small" disabled={busy === s.id || !!progress} onClick={() => act(s, "restart")}>
                        Restart
                      </button>
                      <button
                        className="btn small danger"
                        disabled={busy === s.id || !!progress}
                        onClick={() => act(s, "stop")}
                      >
                        Stop
                      </button>
                    </>
                  ) : (
                    <button
                      className="btn small primary"
                      disabled={busy === s.id || !!progress}
                      onClick={() => act(s, "start")}
                    >
                      Start
                    </button>
                  )}
                </div>
              </div>
            </div>
          );
        })}
      </div>

      {connections.length > 0 && (
        <section className="connections">
          <h2 className="section-title">Connections</h2>
          <p className="muted small">Containers hlm runs to connect your servers to the internet, and the one hlm itself runs in.</p>
          <div className="server-list">
            {connections.map((c) => (
              <ConnectionCard
                key={c.id}
                conn={c}
                stats={stats[c.id]}
                job={jobFor(c.name)}
                progress={updating[c.id]}
                busy={busy === c.id || updatingAll}
                onAction={(a) => connectionAction(c, a)}
                onUpdate={() => updateConnection(c)}
              />
            ))}
          </div>
        </section>
      )}
    </div>
  );
}


/** A container hlm runs for a connection, in the style of a server card. */
function ConnectionCard({
  conn: c,
  stats,
  job,
  progress,
  busy,
  onAction,
  onUpdate,
}: {
  conn: Connection;
  stats?: Stats;
  job?: Job;
  progress?: string;
  busy: boolean;
  onAction: (action: "start" | "stop" | "restart") => void;
  onUpdate: () => void;
}) {
  const running = c.state === "running" || c.state === "restarting";
  const working = !!job || !!progress;
  return (
    <div className="card server-card">
      <Link to={`/connections/${encodeURIComponent(c.name)}`} className="server-main">
        <AppIcon glyph={CONNECTION_GLYPH[c.kind] ?? "🔌"} />
        <div>
          <div className="server-name">
            {c.name} <UpdateBadge update={c.update} />
          </div>
          <div className="muted small">
            {c.title} · <code>{c.image}</code>
          </div>
          <div className="muted small">
            {formatNetwork(c.network)} · restart {c.restartPolicy || "no"}
          </div>
          {job ? (
            <JobStatus job={job} />
          ) : progress ? (
            <div className="small update-inline">
              <Spinner /> {progress}
            </div>
          ) : (
            c.state === "running" && <ResourceSummary stats={stats} />
          )}
        </div>
      </Link>
      <div className="server-side">
        <StateBadge state={c.state} health={c.health} />
        <span className="muted small">{c.status}</span>
        <div className="btn-row">
          {c.update?.state === "available" &&
            (c.self ? (
              <Link className="btn small primary" to="/system">
                Update
              </Link>
            ) : (
              <button className="btn small primary" disabled={busy || working} onClick={onUpdate}>
                Update
              </button>
            ))}
          {running ? (
            <>
              <button className="btn small" disabled={busy || working} onClick={() => onAction("restart")}>
                Restart
              </button>
              {!c.self && (
                <button className="btn small danger" disabled={busy || working} onClick={() => onAction("stop")}>
                  Stop
                </button>
              )}
            </>
          ) : (
            <button className="btn small primary" disabled={busy || working} onClick={() => onAction("start")}>
              Start
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

function UpdateBadge({ update: u }: { update?: UpdateStatus }) {
  if (!u) return null;
  const checked = `Checked ${timeAgo(u.checkedAt)}`;
  switch (u.state) {
    case "available":
      return (
        <span className="badge update-available" title={`${u.reason ?? ""}\n${checked}`}>
          Update available
        </span>
      );
    case "current":
      return (
        <span className="badge update-current" title={checked}>
          Up to date
        </span>
      );
    case "error":
      return (
        <span className="badge update-error" title={`${u.reason ?? ""}\n${checked}`}>
          Update check failed
        </span>
      );
    default:
      return (
        <span className="badge" title={`${u.reason ?? ""}\n${checked}`}>
          Update status unknown
        </span>
      );
  }
}
