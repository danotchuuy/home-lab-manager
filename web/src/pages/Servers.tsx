import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { can, useAdmin } from "../access";
import { UpdateBadge } from "../components/UpdateBadge";
import { api, type BulkAction } from "../api";
import { CopyButton, connectAddresses } from "../components/ConnectInfo";
import { Ago, AppIcon, CONNECTION_GLYPH, CrashLoopBadge, HealthBadge, Spinner, StateBadge } from "../components/common";
import { GameStatusBadge } from "../components/GameStatus";
import { HostBadge, useHosts } from "../components/HostPicker";
import { InstallCard, JobStatus } from "../components/JobProgress";
import { ResourceSummary } from "../components/Resources";
import { StackList } from "../components/StackList";
import { UpdateProgress } from "../components/UpdateProgress";
import type {
  AutoUpdates,
  Connection,
  DeployEvent,
  GameSummary,
  Job,
  Server,
  Stats,
  UpcomingTask,
  UpdateCheckInfo,
} from "../types";
import { usePoll } from "../usePoll";
import { allTags, matches, sortServers, viewFromParams, viewToParams, type ListView } from "../serverlist";
import { bareName, formatInZone, hostOf, formatNetwork, formatPorts, timeUntil, webUILink } from "../util";

export function ServersPage() {
  const admin = useAdmin();
  const [servers, setServers] = useState<Server[] | null>(null);
  const hosts = useHosts();
  const unreachable = hosts?.filter((h) => h.status && !h.status.ok) ?? [];
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
  // Search, filters and order, in the URL so a view can be bookmarked.
  const [params, setParams] = useSearchParams();
  const view = viewFromParams(params);
  const setView = (change: Partial<ListView>) =>
    setParams(viewToParams({ ...view, ...change }), { replace: true });
  const group = view.group;
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    // From the "/" shortcut on another page.
    if (params.get("focus") === "search") searchRef.current?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  // Servers ticked for a bulk action, and its progress.
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkEvents, setBulkEvents] = useState<DeployEvent[]>([]);
  const [bulkRunning, setBulkRunning] = useState<BulkAction>();
  // Each server's next scheduled task, and the time zone it's in.
  const [nextTasks, setNextTasks] = useState<Record<string, UpcomingTask>>({});
  const [timezone, setTimezone] = useState("");
  const [auto, setAuto] = useState<AutoUpdates>();
  const [games, setGames] = useState<Record<string, GameSummary>>({});

  const load = useCallback(async () => {
    // Each part is shown as soon as it arrives, so a slow one (another Docker
    // host taking its time, say) doesn't hold up the server list.
    // Connections are extra, and only for admins; the servers show without them.
    const conns = admin
      ? api.connections().then(setConnections, () => setConnections([]))
      : Promise.resolve(setConnections([]));
    const info = api.updateInfo().then(setUpdateInfo, () => {});
    try {
      setServers(await api.servers());
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    }
    api.autoUpdates().then(setAuto, () => {});
    api.gameStatuses().then(setGames, () => {});
    // Scheduled tasks are extra; the list still works without them.
    api.upcomingTasks().then((up) => {
      const next: Record<string, UpcomingTask> = {};
      for (const u of up) if (!next[u.server]) next[u.server] = u;
      setNextTasks(next);
    }, () => {});
    // The next refresh waits for the rest, so slow requests don't pile up.
    await Promise.all([conns, info]);
  }, [admin]);

  usePoll(load, 5000, [load]);

  useEffect(() => {
    api.timezone().then((t) => setTimezone(t.effective), () => {});
  }, []);

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

  async function toggleFavourite(s: Server) {
    try {
      await api.setFavourite(s.id, !s.favourite);
      setServers((prev) => prev?.map((p) => (p.id === s.id ? { ...p, favourite: !s.favourite } : p)) ?? null);
    } catch (e) {
      setError((e as Error).message);
    }
  }

  function toggleSelected(name: string, on: boolean) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(name);
      else next.delete(name);
      return next;
    });
  }

  async function runBulk(action: BulkAction) {
    const targets = chosen.filter((s) => can(s, "operator"));
    const restarting = targets.filter((s) => s.state === "running" && action !== "start" && action !== "backup");
    const what = `${action === "backup" ? "Back up" : action[0].toUpperCase() + action.slice(1)} ${targets.length} server${targets.length === 1 ? "" : "s"}?`;
    const warn = restarting.length && action !== "update" ? `\n\nPlayers on ${restarting.map((s) => bareName(s.name)).join(", ")} will be disconnected.` : "";
    if (!confirm(what + warn)) return;
    setBulkRunning(action);
    setBulkEvents([]);
    try {
      await api.bulk(
        targets.map((s) => s.name),
        action,
        (e) => setBulkEvents((prev) => [...prev, e]),
      );
    } catch (e) {
      setBulkEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBulkRunning(undefined);
      await load();
    }
  }

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

  // Stacks' services are updated with their stack.
  const updatable = servers?.filter((s) => s.update?.state === "available" && can(s, "operator") && !s.stack) ?? [];
  const groups = [...new Set(servers?.flatMap((s) => s.groups ?? []))].sort();
  const tags = allTags(servers ?? []);
  const listed = servers?.filter((s) => !s.stack) ?? [];
  const shown = servers ? sortServers(listed.filter((s) => matches(s, view)), view, { stats, games }) : undefined;
  const filtering = viewToParams({ ...view, sort: "name" }).toString() !== "";
  const operable = (shown ?? []).filter((s) => can(s, "operator"));
  const chosen = (servers ?? []).filter((s) => selected.has(s.name));
  const serverGroups = Object.fromEntries((servers ?? []).map((s) => [s.name, s.groups]));

  async function checkUpdates() {
    setChecking(true);
    try {
      // It runs in the background: follow it, showing each server's status
      // as its image is looked up.
      let info = await api.checkUpdates();
      setUpdateInfo(info);
      while (info.checking) {
        await new Promise((r) => setTimeout(r, 1000));
        info = await api.updateInfo();
        setUpdateInfo(info);
        if (!info.checking) break;
        setServers(await api.servers());
      }
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
  // A finished install's server is missing from the list while its host
  // can't be reached; that's not an install still going.
  const offline = (server: string) => unreachable.some((h) => h.id === hostOf(server));
  const installs = jobs.filter(
    (j) =>
      j.kind === "install" &&
      (j.error || !j.finished || (!names.has(j.server) && !offline(j.server))),
  );
  const jobFor = (name: string) => jobs.find((j) => j.server === name && j.kind !== "install" && !j.finished);
  const checkingNow = checking || !!updateInfo?.checking;

  return (
    <div className="page">
      <div className="page-header">
        <h1>Servers</h1>
        {admin && (
          <div className="btn-row">
            <Link to="/adopt" className="btn" title="Make containers hlm didn't create into servers">
              Adopt a container
            </Link>
            <Link to="/catalog" className="btn primary">
              + New server
            </Link>
          </div>
        )}
      </div>

      {((servers && servers.length > 0) || connections.length > 0) && (
        <div className="update-bar">
          <span className="muted small">
            {checkingNow ? (
              `Checking for updates…${updateInfo?.total ? ` ${updateInfo.done ?? 0} of ${updateInfo.total} images` : ""}`
            ) : (
              <>
                Updates last checked <Ago iso={updateInfo?.lastCheck} />
              </>
            )}
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

      {listed.length > 0 && (
        <div className="list-toolbar" role="search">
          <input
            ref={searchRef}
            data-search
            type="search"
            className="list-search"
            placeholder="Search servers (press /)"
            aria-label="Search servers"
            value={view.q}
            onChange={(e) => setView({ q: e.target.value })}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                setView({ q: "" });
                e.currentTarget.blur();
              }
            }}
          />
          <select aria-label="Show" value={view.state} onChange={(e) => setView({ state: e.target.value as ListView["state"] })}>
            <option value="">All servers</option>
            <option value="running">Running</option>
            <option value="stopped">Stopped</option>
            <option value="problem">With a problem</option>
            <option value="update">With an update</option>
          </select>
          {groups.length > 0 && (
            <select aria-label="Server group" value={group} onChange={(e) => setView({ group: e.target.value })}>
              <option value="">All groups</option>
              {groups.map((g) => (
                <option key={g} value={g}>
                  {g}
                </option>
              ))}
            </select>
          )}
          {tags.length > 0 && (
            <select aria-label="Tag" value={view.tag} onChange={(e) => setView({ tag: e.target.value })}>
              <option value="">All tags</option>
              {tags.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          )}
          {hosts && hosts.length > 1 && (
            <select aria-label="Docker host" value={view.host} onChange={(e) => setView({ host: e.target.value })}>
              <option value="">All hosts</option>
              {hosts.map((h) => (
                <option key={h.id} value={h.id}>
                  {h.name}
                </option>
              ))}
            </select>
          )}
          <select aria-label="Sort by" value={view.sort} onChange={(e) => setView({ sort: e.target.value as ListView["sort"] })}>
            <option value="name">Sort: name</option>
            <option value="state">Sort: state</option>
            <option value="cpu">Sort: CPU</option>
            <option value="memory">Sort: memory</option>
            <option value="players">Sort: players</option>
          </select>
          {filtering && (
            <button className="btn small ghost" onClick={() => setParams(viewToParams({ ...view, q: "", state: "", group: "", tag: "", host: "" }), { replace: true })}>
              Clear filters
            </button>
          )}
        </div>
      )}

      {operable.length > 0 && (
        <div className="bulk-bar">
          <label className="radio small">
            <input
              type="checkbox"
              aria-label="Select every server shown"
              checked={operable.every((s) => selected.has(s.name))}
              onChange={(e) => setSelected(e.target.checked ? new Set(operable.map((s) => s.name)) : new Set())}
            />
            {chosen.length > 0 ? `${chosen.length} selected` : "Select all"}
          </label>
          {chosen.length > 0 && (
            <>
              {(["start", "stop", "restart", "update", "backup"] as BulkAction[]).map((a) => (
                <button
                  key={a}
                  className={`btn small${a === "stop" ? " danger" : ""}`}
                  disabled={!!bulkRunning || updatingAll}
                  onClick={() => runBulk(a)}
                >
                  {bulkRunning === a ? "Working…" : a === "backup" ? "Back up now" : a[0].toUpperCase() + a.slice(1)}
                </button>
              ))}
              <button className="btn small ghost" onClick={() => setSelected(new Set())} disabled={!!bulkRunning}>
                Clear
              </button>
            </>
          )}
        </div>
      )}
      {bulkEvents.length > 0 && (
        <UpdateProgress
          events={bulkEvents}
          running={!!bulkRunning}
          title={bulkRunning ? "Working on the selected servers…" : "Done"}
          onClose={bulkRunning ? undefined : () => setBulkEvents([])}
        />
      )}

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
      {unreachable.map((h) => (
        <div key={h.id} className="alert warn">
          {h.name} can&apos;t be reached, so its servers aren&apos;t listed{h.status?.error ? `: ${h.status.error}` : "."}
        </div>
      ))}
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

      {shown && shown.length === 0 && listed.length > 0 && (
        <div className="empty">
          <p>No servers match.</p>
          <button className="btn" onClick={() => setParams(new URLSearchParams(), { replace: true })}>
            Show all servers
          </button>
        </div>
      )}

      {!filtering && <StackList group={group} groups={serverGroups} />}

      <div className="server-list">
        {shown?.map((s) => {
          const running = s.state === "running" || s.state === "restarting";
          const link = webUILink(s.webui);
          const job = jobFor(s.name);
          const progress = job ? (job.message ?? "Working…") : updating[s.id];
          return (
            <div key={s.id} className={`card server-card${selected.has(s.name) ? " selected" : ""}`}>
              <div className="server-pick">
                {can(s, "operator") && (
                  <input
                    type="checkbox"
                    aria-label={`Select ${bareName(s.name)}`}
                    checked={selected.has(s.name)}
                    onChange={(e) => toggleSelected(s.name, e.target.checked)}
                  />
                )}
                <button
                  type="button"
                  className={`icon-button star${s.favourite ? " on" : ""}`}
                  aria-pressed={!!s.favourite}
                  aria-label={s.favourite ? `Unpin ${bareName(s.name)}` : `Pin ${bareName(s.name)} to the top`}
                  title={s.favourite ? "Pinned to the top: click to unpin" : "Pin to the top"}
                  onClick={() => toggleFavourite(s)}
                >
                  {s.favourite ? "★" : "☆"}
                </button>
              </div>
              <Link to={`/servers/${encodeURIComponent(s.name)}`} className="server-main">
                <AppIcon src={s.icon} />
                <div>
                  <div className="server-name">
                    {bareName(s.name)}
                    <HostBadge host={s.host} hosts={hosts} />{" "}
                    <UpdateBadge
                      update={s.update}
                      auto={(auto?.servers[s.name] || auto?.default) === "auto"}
                      state={auto?.states[s.name]}
                    />{" "}
                    <NextTaskBadge task={nextTasks[s.name]} timezone={timezone} />
                  </div>
                  <div className="muted small">
                    {s.app} · <code>{s.image}</code>
                  </div>
                  <div className="muted small">
                    {formatNetwork(s.network, s.ipAddress)}
                    {s.ports.length > 0 && ` · ${formatPorts(s.ports)}`}
                  </div>
                  {s.tags && s.tags.length > 0 && (
                    <div className="tag-chips">
                      {s.tags.map((t) => (
                        <button
                          key={t}
                          type="button"
                          className="tag-chip"
                          title={`Show servers tagged ${t}`}
                          onClick={(e) => {
                            e.preventDefault();
                            setView({ tag: t });
                          }}
                        >
                          {t}
                        </button>
                      ))}
                    </div>
                  )}
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
                <StateBadge state={s.state} asleep={s.asleep} />
                <CrashLoopBadge crashLoop={s.crashLoop} />
                <HealthBadge state={s.state} health={s.health} />
                <GameStatusBadge s={games[s.name]} />
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
                  {(() => {
                    const first = connectAddresses(s, hosts)[0];
                    return first ? (
                      <CopyButton text={first.address} label={`Copy ${bareName(s.name)}'s address`}>
                        Copy address
                      </CopyButton>
                    ) : null;
                  })()}
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
                    <>
                      {s.crashLoop && can(s, "operator") && (
                        <Link className="btn ghost small" to={`/servers/${encodeURIComponent(s.name)}`}>
                          View logs
                        </Link>
                      )}
                      <button
                        className="btn small primary"
                        disabled={busy === s.id || !!progress}
                        onClick={() => act(s, "start")}
                      >
                        {s.crashLoop ? "Start anyway" : "Start"}
                      </button>
                    </>
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

function NextTaskBadge({ task, timezone }: { task?: UpcomingTask; timezone: string }) {
  if (!task) return null;
  return (
    <span className="badge next-task" title={`Next scheduled task: ${task.taskName} (${task.when}), ${timeUntil(task.next)}`}>
      ⏰ {task.taskName} {timezone ? formatInZone(task.next, timezone) : timeUntil(task.next)}
    </span>
  );
}

