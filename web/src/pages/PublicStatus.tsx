import { useCallback, useEffect, useState, type FormEvent } from "react";
import { CopyButton } from "../components/ConnectInfo";
import type { PublicStatusPage, PublicStatusServer } from "../types";

const STATE_LABEL: Record<PublicStatusServer["state"], string> = {
  online: "Online",
  starting: "Starting",
  running: "Up",
  offline: "Offline",
  asleep: "Asleep",
};

/**
 * The public status page at /status: for people without an hlm account. It
 * stands on its own (no header, no sign-in) and only reads /api/status-page.
 */
export function PublicStatus() {
  const [page, setPage] = useState<PublicStatusPage>();
  const [error, setError] = useState<string>();
  // Set when the page needs its password; the title to show above the form.
  const [locked, setLocked] = useState<string>();

  // fresh skips the browser's cache of the page (15 seconds), after a change.
  const load = useCallback(async (fresh = false) => {
    try {
      const res = await fetch("/api/status-page", {
        credentials: "same-origin",
        cache: fresh ? "no-cache" : "default",
      });
      if (res.status === 401) {
        const body = (await res.json().catch(() => ({}))) as { title?: string };
        setLocked(body.title ?? "Status");
        setPage(undefined);
        document.title = body.title ?? "Status";
      } else if (res.status === 404) setError("There's no status page here.");
      else if (!res.ok) setError("The status isn't available right now.");
      else {
        const p = (await res.json()) as PublicStatusPage;
        setPage(p);
        setLocked(undefined);
        setError(undefined);
        document.title = p.title;
      }
    } catch {
      setError("The status isn't available right now.");
    }
  }, []);

  useEffect(() => {
    load();
    const timer = setInterval(() => load(), 30000);
    return () => clearInterval(timer);
  }, [load]);

  async function forget() {
    await fetch("/api/status-page/lock", {
      method: "POST",
      headers: { "X-HLM-Request": "1" },
      credentials: "same-origin",
    });
    load();
  }

  return (
    <main className="status-page">
      {locked && !page ? (
        <>
          <h1>{locked}</h1>
          <PasswordForm label="This page needs a password." onUnlocked={load} />
        </>
      ) : error && !page ? (
        <p className="muted">{error}</p>
      ) : !page ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          <h1>{page.title}</h1>
          {page.message && <div className="alert info status-message">{page.message}</div>}
          <div className="status-list">
            {page.servers.map((s) => (
              <StatusCard key={s.slug} s={s} onWoken={() => load(true)} />
            ))}
          </div>
          {page.access?.detailsLocked && (
            <PasswordForm label="Have the password for names and addresses?" compact onUnlocked={load} />
          )}
          <p className="muted small status-updated">
            Updated {new Date(page.updated).toLocaleTimeString()}
            {error ? ` · ${error}` : ""}
            {page.access?.unlocked && (
              <>
                {" · "}
                <button type="button" className="link-button" onClick={forget}>
                  Forget the password on this device
                </button>
              </>
            )}
          </p>
        </>
      )}
    </main>
  );
}

function StatusCard({ s, onWoken }: { s: PublicStatusServer; onWoken: () => void }) {
  const players =
    s.players !== undefined
      ? `${s.players}${s.maxPlayers ? ` / ${s.maxPlayers}` : ""} player${s.players === 1 && !s.maxPlayers ? "" : "s"}`
      : "";
  return (
    <section className="card status-card" aria-labelledby={`status-${s.slug}`}>
      <div className="status-head">
        {s.icon ? (
          <img src={s.icon} alt="" width={48} height={48} className="status-icon" />
        ) : (
          <div className="status-icon" aria-hidden="true">
            🎮
          </div>
        )}
        <div className="grow">
          <h2 id={`status-${s.slug}`}>{s.name}</h2>
          {s.game && s.game !== s.name && <div className="muted small">{s.game}</div>}
        </div>
        <span className={`status-pill ${s.state}`}>
          <span aria-hidden="true">●</span> {STATE_LABEL[s.state]}
        </span>
      </div>
      {s.state === "asleep" && <WakeButton s={s} onWoken={onWoken} />}
      {(players || s.version || s.map) && (
        <div className="small status-facts">
          {players && <span>{players}</span>}
          {s.map && <span>Map: {s.map}</span>}
          {s.version && <span>Version {s.version}</span>}
        </div>
      )}
      {s.playerNames && s.playerNames.length > 0 && (
        <div className="small muted">Playing: {s.playerNames.join(", ")}</div>
      )}
      {s.address && (
        <div className="status-connect">
          <span className="small muted">Connect to</span> <code>{s.address}</code>{" "}
          <CopyButton text={s.address} label={`Copy ${s.name}'s address`} />
        </div>
      )}
      {s.hint && <div className="small muted">{s.hint}</div>}
      {(s.uptime7d !== undefined || s.uptime30d !== undefined) && (
        <div className="small muted">
          Up{" "}
          {[
            s.uptime7d !== undefined && `${pct(s.uptime7d)} of the last 7 days`,
            s.uptime30d !== undefined && `${pct(s.uptime30d)} of 30`,
          ]
            .filter(Boolean)
            .join(", ")}
        </div>
      )}
    </section>
  );
}

/** A sleeping server: why it's off, and a Start button if visitors may use one. */
function WakeButton({ s, onWoken }: { s: PublicStatusServer; onWoken: () => void }) {
  const [state, setState] = useState<"idle" | "busy" | "started">("idle");
  const [error, setError] = useState<string>();

  async function wake() {
    setState("busy");
    setError(undefined);
    try {
      const res = await fetch(`/api/status-page/${encodeURIComponent(s.slug)}/wake`, {
        method: "POST",
        headers: { "X-HLM-Request": "1" },
        credentials: "same-origin",
      });
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? "It couldn't be started.");
      }
      setState("started");
      // Show it starting, then keep checking until it's up.
      onWoken();
      [15, 45, 90].forEach((sec) => setTimeout(onWoken, sec * 1000));
    } catch (e) {
      setError((e as Error).message);
      setState("idle");
    }
  }

  return (
    <div className="status-wake small">
      <span className="muted">Nobody was playing, so it&apos;s asleep.</span>
      {s.canWake && (
        <button className="btn small primary" onClick={wake} disabled={state !== "idle"}>
          {state === "busy" ? "Starting…" : state === "started" ? "Starting: give it a minute or two" : "Start it"}
        </button>
      )}
      {error && <span className="alert error small no-margin">{error}</span>}
    </div>
  );
}

function pct(v: number): string {
  const p = v * 100;
  return `${p >= 99.95 ? "100" : p.toFixed(p >= 99 ? 1 : 0)}%`;
}

/** Asks for the page's view or details password. */
function PasswordForm({ label, compact, onUnlocked }: { label: string; compact?: boolean; onUnlocked: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const res = await fetch("/api/status-page/unlock", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-HLM-Request": "1" },
        credentials: "same-origin",
        body: JSON.stringify({ password }),
      });
      if (res.ok) {
        setPassword("");
        onUnlocked();
      } else if (res.status === 429) setError("Too many tries: wait a moment.");
      else if (res.status === 401) setError("That's not the password.");
      else setError("Couldn't check the password right now.");
    } catch {
      setError("Couldn't check the password right now.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className={`card status-password${compact ? " compact" : ""}`} onSubmit={submit}>
      <label className="field">
        <span className="field-label">{label}</span>
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
          autoFocus={!compact}
          required
        />
      </label>
      {error && (
        <div className="alert error small" role="alert">
          {error}
        </div>
      )}
      <div className="btn-row">
        <button className="btn primary" type="submit" disabled={busy || !password}>
          {busy ? "Checking…" : compact ? "Show" : "Open"}
        </button>
      </div>
    </form>
  );
}
