import { useState, type ChangeEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../api";
import { AppIcon } from "../components/common";
import { ConfigForm } from "../components/ConfigForm";
import { DeployProgress } from "../components/DeployProgress";
import { HostPicker } from "../components/HostPicker";
import { TransferNotes } from "../components/TransferNotes";
import type { DeployEvent, DeployRequest, ServerExport, TransferPreview } from "../types";

/** Installs a server from a file another hlm (or this one) exported. */
export function ImportServerPage() {
  const navigate = useNavigate();
  const [file, setFile] = useState<ServerExport>();
  const [preview, setPreview] = useState<TransferPreview>();
  const [error, setError] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [running, setRunning] = useState(false);
  const [host, setHost] = useState("local");

  async function choose(e: ChangeEvent<HTMLInputElement>) {
    const f = e.target.files?.[0];
    setError(undefined);
    setPreview(undefined);
    setFile(undefined);
    if (!f) return;
    try {
      if (f.size > 1 << 20) throw new Error("That file is too big to be a server's settings.");
      let exp: ServerExport;
      try {
        exp = JSON.parse(await f.text()) as ServerExport;
      } catch {
        throw new Error("That isn't a server exported from hlm (it isn't JSON).");
      }
      setPreview(await api.previewImport(exp, host));
      setFile(exp);
    } catch (err) {
      setError((err as Error).message);
    }
  }

  // Paths and ports are worked out for the host it goes on.
  async function changeHost(h: string) {
    setHost(h);
    if (!file) return;
    try {
      setPreview(await api.previewImport(file, h));
    } catch (err) {
      setError((err as Error).message);
    }
  }

  async function install(req: DeployRequest) {
    if (!file) return;
    setRunning(true);
    setEvents([]);
    let created: string | undefined;
    try {
      await api.importServer(
        file,
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
    if (created) setTimeout(() => navigate(`/servers/${encodeURIComponent(created!)}`), 1200);
  }

  return (
    <div className="page">
      <Link to="/catalog" className="back">
        ← Catalog
      </Link>
      <h1>Import a server</h1>
      <p className="muted">
        Choose a <code>.hlm.json</code> file from <strong>Export settings</strong> on a server&apos;s Settings tab, here
        or on another hlm. You can change anything before installing. Its data isn&apos;t in the file: restore a backup
        into it afterwards, or copy the files across.
      </p>
      <div className="card">
        <HostPicker value={host} onChange={changeHost} />
        <input type="file" accept=".json,application/json" onChange={choose} disabled={running} />
        {error && <div className="alert error">{error}</div>}
      </div>
      {preview && file && (
        <div className="card">
          <div className="app-hero compact">
            <AppIcon src={preview.app.icon} size={48} />
            <div>
              <h2>{preview.app.name}</h2>
              <div className="muted small">
                Exported from {file.server}
                {file.hlmVersion ? ` (hlm ${file.hlmVersion})` : ""} on {new Date(file.exported).toLocaleString()} ·{" "}
                <code>{preview.app.repository}</code>
              </div>
            </div>
          </div>
          <TransferNotes preview={preview} />
          {events.length > 0 && <DeployProgress events={events} />}
          <ConfigForm
            key={file.exported + host}
            app={preview.app}
            initial={preview.request}
            isNew
            host={host}
            submitLabel={running ? "Installing…" : "Install & start"}
            busy={running}
            onSubmit={install}
          />
        </div>
      )}
    </div>
  );
}
