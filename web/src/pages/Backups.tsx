import { Fragment, useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api, download } from "../api";
import { describeAt } from "../components/BackupOptions";
import { Ago, Spinner } from "../components/common";
import { baseName, INTERVALS, STAGES } from "../components/ServerBackups";
import type { AllBackups, BackupObject, BackupProgress, ServerBackupsSummary } from "../types";
import { usePoll } from "../usePoll";
import { formatBytes, timeAgo } from "../util";

const backupsTab = (server: string) => `/servers/${encodeURIComponent(server)}?tab=backups`;

/** Every server's backups: those running now, then each server's schedule, last backup and stored ones. */
export function BackupsPage() {
  const admin = useAdmin();
  const [data, setData] = useState<AllBackups>();
  // The bucket listing, kept between the quick polls while something runs.
  const [stored, setStored] = useState<Record<string, Pick<ServerBackupsSummary, "stored" | "storedSize" | "storedError">>>({});
  const [listedAt, setListedAt] = useState(0);
  const [error, setError] = useState<string>();
  const [open, setOpen] = useState<string>();

  const running = !!data && (!!data.config || data.servers.some((s) => s.running));
  const load = useCallback(async () => {
    // The bucket is listed every minute, and once a running backup ends.
    const full = Date.now() - listedAt > 60_000;
    try {
      const d = await api.allBackups(full);
      setData((prev) => {
        if (!full && prev && prev.servers.some((s) => s.running && !d.servers.find((n) => n.server === s.server)?.running)) setListedAt(0);
        return d;
      });
      if (full) {
        setStored(Object.fromEntries(d.servers.map((s) => [s.server, { stored: s.stored, storedSize: s.storedSize, storedError: s.storedError }])));
        setListedAt(Date.now());
      }
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [listedAt]);
  usePoll(load, running ? 2000 : 30000, [load, running]);

  if (!data) {
    return (
      <div className="page">
        <h1>Backups</h1>
        {error ? <div className="alert error">{error}</div> : <Spinner />}
      </div>
    );
  }

  const now = data.servers.filter((s) => s.running);
  const servers = data.servers.map((s) => ({ ...s, ...(stored[s.server] ?? {}) }));
  const total = servers.reduce((n, s) => n + (s.storedSize ?? 0), 0);
  const count = servers.reduce((n, s) => n + (s.stored?.length ?? 0), 0);

  return (
    <div className="page backups-page">
      <div className="settings-actions">
        <div>
          <h1>Backups</h1>
          <p className="muted small no-margin">
            {data.configured ? (
              <>
                {count} backup{count === 1 ? "" : "s"} of {servers.length} server{servers.length === 1 ? "" : "s"} in{" "}
                <strong>{data.target.name || "the backup storage"}</strong>, {formatBytes(total)} in all.
              </>
            ) : (
              "No backup storage is set up yet, so nothing is backed up."
            )}
          </p>
        </div>
        {admin && (
          <Link className="btn small ghost" to="/system#backup">
            {data.configured ? "Backup storage" : "Set up backup storage"}
          </Link>
        )}
      </div>
      {error && <div className="alert error small">{error}</div>}
      {data.error && <div className="alert error small">{data.error}</div>}

      <div className="card">
        <h2>Running now</h2>
        {now.length === 0 && !data.config ? (
          <p className="muted small no-margin">No backups or restores are running.</p>
        ) : (
          <ul className="backup-running">
            {data.config && <RunningRow label="hlm's settings" to="/system#backup" prog={data.config} />}
            {now.map((s) => (
              <RunningRow key={s.server} label={s.server} to={backupsTab(s.server)} prog={s.running!} last={s.lastOk} />
            ))}
          </ul>
        )}
      </div>

      <div className="card">
        <h2>Servers</h2>
        {servers.length === 0 ? (
          <p className="muted small no-margin">
            No server has a backup plan yet. Choose what to back up on a server&apos;s <strong>Backups</strong> tab.
          </p>
        ) : (
          <div className="table-scroll">
            <table className="data-table backups-table">
              <thead>
                <tr>
                  <th>Server</th>
                  <th>Schedule</th>
                  <th>Last backup</th>
                  <th>Next</th>
                  <th>Stored</th>
                </tr>
              </thead>
              <tbody>
                {servers.map((s) => (
                  <Fragment key={s.server}>
                    <tr>
                      <td data-label="Server">
                        <Link to={backupsTab(s.server)}>{s.server}</Link>
                        {s.running && (
                          <span className="badge state-restarting" title={STAGES[s.running.stage] ?? s.running.stage}>
                            {s.running.kind === "restore" ? "restoring" : "backing up"}
                          </span>
                        )}
                        <div className="muted small" title={s.folders.join("\n")}>
                          {s.folders.length ? s.folders.join(", ") : "no folders chosen"}
                        </div>
                      </td>
                      <td className="small" data-label="Schedule">{schedule(s)}</td>
                      <td className="small" data-label="Last backup">
                        <LastRun s={s} />
                      </td>
                      <td className="small" data-label="Next">{s.next ? <Next iso={s.next} /> : <span className="muted">—</span>}</td>
                      <td className="small" data-label="Stored">
                        {s.storedError ? (
                          <span className="form-error" title={s.storedError}>
                            couldn&apos;t list
                          </span>
                        ) : !s.stored?.length ? (
                          <span className="muted">none</span>
                        ) : (
                          <button
                            type="button"
                            className="link-button"
                            onClick={() => setOpen(open === s.server ? undefined : s.server)}
                            aria-expanded={open === s.server}
                          >
                            {s.stored.length} · {formatBytes(s.storedSize)} {open === s.server ? "▴" : "▾"}
                          </button>
                        )}
                      </td>
                    </tr>
                    {open === s.server && s.stored?.length > 0 && (
                      <tr className="backups-stored">
                        <td colSpan={5}>
                          <StoredList server={s.server} backups={s.stored} />
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function schedule(s: ServerBackupsSummary): string {
  if (s.at) return describeAt(s.at);
  if (s.intervalHours) return INTERVALS.find(([h]) => h === s.intervalHours)?.[1] ?? `Every ${s.intervalHours} hours`;
  return "Only when asked";
}

function LastRun({ s }: { s: ServerBackupsSummary }) {
  const r = s.last;
  if (!r) return <span className="muted">never</span>;
  if (r.error) {
    return (
      <>
        <span className="form-error" title={r.error}>
          failed
        </span>{" "}
        <Ago iso={r.finished || r.started} />
        {s.lastOk && (
          <div className="muted">
            last good <Ago iso={s.lastOk.finished} />
          </div>
        )}
      </>
    );
  }
  return (
    <>
      <Ago iso={r.finished} />
      <div className="muted">
        {formatBytes(r.size)} · {r.files} files{r.warnings?.length ? ` · ${r.warnings.length} warning${r.warnings.length === 1 ? "" : "s"}` : ""}
      </div>
    </>
  );
}

function Next({ iso }: { iso: string }) {
  const d = new Date(iso);
  if (d.getTime() <= Date.now() + 30_000) return <>now</>;
  return (
    <time dateTime={iso} title={d.toLocaleString()}>
      {d.toDateString() === new Date().toDateString() ? d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : d.toLocaleString([], { weekday: "short", hour: "numeric", minute: "2-digit" })}
    </time>
  );
}

/** A backup or restore in progress: what it's doing, how far it's got, and for how long. */
function RunningRow({ label, to, prog, last }: { label: string; to: string; prog: BackupProgress; last?: { size: number } }) {
  const elapsed = Math.max(1, (Date.now() - new Date(prog.started).getTime()) / 1000);
  const rate = prog.transferred / elapsed;
  // How far, against the last good backup's size, when it's an upload.
  const pct = prog.kind === "backup" && prog.stage === "uploading" && last?.size ? Math.min(99, Math.round((prog.transferred / last.size) * 100)) : undefined;
  return (
    <li>
      <div className="settings-actions">
        <div>
          <Link to={to}>
            <strong>{label}</strong>
          </Link>{" "}
          <span className="muted small">
            {prog.kind === "restore" ? "restore" : "backup"} · {prog.trigger === "schedule" ? "scheduled" : prog.trigger} · started {timeAgo(prog.started)}
          </span>
          <div className="small">{STAGES[prog.stage] ?? prog.stage}</div>
        </div>
        <div className="small backup-running-numbers">
          {prog.files > 0 && (
            <div>
              {prog.files.toLocaleString()} files, {formatBytes(prog.bytes)} read
            </div>
          )}
          {prog.transferred > 0 && (
            <div className="muted">
              {formatBytes(prog.transferred)} {prog.kind === "restore" ? "downloaded" : "uploaded"} · {formatBytes(rate)}/s
              {pct !== undefined && ` · ~${pct}% of the last one's size`}
            </div>
          )}
        </div>
      </div>
      {pct !== undefined && <progress value={pct} max={100} aria-label={`${label}: about ${pct}%`} />}
    </li>
  );
}

function StoredList({ server, backups }: { server: string; backups: BackupObject[] }) {
  return (
    <ul className="backups-stored-list small">
      {backups.map((b) => (
        <li key={b.key}>
          <span title={b.key}>{baseName(b.key)}</span>
          <span className="muted">
            <Ago iso={b.lastModified} /> · {formatBytes(b.size)}
          </span>
          <a
            href={api.backupDownloadUrl(server, b.key)}
            onClick={(e) => {
              e.preventDefault();
              download(api.backupDownloadUrl(server, b.key)).catch((err) => alert((err as Error).message));
            }}
          >
            Download
          </a>
        </li>
      ))}
    </ul>
  );
}
