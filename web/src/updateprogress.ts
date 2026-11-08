// Summarizes the event stream of updating several servers, for the update
// panel. Pure functions, so they can be tested on their own.
import type { DeployEvent } from "./types";
import { formatBytes } from "./util.ts";

export type ItemState = "updating" | "done" | "failed";

export interface Item {
  name: string;
  state: ItemState;
  message?: string;
  warnings: string[];
  /** Its image download, layer by layer, while it's being pulled. */
  layers: Map<string, { current: number; total: number; done: boolean }>;
  pulling: boolean;
  extracting: boolean;
}

/** Where an image download is, e.g. "Downloading image: 1.2 GiB of 2.9 GiB · 3 of 9 layers (41%)". */
export function pullText(item: Item): string | undefined {
  if (!item.pulling || item.layers.size === 0) return undefined;
  let current = 0;
  let total = 0;
  let done = 0;
  for (const l of item.layers.values()) {
    current += l.current;
    total += l.total;
    if (l.done) done++;
  }
  if (item.extracting && done === item.layers.size) return "Extracting image…";
  const layers = `${done} of ${item.layers.size} layers`;
  if (total > 0) {
    return `Downloading image: ${formatBytes(current)} of ${formatBytes(total)} · ${layers} (${Math.min(100, (current / total) * 100).toFixed(0)}%)`;
  }
  return `Downloading image: ${layers}`;
}

/** Each server's state in an update of several, and how it ended. */
export function summarizeUpdate(events: DeployEvent[]): { items: Item[]; summary?: string; error?: string } {
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
      item = { name: e.target, state: "updating", warnings: [], layers: new Map(), pulling: false, extracting: false };
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
      default: {
        // Pull progress lines have no message: they count towards the
        // download's progress instead.
        const p = e.progress;
        if (e.stage === "pull" && p?.id && !p.status?.startsWith("Pulling from")) {
          let l = item.layers.get(p.id);
          if (!l) {
            l = { current: 0, total: 0, done: false };
            item.layers.set(p.id, l);
          }
          const st = p.status ?? "";
          if (st === "Downloading" && (p.progressDetail?.total ?? 0) > 0) {
            l.current = p.progressDetail?.current ?? 0;
            l.total = p.progressDetail?.total ?? 0;
          } else if (st === "Download complete" || st === "Pull complete" || st === "Already exists") {
            l.done = true;
            if (l.total > 0) l.current = l.total;
          }
          item.extracting = st === "Extracting";
          item.pulling = true;
        } else if (e.message && item.state === "updating") {
          item.message = e.message;
          item.pulling = e.stage === "pull";
        }
      }
    }
  }
  return { items, summary, error };
}
