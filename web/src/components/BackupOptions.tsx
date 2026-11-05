import type { BackupAt, BackupRetention, BackupVerification } from "../types";
import { formatBytes, timeAgo } from "../util";

export const DEFAULT_RETENTION: BackupRetention = { last: 3, daily: 7, weekly: 4, monthly: 6 };

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

const num = (v: string) => Math.max(0, Math.floor(Number(v) || 0));

/** How many backups to keep: the newest N, or by age. */
export function KeepingFields({
  keep,
  retention,
  onChange,
  what = "backups",
}: {
  keep: number;
  retention?: BackupRetention;
  onChange: (keep: number, retention?: BackupRetention) => void;
  what?: string;
}) {
  const r = retention;
  const setR = (x: Partial<BackupRetention>) => onChange(keep, { ...(r ?? DEFAULT_RETENTION), ...x });
  const field = (label: string, value: number, set: (n: number) => void) => (
    <label className="inline-input small">
      <input type="number" min={0} max={1000} value={value} onChange={(e) => set(num(e.target.value))} />
      {label}
    </label>
  );
  return (
    <div className="field keeping">
      <span className="field-label">Which {what} to keep</span>
      <label className="radio small">
        <input type="radio" checked={!r} onChange={() => onChange(keep || 7, undefined)} />
        The newest
        <input
          type="number"
          className="tiny-number"
          min={0}
          value={r ? "" : keep}
          disabled={!!r}
          onChange={(e) => onChange(num(e.target.value), undefined)}
        />
        (0 keeps them all)
      </label>
      <label className="radio small">
        <input type="radio" checked={!!r} onChange={() => onChange(keep, retention ?? DEFAULT_RETENTION)} />
        By age
      </label>
      {r && (
        <div className="retention-grid indent">
          {field("newest", r.last, (n) => setR({ last: n }))}
          {field("days, one a day", r.daily, (n) => setR({ daily: n }))}
          {field("weeks, one a week", r.weekly, (n) => setR({ weekly: n }))}
          {field("months, one a month", r.monthly, (n) => setR({ monthly: n }))}
        </div>
      )}
      <span className="field-help">
        {r
          ? "A backup is kept if any of these keeps it: the newest few, and the newest one of each recent day, week and month that has one. Older ones are deleted after each backup."
          : "Older ones are deleted after each backup."}
      </span>
    </div>
  );
}

/** Weekday toggles and a time, for backing up at a time of day. */
export function AtFields({ at, onChange }: { at: BackupAt; onChange: (at: BackupAt) => void }) {
  const days = at.weekdays ?? [];
  const toggle = (d: number) => {
    const next = days.includes(d) ? days.filter((x) => x !== d) : [...days, d].sort();
    onChange({ ...at, weekdays: next.length === 7 ? [] : next });
  };
  return (
    <div className="at-fields">
      <input type="time" value={at.time} onChange={(e) => onChange({ ...at, time: e.target.value })} />
      <div className="view-toggle" role="group" aria-label="Weekdays">
        {WEEKDAYS.map((name, d) => (
          <button
            key={d}
            type="button"
            className={days.length === 0 || days.includes(d) ? "active" : ""}
            aria-pressed={days.length === 0 || days.includes(d)}
            onClick={() => toggle(d)}
          >
            {name}
          </button>
        ))}
      </div>
    </div>
  );
}

export function describeAt(at: BackupAt, tz?: string): string {
  const days = at.weekdays ?? [];
  const when = days.length === 0 ? "every day" : `on ${days.map((d) => WEEKDAYS[d]).join(", ")}`;
  return `${when} at ${at.time}${tz ? ` (${tz})` : ""}`;
}

/** A backup's verification: ✓, ✗, or nothing if it hasn't been read back. */
export function VerifyBadge({ v }: { v?: BackupVerification }) {
  if (!v) return null;
  const title = v.ok
    ? `Read back ${timeAgo(v.at)}: ${v.files} files, ${formatBytes(v.bytes)}${v.checksum ? ", checksum matches" : " (no stored checksum to compare)"}`
    : `Reading it back ${timeAgo(v.at)} failed: ${v.error}`;
  return (
    <span className={`badge ${v.ok ? "verify-ok" : "verify-failed"}`} title={title}>
      {v.ok ? "✓ verified" : "✗ damaged"}
    </span>
  );
}
