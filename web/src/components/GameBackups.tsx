import { useState, type ReactNode } from "react";
import type { GameBackupFolder, GameBackupSuggestion, GameBackupSync } from "../types";
import { formatBytes } from "../util";
import { Ago } from "./common";

/** A game backup's name in the list: its folder and its own name, without hlm's suffix. */
export function gameBackupName(key: string): string {
  const parts = key.replace(/@\d+\.tar\.gz$/, "").split("/");
  return parts.slice(-2).join("/");
}

/** When the game wrote a game backup, from its key. */
export function gameBackupTime(key: string): string | undefined {
  const m = key.match(/@(\d+)\.tar\.gz$/);
  return m ? new Date(Number(m[1]) * 1000).toISOString() : undefined;
}

/**
 * The game's own backups: the folders it keeps them in, from which hlm
 * uploads each new one, and those uploaded.
 */
export function GameBackups({
  folders,
  keep,
  onChange,
  suggestions,
  last,
  running,
  dirty,
  busy,
  canManage,
  canRun,
  onSave,
  onSync,
  error,
  children,
}: {
  folders: GameBackupFolder[];
  keep: number;
  onChange: (folders: GameBackupFolder[], keep: number) => void;
  suggestions: GameBackupSuggestion[];
  last?: GameBackupSync;
  /** An upload of them is running. */
  running: boolean;
  dirty: boolean;
  busy?: string;
  canManage: boolean;
  canRun: boolean;
  onSave: () => void;
  onSync: () => void;
  error?: string;
  /** The uploaded game backups. */
  children: ReactNode;
}) {
  const [path, setPath] = useState("");
  const [match, setMatch] = useState("");
  const has = (f: GameBackupFolder) => folders.some((x) => x.path === f.path && (x.match ?? "") === (f.match ?? ""));
  const unused = suggestions.filter((s) => !has(s));
  const add = (f: GameBackupFolder) => {
    if (!has(f)) onChange([...folders, f.match ? { path: f.path, match: f.match } : { path: f.path }], keep);
  };

  return (
    <div className="card game-backups">
      <h2>The game&apos;s own backups</h2>
      <p className="muted small">
        Some games copy their world into a backup folder now and then. hlm checks those folders every 10 minutes while the
        server runs (and once after it stops), and uploads each backup the game wrote that isn&apos;t in the bucket yet,
        including one it rewrote since. They&apos;re whole saves the game made itself, so nothing is copied halfway
        through being written, and only new ones are uploaded.
      </p>

      {folders.length > 0 && (
        <ul className="backup-folders">
          {folders.map((f) => (
            <li key={f.path + "|" + (f.match ?? "")}>
              <code>{f.path}</code>
              {f.match && <span className="muted small"> named {f.match}</span>}
              {canManage && (
                <button
                  className="btn ghost small"
                  onClick={() => onChange(folders.filter((x) => x !== f), keep)}
                  aria-label={`Remove ${f.path}`}
                >
                  Remove
                </button>
              )}
            </li>
          ))}
        </ul>
      )}

      {canManage && unused.length > 0 && (
        <div className="game-backup-suggestions">
          <span className="field-label">Where this game keeps them</span>
          {unused.map((s) => (
            <div key={s.path + "|" + (s.match ?? "")} className="game-backup-suggestion">
              <div>
                <code>{s.path}</code>
                {s.match && <span className="muted small"> named {s.match}</span>}
                {s.note && <div className="muted small">{s.note}</div>}
              </div>
              <button className="btn small" onClick={() => add(s)}>
                Use
              </button>
            </div>
          ))}
        </div>
      )}

      {canManage && (
        <details className="backup-more" open={folders.length === 0 && unused.length === 0}>
          <summary>Add a folder by hand</summary>
          <form
            className="input-row"
            onSubmit={(e) => {
              e.preventDefault();
              if (!path.trim()) return;
              add({ path: path.trim(), match: match.trim() || undefined });
              setPath("");
              setMatch("");
            }}
          >
            <input
              value={path}
              onChange={(e) => setPath(e.target.value)}
              placeholder="/data/Saved/SaveGames/0/*/backup/world"
              aria-label="Folder in the container"
              spellCheck={false}
            />
            <input
              value={match}
              onChange={(e) => setMatch(e.target.value)}
              placeholder="Only names like (optional)"
              aria-label="Only names matching"
              spellCheck={false}
              className="game-backup-match"
            />
            <button type="submit" className="btn small" disabled={!path.trim()}>
              Add
            </button>
          </form>
          <span className="field-help">
            The full path in the container. <code>*</code> stands for any one folder&apos;s name (a world&apos;s ID, say).
            Each file or folder in it is one backup. For a game that keeps its backups beside the live save, give a name
            pattern such as <code>*_backup_*</code> so only those are taken.
          </span>
        </details>
      )}

      <label className="field game-backup-keep">
        <span className="field-label">Keep</span>
        <select value={keep} onChange={(e) => onChange(folders, Number(e.target.value))} disabled={!canManage}>
          <option value={0}>Every one uploaded</option>
          {[5, 10, 20, 50, 100].map((n) => (
            <option key={n} value={n}>
              The newest {n} of each folder
            </option>
          ))}
        </select>
        <span className="field-help">
          Ones the game still has are always kept: deleting them would only upload them again.
        </span>
      </label>

      {last && !running && (
        <div className={`alert small ${last.error ? "error" : last.warnings?.length ? "warn" : "success"}`}>
          {last.error ? (
            <>
              Uploading them <Ago iso={last.started} /> failed: {last.error}
            </>
          ) : (
            <>
              Checked <Ago iso={last.finished} />: {last.found} backup{last.found === 1 ? "" : "s"} in the folders,{" "}
              {last.uploaded ? `${last.uploaded} new uploaded (${formatBytes(last.bytes)})` : "none new"}
              {last.deleted ? `, ${last.deleted} old one${last.deleted === 1 ? "" : "s"} deleted` : ""}.
            </>
          )}
          {last.warnings?.length ? (
            <ul className="small">
              {last.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          ) : null}
        </div>
      )}
      {error && <div className="alert error">{error}</div>}
      <div className="btn-row">
        {canManage && (
          <button className="btn small primary" onClick={onSave} disabled={!!busy || !dirty}>
            {busy === "save" ? "Saving…" : "Save"}
          </button>
        )}
        <button
          className="btn small"
          onClick={onSync}
          disabled={!!busy || dirty || running || folders.length === 0 || !canRun}
          title={dirty ? "Save your changes first" : undefined}
        >
          {busy === "game" ? "Starting…" : running ? "Uploading…" : "Upload new ones now"}
        </button>
        {dirty && <span className="muted small">Unsaved changes</span>}
      </div>
      {children}
    </div>
  );
}
