import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api } from "../api";
import type { IdleSettings as Settings, IdleStatus } from "../types";
import { Ago, Spinner } from "./common";

const DEFAULT_MINUTES = 30;

/** Schedule tab → When nobody's playing: stop the server, and wake it again. */
export function IdleSettings({ server, canManage }: { server: string; canManage: boolean }) {
  const admin = useAdmin();
  const [status, setStatus] = useState<IdleStatus>();
  const [form, setForm] = useState<Settings>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api.serverIdle(server).then(
      (st) => {
        setStatus(st);
        setForm({ minutes: st.minutes, wakeOnConnect: st.wakeOnConnect, wakeFromStatusPage: st.wakeFromStatusPage });
      },
      (e) => setError((e as Error).message),
    );
  }, [server]);

  if (!status || !form) {
    return <div className="card">{error ? <div className="alert error small">{error}</div> : <Spinner />}</div>;
  }
  const on = form.minutes > 0;
  const set = (patch: Partial<Settings>) => {
    setForm({ ...form, ...patch });
    setNotice(undefined);
  };
  const changed =
    form.minutes !== status.minutes ||
    form.wakeOnConnect !== status.wakeOnConnect ||
    form.wakeFromStatusPage !== status.wakeFromStatusPage;

  async function save() {
    if (!form) return;
    setSaving(true);
    setError(undefined);
    try {
      const st = await api.setServerIdle(server, form);
      setStatus(st);
      setForm({ minutes: st.minutes, wakeOnConnect: st.wakeOnConnect, wakeFromStatusPage: st.wakeFromStatusPage });
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    }
    setSaving(false);
  }

  return (
    <div className="card idle-settings">
      <h2>When nobody&apos;s playing</h2>
      <p className="muted small">
        Stop the server after a while without players, so it isn&apos;t holding memory and CPU, and start it again when
        someone wants to play. Players are warned a minute before (with the game&apos;s say command, if its console is
        set up), and games that need it save first.
      </p>
      {status.asleep && (
        <div className="alert info small">
          <strong>Asleep</strong> since <Ago iso={status.asleep.since} />: nobody played for {status.asleep.minutes}{" "}
          minutes.{" "}
          {status.asleep.listener
            ? "It starts when someone connects to it."
            : form.wakeOnConnect && status.asleep.listenerError
              ? `It can't wake on a connection right now: ${status.asleep.listenerError}.`
              : "Start it from here, or from the status page if that's allowed."}
        </div>
      )}
      {!status.asleep && on && status.idleSince && (
        <p className="small">
          Nobody on since <Ago iso={status.idleSince} />.
        </p>
      )}
      {on && !status.canStop && !status.asleep && <div className="alert small">{status.why}</div>}
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert info small">{notice}</div>}
      <fieldset className="plain-fieldset config-form" disabled={!canManage}>
        <label className="radio small">
          <input
            type="checkbox"
            checked={on}
            onChange={(e) => set({ minutes: e.target.checked ? DEFAULT_MINUTES : 0 })}
          />
          Stop it when nobody has played for
          <span className="inline-input">
            <input
              type="number"
              min={5}
              max={1440}
              value={on ? form.minutes : DEFAULT_MINUTES}
              disabled={!on}
              onChange={(e) => set({ minutes: Number(e.target.value) || 0 })}
            />
          </span>
          minutes
        </label>
        <label className="radio small">
          <input
            type="checkbox"
            checked={form.wakeOnConnect}
            disabled={!on || (!status.canListen && !form.wakeOnConnect)}
            onChange={(e) => set({ wakeOnConnect: e.target.checked })}
          />
          Start it when someone connects to it
        </label>
        <span className="field-help">
          {status.canListen ? (
            <>
              While it&apos;s stopped, a small listener takes its ports. Someone joining starts it; they connect again a
              minute or two later. Minecraft&apos;s server list shows it as asleep, and server browsers (Steam&apos;s
              queries) and port scans don&apos;t wake it. With ports open to the internet, anyone who connects can start
              it.
            </>
          ) : (
            status.listenWhy
          )}
        </span>
        <label className="radio small">
          <input
            type="checkbox"
            checked={form.wakeFromStatusPage}
            disabled={!on}
            onChange={(e) => set({ wakeFromStatusPage: e.target.checked })}
          />
          Let visitors start it from the status page
        </label>
        <span className="field-help">
          A Start button next to it on the public status page while it&apos;s asleep.{" "}
          {status.onStatusPage ? (
            "It's on the page."
          ) : (
            <strong>
              It isn&apos;t on the page{admin ? <>: add it under <Link to="/system#status-page">System → Sharing → Status page</Link></> : " (an admin adds it)"}.
            </strong>
          )}
        </span>
      </fieldset>
      {canManage && (
        <div className="btn-row">
          <button className="btn primary small" onClick={save} disabled={!changed || saving}>
            {saving ? "Saving…" : "Save"}
          </button>
        </div>
      )}
    </div>
  );
}
