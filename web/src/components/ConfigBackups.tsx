import { useCallback, useState } from "react";
import { KeepingFields } from "./BackupOptions";
import { api, download } from "../api";
import type { ConfigBackupOverview, ConfigBackupPlan } from "../types";
import { usePoll } from "../usePoll";
import { formatBytes } from "../util";
import { Ago, Spinner } from "./common";
import { baseName, INTERVALS, STAGES } from "./ServerBackups";

type Draft = Omit<ConfigBackupPlan, "last">;

/**
 * Backs up hlm's own configuration (integration settings and credentials,
 * templates made from images, and every server's settings) to the backup
 * bucket, on a schedule.
 */
export function ConfigBackups({ storageVersion }: { storageVersion: number }) {
  const [overview, setOverview] = useState<ConfigBackupOverview>();
  const [draft, setDraft] = useState<Draft>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const load = useCallback(async () => {
    try {
      const o = await api.configBackups();
      setOverview(o);
      setDraft(
        (d) =>
          d ?? { intervalHours: o.plan.intervalHours, keep: o.plan.keep, retention: o.plan.retention, includeKey: o.plan.includeKey },
      );
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  // Quick while a backup runs; reloads when the storage settings change.
  usePoll(load, overview?.running ? 1500 : 30000, [load, !!overview?.running, storageVersion]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
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
      <p className="muted small no-margin">
        Once backup storage is set up above, hlm backs up its own configuration there every day: integration settings
        and credentials, templates made from images, and every server&apos;s settings.
      </p>
    );
  }

  const plan = overview.plan;
  const prog = overview.running;
  const dirty =
    draft.intervalHours !== plan.intervalHours ||
    draft.keep !== plan.keep ||
    JSON.stringify(draft.retention ?? null) !== JSON.stringify(plan.retention ?? null) ||
    draft.includeKey !== plan.includeKey;

  const save = () =>
    run("save", async () => {
      const p = await api.setConfigBackupPlan(draft);
      setDraft({ intervalHours: p.intervalHours, keep: p.keep, retention: p.retention, includeKey: p.includeKey });
      await load();
      setNotice("Saved.");
    });

  const backupNow = () =>
    run("start", async () => {
      await api.startConfigBackup();
      await load();
    });

  const remove = (key: string) => {
    if (!confirm(`Delete ${baseName(key)} from the bucket? This can't be undone.`)) return;
    run("delete", async () => {
      await api.deleteConfigBackup(key);
      await load();
    });
  };

  return (
    <div className="backup-plan config-backups">
      <p className="muted small no-margin">
        Backs up hlm&apos;s data directory (integration settings and credentials, port options, backup plans, templates
        made from images) and a <code>servers.json</code> with every server&apos;s settings, so hlm can be set up again
        on a new host. Game data is in each server&apos;s own backups.
      </p>
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
        <KeepingFields
          keep={draft.keep}
          retention={draft.retention}
          onChange={(keep, retention) => setDraft({ ...draft, keep, retention })}
        />
      </div>
      {overview.keyFromEnv ? (
        <p className="muted small no-margin">
          The stored credentials are encrypted with <code>HLM_SECRET_KEY</code>, which isn&apos;t backed up. Keep a copy
          of it somewhere safe: restoring needs the same key.
        </p>
      ) : (
        <div className="field">
          <label className="radio small">
            <input
              type="checkbox"
              checked={draft.includeKey}
              onChange={(e) => setDraft({ ...draft, includeKey: e.target.checked })}
            />
            Include the encryption key (<code>secrets/secret.key</code>)
          </label>
          <span className="field-help">
            {draft.includeKey
              ? "Each backup can then be restored on its own, but anyone who can read the bucket can also read every stored credential, including the bucket's own keys."
              : "Without it, the stored credentials in a backup can only be read with this machine's key. Keep a copy of secrets/secret.key from the data directory somewhere safe, or set HLM_SECRET_KEY."}
          </span>
        </div>
      )}

      {error && <div className="alert error">{error}</div>}
      {notice && !dirty && <div className="alert success">{notice}</div>}
      <div className="btn-row">
        <button className="btn small primary" onClick={save} disabled={!!busy || !dirty}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button
          className="btn small"
          onClick={backupNow}
          disabled={!!busy || dirty || !!prog}
          title={dirty ? "Save your changes first" : undefined}
        >
          {busy === "start" ? "Starting…" : "Back up now"}
        </button>
        {dirty && <span className="muted small">Unsaved changes</span>}
      </div>

      {prog && (
        <div className="alert info">
          <Spinner /> {STAGES[prog.stage] ?? prog.stage}
        </div>
      )}
      {!prog && plan.last && (
        <div className={`alert ${plan.last.error ? "error" : plan.last.warnings?.length ? "warn" : "success"}`}>
          {plan.last.error ? (
            <>
              The last backup ({plan.last.trigger === "schedule" ? "scheduled" : "manual"}, <Ago iso={plan.last.started} />
              ) failed: {plan.last.error}
            </>
          ) : (
            <>
              Last backup <Ago iso={plan.last.finished} />: {plan.last.files} files, stored as{" "}
              {formatBytes(plan.last.size)}.
              {plan.intervalHours > 0 && (
                <>
                  {" "}
                  Next{" "}
                  {new Date(new Date(plan.last.started).getTime() + plan.intervalHours * 3600_000).toLocaleString()}.
                </>
              )}
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
      {!prog && !plan.last && plan.intervalHours > 0 && (
        <p className="muted small no-margin">The first backup runs within a minute.</p>
      )}
      {overview.error && <div className="alert error">Couldn&apos;t list the bucket: {overview.error}</div>}

      {overview.backups.length > 0 && (
        <div className="file-list backup-list" role="list">
          {overview.backups.map((b) => (
            <div className="backup-item" role="listitem" key={b.key}>
              <div className="backup-row">
                <code className="file-name" title={b.key}>
                  {baseName(b.key)}
                </code>
                <span className="muted small">{formatBytes(b.size)}</span>
                <span className="muted small" title={new Date(b.lastModified).toLocaleString()}>
                  <Ago iso={b.lastModified} />
                </span>
                <span className="btn-row">
                  <a
                    className="btn ghost small"
                    href={api.configBackupDownloadUrl(b.key)}
                    download
                    onClick={(e) => {
                      e.preventDefault();
                      download(api.configBackupDownloadUrl(b.key)).catch((err) => alert((err as Error).message));
                    }}
                  >
                    Download
                  </a>
                  <button className="btn ghost small danger" onClick={() => remove(b.key)} disabled={!!busy}>
                    Delete
                  </button>
                </span>
              </div>
            </div>
          ))}
        </div>
      )}
      <p className="muted small no-margin">
        To restore, stop hlm, extract a backup into its data directory (<code>HLM_DATA_DIR</code>; <code>/data</code> in
        the Docker image), and start it again. The archive&apos;s <code>README.txt</code> explains each file.
      </p>
    </div>
  );
}
