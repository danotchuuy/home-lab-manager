import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { FirewallStatus } from "../types";
import { Spinner } from "./common";
import { FirewallRulesLine } from "./LinodeSettings";

/**
 * Shows whether the proxy server's Linode firewall is open for the relays.
 * It's set up under System; `version` changes after the relays are applied
 * or removed, which also updates the firewall, so the status is reloaded.
 */
export function ProxyFirewall({ version }: { version: number }) {
  const [status, setStatus] = useState<FirewallStatus>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const load = useCallback(async () => {
    try {
      setStatus(await api.linode());
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load, version]);

  async function sync() {
    setBusy(true);
    setError(undefined);
    try {
      setStatus(await api.syncProxyFirewall());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!status) return <section className="card">{error ? <div className="alert error">{error}</div> : <Spinner />}</section>;

  return (
    <section className="card proxy-firewall">
      <h2>6. Firewall</h2>
      {!status.firewallId ? (
        <p className="muted small no-margin">
          If the proxy server is a Linode with a Cloud Firewall, hlm can open the relayed ports in it when you apply and
          close them when you remove the relays (and keep the WireGuard tunnel&apos;s port open while it&apos;s set up).{" "}
          <Link to="/system#linode">{status.configured ? "Choose the firewall" : "Connect Linode"}</Link> under System.
        </p>
      ) : (
        <>
          <div className="server-info">
            <ul>
              <li className={status.firewallState && status.firewallState !== "enabled" ? "bad" : "ok"}>
                Linode firewall <strong>{status.firewallLabel ?? `#${status.firewallId}`}</strong>
                {status.firewallState && status.firewallState !== "enabled" && ` is ${status.firewallState}`}
              </li>
              <FirewallRulesLine status={status} />
            </ul>
          </div>
          {error && <div className="alert error">{error}</div>}
          <div className="btn-row">
            {!status.inSync && !status.error && (
              <button className="btn small primary" onClick={sync} disabled={busy}>
                {busy ? "Updating…" : "Update firewall"}
              </button>
            )}
            <Link className="btn small" to="/system#linode">
              Linode settings
            </Link>
          </div>
        </>
      )}
    </section>
  );
}
