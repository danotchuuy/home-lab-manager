import { useEffect, useState } from "react";
import { api } from "../api";
import type { CloudflareZone, TunnelApply, TunnelRoute, TunnelRouteState, TunnelRoutes, WebPort } from "../types";
import { Spinner } from "./common";

/** A route as it's edited: the hostname split into its subdomain and zone. */
interface Row {
  key: number;
  sub: string;
  zoneId: string;
  server: string;
  /** Goes to another web server at `address` rather than a game server. */
  other: boolean;
  /** Goes to Home Lab Manager itself (its status page sites). */
  hlm: boolean;
  address: string;
  port: number;
  scheme: "http" | "https";
  noTLSVerify: boolean;
  /** The saved route's state, until the row is changed. */
  saved?: TunnelRouteState;
}

let nextKey = 0;

/** The server menu's choice for another web server. */
const OTHER = "\u0000other";
/** The server menu's choice for hlm itself. */
const HLM = "\u0000hlm";

function toRow(r: TunnelRouteState, zones: CloudflareZone[]): Row {
  const zone = zones.find((z) => z.id === r.zoneId) ?? zones.find((z) => r.hostname.endsWith(`.${z.name}`));
  const sub = zone ? (r.hostname === zone.name ? "" : r.hostname.slice(0, -zone.name.length - 1)) : r.hostname;
  return {
    key: nextKey++,
    sub,
    zoneId: zone?.id ?? "",
    server: r.server ?? "",
    other: !!r.address,
    hlm: !!r.hlm,
    address: r.address ?? "",
    port: r.port,
    scheme: r.scheme,
    noTLSVerify: !!r.noTLSVerify,
    saved: r,
  };
}

function hostname(row: Row, zones: CloudflareZone[]): string {
  const zone = zones.find((z) => z.id === row.zoneId)?.name ?? "";
  const sub = row.sub.trim().toLowerCase().replace(/\.+$/, "");
  return sub && zone ? `${sub}.${zone}` : sub || zone;
}

function toRoute(row: Row, zones: CloudflareZone[]): TunnelRoute {
  if (row.hlm) return { hostname: hostname(row, zones), zoneId: row.zoneId, hlm: true, port: 0, scheme: "http" };
  return {
    hostname: hostname(row, zones),
    zoneId: row.zoneId,
    ...(row.other ? { address: row.address.trim() } : { server: row.server }),
    port: row.port,
    scheme: row.scheme,
    noTLSVerify: row.scheme === "https" && row.noTLSVerify,
  };
}

/** The port a new route to a server starts with: its web UI's, or its first TCP port. */
function defaultPort(srv?: TunnelRoutes["servers"][number]): { port: number; scheme: "http" | "https" } {
  if (!srv) return { port: 0, scheme: "http" };
  const webui = srv.ports.find((p) => p.port === srv.webuiPort);
  const port = webui?.port ?? srv.ports[0]?.port ?? 0;
  return { port, scheme: webui && srv.webuiScheme === "https" ? "https" : "http" };
}

export function portLabel(p: WebPort, webuiPort?: number): string {
  let s = String(p.port);
  if (p.container && p.container !== p.port) s += ` (container ${p.container})`;
  if (p.hostIp) s += ` on ${p.hostIp}`;
  if (p.port === webuiPort) s += " · web UI";
  return s;
}

export function RouteBadge({ route }: { route: TunnelRouteState }) {
  switch (route.state) {
    case "published":
      return <span className="badge state-running">published</span>;
    case "pending":
      return <span className="badge state-restarting">not yet</span>;
    case "conflict":
      return <span className="badge state-dead">conflict</span>;
    default:
      return <span className="badge state-dead">error</span>;
  }
}

/** Says what publishing did. */
export function applyNote(r: TunnelApply, tunnel: boolean): string {
  if (!tunnel) return "Saved. The hostnames are published once the tunnel is set up.";
  const parts = [];
  if (r.created) parts.push(`added ${r.created} DNS record${r.created === 1 ? "" : "s"}`);
  if (r.updated) parts.push(`updated ${r.updated}`);
  if (r.removed) parts.push(`removed ${r.removed}`);
  const problems = r.routes.filter((x) => x.state === "conflict" || x.state === "error").length;
  let text = `Published${parts.length ? `: ${parts.join(", ")}` : ""}.`;
  if (problems) text += ` ${problems} hostname${problems === 1 ? " has a problem" : "s have problems"}; see below.`;
  return text;
}

/**
 * Edits the hostnames the tunnel publishes. `zones` are the account's
 * domains; `onChange` runs after the routes were saved.
 */
export function HostnamesEditor({
  zones,
  onChange,
}: {
  zones: CloudflareZone[];
  onChange?: (routes: TunnelRoutes) => void;
}) {
  const [data, setData] = useState<TunnelRoutes>();
  const [rows, setRows] = useState<Row[]>([]);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const load = async () => {
    const d = await api.tunnelRoutes();
    setData(d);
    setRows(d.routes.map((r) => toRow(r, zones)));
    setDirty(false);
    return d;
  };

  useEffect(() => {
    load().catch((e) => setError((e as Error).message));
    // Zones arrive with the tunnel's status; rows need them to split hostnames.
  }, [zones]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const save = () =>
    run("save", async () => {
      const r = await api.setTunnelRoutes(rows.map((row) => toRoute(row, zones)));
      const d = await load();
      setNotice(applyNote(r, d.tunnel));
      onChange?.(d);
    });

  const publish = () =>
    run("apply", async () => {
      const r = await api.applyTunnel();
      const d = await load();
      setNotice(applyNote(r, d.tunnel));
      onChange?.(d);
    });

  if (!data) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const update = (key: number, change: Partial<Row>) => {
    setRows((rs) => rs.map((r) => (r.key === key ? { ...r, ...change, saved: undefined } : r)));
    setDirty(true);
  };
  const add = () => {
    const srv = data.servers.find((s) => s.ports.length > 0);
    // Without game servers to choose, start with another server's address.
    const target = srv ? defaultPort(srv) : { port: 80, scheme: "http" as const };
    setRows((rs) => [
      ...rs,
      { key: nextKey++, sub: "", zoneId: zones[0]?.id ?? "", server: srv?.name ?? "", other: !srv, hlm: false, address: "", ...target, noTLSVerify: false },
    ]);
    setDirty(true);
  };
  const remove = (key: number) => {
    setRows((rs) => rs.filter((r) => r.key !== key));
    setDirty(true);
  };

  const withPorts = data.servers.filter((s) => s.ports.length > 0);

  return (
    <div className="hostnames">
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}
      {rows.length === 0 ? (
        <p className="muted small">No hostnames yet.</p>
      ) : (
        <div className="table-scroll">
          <table className="data-table hostnames-table">
            <thead>
              <tr>
                <th>Hostname</th>
                <th>Server</th>
                <th>Port</th>
                <th>Protocol</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const srv = data.servers.find((s) => s.name === row.server);
                const ports = srv?.ports ?? [];
                return (
                  <tr key={row.key}>
                    <td>
                      <div className="hostname-input">
                        <input
                          value={row.sub}
                          onChange={(e) => update(row.key, { sub: e.target.value })}
                          placeholder={row.saved ? "(none)" : "panel"}
                          title="Leave empty to use the domain itself"
                          aria-label="Subdomain"
                          spellCheck={false}
                        />
                        <span className="muted">.</span>
                        <select value={row.zoneId} onChange={(e) => update(row.key, { zoneId: e.target.value })} aria-label="Domain">
                          {!zones.some((z) => z.id === row.zoneId) && <option value={row.zoneId}>{row.saved?.zone ?? "choose…"}</option>}
                          {zones.map((z) => (
                            <option key={z.id} value={z.id}>
                              {z.name}
                              {z.status !== "active" ? ` (${z.status})` : ""}
                            </option>
                          ))}
                        </select>
                      </div>
                      {row.saved?.state === "published" && (
                        <a className="small" href={row.saved.url} target="_blank" rel="noreferrer">
                          {row.saved.url} ↗
                        </a>
                      )}
                    </td>
                    <td>
                      <select
                        value={row.hlm ? HLM : row.other ? OTHER : row.server}
                        onChange={(e) => {
                          if (e.target.value === HLM) {
                            update(row.key, { hlm: true, other: false, port: 0, scheme: "http" });
                            return;
                          }
                          if (e.target.value === OTHER) {
                            update(row.key, { hlm: false, other: true, port: row.other ? row.port : 80, scheme: row.other ? row.scheme : "http" });
                            return;
                          }
                          const next = data.servers.find((s) => s.name === e.target.value);
                          update(row.key, { hlm: false, other: false, server: e.target.value, ...defaultPort(next) });
                        }}
                        aria-label="Server"
                      >
                        {!row.other && !row.hlm && !srv && <option value={row.server}>{row.server || "choose…"}</option>}
                        {withPorts.map((s) => (
                          <option key={s.name} value={s.name}>
                            {s.name}
                            {s.running ? "" : " (stopped)"}
                          </option>
                        ))}
                        <option value={OTHER}>Other server…</option>
                        <option value={HLM}>Home Lab Manager (status page)</option>
                      </select>
                      {row.other && (
                        <input
                          className="address-input"
                          value={row.address}
                          onChange={(e) => update(row.key, { address: e.target.value })}
                          placeholder="192.168.1.20 or nas.lan"
                          aria-label="Server address"
                          spellCheck={false}
                        />
                      )}
                    </td>
                    <td>
                      {row.hlm ? (
                        <span className="muted small">hlm&apos;s own</span>
                      ) : row.other ? (
                        <input
                          type="number"
                          className="port-input"
                          value={row.port || ""}
                          onChange={(e) => update(row.key, { port: Number(e.target.value) })}
                          min={1}
                          max={65535}
                          placeholder="80"
                          aria-label="Port"
                        />
                      ) : (
                        <select value={row.port} onChange={(e) => update(row.key, { port: Number(e.target.value) })} aria-label="Port">
                          {!ports.some((p) => p.port === row.port) && <option value={row.port}>{row.port || "–"}</option>}
                          {ports.map((p) => (
                            <option key={p.port} value={p.port}>
                              {portLabel(p, srv?.webuiPort)}
                            </option>
                          ))}
                        </select>
                      )}
                    </td>
                    <td>
                      {row.hlm ? (
                        <span className="muted small">automatic</span>
                      ) : (
                      <select
                        value={row.scheme}
                        onChange={(e) => update(row.key, { scheme: e.target.value as Row["scheme"] })}
                        aria-label="Protocol"
                      >
                        <option value="http">HTTP</option>
                        <option value="https">HTTPS</option>
                      </select>
                      )}
                      {!row.hlm && row.scheme === "https" && (
                        <label className="radio small">
                          <input
                            type="checkbox"
                            checked={row.noTLSVerify}
                            onChange={(e) => update(row.key, { noTLSVerify: e.target.checked })}
                          />{" "}
                          self-signed
                        </label>
                      )}
                    </td>
                    <td>
                      {row.saved ? <RouteBadge route={row.saved} /> : <span className="badge">unsaved</span>}
                      {row.saved?.origin && (
                        <div className="muted small">
                          → <code>{row.saved.origin}</code>
                        </div>
                      )}
                      {row.saved?.error && <div className="small form-error">{row.saved.error}</div>}
                    </td>
                    <td>
                      <button className="btn small ghost" onClick={() => remove(row.key)} disabled={!!busy} aria-label="Remove hostname">
                        ✕
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {withPorts.length === 0 && (
        <p className="muted small">
          None of your game servers has a TCP port to publish, but hostnames can still go to other web servers by address.
        </p>
      )}
      <div className="btn-row">
        <button className="btn small" onClick={add} disabled={!!busy || zones.length === 0}>
          Add hostname
        </button>
        <button className="btn small primary" onClick={save} disabled={!!busy || !dirty}>
          {busy === "save" ? "Saving…" : data.tunnel ? "Save & publish" : "Save"}
        </button>
        {dirty && (
          <button className="btn small ghost" onClick={() => load()} disabled={!!busy}>
            Discard changes
          </button>
        )}
        {!dirty && data.tunnel && data.pending && (
          <button className="btn small primary" onClick={publish} disabled={!!busy}>
            {busy === "apply" ? "Publishing…" : "Publish changes"}
          </button>
        )}
      </div>
    </div>
  );
}
