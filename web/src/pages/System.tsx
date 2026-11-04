import { useEffect, useRef, useState } from "react";
import { useAdmin } from "../access";
import { api } from "../api";
import { Overview, Spinner } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import { BackupSettings } from "../components/BackupSettings";
import { ConfigBackups } from "../components/ConfigBackups";
import { LinodeSettings } from "../components/LinodeSettings";
import { MyAccount } from "../components/MyAccount";
import { NotificationSettings } from "../components/NotificationSettings";
import { TimeSettings } from "../components/TimeSettings";
import { SystemUpdateSettings } from "../components/UpdatePolicy";
import { ServerGroups } from "../components/ServerGroups";
import { APIKeys } from "../components/APIKeys";
import { SignInSettings } from "../components/SignInSettings";
import { HTTPSSettings } from "../components/HTTPSSettings";
import { SSOSettings } from "../components/SSOSettings";
import { UnifiSettings } from "../components/UnifiSettings";
import type { DeployEvent, SystemInfo } from "../types";
import { timeAgo } from "../util";

type Phase = "idle" | "updating" | "restarting" | "timeout";

/** How long to wait for hlm to come back after an update before giving up. */
const RESTART_TIMEOUT = 3 * 60 * 1000;

/** Set across the reload that follows an update. */
const JUST_UPDATED = "hlm.justUpdated";
/** Set instead by the page of a version from before the short name changed from gsh to hlm. */
const LEGACY_JUST_UPDATED = "gsh.justUpdated";

export function SystemPage() {
  const admin = useAdmin();
  // Bumped when backup storage is saved or removed, so configuration backups reload.
  const [storageVersion, setStorageVersion] = useState(0);
  // hlm's time zone, which update windows are in.
  const [timezone, setTimezone] = useState("");
  useEffect(() => {
    api.timezone().then((t) => setTimezone(t.effective), () => {});
  }, []);
  const [info, setInfo] = useState<SystemInfo>();
  const [error, setError] = useState<string>();
  const [checking, setChecking] = useState(false);
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [phase, setPhase] = useState<Phase>("idle");
  const fromVersion = useRef<string>("");

  useEffect(() => {
    api.system().then(setInfo, (e) => setError(e.message));
    // A container update's helper records the outcome only after watching
    // the new container for ~20s, so keep looking for a minute after the
    // reload that follows an update.
    let flagged = false;
    try {
      flagged = sessionStorage.getItem(JUST_UPDATED) !== null || sessionStorage.getItem(LEGACY_JUST_UPDATED) !== null;
      sessionStorage.removeItem(JUST_UPDATED);
      sessionStorage.removeItem(LEGACY_JUST_UPDATED);
    } catch {
      // Without storage the outcome just shows on the next visit.
    }
    if (!flagged) return;
    const until = Date.now() + 60_000;
    const t = setInterval(async () => {
      try {
        const now = await api.system();
        setInfo(now);
        if (now.update.lastResult || Date.now() > until) clearInterval(t);
      } catch {
        // Still starting.
      }
    }, 3000);
    return () => clearInterval(t);
  }, []);

  // Links like /system#linode jump to their section once the page is shown.
  const loaded = !!info;
  useEffect(() => {
    if (loaded && window.location.hash) document.getElementById(window.location.hash.slice(1))?.scrollIntoView();
  }, [loaded]);

  // After an update, wait for hlm to come back (on a new version, or on the
  // old one if the update was rolled back), then reload to get the new UI.
  useEffect(() => {
    if (phase !== "restarting") return;
    const started = Date.now();
    let sawDown = false;
    const t = setInterval(async () => {
      try {
        const now = await api.system();
        if (now.version !== fromVersion.current || (sawDown && now.update.lastResult)) {
          try {
            sessionStorage.setItem(JUST_UPDATED, "1");
          } catch {
            // See above.
          }
          window.location.reload();
        }
      } catch {
        sawDown = true; // hlm is restarting
      }
      if (Date.now() - started > RESTART_TIMEOUT) {
        clearInterval(t);
        setPhase("timeout");
      }
    }, 2000);
    return () => clearInterval(t);
  }, [phase]);

  async function check() {
    setChecking(true);
    try {
      setInfo(await api.checkSelfUpdate());
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }

  async function update() {
    const u = info!.update;
    const how =
      u.mode === "container"
        ? "hlm's container will be replaced with the new version."
        : "hlm's binary will be replaced and hlm will restart.";
    if (!confirm(`Update hlm from ${u.current} to ${u.latest!.version}? ${how} Game servers keep running.`)) return;
    fromVersion.current = u.current;
    setEvents([]);
    setPhase("updating");
    let restarting = false;
    let failed = false;
    try {
      await api.applySelfUpdate((e) => {
        setEvents((prev) => [...prev, e]);
        if (e.stage === "restart") restarting = true;
        if (e.stage === "error") failed = true;
      });
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
      failed = true;
    }
    setPhase(restarting && !failed ? "restarting" : "idle");
  }

  async function dismiss() {
    await api.dismissSelfUpdate();
    setInfo(await api.system());
  }

  if (error && !info) return <div className="page"><div className="alert error">{error}</div></div>;
  if (!info) return <div className="page"><Spinner /></div>;
  if (!admin) {
    return (
      <div className="page system-page">
        <h1>System</h1>
        <section className="card">
          <h2>Your account</h2>
          <MyAccount />
        </section>
        <section className="card">
          <h2>Home Lab Manager</h2>
          <p className="muted">
            Version <code>{info.version}</code>
          </p>
        </section>
      </div>
    );
  }
  const u = info.update;
  const last = u.lastResult;

  return (
    <div className="page system-page">
      <h1>System</h1>

      {last && (
        <div className={`alert ${last.ok ? "success" : "error"}`}>
          {last.ok
            ? `Updated from ${last.from} to ${last.to} ${timeAgo(last.at)}.`
            : `Updating from ${last.from} to ${last.to} failed ${timeAgo(last.at)}, and ${last.from} was restored: ${last.error}`}{" "}
          <button className="link-button" onClick={dismiss}>
            Dismiss
          </button>
        </div>
      )}

      <section className="card">
        <h2>Home Lab Manager</h2>
        <dl className="facts">
          <dt>Version</dt>
          <dd>
            <code>{info.version}</code>
          </dd>
          <dt>Installed as</dt>
          <dd>
            {u.mode === "container" ? "Docker container" : "Binary"}
            {u.target && (
              <>
                {" "}
                · <code>{u.target}</code>
              </>
            )}
          </dd>
          <dt>Latest release</dt>
          <dd>
            {u.latest ? (
              <>
                <code>{u.latest.version}</code>
                {u.latest.published && <span className="muted"> · published {timeAgo(u.latest.published)}</span>}
                {u.latest.url && (
                  <>
                    {" "}
                    ·{" "}
                    <a href={u.latest.url} target="_blank" rel="noreferrer">
                      {u.latest.notes ? "release page" : "details"} ↗
                    </a>
                  </>
                )}
              </>
            ) : (
              <span className="muted">{u.checkError ?? "not checked yet"}</span>
            )}
          </dd>
          <dt>Last checked</dt>
          <dd>{timeAgo(u.checkedAt)}</dd>
        </dl>

        {u.latest && u.checkError && <div className="alert error">Last check failed: {u.checkError}</div>}
        {u.available ? (
          <div className="alert info">
            <strong>{u.latest!.version}</strong> is available (you're on {u.current}).
          </div>
        ) : (
          u.latest && !u.checkError && <p className="muted">You're running the latest version.</p>
        )}
        {!u.canUpdate && u.reason && <p className="muted small">Automatic updates aren't available: {u.reason}.</p>}

        <div className="btn-row">
          <button className="btn" onClick={check} disabled={checking || phase !== "idle"}>
            {checking ? "Checking…" : "Check for updates"}
          </button>
          {u.available && u.canUpdate && (
            <button className="btn primary" onClick={update} disabled={phase !== "idle" || u.updating}>
              {phase === "updating" ? "Updating…" : `Update to ${u.latest!.version}`}
            </button>
          )}
        </div>

        {events.length > 0 && (
          <div className="system-progress">
            <DeployProgress events={events.filter((e) => e.stage !== "restart")} />
            {phase === "restarting" && (
              <div className="alert info">
                <Spinner /> {events.find((e) => e.stage === "restart")?.message} This page reloads when hlm is back.
              </div>
            )}
            {phase === "timeout" && (
              <div className="alert error">
                hlm hasn't come back after 3 minutes. Check its logs (or `docker logs home-lab-manager`).
              </div>
            )}
          </div>
        )}
      </section>

      <section className="card" id="backup">
        <h2>Backup storage</h2>
        <BackupSettings onChange={() => setStorageVersion((v) => v + 1)} />
      </section>

      <section className="card" id="config-backup">
        <h2>Backups of hlm&apos;s configuration</h2>
        <ConfigBackups storageVersion={storageVersion} />
      </section>

      <section className="card" id="updates">
        <h2>Automatic updates</h2>
        <SystemUpdateSettings timezone={timezone} />
      </section>

      <section className="card" id="time">
        <h2>Time zone and schedule</h2>
        <TimeSettings />
      </section>

      <section className="card" id="notifications">
        <h2>Notifications</h2>
        <NotificationSettings />
      </section>

      <section className="card" id="sign-in">
        <h2>Sign-in</h2>
        <SignInSettings />
      </section>

      <section className="card" id="https">
        <h2>HTTPS</h2>
        <HTTPSSettings />
      </section>

      <section className="card" id="server-groups">
        <h2>Server groups</h2>
        <ServerGroups />
      </section>

      <section className="card" id="sso">
        <h2>Single sign-on (Authelia, OpenID Connect)</h2>
        <SSOSettings />
      </section>

      <section className="card" id="api-keys">
        <h2>API keys</h2>
        <APIKeys />
      </section>

      <section className="card">
        <h2>UniFi port forwarding</h2>
        <UnifiSettings />
      </section>

      <section className="card" id="linode">
        <h2>Linode firewall</h2>
        <LinodeSettings />
      </section>

      {u.latest?.notes && (
        <section className="card release-notes">
          <h2>What's new in {u.latest.version}</h2>
          <Overview text={u.latest.notes} />
        </section>
      )}
    </div>
  );
}
