import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../api";
import type { ConfigFiles } from "../types";

const baseName = (p: string) => p.slice(p.lastIndexOf("/") + 1);

/** The server's settings files on its Settings tab: pinned ones, and the game's usual ones hlm found. */
export function ConfigFilesCard({ server, canManage }: { server: string; canManage: boolean }) {
  const [, setParams] = useSearchParams();
  const [data, setData] = useState<ConfigFiles>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    api.configFiles(server).then(setData, (e) => setError((e as Error).message));
  }, [server]);

  const open = (path: string) => setParams({ tab: "files", file: path });
  // Pinning and unpinning, as the Files tab does.
  const pin = async (path: string, on = true) => {
    if (!data) return;
    try {
      setData(await api.setConfigFiles(server, on ? [...data.pinned, { path }] : data.pinned.filter((f) => f.path !== path)));
    } catch (e) {
      setError((e as Error).message);
    }
  };

  return (
    <div className="config-files-card">
      <h2>Game settings files</h2>
      {error && <div className="alert error small">{error}</div>}
      {data && data.pinned.length === 0 && data.suggested.length === 0 && (
        <p className="muted small">
          None pinned yet. Open a settings file on the Files tab and click <strong>☆ Pin</strong> to open it from here in
          one click.
        </p>
      )}
      {data && (data.pinned.length > 0 || data.suggested.length > 0) && (
        <ul className="config-file-list">
          {data.pinned.map((f) => (
            <li key={f.path}>
              <span>
                ★ <strong>{f.label || baseName(f.path)}</strong> <code className="muted small">{f.path}</code>
              </span>
              <span className="btn-row">
                {canManage && (
                  <button className="btn small ghost" onClick={() => pin(f.path, false)}>
                    Unpin
                  </button>
                )}
                <button className="btn small" onClick={() => open(f.path)}>
                  Edit
                </button>
              </span>
            </li>
          ))}
          {data.suggested.map((f) => (
            <li key={f.path}>
              <span>
                <strong>{baseName(f.path)}</strong> <code className="muted small">{f.path}</code>{" "}
                <span className="badge">usual for this game</span>
              </span>
              <span className="btn-row">
                {canManage && (
                  <button className="btn small ghost" onClick={() => pin(f.path)}>
                    Pin
                  </button>
                )}
                <button className="btn small" onClick={() => open(f.path)}>
                  Edit
                </button>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
