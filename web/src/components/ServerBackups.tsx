import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, download } from "../api";
import type { BackupOverview, BackupPlan, FileListing } from "../types";
import { usePoll } from "../usePoll";
import { formatBytes, timeAgo } from "../util";
import { Spinner } from "./common";

export const INTERVALS: [number, string][] = [
  [0, "Off (only when asked)"],
  [1, "Every hour"],
  [3, "Every 3 hours"],
  [6, "Every 6 hours"],
  [12, "Every 12 hours"],
  [24, "Every day"],
  [48, "Every 2 days"],
  [168, "Every week"],
];

export const STAGES: Record<string, string> = {
  preparing: "Getting ready…",
  stopping: "Stopping the server…",
  uploading: "Backing up…",
  restoring: "Restoring…",
  cleaning: "Deleting old backups…",
  starting: "Starting the server again…",
};

export const baseName = (key: string) => key.split("/").pop() ?? key;

type Draft = Omit<BackupPlan, "last" | "lastRestore">;

const sameDraft = (a: Draft, b: Draft) =>
  a.folders.join("\n") === b.folders.join("\n") &&
  a.intervalHours === b.intervalHours &&
  a.keep === b.keep &&
  a.stopServer === b.stopServer;

/** Whether p is dir or inside it. */
const within = (p: string, dir: string) => p === dir || p.startsWith(dir.endsWith("/") ? dir : dir + "/");

/** Adds a folder, dropping any already chosen inside it; one inside a chosen folder is already covered. */
function addFolder(folders: string[], p: string): string[] {
  if (folders.some((f) => within(p, f))) return folders;
  return [...folders.filter((f) => !within(f, p)), p].sort();
}

/** A server's Backups tab: which folders go to the bucket, when, and the backups already there. */
export function ServerBackups({
  server,
  running: serverRunning,
  canRun,
  canManage,
}: {
  server: string;
  running: boolean;
  /** May take and download backups. */
  canRun: boolean;
  /** May change the plan, and restore and delete backups. */
  canManage: boolean;
}) {
  const [overview, setOverview] = useState<BackupOverview>();
  const [draft, setDraft] = useState<Draft>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState<string>();
  // Which card the error belongs to: the plan's actions or the list's.
  const [errorIn, setErrorIn] = useState<"plan" | "list">("plan");
  // The backup whose restore is being confirmed, and whether to back up the current files first.
  const [restoring, setRestoring] = useState<string>();
  const [backupFirst, setBackupFirst] = useState(true);

  const load = useCallback(async () => {
    try {
      const o = await api.serverBackups(server);
      setOverview(o);
      setDraft(
        (d) =>
          d ?? {
            folders: o.plan.folders,
            intervalHours: o.plan.intervalHours,
            keep: o.plan.keep,
            stopServer: o.plan.stopServer,
          },
      );
    } catch (e) {
      setError((e as Error).message);
    }
  }, [server]);

  // Quick while a backup runs, to show its progress.
  usePoll(load, overview?.running ? 1500 : 15000, [load, !!overview?.running]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setErrorIn(label === "save" || label === "start" ? "plan" : "list");
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  if (!overview || !draft) return error ? <div className="alert error">{error}</div> : <Spinner />;

  if (!overview.configured) {
    return (
      <div className="card">
        <h2>Backups</h2>
        <p className="muted">
          Backups go to S3-compatible storage, such as SeaweedFS or MinIO. Set it up under{" "}
          <Link to="/system#backup">System → Backup storage</Link> first.
        </p>
      </div>
    );
  }

  const saved: Draft = overview.plan;
  const dirty = !sameDraft(draft, saved);
  const plan = overview.plan;
  const prog = overview.running;

  const save = () =>
    run("save", async () => {
      const p = await api.setBackupPlan(server, draft);
      setDraft({ folders: p.folders, intervalHours: p.intervalHours, keep: p.keep, stopServer: p.stopServer });
      await load();
      setNotice("Saved.");
    });

  const backupNow = () =>
    run("start", async () => {
      await api.startBackup(server);
      await load();
    });

  const restore = (key: string) =>
    run("restore", async () => {
      await api.restoreBackup(server, key, backupFirst && plan.folders.length > 0);
      setRestoring(undefined);
      await load();
    });

  const remove = (key: string) => {
    if (!confirm(`Delete ${baseName(key)} from the bucket? This can't be undone.`)) return;
    run("delete", async () => {
      await api.deleteBackup(server, key);
      await load();
    });
  };

  return (
    <>
      <fieldset className="plain-fieldset" disabled={!canManage && !canRun}>
      <div className="card backup-plan">
        <h2>What to back up</h2>
        {draft.folders.length === 0 ? (
          <p className="muted small">
            No folders chosen yet. Pick them below; each backup holds everything inside them.
          </p>
        ) : (
          <ul className="backup-folders">
            {draft.folders.map((f) => (
              <li key={f}>
                <code>{f}</code>
                <button
                  className="btn ghost small"
                  onClick={() => setDraft({ ...draft, folders: draft.folders.filter((x) => x !== f) })}
                  aria-label={`Remove ${f}`}
                >
                  Remove
                </button>
              </li>
            ))}
          </ul>
        )}
        <FolderPicker
          server={server}
          chosen={draft.folders}
          onAdd={(p) => setDraft({ ...draft, folders: addFolder(draft.folders, p) })}
        />

        <div className="field-grid backup-options">
          <label className="field">
            <span className="field-label">Schedule</span>
            <select
              value={draft.intervalHours}
              onChange={(e) => setDraft({ ...draft, intervalHours: Number(e.target.value) })}
            >
              {INTERVALS.some(([h]) => h === draft.intervalHours) ? null : (
                <option value={draft.intervalHours}>Every {draft.intervalHours} hours</option>
              )}
              {INTERVALS.map(([h, label]) => (
                <option key={h} value={h}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field-label">Backups to keep</span>
            <input
              type="number"
              min={0}
              value={draft.keep}
              onChange={(e) => setDraft({ ...draft, keep: Math.max(0, Math.floor(Number(e.target.value) || 0)) })}
            />
            <span className="field-help">Older ones are deleted after each backup. 0 keeps them all.</span>
          </label>
        </div>
        <label className="radio small">
          <input
            type="checkbox"
            checked={draft.stopServer}
            onChange={(e) => setDraft({ ...draft, stopServer: e.target.checked })}
          />
          Stop the server while backing up, so saves aren&apos;t copied halfway through being written (it&apos;s started
          again afterwards)
        </label>

        {error && errorIn === "plan" && <div className="alert error">{error}</div>}
        {notice && !dirty && <div className="alert success">{notice}</div>}
        <div className="btn-row">
          {canManage && (
            <button className="btn small primary" onClick={save} disabled={!!busy || !dirty}>
              {busy === "save" ? "Saving…" : "Save"}
            </button>
          )}
          <button
            className="btn small"
            onClick={backupNow}
            disabled={!!busy || dirty || !!prog || plan.folders.length === 0 || !canRun}
            title={dirty ? "Save your changes first" : undefined}
          >
            {busy === "start" ? "Starting…" : "Back up now"}
          </button>
          {dirty && <span className="muted small">Unsaved changes</span>}
        </div>
        {draft.stopServer && serverRunning && !prog && (
          <p className="muted small">The server is running, so a backup will restart it.</p>
        )}
      </div>
      </fieldset>

      <div className="card">
        <h2>Backups</h2>
        {prog && (
          <div className="alert info">
            <Spinner /> {prog.kind === "restore" && prog.key && <>Restoring {baseName(prog.key)}. </>}
            {STAGES[prog.stage] ?? prog.stage}
            {prog.stage === "uploading" && (
              <>
                {" "}
                {prog.files} file{prog.files === 1 ? "" : "s"} ({formatBytes(prog.bytes)}) read,{" "}
                {formatBytes(prog.transferred)} uploaded
              </>
            )}
            {prog.stage === "restoring" && (
              <>
                {" "}
                {prog.files} file{prog.files === 1 ? "" : "s"} ({formatBytes(prog.bytes)}) written,{" "}
                {formatBytes(prog.transferred)} downloaded
              </>
            )}
          </div>
        )}
        {!prog && plan.lastRestore && (!plan.last || plan.lastRestore.started > plan.last.started) && (
          <div
            className={`alert ${plan.lastRestore.error ? "error" : plan.lastRestore.warnings?.length ? "warn" : "success"}`}
          >
            {plan.lastRestore.error ? (
              <>
                Restoring {baseName(plan.lastRestore.key ?? "")} ({timeAgo(plan.lastRestore.started)}) failed:{" "}
                {plan.lastRestore.error}
                {plan.lastRestore.before && (
                  <> The files were backed up first, as {baseName(plan.lastRestore.before)}.</>
                )}
              </>
            ) : (
              <>
                Restored {baseName(plan.lastRestore.key ?? "")} {timeAgo(plan.lastRestore.finished)}:{" "}
                {plan.lastRestore.files} file
                {plan.lastRestore.files === 1 ? "" : "s"}, {formatBytes(plan.lastRestore.bytes)}.
                {plan.lastRestore.before && (
                  <>
                    {" "}
                    The files it replaced were backed up first as {baseName(plan.lastRestore.before)}; restore that to
                    undo it.
                  </>
                )}
              </>
            )}
            {plan.lastRestore.warnings?.length ? (
              <ul className="small">
                {plan.lastRestore.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            ) : null}
          </div>
        )}
        {!prog && plan.last && (
          <div className={`alert ${plan.last.error ? "error" : plan.last.warnings?.length ? "warn" : "success"}`}>
            {plan.last.error ? (
              <>
                The last backup ({plan.last.trigger === "schedule" ? "scheduled" : "manual"},{" "}
                {timeAgo(plan.last.started)}) failed: {plan.last.error}
              </>
            ) : (
              <>
                Last backup {timeAgo(plan.last.finished)}: {plan.last.files} file{plan.last.files === 1 ? "" : "s"},{" "}
                {formatBytes(plan.last.bytes)}, stored as {formatBytes(plan.last.size)}.
              </>
            )}
            {plan.last.warnings?.length ? (
              <ul className="small">
                {plan.last.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            ) : null}
          </div>
        )}
        {overview.error && <div className="alert error">Couldn&apos;t list the bucket: {overview.error}</div>}
        {error && errorIn === "list" && <div className="alert error">{error}</div>}
        {plan.intervalHours > 0 && plan.last && !prog && (
          <p className="muted small">
            Next backup{" "}
            {new Date(
              new Date(plan.last.started).getTime() +
                (plan.last.error ? Math.min(plan.intervalHours, 1) : plan.intervalHours) * 3600_000,
            ).toLocaleString()}
            .
          </p>
        )}
        {overview.backups.length === 0 && !overview.error ? (
          <p className="muted small">No backups in the bucket yet.</p>
        ) : (
          <div className="file-list backup-list" role="list">
            {overview.backups.map((b) => (
              <div className="backup-item" role="listitem" key={b.key}>
                <div className="backup-row">
                  <code className="file-name" title={b.key}>
                    {b.key.split("/").pop()}
                  </code>
                  <span className="muted small">{formatBytes(b.size)}</span>
                  <span className="muted small" title={new Date(b.lastModified).toLocaleString()}>
                    {timeAgo(b.lastModified)}
                  </span>
                  <span className="btn-row">
                    {canManage && (
                      <button
                        className="btn ghost small"
                        onClick={() => setRestoring(restoring === b.key ? undefined : b.key)}
                        disabled={!!busy || !!prog}
                      >
                        Restore
                      </button>
                    )}
                    {canRun && (
                      <a
                        className="btn ghost small"
                        href={api.backupDownloadUrl(server, b.key)}
                        download
                        onClick={(e) => {
                          e.preventDefault();
                          download(api.backupDownloadUrl(server, b.key)).catch((err) => alert((err as Error).message));
                        }}
                      >
                        Download
                      </a>
                    )}
                    {canManage && (
                      <button
                        className="btn ghost small danger"
                        onClick={() => remove(b.key)}
                        disabled={!!busy || prog?.key === b.key}
                      >
                        Delete
                      </button>
                    )}
                  </span>
                </div>
                {restoring === b.key && !prog && (
                  <div className="restore-confirm">
                    <p className="small no-margin">
                      Put back the files in <strong>{baseName(b.key)}</strong> from{" "}
                      {new Date(b.lastModified).toLocaleString()}?{" "}
                      {serverRunning ? "The server is stopped, the files are restored, and it's started again. " : ""}
                      Files in the backup replace the current ones; files created since then are left in place.
                    </p>
                    <label className="radio small">
                      <input
                        type="checkbox"
                        checked={backupFirst && plan.folders.length > 0}
                        disabled={plan.folders.length === 0}
                        onChange={(e) => setBackupFirst(e.target.checked)}
                      />
                      Back up the current files first, so this can be undone
                      {plan.folders.length === 0 && " (choose folders above to do this)"}
                    </label>
                    <div className="btn-row">
                      <button className="btn small primary" onClick={() => restore(b.key)} disabled={!!busy}>
                        {busy === "restore" ? "Starting…" : "Restore"}
                      </button>
                      <button className="btn small ghost" onClick={() => setRestoring(undefined)}>
                        Cancel
                      </button>
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>
        )}
        <p className="muted small">
          Downloaded backups hold container paths (e.g. <code>{(plan.folders[0] ?? "/data").slice(1)}/…</code>), so they
          can also be extracted by hand at <code>/</code> in the container, or into the matching data folders on the
          host.
        </p>
      </div>
    </>
  );
}

/** Browses the container's folders to add them to the backup. */
function FolderPicker({ server, chosen, onAdd }: { server: string; chosen: string[]; onAdd: (path: string) => void }) {
  const [listing, setListing] = useState<FileListing>();
  const [dir, setDir] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .listFiles(server, dir)
      .then(
        (l) => {
          if (cancelled) return;
          setListing(l);
          setError(undefined);
        },
        (e) => !cancelled && setError((e as Error).message),
      )
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [server, dir]);

  if (!listing) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const path = listing.path;
  const parts = path.split("/").filter(Boolean);
  const covered = chosen.some((f) => within(path, f));
  const dirs = listing.entries.filter((e) => e.type === "dir" || (e.type === "symlink" && e.isMount));

  return (
    <div className="folder-picker">
      <span className="field-label">Add folders</span>
      {listing.mounts.length > 0 && (
        <div className="btn-row">
          <span className="muted small">Data folders:</span>
          {listing.mounts.map((m) => (
            <button
              key={m.path}
              className="btn ghost small"
              onClick={() => onAdd(m.path)}
              disabled={chosen.some((f) => within(m.path, f))}
              title={m.source}
            >
              + {m.path}
            </button>
          ))}
        </div>
      )}
      <div className="file-toolbar">
        <nav className="crumbs" aria-label="Folder">
          <button className="link-button" onClick={() => setDir("/")}>
            /
          </button>
          {parts.map((p, i) => {
            const full = "/" + parts.slice(0, i + 1).join("/");
            return (
              <span key={full}>
                <button className="link-button" onClick={() => setDir(full)}>
                  {p}
                </button>
                {i < parts.length - 1 && <span className="muted">/</span>}
              </span>
            );
          })}
        </nav>
        <div className="spacer" />
        <button className="btn small" onClick={() => onAdd(path)} disabled={path === "/" || covered}>
          {covered ? "Included" : "Back up this folder"}
        </button>
      </div>
      {error && <div className="alert error">{error}</div>}
      <div className={`file-list${loading ? " loading" : ""}`} role="list">
        {path !== "/" && (
          <button className="file-row" onClick={() => setDir(path.replace(/\/[^/]+$/, "") || "/")}>
            <span className="file-icon">↩</span>
            <span className="file-name">..</span>
          </button>
        )}
        {dirs.map((e) => {
          const full = (path === "/" ? "" : path) + "/" + e.name;
          return (
            <button className="file-row" key={e.name} onClick={() => setDir(full)}>
              <span className="file-icon" aria-hidden="true">
                📁
              </span>
              <span className="file-name">
                {e.name}
                {e.isMount && <span className="badge">data folder</span>}
                {chosen.some((f) => within(full, f)) && <span className="badge state-running">backed up</span>}
              </span>
            </button>
          );
        })}
        {dirs.length === 0 && <p className="muted small empty-folder">No folders in here.</p>}
      </div>
    </div>
  );
}
