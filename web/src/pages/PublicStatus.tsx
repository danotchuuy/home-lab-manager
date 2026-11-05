import { useEffect, useState } from "react";
import { CopyButton } from "../components/ConnectInfo";
import type { PublicStatusPage, PublicStatusServer } from "../types";

const STATE_LABEL: Record<PublicStatusServer["state"], string> = {
  online: "Online",
  starting: "Starting",
  running: "Up",
  offline: "Offline",
};

/**
 * The public status page at /status: for people without an hlm account. It
 * stands on its own (no header, no sign-in) and only reads /api/status-page.
 */
export function PublicStatus() {
  const [page, setPage] = useState<PublicStatusPage>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const res = await fetch("/api/status-page", { credentials: "omit" });
        if (res.status === 404) setError("There's no status page here.");
        else if (!res.ok) setError("The status isn't available right now.");
        else {
          const p = (await res.json()) as PublicStatusPage;
          setPage(p);
          setError(undefined);
          document.title = p.title;
        }
      } catch {
        setError("The status isn't available right now.");
      }
      timer = setTimeout(load, 30000);
    };
    load();
    return () => clearTimeout(timer);
  }, []);

  return (
    <main className="status-page">
      {error && !page ? (
        <p className="muted">{error}</p>
      ) : !page ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          <h1>{page.title}</h1>
          {page.message && <div className="alert info status-message">{page.message}</div>}
          <div className="status-list">
            {page.servers.map((s) => (
              <StatusCard key={s.slug} s={s} />
            ))}
          </div>
          <p className="muted small status-updated">
            Updated {new Date(page.updated).toLocaleTimeString()}
            {error ? ` · ${error}` : ""}
          </p>
        </>
      )}
    </main>
  );
}

function StatusCard({ s }: { s: PublicStatusServer }) {
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

function pct(v: number): string {
  const p = v * 100;
  return `${p >= 99.95 ? "100" : p.toFixed(p >= 99 ? 1 : 0)}%`;
}
