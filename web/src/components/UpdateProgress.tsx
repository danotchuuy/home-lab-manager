import { useMemo } from "react";
import type { DeployEvent } from "../types";
import { pullText, summarizeUpdate } from "../updateprogress";

/** Shows the progress of updating several servers from its event stream. */
export function UpdateProgress({
  events,
  running,
  title,
  onClose,
}: {
  events: DeployEvent[];
  running: boolean;
  /** Defaults to an update's. */
  title?: string;
  onClose?: () => void;
}) {
  const { items, summary, error } = useMemo(() => summarizeUpdate(events), [events]);

  return (
    <div className="card update-progress">
      <div className="section-head">
        <h2>{title ?? (running ? "Updating servers…" : "Update finished")}</h2>
        {onClose && (
          <button type="button" className="icon-button" aria-label="Close" onClick={onClose}>
            ×
          </button>
        )}
      </div>
      <ul className="update-items" aria-live="polite">
        {items.map((item) => (
          <li key={item.name} className={`update-item ${item.state}`}>
            <span className="update-icon" aria-hidden="true">
              {item.state === "done" ? "✓" : item.state === "failed" ? "✕" : "…"}
            </span>
            <span className="update-name">{item.name}</span>
            <span className="muted small update-message">
              {(item.state === "updating" && pullText(item)) || item.message}
            </span>
            {item.warnings.map((w, i) => (
              <span key={i} className="update-warning small">
                ⚠ {w}
              </span>
            ))}
          </li>
        ))}
      </ul>
      {summary && <div className="alert success">{summary}</div>}
      {error && <div className="alert error">{error}</div>}
    </div>
  );
}
