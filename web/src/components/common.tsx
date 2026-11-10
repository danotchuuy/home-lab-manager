import { Fragment, useState } from "react";
import type { CrashLoop, Server, StopReason } from "../types";
import { timeAgo } from "../util";

export function StateBadge({
  state,
  health,
  asleep,
  stop,
}: {
  state: string;
  health?: string;
  /** hlm stopped it because nobody played. */
  asleep?: { since: string; minutes: number };
  /** Why it stopped, if it isn't running. */
  stop?: Pick<Server, "stopReason" | "stoppedAt" | "exitCode">;
}) {
  if (asleep && state !== "running") {
    return (
      <span
        className="badge state-asleep"
        title={`Nobody played for ${asleep.minutes} minutes, so hlm stopped it ${timeAgo(asleep.since)}. It starts again when someone wants to play.`}
      >
        asleep
      </span>
    );
  }
  const why = state !== "running" ? stopReasonText(stop) : undefined;
  if (why) {
    return (
      <span className={`badge stop-reason ${why.className}`} title={why.title}>
        {why.label}
      </span>
    );
  }
  const label = health && state === "running" ? `${state} · ${health}` : state;
  return <span className={`badge state-${state}`}>{label}</span>;
}

/** Whether a stop reason means nobody asked for the server to go down. */
export function stoppedUnexpectedly(reason?: StopReason): boolean {
  return reason === "crashed" || reason === "exited" || reason === "unhealthy";
}

/**
 * How to show why a server stopped: undefined for no reason, a crash loop
 * (which has its own badge) or one hlm didn't see.
 */
export function stopReasonText(
  stop?: Pick<Server, "stopReason" | "stoppedAt" | "exitCode">,
): { label: string; title: string; className: string } | undefined {
  if (!stop?.stopReason) return undefined;
  const when = stop.stoppedAt ? ` ${timeAgo(stop.stoppedAt)}` : "";
  const code = stop.exitCode ? ` (exit code ${stop.exitCode})` : "";
  switch (stop.stopReason) {
    case "stopped":
      return { label: "Stopped", className: "state-exited", title: `Someone stopped it${when}: hlm, a schedule, a person, or the host shutting down.` };
    case "exited":
      return {
        label: "Exited on its own",
        className: "state-restarting",
        title: `It exited cleanly${when}${code} without anyone asking, such as after an in-game shutdown command.`,
      };
    case "crashed":
      return {
        label: "Crashed",
        className: "state-dead",
        title: `It crashed${when}${code}: it exited with an error, or ran out of memory, without anyone asking.`,
      };
    case "unhealthy":
      return {
        label: "Down: failing health",
        className: "state-dead",
        title: `hlm restarted it${when} because its health was failing, and it didn't come back.`,
      };
    default:
      return undefined;
  }
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
