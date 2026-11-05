import { Fragment, useState } from "react";
import type { CrashLoop } from "../types";
import { timeAgo } from "../util";

export function StateBadge({ state, health }: { state: string; health?: string }) {
  const label = health && state === "running" ? `${state} · ${health}` : state;
  return <span className={`badge state-${state}`}>{label}</span>;
}

/** Docker's health check state, for a running server that has one. */
export function HealthBadge({ state, health }: { state: string; health?: string }) {
  if (!health || state !== "running") return null;
  const text: Record<string, string> = {
    healthy: "Its health check is passing",
    unhealthy: "Its health check is failing",
    starting: "Its health check hasn't passed yet since it started",
  };
  return (
    <span className={`badge health-${health}`} title={text[health] ?? `Health: ${health}`}>
      {health}
    </span>
  );
}

/** Shown when hlm stopped a server for a crash loop. */
export function CrashLoopBadge({ crashLoop }: { crashLoop?: CrashLoop }) {
  if (!crashLoop) return null;
  return (
    <span
      className="badge crash-loop"
      title={`It exited (or had to be restarted) ${crashLoop.exits} times in ${crashLoop.windowMinutes} minutes (last: ${crashLoop.reason ?? "unknown"}), so hlm stopped it ${timeAgo(crashLoop.since)}. It stays stopped until someone starts it.`}
    >
      Crash loop: stopped by hlm
    </span>
  );
}

/** An app's icon; `glyph` is shown instead when there's no image (🎮 by default). */
export function AppIcon({ src, size = 48, glyph = "🎮" }: { src?: string; size?: number; glyph?: string }) {
  const [failed, setFailed] = useState(false);
  if (!src || failed) {
    return (
      <div className="app-icon placeholder" style={{ width: size, height: size }}>
        {glyph}
      </div>
    );
  }
  return (
    <img
      className="app-icon"
      src={src}
      width={size}
      height={size}
      alt=""
      loading="lazy"
      referrerPolicy="no-referrer"
      onError={() => setFailed(true)}
    />
  );
}

/**
 * Renders template overview text. Templates use **bold** markers and blank
 * lines; anything else is shown as plain text.
 */
export function Overview({ text }: { text?: string }) {
  if (!text) return null;
  return (
    <div className="overview">
      {text.split(/\n{2,}/).map((para, i) => (
        <p key={i}>
          {para.split(/\*\*(.+?)\*\*/g).map((part, j) =>
            j % 2 === 1 ? <strong key={j}>{part}</strong> : <Fragment key={j}>{part}</Fragment>,
          )}
        </p>
      ))}
    </div>
  );
}

export function Spinner() {
  return <span className="spinner" aria-label="Loading" />;
}

/** Icons for the containers hlm runs for connections, by kind. */
export const CONNECTION_GLYPH: Record<string, string> = { wireguard: "🔒", cloudflared: "☁️", hlm: "🎛️" };

/** A time as "5 min ago", with the exact time on hover (and for screen readers, in the element). */
export function Ago({ iso, never = "never" }: { iso?: string; never?: string }) {
  if (!iso) return <>{never}</>;
  const d = new Date(iso);
  return (
    <time dateTime={iso} title={d.toLocaleString()}>
      {timeAgo(iso)}
    </time>
  );
}
