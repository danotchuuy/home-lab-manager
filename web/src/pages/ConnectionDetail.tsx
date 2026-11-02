import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, ApiError } from "../api";
import { AppIcon, CONNECTION_GLYPH, Spinner, StateBadge } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import { LogViewer } from "../components/LogViewer";
import { StatTiles } from "../components/Resources";
import type { Connection, DeployEvent, Stats, WireGuardCheck, WireGuardStatus } from "../types";
import { usePoll } from "../usePoll";
import { formatNetwork, timeAgo } from "../util";

type Tab = "logs" | "info";

/** A container hlm runs for a connection, in the style of a server's page. */
export function ConnectionDetailPage() {
  const { name = "" } = useParams();
  const navigate = useNavigate();
  const [conn, setConn] = useState<Connection>();
  const [stats, setStats] = useState<Stats>();
  const [error, setError] = useState<string>();
  const [params, setParams] = useSearchParams();
  const tab: Tab = params.get("tab") === "info" ? "info" : "logs";
  const setTab = (t: Tab) => setParams(t === "logs" ? {} : { tab: t });
  const [busy, setBusy] = useState(false);
  const [events, setEvents] = useState<DeployEvent[]>([]);

  const load = useCallback(async () => {
    try {
      setConn(await api.connection(name));
      setError(undefined);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) navigate("/", { replace: true });
      else setError((e as Error).message);
    }
  }, [name, navigate]);

  usePoll(load, 5000, [load]);

  const running = conn?.state === "running";
  useEffect(() => {
    if (!running) setStats(undefined);
  }, [running]);
  usePoll(
    async () => {
      // Stats are best-effort; usePoll ignores failures.
      if (running) setStats(await api.connectionStats(name));
    },
    5000,
    [name, running],
  );

  async function act(action: "start" | "stop" | "restart") {
    setBusy(true);
    try {
      if (conn?.self) await api.restartSelf(name);
      else await api.connectionAction(name, action);
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function update() {
    if (!conn) return;
    if (!confirm(`Update ${conn.name}? It's recreated on the new image, so the ${conn.title.toLowerCase()} drops for a few seconds.`))
      return;
    setBusy(true);
    setEvents([]);
    try {
      await api.updateConnection(name, (e) => setEvents((prev) => [...prev, e]));
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(false);
      load();
    }
  }

  if (error && !conn) return <div className="page"><div className="alert error">{error}</div></div>;
  if (!conn) return <div className="page"><Spinner /></div>;

  return (
    <div className="page">
      <Link to="/" className="back">
        ← Servers
      </Link>
      <div className="app-hero">
        <AppIcon glyph={CONNECTION_GLYPH[conn.kind] ?? "🔌"} size={72} />
        <div className="grow">
          <h1>
            {conn.name} <StateBadge state={conn.state} health={conn.health} />
          </h1>
          <div className="muted">
            {conn.title} · <code>{conn.image}</code> · {conn.status}
          </div>
          <div className="muted small">
            Network: {formatNetwork(conn.network)} · restart {conn.restartPolicy || "no"}
          </div>
        </div>
        <div className="btn-row">
          <Link className="btn ghost" to={conn.page}>
            {conn.self ? "Version and updates" : `Manage ${conn.title.toLowerCase()}`}
          </Link>
          {conn.update?.state === "available" &&
            (conn.self ? (
              <Link className="btn primary" to="/system">
                Update
              </Link>
            ) : (
              <button className="btn primary" disabled={busy} onClick={update}>
                Update
              </button>
            ))}
          {running ? (
            <>
              <button className="btn" disabled={busy} onClick={() => act("restart")}>
                Restart
              </button>
              {!conn.self && (
                <button className="btn danger" disabled={busy} onClick={() => act("stop")}>
                  Stop
                </button>
              )}
            </>
          ) : (
            <button className="btn primary" disabled={busy} onClick={() => act("start")}>
              Start
            </button>
          )}
        </div>
      </div>

      {error && <div className="alert error">{error}</div>}

      {stats && <StatTiles stats={stats} />}

      <div className="tabs">
        <button className={tab === "logs" ? "active" : ""} onClick={() => setTab("logs")}>
          Console
        </button>
        <button className={tab === "info" ? "active" : ""} onClick={() => setTab("info")}>
          Details
        </button>
      </div>

      {events.length > 0 && (
        <div className="card">
          <DeployProgress events={events} />
        </div>
      )}

      {tab === "logs" && <LogViewer url={api.connectionLogsUrl(conn.name)} running={running} />}

      {tab === "info" && (
        <>
          {conn.kind === "wireguard" && <WireGuardDetails />}
          <div className="card">
            <h2>Container</h2>
            <dl className="facts">
              <dt>Image</dt>
              <dd>
                <code>{conn.image}</code>
              </dd>
              <dt>Created</dt>
              <dd>{new Date(conn.created).toLocaleString()}</dd>
              <dt>Network</dt>
              <dd>{formatNetwork(conn.network)}</dd>
              <dt>Restart policy</dt>
              <dd>{conn.restartPolicy || "no"}</dd>
              {conn.update && (
                <>
                  <dt>Updates</dt>
                  <dd>
                    {conn.update.state === "available" ? "Update available" : conn.update.state === "current" ? "Up to date" : conn.update.state}
                    {conn.update.reason && ` (${conn.update.reason})`} · checked {timeAgo(conn.update.checkedAt)}
                  </dd>
                </>
              )}
            </dl>
          </div>
        </>
      )}
    </div>
  );
}

/** The tunnel's addresses and the proxy end's view of it. */
function WireGuardDetails() {
  const [wg, setWG] = useState<WireGuardStatus>();
  const [check, setCheck] = useState<WireGuardCheck>();
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    api.wireGuard().then(setWG, (e) => setError(e.message));
  }, []);

  async function runCheck() {
    setChecking(true);
    setError(undefined);
    try {
      setCheck(await api.checkWireGuard());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }

  return (
    <div className="card">
      <div className="settings-actions">
        <h2>Tunnel</h2>
        <button className="btn small" disabled={checking || !wg?.configured} onClick={runCheck}>
          {checking ? "Checking…" : "Check tunnel"}
        </button>
      </div>
      {error && <div className="alert error">{error}</div>}
      {!wg && !error && <Spinner />}
      {wg && !wg.configured && <p className="muted small">The tunnel isn&apos;t set up.</p>}
      {wg?.configured && (
        <dl className="facts">
          <dt>Tunnel network</dt>
          <dd>
            <code>{wg.network}</code>
          </dd>
          <dt>This host</dt>
          <dd>
            <code>{wg.homeAddress}</code>
          </dd>
          <dt>Proxy server</dt>
          <dd>
            <code>{wg.proxyAddress}</code> · connects to <code>{wg.endpoint}</code>
          </dd>
          {wg.lanNetwork && (
            <>
              <dt>LAN mode</dt>
              <dd>
                {wg.lanNetwork} (<code>{wg.lanSubnet}</code>)
                {wg.home.lanAddress && (
                  <>
                    {" "}
                    at <code>{wg.home.lanAddress}</code>
                  </>
                )}
              </dd>
            </>
          )}
          <dt>Relays</dt>
          <dd>{wg.inUse ? "Go through the tunnel" : "Don't use the tunnel"}</dd>
          {wg.home.error && (
            <>
              <dt>Error</dt>
              <dd>{wg.home.error}</dd>
            </>
          )}
          {check && (
            <>
              <dt>Last handshake</dt>
              <dd>{check.handshake ? timeAgo(check.handshake) : "Never, since the proxy end started"}</dd>
              {check.reachable !== undefined && (
                <>
                  <dt>Ping through the tunnel</dt>
                  <dd>{check.reachable ? "Reachable" : "No reply"}</dd>
                </>
              )}
            </>
          )}
        </dl>
      )}
    </div>
  );
}
