import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { TimezoneInfo, UpcomingTask } from "../types";
import { formatInZone, timeUntil } from "../util";
import { Spinner } from "./common";

/** System → Time zone and schedule: hlm's time zone, and the next scheduled tasks on every server. */
export function TimeSettings() {
  const [info, setInfo] = useState<TimezoneInfo>();
  const [zone, setZone] = useState("");
  const [upcoming, setUpcoming] = useState<UpcomingTask[]>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const zones = useMemo(() => {
    try {
      return Intl.supportedValuesOf("timeZone");
    } catch {
      return [];
    }
  }, []);

  useEffect(() => {
    api.timezone().then((i) => {
      setInfo(i);
      setZone(i.timezone);
    }, (e) => setError((e as Error).message));
    api.upcomingTasks().then(setUpcoming, () => setUpcoming([]));
  }, []);

  async function save() {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const i = await api.setTimezone(zone);
      setInfo(i);
      setNotice(`Scheduled tasks and quiet hours now use ${i.effective}.`);
      setUpcoming(await api.upcomingTasks());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!info) return error ? <div className="alert error">{error}</div> : <Spinner />;
  const browser = Intl.DateTimeFormat().resolvedOptions().timeZone;

  return (
    <div className="time-settings">
      <p className="muted small no-margin">
        Scheduled tasks and notifications&apos; quiet hours use this time zone. It&apos;s <strong>{info.effective}</strong>
        {info.timezone ? "" : " (the system's)"}.
      </p>
      <div className="inline-input">
        <select value={zone} onChange={(e) => setZone(e.target.value)} aria-label="Time zone">
          <option value="">The system&apos;s ({info.system})</option>
          {zones.map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </select>
        <button className="btn small primary" onClick={save} disabled={busy || zone === info.timezone}>
          {busy ? "Saving…" : "Save"}
        </button>
        {browser && browser !== info.effective && zone !== browser && (
          <button className="link-button" onClick={() => setZone(browser)}>
            Use this browser&apos;s ({browser})
          </button>
        )}
      </div>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}

      <h3>Coming up</h3>
      {!upcoming ? (
        <Spinner />
      ) : upcoming.length === 0 ? (
        <p className="muted small">No scheduled tasks. Add them on a server&apos;s Schedule tab.</p>
      ) : (
        <ul className="upcoming-list small">
          {upcoming.slice(0, 10).map((u) => (
            <li key={u.server + u.taskId}>
              <span className="upcoming-time">{formatInZone(u.next, info.effective)}</span>{" "}
              <Link to={`/servers/${encodeURIComponent(u.server)}?tab=schedule`}>{u.server}</Link>: {u.taskName}{" "}
              <span className="muted">({timeUntil(u.next)})</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
