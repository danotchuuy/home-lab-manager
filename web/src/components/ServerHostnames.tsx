import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { TunnelRoutes } from "../types";
import { RouteBadge } from "./Hostnames";

/** Lists the hostnames the Cloudflare Tunnel publishes for a server, with a link to manage them. */
export function ServerHostnames({ server }: { server: string }) {
  const [data, setData] = useState<TunnelRoutes>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    api.tunnelRoutes().then(setData, (e) => setError((e as Error).message));
  }, [server]);

  if (error) return <div className="card"><div className="alert error">{error}</div></div>;
  if (!data) return null;
  // Only servers with a TCP port can have a web address.
  const srv = data.servers.find((s) => s.name === server);
  if (!srv?.ports.length) return null;
  const routes = data.routes.filter((r) => r.server === server);

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
        <Link className="btn small" to="/tunnel">
          {routes.length ? "Manage hostnames" : data.tunnel ? "Add a hostname" : "Set up Cloudflare Tunnel"}
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
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
