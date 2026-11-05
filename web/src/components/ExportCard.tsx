import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";

/** Settings tab: export the server's settings to a file, and clone it. */
export function ExportCard({ server, canManage, canClone }: { server: string; canManage: boolean; canClone: boolean }) {
  const [secrets, setSecrets] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function exportFile() {
    setBusy(true);
    setError(undefined);
    try {
      const exp = await api.exportServer(server, secrets);
      const blob = new Blob([JSON.stringify(exp, null, 2) + "\n"], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${server}.hlm.json`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="export-card">
      <div className="settings-actions">
        <div>
          <h2>Export and clone</h2>
          <p className="muted small">
            <strong>Export settings</strong> saves this server&apos;s settings, its template, scheduled tasks, backup
            plan and other settings to a file, to install it again here or on another hlm (Catalog → Import a server).
            Its data isn&apos;t in the file.
            {canClone && (
              <>
                {" "}
                <strong>Clone</strong> makes a copy here, with its own data folders and free ports, and copies its data
                now or from a backup.
              </>
            )}
          </p>
          {canManage && (
            <label className="radio small">
              <input type="checkbox" checked={secrets} onChange={(e) => setSecrets(e.target.checked)} />
              Include passwords and keys (you&apos;ll be asked to confirm it&apos;s you; keep the file safe)
            </label>
          )}
          {error && <div className="alert error small">{error}</div>}
        </div>
        <div className="btn-row">
          <button className="btn" onClick={exportFile} disabled={busy}>
            {busy ? "Exporting…" : "Export settings"}
          </button>
          {canClone && (
            <Link className="btn" to={`/servers/${encodeURIComponent(server)}/clone`}>
              Clone…
            </Link>
          )}
        </div>
      </div>
    </div>
  );
}
