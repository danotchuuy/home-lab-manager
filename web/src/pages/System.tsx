import { DiscordSettings } from "../components/DiscordSettings";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useLocation, useSearchParams } from "react-router-dom";
import { sectionInfo, systemSection, SYSTEM_TABS as TABS, type SystemSection, type SystemTab as TabID } from "../systemSections";
import { StatusPageSettings } from "../components/StatusPageSettings";
import { useAdmin } from "../access";
import { api } from "../api";
import { Ago, Overview, Spinner } from "../components/common";
import { DeployProgress } from "../components/DeployProgress";
import { BackupSettings } from "../components/BackupSettings";
import { BackupTargets } from "../components/BackupTargets";
import { ConfigBackups } from "../components/ConfigBackups";
import { LinodeSettings } from "../components/LinodeSettings";
import { MyAccount } from "../components/MyAccount";
import { NotificationSettings } from "../components/NotificationSettings";
import { TimeSettings } from "../components/TimeSettings";
import { SystemActivity } from "../components/ActivityLog";
import { RegistrySettings } from "../components/RegistrySettings";
import { GameProfiles } from "../components/GameProfiles";
import { PrometheusSettings } from "../components/PrometheusSettings";
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

/** A card on the System page: where it is and what's in it. */
interface Section extends SystemSection {
  body: ReactNode;
  className?: string;
}

/** Sections whose title or keywords match every word typed. */
function findSections(sections: Section[], query: string): Section[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return [];
  return sections.filter((s) => {
    const text = `${s.title} ${s.keywords}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

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

  // The tab is in the address (?tab=), so it can be linked to and Back works.
  const [params, setParams] = useSearchParams();
  const [query, setQuery] = useState("");
  // A section to scroll to once its tab is shown.
  const [jumpTo, setJumpTo] = useState<string>();
  const loaded = !!info;
  // Links like /system#linode open their section's tab and scroll to it.
  const { hash } = useLocation();
  useEffect(() => {
    const id = hash.slice(1);
    const t = sectionInfo(id)?.tab;
    if (!loaded || !t) return;
    setParams(t === "overview" ? {} : { tab: t }, { replace: true });
    setJumpTo(id);
  }, [loaded, hash, setParams]);
  // On a narrow screen the tabs scroll sideways: keep the chosen one in view.
  const tabParamNow = params.get("tab");
  useEffect(() => {
    document.querySelector(".system-nav .tabs button.active")?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [tabParamNow, loaded]);
  useEffect(() => {
    if (!jumpTo) return;
    const el = document.getElementById(jumpTo);
    if (el) {
      el.scrollIntoView({ block: "start" });
      setJumpTo(undefined);
    }
  });

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

  const sections: Section[] = [
    {
      ...systemSection("version"),
      body: (
        <>
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
                {u.latest.published && <span className="muted"> · published <Ago iso={u.latest.published} /></span>}
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
          <dd><Ago iso={u.checkedAt} /></dd>
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
        </>
      ),
    },
    ...(u.latest?.notes
      ? [
          {
            ...systemSection("release-notes"),
            title: `What's new in ${u.latest.version}`,
            className: "release-notes",
            body: <Overview text={u.latest.notes} />,
          },
        ]
      : []),
    {
      ...systemSection("updates"),
      body: <SystemUpdateSettings timezone={timezone} />,
    },
    { ...systemSection("time"), body: <TimeSettings /> },

    { ...systemSection("sign-in"), body: <SignInSettings /> },
    { ...systemSection("sso"), body: <SSOSettings /> },
    { ...systemSection("https"), body: <HTTPSSettings /> },
    { ...systemSection("api-keys"), body: <APIKeys /> },
    { ...systemSection("server-groups"), body: <ServerGroups /> },

    {
      ...systemSection("backup"),
      body: (
        <>
          <BackupSettings onChange={() => setStorageVersion((v) => v + 1)} />
          <BackupTargets version={storageVersion} onChange={() => setStorageVersion((v) => v + 1)} />
        </>
      ),
    },
    { ...systemSection("config-backup"), body: <ConfigBackups storageVersion={storageVersion} /> },

    { ...systemSection("game-profiles"), body: <GameProfiles /> },
    { ...systemSection("registries"), body: <RegistrySettings /> },

    { ...systemSection("status-page"), body: <StatusPageSettings /> },
    { ...systemSection("discord"), body: <DiscordSettings /> },

    { ...systemSection("unifi"), body: <UnifiSettings /> },
    { ...systemSection("linode"), body: <LinodeSettings /> },

    { ...systemSection("notifications"), body: <NotificationSettings /> },
    { ...systemSection("activity"), body: <SystemActivity /> },
    { ...systemSection("prometheus"), body: <PrometheusSettings /> },
  ];
  const tabParam = params.get("tab");
  const tab: TabID = TABS.some((t) => t.id === tabParam) ? (tabParam as TabID) : "overview";
  const showTab = (t: TabID) => setParams(t === "overview" ? {} : { tab: t });
  const showSection = (id: string) => {
    const sec = sections.find((x) => x.id === id);
    if (!sec) return;
    showTab(sec.tab);
    setJumpTo(id);
  };
  const found = findSections(sections, query);

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

      <div className="system-nav">
        <div className="tabs" role="tablist">
          {TABS.map((t) => (
            <button
              key={t.id}
              role="tab"
              aria-selected={tab === t.id}
              className={tab === t.id ? "active" : ""}
              onClick={() => showTab(t.id)}
            >
              {t.label}
              {t.id === "overview" && u.available && <span className="tab-dot" title="An update is available" />}
            </button>
          ))}
        </div>
        <div className="system-find">
          <input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Find a setting…"
            aria-label="Find a setting"
          />
          {query.trim() && (
            <ul className="system-find-results">
              {found.length === 0 && <li className="muted small">Nothing matches.</li>}
              {found.map((f) => (
                <li key={f.id}>
                  <button
                    type="button"
                    className="link-button"
                    onClick={() => {
                      setQuery("");
                      showSection(f.id);
                    }}
                  >
                    {f.title}
                  </button>{" "}
                  <span className="muted small">{TABS.find((t) => t.id === f.tab)?.label}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      {sections
        .filter((sec) => sec.tab === tab)
        .map((sec) => (
          <section key={sec.id} className={`card${sec.className ? " " + sec.className : ""}`} id={sec.id}>
            <h2>{sec.title}</h2>
            {sec.body}
          </section>
        ))}
    </div>
  );
}
