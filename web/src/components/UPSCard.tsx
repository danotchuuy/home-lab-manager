import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { HostUPS } from "../types";
import { usePoll } from "./charts";
import { Ago } from "./common";
import { timeUntil } from "../util";

function formatRuntime(s: number): string {
  const m = Math.floor(s / 60);
  if (m >= 60) return `${Math.floor(m / 60)} h ${m % 60} min`;
  return m >= 1 ? `${m} min` : `${Math.round(s)} s`;
}

/** A battery meter: it turns amber, then red, as the charge runs down. */
function ChargeMeter({ percent }: { percent: number }) {
  const p = Math.min(Math.max(percent, 0), 100);
  const level = p <= 25 ? "critical" : p <= 50 ? "warning" : "ok";
  return (
    <div className="host-meter" role="meter" aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100} aria-label="Battery">
      <span className={`host-meter-fill ${level}`} style={{ width: `${p}%` }} />
    </div>
  );
}

function state(u: HostUPS): { label: string; badge: string } {
  const s = u.status;
  if (!s) return { label: "Can't read", badge: "state-dead" };
  if (s.onBattery && s.lowBattery) return { label: "Battery low", badge: "state-dead" };
  if (s.onBattery) return { label: "On battery", badge: "state-restarting" };
  if (s.charging) return { label: "On mains, charging", badge: "state-running" };
  if (s.onLine) return { label: "On mains", badge: "state-running" };
  return { label: s.flags || "Unknown", badge: "state-paused" };
}

/** The Host page's UPS card for a host whose UPS hlm watches. */
export function UPSCard({ hostId }: { hostId: string }) {
  const [list, , reload] = usePoll(api.ups, 15000);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const u = list?.find((x) => x.host === hostId);
  if (!u) return null;
  const s = u.status;
  const st = state(u);

  async function forget() {
    setBusy(true);
    setError(undefined);
    try {
      await api.forgetUPSStopped(hostId);
      reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const stopped = u.stopped;
  const stoppedList = stopped ? [...stopped.servers, ...stopped.stacks.map((n) => `${n} (stack)`)] : [];
  return (
    <section className="card ups-card">
      <div className="ups-card-head">
        <h2>
          UPS <span className="muted">{u.ups}</span>
        </h2>
        <span className={`badge ${st.badge}`}>{st.label}</span>
        {s?.model && <span className="muted small">{s.model}</span>}
        <Link className="muted small ups-card-settings" to="/system?tab=monitoring#ups">
          Settings
        </Link>
      </div>
      {u.error && <div className="alert error small">{u.error}</div>}
      {s && (
        <div className="stats">
          {s.charge !== undefined && (
            <div className="stat">
              <div className="stat-label">Battery</div>
              <div className="stat-value">{Math.round(s.charge)}%</div>
              <ChargeMeter percent={s.charge} />
            </div>
          )}
          {s.runtime !== undefined && (
            <div className="stat">
              <div className="stat-label">Runtime</div>
              <div className="stat-value">{formatRuntime(s.runtime)}</div>
              <div className="stat-detail muted small">on battery, at this load</div>
            </div>
          )}
          {s.load !== undefined && (
            <div className="stat">
              <div className="stat-label">Load</div>
              <div className="stat-value">{Math.round(s.load)}%</div>
            </div>
          )}
          {s.inputVoltage !== undefined && (
            <div className="stat">
              <div className="stat-label">Input</div>
              <div className="stat-value">{Math.round(s.inputVoltage)} V</div>
            </div>
          )}
        </div>
      )}
      {stopped && (
        <div className="alert warn small">
          hlm stopped {stoppedList.length > 0 ? stoppedList.join(", ") : "nothing"} <Ago iso={stopped.at} /> because the
          battery was low{stopped.shutDown ? ", then shut the host down" : ""}.{" "}
          {stopped.shutdownError && <>Shutting the host down failed: {stopped.shutdownError}. </>}
          {u.restartAt ? (
            <>
              It starts them again{" "}
              <time dateTime={u.restartAt} title={new Date(u.restartAt).toLocaleString()}>
                {timeUntil(u.restartAt)}
              </time>{" "}
              if the power stays on.{" "}
            </>
          ) : (
            "It starts them again once the power has been back for a while, if set to. "
          )}
          <button className="btn small" onClick={forget} disabled={busy}>
            Don&apos;t start them again
          </button>
        </div>
      )}
      {error && <div className="alert error small">{error}</div>}
    </section>
  );
}
