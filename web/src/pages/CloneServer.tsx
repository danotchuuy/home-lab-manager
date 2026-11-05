import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api } from "../api";
import { Spinner } from "../components/common";
import { ConfigForm } from "../components/ConfigForm";
import { DeployProgress } from "../components/DeployProgress";
import { TransferNotes } from "../components/TransferNotes";
import type { BackupObject, CopyData, DeployEvent, DeployRequest, Server, TransferPreview } from "../types";
import { formatBytes, timeAgo } from "../util";

/**
 * Makes a copy of a server: its settings under a new name, with its own data
 * folders and free host ports, and its data copied now or from a backup.
 * With ?backup=<key>, it's "restore as a new server".
 */
export function CloneServerPage() {
  const { name = "" } = useParams();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [server, setServer] = useState<Server>();
  const [newName, setNewName] = useState(`${name}-copy`);
  const [preview, setPreview] = useState<TransferPreview>();
  const [previewing, setPreviewing] = useState(false);
  const [copyData, setCopyData] = useState<CopyData>(params.get("backup") ? "backup" : "now");
  const [backupKey, setBackupKey] = useState(params.get("backup") ?? "");
  const [backups, setBackups] = useState<BackupObject[]>();
  const [stopSource, setStopSource] = useState(true);
  const [start, setStart] = useState(true);
  const [error, setError] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [running, setRunning] = useState(false);

  useEffect(() => {
    api.server(name).then(setServer, (e) => setError((e as Error).message));
    api.serverBackups(name).then(
      (o) => setBackups(o.backups),
      () => setBackups([]),
    );
  }, [name]);

  async function load() {
    setPreviewing(true);
    setError(undefined);
    setPreview(undefined);
    try {
      setPreview(await api.previewClone(name, newName.trim()));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPreviewing(false);
    }
  }

  async function clone(req: DeployRequest) {
    setRunning(true);
    setEvents([]);
    let created: string | undefined;
    try {
      await api.cloneServer(
        name,
        {
          name: req.name,
          request: req,
          copyData,
          backupKey: copyData === "backup" ? backupKey : undefined,
          stopSource,
          start,
        },
        (e) => {
          setEvents((prev) => [...prev, e]);
          if (e.stage === "done" && e.server) created = e.server.name;
        },
      );
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setRunning(false);
    }
    if (created) setTimeout(() => navigate(`/servers/${encodeURIComponent(created!)}`), 1200);
  }

  const fromBackup = params.get("backup");
  const running0 = server?.state === "running";
  const chosen = backups?.find((b) => b.key === backupKey);

  return (
    <div className="page">
      <Link to={`/servers/${encodeURIComponent(name)}`} className="back">
        ← {name}
      </Link>
      <h1>{fromBackup ? `Restore a backup of ${name} as a new server` : `Clone ${name}`}</h1>
      <p className="muted">
        The copy gets {name}&apos;s settings, scheduled tasks, backup plan and other settings, with its own data folders
        and free host ports. Its web addresses, port forwarding and server groups aren&apos;t copied: they&apos;d clash
        with the original&apos;s. {name} itself isn&apos;t changed.
      </p>

      <div className="card">
        <h2>Name and data</h2>
        <div className="config-form">
          <div className="field-grid">
            <label className="field">
              <span className="field-label">The copy&apos;s name</span>
              <input
                value={newName}
                onChange={(e) => {
                  setNewName(e.target.value);
                  setPreview(undefined);
                }}
                spellCheck={false}
                disabled={running}
              />
            </label>
          </div>
          <div className="field">
            <span className="field-label">Data</span>
            <label className="radio small">
              <input type="radio" checked={copyData === "now"} onChange={() => setCopyData("now")} disabled={running} />
              Copy {name}&apos;s data folders as they are now
            </label>
            {copyData === "now" && running0 && (
              <label className="radio small indent">
                <input
                  type="checkbox"
                  checked={stopSource}
                  onChange={(e) => setStopSource(e.target.checked)}
                  disabled={running}
                />
                Stop {name} while copying (recommended: a running game can be halfway through saving), and start it
                again right after
              </label>
            )}
            <label className="radio small">
              <input
                type="radio"
                checked={copyData === "backup"}
                onChange={() => setCopyData("backup")}
                disabled={running || backups?.length === 0}
              />
              Copy one of its backups
              {backups?.length === 0 && <span className="muted"> (there are none)</span>}
            </label>
            {copyData === "backup" && backups && backups.length > 0 && (
              <select
                className="indent"
                value={backupKey}
                onChange={(e) => setBackupKey(e.target.value)}
                disabled={running}
              >
                <option value="">Choose a backup…</option>
                {backups.map((b) => (
                  <option key={b.key} value={b.key}>
                    {b.key.split("/").pop()} ({timeAgo(b.lastModified)}, {formatBytes(b.size)})
                  </option>
                ))}
              </select>
            )}
            <label className="radio small">
              <input
                type="radio"
                checked={copyData === "none"}
                onChange={() => setCopyData("none")}
                disabled={running}
              />
              Settings only: the copy starts with empty data folders
            </label>
          </div>
          <label className="radio small">
            <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} disabled={running} />
            Start the copy when it&apos;s ready
          </label>
          {!preview && (
            <div className="btn-row">
              <button className="btn primary" onClick={load} disabled={previewing || !newName.trim()}>
                {previewing ? "Working it out…" : "Next: check its settings"}
              </button>
            </div>
          )}
        </div>
        {error && <div className="alert error">{error}</div>}
      </div>

      {preview && (
        <div className="card">
          <h2>{preview.request.name}&apos;s settings</h2>
          <TransferNotes preview={preview} />
          {copyData !== "none" && preview.copies && preview.copies.length > 0 && (
            <div className="small">
              <strong>{copyData === "backup" ? "Restored from the backup into" : "Copied"}:</strong>
              <ul className="copies">
                {preview.copies.map((c) => (
                  <li key={c.target}>
                    <code>{c.target}</code>:{" "}
                    {copyData === "backup" ? (
                      <code>{c.to}</code>
                    ) : (
                      <>
                        <code>{c.from}</code> → <code>{c.to}</code>
                      </>
                    )}
                  </li>
                ))}
              </ul>
              {copyData === "backup" && (
                <p className="muted">
                  Only the folders the backup holds{chosen ? "" : " (choose one above)"} are filled in.
                </p>
              )}
            </div>
          )}
          {events.length > 0 && <DeployProgress events={events} />}
          <ConfigForm
            key={preview.request.name}
            app={preview.app}
            initial={preview.request}
            isNew
            submitLabel={running ? "Cloning…" : fromBackup ? "Create and restore" : "Clone"}
            busy={running || (copyData === "backup" && !backupKey)}
            onSubmit={clone}
          />
        </div>
      )}
      {!server && !error && <Spinner />}
    </div>
  );
}
