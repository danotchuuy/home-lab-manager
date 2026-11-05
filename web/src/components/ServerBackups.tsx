import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api, download } from "../api";
import type { BackupObject, BackupOverview, BackupPlan, BackupRun, BackupTargetRef, FileListing } from "../types";
import { usePoll } from "../usePoll";
import { formatBytes } from "../util";
import { AtFields, describeAt, KeepingFields, VerifyBadge } from "./BackupOptions";
import { Ago, Spinner } from "./common";

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
  commands: "Sending the commands before the backup…",
  stopping: "Stopping the server…",
  uploading: "Backing up…",
  verifying: "Reading the backup back to check it…",
  copying: "Copying it to the other storage…",
  restoring: "Restoring…",
  cleaning: "Deleting old backups…",
  starting: "Starting the server again…",
};

export const baseName = (key: string) => key.split("/").pop() ?? key;

type Draft = Omit<BackupPlan, "last" | "lastOk" | "lastRestore" | "checks">;

/** The editable part of a plan, in a fixed shape for comparing. */
const toDraft = (p: BackupPlan): Draft => ({
  folders: p.folders,
  intervalHours: p.intervalHours,
  at: p.at,
  keep: p.keep,
  retention: p.retention,
  stopServer: p.stopServer,
  preCommand: p.preCommand ?? "",
  postCommand: p.postCommand ?? "",
  exclude: p.exclude ?? [],
  verify: p.verify ?? "",
  copyTo: p.copyTo,
});

const sameDraft = (a: Draft, b: Draft) => JSON.stringify(toDraft(a as BackupPlan)) === JSON.stringify(toDraft(b as BackupPlan));

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
  const admin = useAdmin();
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
      setDraft((d) => d ?? toDraft(o.plan));
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
      setDraft(toDraft(p));
      await load();
      setNotice("Saved.");
    });

  const backupNow = () =>
    run("start", async () => {
      await api.startBackup(server);
      await load();
    });

  const restore = (key: string, target: string) =>
    run("restore", async () => {
      await api.restoreBackup(server, key, backupFirst && plan.folders.length > 0, target);
      setRestoring(undefined);
      await load();
    });

  const remove = (key: string, target: string) => {
    if (!confirm(`Delete ${baseName(key)} from the bucket? This can't be undone.`)) return;
    run("delete", async () => {
      await api.deleteBackup(server, key, target);
      await load();
    });
  };

  const verify = (key: string, target: string) =>
    run("verify:" + target + ":" + key, async () => {
      const v = await api.verifyBackup(server, key, target);
      await load();
      setNotice(v.ok ? `${baseName(key)} reads back fine: ${v.files} files.` : undefined);
      if (!v.ok) setError(`${baseName(key)} is damaged: ${v.error}`);
    });

  const list = (backups: BackupObject[], target: BackupTargetRef, primary: boolean) => (
    <BackupList
      server={server}
      backups={backups}
      target={primary ? "" : target.id}
      checks={plan.checks ?? {}}
      checkPrefix={target.id + ":"}
      busy={busy}
      prog={!!prog}
      progKey={prog?.key}
      restoring={restoring}
      setRestoring={setRestoring}
      backupFirst={backupFirst}
      setBackupFirst={setBackupFirst}
      hasFolders={plan.folders.length > 0}
      serverRunning={serverRunning}
      canRun={canRun}
      canManage={canManage}
      admin={admin && primary}
      onRestore={(k) => restore(k, primary ? "" : target.id)}
      onDelete={(k) => remove(k, primary ? "" : target.id)}
      onVerify={(k) => verify(k, primary ? "" : target.id)}
    />
  );

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
              value={draft.at ? -1 : draft.intervalHours}
              onChange={(e) => {
                const v = Number(e.target.value);
                setDraft(
                  v === -1
                    ? { ...draft, intervalHours: 0, at: draft.at ?? { time: "04:00" } }
                    : { ...draft, intervalHours: v, at: undefined },
                );
              }}
            >
              {INTERVALS.some(([h]) => h === draft.intervalHours) || draft.at ? null : (
                <option value={draft.intervalHours}>Every {draft.intervalHours} hours</option>
              )}
              {INTERVALS.map(([h, label]) => (
                <option key={h} value={h}>
                  {label}
                </option>
              ))}
              <option value={-1}>At a time of day…</option>
            </select>
            {draft.at && <AtFields at={draft.at} onChange={(at) => setDraft({ ...draft, at })} />}
            {draft.at && <span className="field-help">In hlm&apos;s time zone (System → Time zone and schedule).</span>}
          </label>
          <KeepingFields
            keep={draft.keep}
            retention={draft.retention}
            onChange={(keep, retention) => setDraft({ ...draft, keep, retention })}
          />
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

        <details className="backup-more" open={moreOpen(draft)}>
          <summary>More options: excludes, commands, checking, a second copy</summary>
          <label className="field">
            <span className="field-label">Leave out (one pattern per line)</span>
            <textarea
              rows={3}
              value={(draft.exclude ?? []).join("\n")}
              onChange={(e) => setDraft({ ...draft, exclude: e.target.value.split("\n") })}
              placeholder={"*.log\ncache/\nsteamapps/"}
              spellCheck={false}
            />
            <span className="field-help">
              <code>*.log</code> matches names anywhere, <code>cache/</code> only folders, <code>/data/logs</code> a whole
              path, and <code>mods/*.tmp</code> the end of one. Handy for logs, caches and game files SteamCMD downloads
              again anyway.
            </span>
          </label>
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Console commands before (one per line)</span>
              <textarea
                rows={2}
                value={draft.preCommand ?? ""}
                onChange={(e) => setDraft({ ...draft, preCommand: e.target.value })}
                placeholder={"save-off\nsave-all flush"}
                spellCheck={false}
              />
            </label>
            <label className="field">
              <span className="field-label">…and after</span>
              <textarea
                rows={2}
                value={draft.postCommand ?? ""}
                onChange={(e) => setDraft({ ...draft, postCommand: e.target.value })}
                placeholder="save-on"
                spellCheck={false}
              />
            </label>
          </div>
          <span className="field-help">
            Sent through the server&apos;s console when it&apos;s running, so a game can save and pause writing while
            it&apos;s backed up without stopping (Minecraft&apos;s <code>save-off</code>, <code>save-all flush</code>, then{" "}
            <code>save-on</code>). A command that fails is noted, and the backup goes ahead.
          </span>
          <label className="field">
            <span className="field-label">Check each backup</span>
            <select
              value={draft.verify ?? ""}
              onChange={(e) => setDraft({ ...draft, verify: e.target.value as Draft["verify"] })}
            >
              <option value="">Read it back if it&apos;s under 5 GB</option>
              <option value="always">Always read it back</option>
              <option value="off">Don&apos;t read it back</option>
            </select>
            <span className="field-help">
              After it&apos;s stored, the backup is downloaded and every file in it read, without saving anything, to make
              sure it can be restored. Its checksum is stored beside it.
            </span>
          </label>
          {overview.targets.length > 0 ? (
            <div className="field">
              <span className="field-label">Also copy each backup to</span>
              <select
                value={draft.copyTo?.target ?? ""}
                onChange={(e) =>
                  setDraft({
                    ...draft,
                    copyTo: e.target.value ? { target: e.target.value, keep: draft.copyTo?.keep ?? 7, retention: draft.copyTo?.retention } : undefined,
                  })
                }
              >
                <option value="">Nowhere else</option>
                {overview.targets.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                    {t.encrypt ? " (encrypted)" : ""}
                  </option>
                ))}
              </select>
              {draft.copyTo && (
                <div className="indent">
                  <KeepingFields
                    what="copies"
                    keep={draft.copyTo.keep}
                    retention={draft.copyTo.retention}
                    onChange={(keep, retention) => setDraft({ ...draft, copyTo: { ...draft.copyTo!, keep, retention } })}
                  />
                </div>
              )}
            </div>
          ) : (
            <p className="muted small">
              To keep a second copy somewhere else (an off-site bucket, say), add more backup storage under{" "}
              <Link to="/system#backup">System → Backup storage</Link>.
            </p>
          )}
        </details>

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
        {plan.at && !dirty && <p className="muted small">Backs up {describeAt(plan.at)}.</p>}
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
                Restoring {baseName(plan.lastRestore.key ?? "")} (<Ago iso={plan.lastRestore.started} />) failed:{" "}
                {plan.lastRestore.error}
                {plan.lastRestore.before && (
                  <> The files were backed up first, as {baseName(plan.lastRestore.before)}.</>
                )}
              </>
            ) : (
              <>
                Restored {baseName(plan.lastRestore.key ?? "")} <Ago iso={plan.lastRestore.finished} />:{" "}
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
                <Ago iso={plan.last.started} />) failed: {plan.last.error}
              </>
            ) : (
              <>
                Last backup <Ago iso={plan.last.finished} />: {plan.last.files} file{plan.last.files === 1 ? "" : "s"},{" "}
                {formatBytes(plan.last.bytes)}, stored as {formatBytes(plan.last.size)}
                {lastExtras(plan.last)}.
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
        {plan.intervalHours > 0 && !plan.at && plan.last && !prog && (
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
          list(overview.backups, overview.target, true)
        )}
        {overview.copies && (
          <>
            <h3 className="backup-copies-title">
              Copies in {overview.copies.target.name}
              {overview.copies.target.encrypt ? " (encrypted)" : ""}
            </h3>
            {overview.copies.error && (
              <div className="alert error">Couldn&apos;t list {overview.copies.target.name}: {overview.copies.error}</div>
            )}
            {overview.copies.backups.length === 0 && !overview.copies.error ? (
              <p className="muted small">No copies there yet.</p>
            ) : (
              list(overview.copies.backups, overview.copies.target, false)
            )}
          </>
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

/** The options under "More options" are open when any is set. */
const moreOpen = (d: Draft) =>
  !!(d.exclude?.some((x) => x.trim()) || d.preCommand || d.postCommand || d.verify || d.copyTo);

/** What else the last backup did: left out, checked, copied. */
function lastExtras(r: BackupRun): string {
  const parts: string[] = [];
  if (r.excluded) parts.push(`${r.excluded} left out by patterns`);
  if (r.verify?.ok) parts.push("read back and checked");
  if (r.copy && !r.copy.error) parts.push(`copied to ${r.copy.target}`);
  return parts.length ? `; ${parts.join(", ")}` : "";
}

/** A list of backups in one storage, with their actions. */
function BackupList(p: {
  server: string;
  backups: BackupObject[];
  /** The storage's ID, or "" for the primary. */
  target: string;
  checks: Record<string, import("../types").BackupVerification>;
  checkPrefix: string;
  busy?: string;
  prog: boolean;
  progKey?: string;
  restoring?: string;
  setRestoring: (k?: string) => void;
  backupFirst: boolean;
  setBackupFirst: (v: boolean) => void;
  hasFolders: boolean;
  serverRunning: boolean;
  canRun: boolean;
  canManage: boolean;
  admin: boolean;
  onRestore: (key: string) => void;
  onDelete: (key: string) => void;
  onVerify: (key: string) => void;
}) {
  const id = (key: string) => p.target + "|" + key;
  return (
    <div className="file-list backup-list" role="list">
      {p.backups.map((b) => (
        <div className="backup-item" role="listitem" key={b.key}>
          <div className="backup-row">
            <code className="file-name" title={b.key}>
              {b.key.split("/").pop()}
            </code>
            <VerifyBadge v={p.checks[p.checkPrefix + b.key]} />
            <span className="muted small">{formatBytes(b.size)}</span>
            <span className="muted small" title={new Date(b.lastModified).toLocaleString()}>
              <Ago iso={b.lastModified} />
            </span>
            <span className="btn-row">
              {p.canManage && (
                <button
                  className="btn ghost small"
                  onClick={() => p.setRestoring(p.restoring === id(b.key) ? undefined : id(b.key))}
                  disabled={!!p.busy || p.prog}
                >
                  Restore
                </button>
              )}
              {p.admin && (
                <Link
                  className="btn ghost small"
                  to={`/servers/${encodeURIComponent(p.server)}/clone?backup=${encodeURIComponent(b.key)}`}
                  title="Make a new server with this backup's files, leaving this one as it is"
                >
                  Restore as new server…
                </Link>
              )}
              {p.canRun && (
                <button
                  className="btn ghost small"
                  onClick={() => p.onVerify(b.key)}
                  disabled={!!p.busy}
                  title="Download the backup and read every file in it, to check it can be restored"
                >
                  {p.busy === `verify:${p.target}:${b.key}` ? "Checking…" : "Verify"}
                </button>
              )}
              {p.canRun && (
                <a
                  className="btn ghost small"
                  href={api.backupDownloadUrl(p.server, b.key, p.target)}
                  download
                  onClick={(e) => {
                    e.preventDefault();
                    download(api.backupDownloadUrl(p.server, b.key, p.target)).catch((err) => alert((err as Error).message));
                  }}
                >
                  Download
                </a>
              )}
              {p.canManage && (
                <button
                  className="btn ghost small danger"
                  onClick={() => p.onDelete(b.key)}
                  disabled={!!p.busy || p.progKey === b.key}
                >
                  Delete
                </button>
              )}
            </span>
          </div>
          {p.restoring === id(b.key) && !p.prog && (
            <div className="restore-confirm">
              <p className="small no-margin">
                Put back the files in <strong>{baseName(b.key)}</strong> from {new Date(b.lastModified).toLocaleString()}?{" "}
                {p.serverRunning ? "The server is stopped, the files are restored, and it's started again. " : ""}
                Files in the backup replace the current ones; files created since then are left in place.
              </p>
              <label className="radio small">
                <input
                  type="checkbox"
                  checked={p.backupFirst && p.hasFolders}
                  disabled={!p.hasFolders}
                  onChange={(e) => p.setBackupFirst(e.target.checked)}
                />
                Back up the current files first, so this can be undone
                {!p.hasFolders && " (choose folders above to do this)"}
              </label>
              <div className="btn-row">
                <button className="btn small primary" onClick={() => p.onRestore(b.key)} disabled={!!p.busy}>
                  {p.busy === "restore" ? "Starting…" : "Restore"}
                </button>
                <button className="btn small ghost" onClick={() => p.setRestoring(undefined)}>
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>
      ))}
    </div>
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
