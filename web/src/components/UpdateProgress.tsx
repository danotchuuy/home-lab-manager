import { useMemo } from "react";
import type { DeployEvent } from "../types";

type ItemState = "updating" | "done" | "failed";

interface Item {
  name: string;
  state: ItemState;
  message?: string;
  warnings: string[];
}

/** Shows the progress of updating several servers from its event stream. */
export function UpdateProgress({ events, running }: { events: DeployEvent[]; running: boolean }) {
  const { items, summary, error } = useMemo(() => {
    const items: Item[] = [];
    const byName = new Map<string, Item>();
    let summary: string | undefined;
    let error: string | undefined;
    for (const e of events) {
      if (!e.target) {
        if (e.stage === "summary") summary = e.message;
        if (e.stage === "error") error = e.message;
        continue;
      }
      let item = byName.get(e.target);
      if (!item) {
        item = { name: e.target, state: "updating", warnings: [] };
        byName.set(e.target, item);
        items.push(item);
      }
      switch (e.stage) {
        case "done":
          item.state = "done";
          item.message = e.message ?? "Updated";
          break;
        case "failed":
          item.state = "failed";
          item.message = e.message;
          break;
        case "warning":
          if (e.message) item.warnings.push(e.message);
          break;
        default:
          // Pull progress lines have no message; keep the last step's.
          if (e.message && item.state === "updating") item.message = e.message;
      }
    }
    return { items, summary, error };
  }, [events]);

  return (
    <div className="card update-progress">
      <h2>{running ? "Updating servers…" : "Update finished"}</h2>
      <ul className="update-items">
        {items.map((item) => (
          <li key={item.name} className={`update-item ${item.state}`}>
            <span className="update-icon" aria-hidden="true">
              {item.state === "done" ? "✓" : item.state === "failed" ? "✕" : "…"}
            </span>
            <span className="update-name">{item.name}</span>
            <span className="muted small update-message">{item.message}</span>
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
