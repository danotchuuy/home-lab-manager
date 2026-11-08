// Describes a data copy's progress for its progress bar. Pure functions, so
// they can be tested on their own.
import type { CopyProgress } from "./types";
import { formatBytes } from "./util.ts";

/** A rough time left, e.g. "about 3 min". */
export function timeLeft(seconds: number): string {
  if (!isFinite(seconds) || seconds < 0) return "";
  if (seconds < 10) return "a few seconds";
  if (seconds < 60) return `about ${Math.round(seconds / 5) * 5} s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `about ${minutes} min`;
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  return m ? `about ${h} h ${m} min` : `about ${h} h`;
}

/**
 * The bar's value (a percentage, or undefined while the total isn't known)
 * and its lines: what's copied of what, the speed, and what's left.
 */
export function describeCopy(p: CopyProgress): { percent?: number; copied: string; speed: string; left: string } {
  const total = p.total && p.total > 0 ? Math.max(p.total, p.bytes) : undefined;
  const files = `${p.files} file${p.files === 1 ? "" : "s"}`;
  const speed = p.rate > 0 ? `${formatBytes(p.rate)}/s` : "";
  if (!total) {
    return { copied: `${formatBytes(p.bytes)} copied (${files})`, speed, left: "Working out how much there is…" };
  }
  const remaining = total - p.bytes;
  const eta = p.rate > 0 ? timeLeft(remaining / p.rate) : "";
  return {
    percent: (p.bytes / total) * 100,
    copied: `${formatBytes(p.bytes)} of ${formatBytes(total)} (${files})`,
    speed,
    left: `${formatBytes(remaining)} to go${eta ? ` · ${eta} left` : ""}`,
  };
}
