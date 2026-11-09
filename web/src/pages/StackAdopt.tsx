import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useAdmin } from "../access";
import { api } from "../api";
import { DeployProgress } from "../components/DeployProgress";
import { HostPicker } from "../components/HostPicker";
import { StackPlanCard } from "../components/StackPlanCard";
import type { DeployEvent, StackAdoptPlan } from "../types";

/** Adopt a Compose stack: hlm reads its file on the host and takes over the containers Compose made. */
export function StackAdoptPage() {
  const admin = useAdmin();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [host, setHost] = useState(params.get("host") || "local");
  const [path, setPath] = useState(params.get("path") ?? "");
  const [found, setFound] = useState<StackAdoptPlan>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [showFile, setShowFile] = useState(false);

  const edit = (f: (v: string) => void) => (v: string) => {
    f(v);
    setFound(undefined);
  };

  async function check() {
    setBusy("check");
    setError(undefined);
    try {
      setFound(await api.previewAdoptStack(host, path.trim()));
    } catch (e) {
      setError((e as Error).message);
      setFound(undefined);
    } finally {
      setBusy(undefined);
    }
  }

  // Came from a Compose container on the Adopt page: look straight away.
  useEffect(() => {
    if (params.get("path")) check();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function adopt() {
    if (!found) return;
    setBusy("adopt");
    setEvents([]);
    let ok = false;
    try {
      await api.adoptStack(host, found.found.source, (e) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "done") ok = true;
      });
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
    }
    if (ok) setTimeout(() => navigate(`/stacks/${encodeURIComponent(found.found.name)}`), 1500);
  }

  if (!admin) return <div className="page">Only admins can adopt stacks.</div>;
  const running = found ? Object.keys(found.found.containers).length : 0;

  return (
    <div className="page stack-editor">
      <Link to="/adopt" className="back">
        ← Adopt a container
      </Link>
      <h1>Adopt a Compose stack</h1>
      <p className="muted">
        A stack you started with <code>docker compose up</code>. hlm reads its <code>docker-compose.yml</code> and{" "}
        <code>.env</code> on the host, keeps a copy as the stack&apos;s, and replaces each container Compose made with
        one of its own servers: same name, image, folders, volumes and networks. Docker can&apos;t add hlm&apos;s labels
        to a container, so each one is recreated, as an update does, and the old one stays until the new one runs.
        Relative paths stay relative to the file&apos;s folder. The stack keeps Compose&apos;s project name, as its
        networks and volumes are named after it.
      </p>
      <div className="card config-form">
        <HostPicker value={host} onChange={edit(setHost)} help="The machine the stack runs on." />
        <label className="field">
          <span className="field-label">Compose file</span>
          <input
            value={path}
            onChange={(e) => edit(setPath)(e.target.value)}
            placeholder="/opt/stacks/nextcloud/docker-compose.yml"
            spellCheck={false}
            onKeyDown={(e) => {
              if (e.key === "Enter" && path.trim()) check();
            }}
          />
          <span className="field-help">
            Its full path on the host, or the folder it&apos;s in (hlm looks for <code>compose.yaml</code>,{" "}
            <code>compose.yml</code>, <code>docker-compose.yaml</code> and <code>docker-compose.yml</code>, as Compose
            does). The <code>.env</code> beside it is read too.
          </span>
        </label>
        {error && <div className="alert error">{error}</div>}
        <div className="btn-row">
          <button className="btn" onClick={check} disabled={!!busy || !path.trim()}>
            {busy === "check" ? "Reading…" : "Check"}
          </button>
          {found && (
            <button className="btn primary" onClick={adopt} disabled={!!busy}>
              {busy === "adopt" ? "Adopting…" : `Adopt ${found.found.name}`}
            </button>
          )}
        </div>
      </div>

      {found && (
        <div className="card">
          <p className="no-margin">
            <strong>{found.found.name}</strong> from <code>{found.found.source}</code>:{" "}
            {running
              ? `${running} container${running === 1 ? "" : "s"} Compose made ${running === 1 ? "is" : "are"} taken over`
              : "Compose hasn't made any containers for it, so hlm creates them"}
            {found.found.env ? ", with its .env." : "."}
          </p>
          <button type="button" className="link-button small" onClick={() => setShowFile((v) => !v)}>
            {showFile ? "Hide the file" : "Show the file"}
          </button>
          {showFile && <pre className="code-input small">{found.found.yaml}</pre>}
        </div>
      )}
      {found && <StackPlanCard plan={found.plan} />}
      {events.length > 0 && (
        <div className="card">
          <DeployProgress events={events} />
        </div>
      )}
    </div>
  );
}
