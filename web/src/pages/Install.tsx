import { useEffect, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { api } from "../api";
import { AppIcon, Overview, Spinner } from "../components/common";
import { ConfigForm } from "../components/ConfigForm";
import { DeployProgress } from "../components/DeployProgress";
import { HostPicker } from "../components/HostPicker";
import { ImportImage } from "../components/ImportImage";
import type { App, DeployEvent, DeployRequest } from "../types";

export function InstallPage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [app, setApp] = useState<App>();
  const [error, setError] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [running, setRunning] = useState(false);
  const [rereading, setRereading] = useState(false);
  const [host, setHost] = useState("local");
  // Reload when the template was just re-read from its image.
  const refreshed = (useLocation().state as { refreshed?: number } | null)?.refreshed;

  // The template's paths are the chosen host's.
  useEffect(() => {
    api.app(id, host).then(setApp, (e) => setError(e.message));
  }, [id, refreshed, host]);

  async function deleteTemplate() {
    if (!confirm("Delete this template? Servers already installed from it keep running, but can't be reconfigured from it.")) return;
    try {
      await api.deleteTemplate(id);
      navigate("/catalog?category=Custom");
    } catch (e) {
      setError((e as Error).message);
    }
  }

  async function install(req: DeployRequest) {
    setRunning(true);
    setEvents([]);
    let created: string | undefined;
    try {
      await api.install(
        req,
        (e) => {
          setEvents((prev) => [...prev, e]);
          if (e.stage === "done" && e.server) created = e.server.name;
        },
        host,
      );
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setRunning(false);
    }
    if (created) setTimeout(() => navigate(`/servers/${encodeURIComponent(created!)}`), 800);
  }

  if (error) return <div className="page"><div className="alert error">{error}</div></div>;
  if (!app) return <div className="page"><Spinner /></div>;

  const links = [
    ["Project", app.project],
    ["Support", app.support],
    ["Readme", app.readme],
    ["Registry", app.registry],
    ["Template", app.templateUrl],
  ].filter(([, url]) => url) as [string, string][];

  return (
    <div className="page">
      <Link to="/catalog" className="back">
        ← Catalog
      </Link>
      <div className="app-hero">
        <AppIcon src={app.icon} size={72} />
        <div>
          <h1>{app.name}</h1>
          <div className="muted">
            {[app.author || app.maintainer, app.repo].filter(Boolean).join(" · ")} · <code>{app.repository}</code>
          </div>
          <div className="link-row">
            {links.map(([label, url]) => (
              <a key={label} href={url} target="_blank" rel="noreferrer">
                {label} ↗
              </a>
            ))}
          </div>
        </div>
      </div>

      {app.fromImage && (
        <div className="alert info from-image">
          <div>
            Made from the defaults built into <code>{app.fromImage}</code>. Images don&apos;t say which settings matter, so
            check the ports, data folders and variables below; variables you leave unchanged aren&apos;t set on the
            container, so the image&apos;s own defaults apply.
          </div>
          <div className="btn-row">
            <button className="btn small" onClick={() => setRereading((v) => !v)}>
              Read the image again
            </button>
            <button className="btn small danger" onClick={deleteTemplate}>
              Delete template
            </button>
          </div>
        </div>
      )}
      {rereading && app.fromImage && <ImportImage initial={app.fromImage} onClose={() => setRereading(false)} />}

      <div className="two-col">
        <div className="card">
          <h2>About</h2>
          <Overview text={app.overview} />
          {app.requires && (
            <div className="alert info">
              <strong>Requirements:</strong> {app.requires}
            </div>
          )}
          {app.privileged && <div className="alert warn">This template runs the container in privileged mode.</div>}
          {app.extraParams && (
            <p className="muted small">
              Extra docker flags: <code>{app.extraParams}</code>
            </p>
          )}
        </div>

        <div className="card">
          <h2>Configure</h2>
          {events.length > 0 && <DeployProgress events={events} />}
          <HostPicker value={host} onChange={setHost} />
          <ConfigForm
            key={host}
            app={app}
            host={host}
            submitLabel={running ? "Installing…" : "Install & start"}
            busy={running}
            onSubmit={install}
          />
        </div>
      </div>
    </div>
  );
}
