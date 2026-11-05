// The server list's search, filters and order, kept in the page's URL so a
// view can be bookmarked. Pure functions, so they can be tested on their own.
import type { GameSummary, Server, Stats } from "./types";

export type StateFilter = "" | "running" | "stopped" | "problem" | "update";
export type SortKey = "name" | "state" | "cpu" | "memory" | "players";

export interface ListView {
  q: string;
  state: StateFilter;
  group: string;
  tag: string;
  host: string;
  sort: SortKey;
}

export const DEFAULT_VIEW: ListView = { q: "", state: "", group: "", tag: "", host: "", sort: "name" };

const STATES: StateFilter[] = ["", "running", "stopped", "problem", "update"];
const SORTS: SortKey[] = ["name", "state", "cpu", "memory", "players"];

export function viewFromParams(p: URLSearchParams): ListView {
  const state = (p.get("state") ?? "") as StateFilter;
  const sort = (p.get("sort") ?? "name") as SortKey;
  return {
    q: p.get("q") ?? "",
    state: STATES.includes(state) ? state : "",
    group: p.get("group") ?? "",
    tag: p.get("tag") ?? "",
    host: p.get("host") ?? "",
    sort: SORTS.includes(sort) ? sort : "name",
  };
}

/** The view as query parameters, leaving out what's the default. */
export function viewToParams(v: ListView): URLSearchParams {
  const p = new URLSearchParams();
  for (const k of Object.keys(DEFAULT_VIEW) as (keyof ListView)[]) {
    if (v[k] && v[k] !== DEFAULT_VIEW[k]) p.set(k, v[k]);
  }
  return p;
}

const running = (s: Server) => s.state === "running" || s.state === "restarting";

/** Something's wrong: a crash loop, a failing health check, or a server that died. */
export function hasProblem(s: Server): boolean {
  return !!s.crashLoop || s.health === "unhealthy" || s.state === "dead" || s.state === "restarting";
}

/** Whether a server is in the view: every word of the search in its name, app, image, tags, host or address. */
export function matches(s: Server, v: ListView): boolean {
  const words = v.q.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length > 0) {
    const ports = s.ports.map((p) => `${p.PublicPort ?? ""} ${p.PrivatePort}`).join(" ");
    const text = [s.name, s.app, s.image, s.host, s.ipAddress, s.network, ports, ...(s.tags ?? [])]
      .filter(Boolean)
      .join(" ")
      .toLowerCase();
    if (!words.every((w) => text.includes(w))) return false;
  }
  switch (v.state) {
    case "running":
      if (!running(s)) return false;
      break;
    case "stopped":
      if (running(s)) return false;
      break;
    case "problem":
      if (!hasProblem(s)) return false;
      break;
    case "update":
      if (s.update?.state !== "available") return false;
      break;
  }
  if (v.group && !s.groups?.includes(v.group)) return false;
  if (v.tag && !s.tags?.some((t) => t.toLowerCase() === v.tag.toLowerCase())) return false;
  if (v.host && (s.host || "local") !== v.host) return false;
  return true;
}

export interface Usage {
  stats: Record<string, Stats | undefined>; // by server ID
  games: Record<string, GameSummary | undefined>; // by server name
}

const STATE_ORDER: Record<string, number> = { running: 0, restarting: 1, paused: 2, created: 3, exited: 4, dead: 5 };

/** The servers in the view's order, favourites first. Ties go by name. */
export function sortServers(list: Server[], v: ListView, u: Usage): Server[] {
  const value = (s: Server): number => {
    switch (v.sort) {
      case "state":
        return STATE_ORDER[s.state] ?? 9;
      case "cpu":
        return -(u.stats[s.id]?.cpuPercent ?? -1);
      case "memory":
        return -(u.stats[s.id]?.memoryUsage ?? -1);
      case "players":
        return -(u.games[s.name]?.players ?? -1);
      default:
        return 0;
    }
  };
  return [...list].sort(
    (a, b) => Number(!!b.favourite) - Number(!!a.favourite) || value(a) - value(b) || a.name.localeCompare(b.name),
  );
}

/** Every tag in use, sorted, each once whatever its case. */
export function allTags(list: Server[]): string[] {
  const seen = new Map<string, string>();
  for (const s of list) for (const t of s.tags ?? []) if (!seen.has(t.toLowerCase())) seen.set(t.toLowerCase(), t);
  return [...seen.values()].sort((a, b) => a.localeCompare(b));
}
