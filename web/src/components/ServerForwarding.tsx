import { useState } from "react";
import { useAdmin } from "../access";
import { PortForwarding } from "./PortForwarding";
import { PortSettings } from "./PortSettings";
import { ServerHostnames } from "./ServerHostnames";

/**
 * Everything about how one server is reached from the internet: its ports
 * (internet ports, and whether UniFi forwards, the proxy relays and its
 * firewall opens each), its UniFi rules and its web addresses. The same on
 * the server's Port forwarding tab, the Proxy page and under System → UniFi.
 */
export function ServerForwarding({
  server,
  canManage,
  onSaved,
  hostnames = true,
}: {
  server: string;
  canManage: boolean;
  /** Called after its port settings (and relay) are saved, or its rules changed. */
  onSaved?: () => void;
  /** Show its web addresses (Cloudflare Tunnel hostnames). */
  hostnames?: boolean;
}) {
  const admin = useAdmin();
  const [portsVersion, setPortsVersion] = useState(0);
  // Bumped when the UniFi rules change, so the port table's states refresh.
  const [rulesVersion, setRulesVersion] = useState(0);
  // The UniFi rules card is of no use through the WireGuard tunnel, unless rules are left over.
  const [showForwarding, setShowForwarding] = useState(true);
  return (
    <>
      <fieldset className="plain-fieldset" disabled={!canManage}>
        <div id="ports">
        <PortSettings
          server={server}
          rulesVersion={rulesVersion}
          onSaved={() => {
            setPortsVersion((v) => v + 1);
            onSaved?.();
          }}
          onForwardingNeeded={setShowForwarding}
        />
        </div>
        {showForwarding && (
          <div id="unifi-rules">
          <PortForwarding
            key={portsVersion}
            server={server}
            onChanged={() => {
              setRulesVersion((v) => v + 1);
              onSaved?.();
            }}
          />
          </div>
        )}
      </fieldset>
      {admin && hostnames && (
        <div id="web-address">
          <ServerHostnames server={server} />
        </div>
      )}
    </>
  );
}
