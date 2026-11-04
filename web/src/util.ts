import type { PortSummary } from "./types";

export function formatBytes(n: number): string {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(Math.floor(Math.log2(n) / 10), units.length - 1);
  return `${(n / 1024 ** i).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function formatCount(n?: number): string {
  if (!n) return "0";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

/**
 * Formats published ports, collapsing consecutive ports into ranges:
 * "2456-2458→2456-2458/udp, 8080→80/tcp".
 */
export function formatPorts(ports: PortSummary[]): string {
  const sorted = [...ports].sort((a, b) => a.Type.localeCompare(b.Type) || a.PrivatePort - b.PrivatePort);
  const runs: { type: string; from: PortSummary; to: PortSummary }[] = [];
  for (const p of sorted) {
    const last = runs[runs.length - 1];
    const continues =
      last &&
      last.type === p.Type &&
      p.PrivatePort === last.to.PrivatePort + 1 &&
      (p.PublicPort ?? 0) === (last.to.PublicPort ? last.to.PublicPort + 1 : 0);
    if (continues) last.to = p;
    else runs.push({ type: p.Type, from: p, to: p });
  }
  const span = (a?: number, b?: number) => (a === b ? `${a}` : `${a}-${b}`);
  return runs
    .map(({ type, from, to }) => {
      const priv = span(from.PrivatePort, to.PrivatePort);
      return from.PublicPort ? `${span(from.PublicPort, to.PublicPort)}→${priv}/${type}` : `${priv}/${type}`;
    })
    .join(", ");
}

/** Describes where a server can be reached, e.g. "games · 172.30.0.5". */
export function formatNetwork(network: string, ip?: string): string {
  return ip ? `${network} · ${ip}` : network;
}

/** Fills in the [IP] placeholder of an Unraid WebUI link with the host we're served from. */
export function webUILink(webui?: string): string | undefined {
  if (!webui) return undefined;
  return webui.replaceAll("[IP]", window.location.hostname);
}

/** Formats a timestamp relative to now, e.g. "5 min ago". */
export function timeAgo(iso?: string): string {
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** Suggests a container name for a new server from the app name. */
export function suggestName(appName: string): string {
  return (
    appName
      .toLowerCase()
      .replace(/[^a-z0-9_.-]+/g, "-")
      .replace(/^[^a-z0-9]+|-+$/g, "") || "server"
  );
}

/** Formats a time in a time zone (hlm's, for schedules), like "Thu 5 Nov 04:00". */
export function formatInZone(iso: string | undefined, timeZone: string, withDate = true): string {
  if (!iso) return "";
  try {
    return new Intl.DateTimeFormat(undefined, {
      timeZone,
      weekday: withDate ? "short" : undefined,
      day: withDate ? "numeric" : undefined,
      month: withDate ? "short" : undefined,
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    }).format(new Date(iso));
  } catch {
    return new Date(iso).toLocaleString();
  }
}

/** How long until a time, like "in 6 h" or "in 12 min". */
export function timeUntil(iso?: string): string {
  if (!iso) return "";
  const s = (new Date(iso).getTime() - Date.now()) / 1000;
  if (s < 60) return "in under a minute";
  if (s < 3600) return `in ${Math.round(s / 60)} min`;
  if (s < 48 * 3600) return `in ${Math.round(s / 3600)} h`;
  return `in ${Math.round(s / 86400)} days`;
}
