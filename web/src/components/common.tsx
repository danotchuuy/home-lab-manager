import { Fragment, useState } from "react";

export function StateBadge({ state, health }: { state: string; health?: string }) {
  const label = health && state === "running" ? `${state} · ${health}` : state;
  return <span className={`badge state-${state}`}>{label}</span>;
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
