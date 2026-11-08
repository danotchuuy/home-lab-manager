import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { TunnelRoute, TunnelRoutes, TunnelRouteState } from "../types";
import { RouteBadge } from "./Hostnames";

/** A saved route as it's sent back when saving the list. */
function asRoute(r: TunnelRouteState): TunnelRoute {
  return {
    hostname: r.hostname,
    zoneId: r.zoneId,
    server: r.server,
    address: r.address,
    port: r.port,
    scheme: r.scheme,
    noTLSVerify: r.noTLSVerify,
    hlm: r.hlm,
  };
}

/**
 * A server's hostnames on the Cloudflare Tunnel: listed, added and removed
 * here as on the Tunnel page (which also routes other web servers and hlm).
 */
export function ServerHostnames({ server }: { server: string }) {
  const [data, setData] = useState<TunnelRoutes>();
  const [error, setError] = useState<string>();
  const [hostname, setHostname] = useState("");
  const [port, setPort] = useState(0);
  const [scheme, setScheme] = useState<"http" | "https">("http");
  // For https: accept the server's certificate without checking it (most are self-signed), as on the Tunnel page.
  const [noTLSVerify, setNoTLSVerify] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.tunnelRoutes().then(setData, (e) => setError((e as Error).message));
  }, [server]);

  const srv = data?.servers.find((s) => s.name === server);
  useEffect(() => {
    // Its web UI's port first, as the Tunnel page does.
    if (srv && !port) {
      setPort(srv.webuiPort ?? srv.ports[0]?.port ?? 0);
      if (srv.webuiScheme === "https") setScheme("https");
    }
  }, [srv, port]);

  if (error && !data)
    return (
      <div className="card">
        <div className="alert error">{error}</div>
      </div>
    );
  if (!data) return null;
  // Only servers with a TCP port can have a web address.
  if (!srv?.ports.length) return null;
  const routes = data.routes.filter((r) => r.server === server);

  async function save(next: TunnelRoute[]) {
    setBusy(true);
    setError(undefined);
    try {
      await api.setTunnelRoutes(next);
      setData(await api.tunnelRoutes());
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }

  async function add(e: FormEvent) {
    e.preventDefault();
    const h = hostname.trim().toLowerCase().replace(/\.+$/, "");
    if (!h || !port || !data) return;
    const route: TunnelRoute = { hostname: h, server, port, scheme, noTLSVerify: scheme === "https" && noTLSVerify };
    if (await save([...data.routes.map(asRoute), route])) setHostname("");
  }

  async function remove(h: string) {
    if (!data || !confirm(`Stop publishing ${h}? Its Cloudflare DNS record is removed too.`)) return;
    await save(data.routes.filter((r) => r.hostname !== h).map(asRoute));
  }

  return (
    <div className="card">
      <div className="settings-actions">
        <div>
          <h2>Web address</h2>
          <p className="muted small no-margin">
            {routes.length
              ? "Hostnames on your domain that the Cloudflare Tunnel sends to this server, over HTTPS."
              : "Publish this server's web interface on your own domain, like panel.example.com, through a Cloudflare Tunnel. Nothing needs forwarding at home."}
          </p>
        </div>
        <Link className="btn small ghost" to="/tunnel">
          {data.tunnel ? "All hostnames" : "Set up Cloudflare Tunnel"}
        </Link>
      </div>
      {routes.length > 0 && (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Hostname</th>
                <th>Goes to</th>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {routes.map((r) => (
                <tr key={r.hostname}>
                  <td>
                    {r.state === "published" ? (
                      <a href={r.url} target="_blank" rel="noreferrer">
                        {r.hostname} ↗
                      </a>
                    ) : (
                      r.hostname
                    )}
                  </td>
                  <td>{r.origin ? <code>{r.origin}</code> : <span className="muted">port {r.port}</span>}</td>
                  <td>
                    <RouteBadge route={r} />
                    {r.error && <div className="small form-error">{r.error}</div>}
                    {r.state === "pending" && !data.tunnel && <div className="muted small">published once the tunnel is set up</div>}
                  </td>
                  <td className="actions">
                    <button type="button" className="btn small ghost danger" onClick={() => remove(r.hostname)} disabled={busy}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {data.tunnel && (
        <form className="input-row hostname-add" onSubmit={add}>
          <input
            value={hostname}
            onChange={(e) => setHostname(e.target.value)}
            placeholder="panel.example.com"
            aria-label="Hostname"
            spellCheck={false}
          />
          <select value={port} onChange={(e) => setPort(Number(e.target.value))} aria-label="Port">
            {srv.ports.map((p) => (
              <option key={p.port} value={p.port}>
                port {p.port}
                {p.container && p.container !== p.port ? ` (container ${p.container})` : ""}
              </option>
            ))}
          </select>
          <select value={scheme} onChange={(e) => setScheme(e.target.value as "http" | "https")} aria-label="Protocol">
            <option value="http">http</option>
            <option value="https">https</option>
          </select>
          {scheme === "https" && (
            <label className="radio small" title="Most servers' own certificates are self-signed">
              <input type="checkbox" checked={noTLSVerify} onChange={(e) => setNoTLSVerify(e.target.checked)} /> Don&apos;t
              verify its certificate
            </label>
          )}
          <button type="submit" className="btn small" disabled={busy || !hostname.trim() || !port}>
            {busy ? "Saving…" : "Add hostname"}
          </button>
        </form>
      )}
      {error && <div className="alert error small">{error}</div>}
    </div>
  );
}
