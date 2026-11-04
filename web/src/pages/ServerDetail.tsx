import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { can, useAdmin } from "../access";
import { api, ApiError } from "../api";
import { AppIcon, Spinner, StateBadge } from "../components/common";
import { ConfigFilesCard } from "../components/ConfigFilesCard";
import { ConfigForm } from "../components/ConfigForm";
import { ConsoleInputBar } from "../components/Console";
import { DeployProgress } from "../components/DeployProgress";
import { FileBrowser } from "../components/FileBrowser";
import { GameStatusCard } from "../components/GameStatus";
import { LogViewer } from "../components/LogViewer";
import { PortSettings } from "../components/PortSettings";
import { PortForwarding } from "../components/PortForwarding";
import { ServerBackups } from "../components/ServerBackups";
import { ServerGroupPicker } from "../components/ServerGroups";
import { ServerHostnames } from "../components/ServerHostnames";
import { ServerSchedule } from "../components/ServerSchedule";
import { ServerUpdatePolicyCard } from "../components/UpdatePolicy";
import { StatTiles } from "../components/Resources";
import type { App, DeployEvent, DeployRequest, Server, Stats } from "../types";
import { usePoll } from "../usePoll";
import { formatNetwork, formatPorts, webUILink } from "../util";

type Tab = "logs" | "files" | "backups" | "schedule" | "ports" | "settings";

export function ServerDetailPage() {
  const { name = "" } = useParams();
  const navigate = useNavigate();
  const admin = useAdmin();
  const [server, setServer] = useState<Server>();
  const [stats, setStats] = useState<Stats>();
  // Bumped when internet ports change, so port forwarding reloads.
  const [portsVersion, setPortsVersion] = useState(0);
  // Bumped when the UniFi rules change, so the ports' sync states reload.
  const [rulesVersion, setRulesVersion] = useState(0);
  // Through the WireGuard tunnel, gateway rules aren't needed, so their card
  // is hidden unless some are still in place.
  const [showForwarding, setShowForwarding] = useState(true);
  const [error, setError] = useState<string>();
  const [params, setParams] = useSearchParams();
  const operator = !!server && can(server, "operator");
  const manager = !!server && can(server, "manager");
  // Viewers don't get the tabs that show logs, files or settings.
  const tabs: Tab[] =
    operator || !server ? ["logs", "files", "backups", "schedule", "ports", "settings"] : ["backups", "schedule", "ports"];
  const tab = (tabs.includes(params.get("tab") as Tab) ? params.get("tab") : tabs[0]) as Tab;
  const setTab = (t: Tab) => setParams(t === "logs" ? {} : { tab: t });
  const [busy, setBusy] = useState(false);
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [config, setConfig] = useState<{ app: App; request: DeployRequest; hidden?: string[] }>();
  const [configError, setConfigError] = useState<string>();

  const load = useCallback(async () => {
    try {
      setServer(await api.server(name));
      setError(undefined);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) navigate("/", { replace: true });
      else setError((e as Error).message);
    }
  }, [name, navigate]);

  usePoll(load, 5000, [load]);

  const running = server?.state === "running";
  useEffect(() => {
    if (!running) setStats(undefined);
  }, [running]);
  usePoll(
    async () => {
      // Stats are best-effort; usePoll ignores failures.
      if (running) setStats(await api.stats(name));
    },
    5000,
    [name, running],
  );

  useEffect(() => {
    if (tab !== "settings" || config || !operator) return;
    api.serverConfig(name).then(setConfig, (e) => setConfigError(e.message));
  }, [tab, name, config, operator]);

  async function act(action: "start" | "stop" | "restart") {
    setBusy(true);
    try {
      setServer(await api.action(name, action));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (
      !confirm(
        `Delete ${name}? The container and any port forwarding rules it has on your UniFi gateway are removed; its data folders on the host are kept.`,
      )
    )
      return;
    setBusy(true);
    try {
      await api.remove(name);
      navigate("/");
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  }

  async function runStream(op: (onEvent: (e: DeployEvent) => void) => Promise<void>) {
    setBusy(true);
    setEvents([]);
    try {
      await op((e) => setEvents((prev) => [...prev, e]));
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(false);
      setConfig(undefined);
      load();
    }
  }

  if (error && !server) return <div className="page"><div className="alert error">{error}</div></div>;
  if (!server) return <div className="page"><Spinner /></div>;

  const link = webUILink(server.webui);

  return (
    <div className="page">
      <Link to="/" className="back">
        ← Servers
      </Link>
      <div className="app-hero">
        <AppIcon src={server.icon} size={72} />
        <div className="grow">
          <h1>
            {server.name} <StateBadge state={server.state} health={server.health} />
          </h1>
          <div className="muted">
            {server.app} · <code>{server.image}</code> · {server.status}
          </div>
          <div className="muted small">
            Network: {formatNetwork(server.network, server.ipAddress)}
            {server.ports.length > 0 && ` · ${formatPorts(server.ports)}`}
          </div>
          {server.groups && server.groups.length > 0 && (
            <div className="muted small">Server groups: {server.groups.join(", ")}</div>
          )}
        </div>
        <div className="btn-row">
          {link && running && (
            <a className="btn ghost" href={link} target="_blank" rel="noreferrer">
              Web UI ↗
            </a>
          )}
          {!operator ? null : running ? (
            <>
              <button className="btn" disabled={busy} onClick={() => act("restart")}>
                Restart
              </button>
              <button className="btn danger" disabled={busy} onClick={() => act("stop")}>
                Stop
              </button>
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

      <GameStatusCard server={server.name} running={running} canManage={manager} />

      <div className="tabs">
        {operator && (
          <>
            <button className={tab === "logs" ? "active" : ""} onClick={() => setTab("logs")}>
              Console
            </button>
            <button className={tab === "files" ? "active" : ""} onClick={() => setTab("files")}>
              Files
            </button>
          </>
        )}
        <button className={tab === "backups" ? "active" : ""} onClick={() => setTab("backups")}>
          Backups
        </button>
        <button className={tab === "schedule" ? "active" : ""} onClick={() => setTab("schedule")}>
          Schedule
        </button>
        <button className={tab === "ports" ? "active" : ""} onClick={() => setTab("ports")}>
          Port forwarding
        </button>
        {operator && (
          <button className={tab === "settings" ? "active" : ""} onClick={() => setTab("settings")}>
            Settings
          </button>
        )}
      </div>

      {events.length > 0 && (
        <div className="card">
          <DeployProgress events={events} />
        </div>
      )}

      {tab === "logs" && (
        <LogViewer
          serverId={server.name}
          running={running}
          footer={(append) => (
            <ConsoleInputBar server={server.name} running={running} canManage={manager} append={append} />
          )}
        />
      )}

      {tab === "files" && <FileBrowser server={server.name} running={running} canEdit={manager} />}

      {tab === "backups" && <ServerBackups server={server.name} running={running} canRun={operator} canManage={manager} />}

      {tab === "schedule" && <ServerSchedule server={server.name} canRun={operator} canManage={manager} />}

      {tab === "ports" && (
        <>
          <fieldset className="plain-fieldset" disabled={!manager}>
            <PortSettings
              server={server.name}
              rulesVersion={rulesVersion}
              onSaved={() => setPortsVersion((v) => v + 1)}
              onForwardingNeeded={setShowForwarding}
            />
            {showForwarding && (
              <PortForwarding key={portsVersion} server={server.name} onChanged={() => setRulesVersion((v) => v + 1)} />
            )}
          </fieldset>
          {admin && <ServerHostnames server={server.name} />}
        </>
      )}

      {tab === "settings" && (
        <div className="card">
          <div className="settings-actions">
            <div>
              <h2>Update</h2>
              <p className="muted small">Pull the latest image and recreate the container with the same settings.</p>
            </div>
            <button className="btn" disabled={busy} onClick={() => runStream((cb) => api.update(name, cb))}>
              Update image
            </button>
          </div>
          <ServerUpdatePolicyCard server={server.name} canManage={manager} />
          <hr />
          <ConfigFilesCard server={server.name} canManage={manager} />
          <hr />
          <h2>Configuration</h2>
          <p className="muted small">Saving recreates the container. The old one is restored if the new one fails to start.</p>
          {configError && <div className="alert error">{configError}</div>}
          {!config && !configError && <Spinner />}
          {config?.hidden && config.hidden.length > 0 && (
            <div className="alert info small">
              Settings that look like passwords, keys or tokens are blank here: only managers of this server can see
              them.
            </div>
          )}
          {config && (
            <ConfigForm
              key={server.id}
              app={config.app}
              initial={{ ...config.request, name: server.name }}
              submitLabel={busy ? "Applying…" : "Save & recreate"}
              busy={busy}
              restricted={!admin}
              readOnly={!manager}
              onSubmit={(req) => runStream((cb) => api.reconfigure(name, req, cb))}
            />
          )}
          {admin && (
            <>
              <hr />
              <ServerGroupPicker server={server} onChanged={load} />
              <hr />
              <div className="settings-actions">
                <div>
                  <h2>Delete server</h2>
                  <p className="muted small">Removes the container. Data folders on the host are kept.</p>
                </div>
                <button className="btn danger" disabled={busy} onClick={remove}>
                  Delete
                </button>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}
