import { useEffect, useRef, useState } from "react";
import { api, ApiError } from "../api";
import type { WireGuardCheck, WireGuardInput, WireGuardResult, WireGuardStatus } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

/**
 * Sets up a WireGuard tunnel between the proxy server and this Docker host,
 * so nginx can relay to the host directly instead of through the router.
 * `beforeSetup` saves the proxy form (setup uses the saved server);
 * `onChange` runs after the tunnel or the proxy's settings changed.
 */
export function WireGuardTunnel({
  beforeSetup,
  onChange,
  disabled,
  hasServer,
}: {
  beforeSetup: () => Promise<void>;
  onChange: (status: WireGuardStatus, result?: WireGuardResult) => void;
  disabled: boolean;
  /** The proxy server's host is filled in, so the tunnel's proxy end can be set up. */
  hasServer: boolean;
}) {
  const [status, setStatus] = useState<WireGuardStatus>();
  const [input, setInput] = useState<WireGuardInput>({
    network: "",
    listenPort: 51820,
    endpoint: "",
    useForRelays: true,
    lanNetwork: "",
    lanAddress: "",
  });
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<{ message: string; output?: string }>();
  const [note, setNote] = useState<{ text: string; output?: string }>();
  const [check, setCheck] = useState<WireGuardCheck>();
  // The last setup's console: its steps and the output of each, as they
  // happen. It stays up afterwards, until the next setup.
  const [log, setLog] = useState<{ step: boolean; text: string }[]>([]);
  const consoleRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const el = consoleRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [log]);

  const load = (st: WireGuardStatus) => {
    setStatus(st);
    // The endpoint is shown as host:port; the field only takes an override.
    setInput((i) => ({
      ...i,
      network: st.network,
      listenPort: st.listenPort,
      lanNetwork: st.configured ? (st.lanNetwork ?? "") : i.lanNetwork,
      lanAddress: st.configured ? (st.lanAddress ?? "") : i.lanAddress,
    }));
  };

  useEffect(() => {
    api
      .wireGuard()
      .then(load)
      .catch((e) => setError({ message: (e as Error).message }));
  }, []);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNote(undefined);
    try {
      await fn();
    } catch (e) {
      setError({
        message: (e as Error).message,
        output: e instanceof ApiError ? ((e.body?.output as string) || undefined) : undefined,
      });
      // A failed setup may have changed part of it.
      api.wireGuard().then(load, () => {});
    } finally {
      setBusy(undefined);
    }
  }

  const setup = () =>
    run("setup", async () => {
      await beforeSetup();
      setLog([]);
      let r: WireGuardResult | undefined;
      let failure: string | undefined;
      await api.setupWireGuard(input, (e) => {
        if (e.stage === "step" || e.stage === "output") {
          const line = { step: e.stage === "step", text: e.message ?? "" };
          setLog((l) => [...l.slice(-1999), line]);
        } else if (e.stage === "done") r = e.result;
        else if (e.stage === "error") failure = e.message;
      });
      // The console already shows the output, so the error is just the message.
      if (failure) throw new Error(failure);
      if (!r) throw new Error("The setup stopped without finishing; check the tunnel below and set it up again.");
      load(r.status);
      setEditing(false);
      setCheck(undefined);
      let text = `The tunnel is up: the proxy server is ${r.status.proxyAddress} and this host ${r.status.homeAddress}.`;
      if (r.apply && !r.applyError) text += " nginx now relays through it.";
      else if (r.status.inUse && !r.applyError) text += " nginx will relay through it when you apply.";
      if (r.firewall?.error) text += ` The firewall couldn't be updated: ${r.firewall.error}`;
      else if (r.firewall?.changed) text += ` Port ${r.status.listenPort}/udp was opened in the firewall.`;
      setNote({ text });
      if (r.applyError) setError({ message: `The tunnel is up, but applying nginx failed: ${r.applyError}`, output: r.apply?.output });
      onChange(r.status, r);
    });

  const runCheck = () =>
    run("check", async () => {
      setCheck(await api.checkWireGuard());
    });

  const remove = () => {
    if (!confirm("Take the WireGuard tunnel down on both ends and forget its keys?")) return;
    run("remove", async () => {
      const r = await api.removeWireGuard();
      load(r.status);
      setCheck(undefined);
      setNote({ text: "Removed the tunnel." + (r.firewall?.changed ? " Its port was closed in the firewall." : ""), output: r.output || undefined });
      onChange(r.status, r);
    });
  };

  if (!status) return <section className="card">{error ? <div className="alert error">{error.message}</div> : <Spinner />}</section>;

  const form = (
    <div className="field-grid">
      <label className="field">
        <span className="field-label">Tunnel network</span>
        <input value={input.network} onChange={(e) => setInput({ ...input, network: e.target.value })} placeholder="10.77.77.0/24" spellCheck={false} />
        <span className="field-help">A private range not used on either network. The proxy gets its first address, this host the second.</span>
      </label>
      <label className="field">
        <span className="field-label">WireGuard port (UDP)</span>
        <input type="number" value={input.listenPort} onChange={(e) => setInput({ ...input, listenPort: Number(e.target.value) })} min={1} max={65535} />
        <span className="field-help">The proxy server listens on it; open it in its firewall (hlm does for a Linode firewall).</span>
      </label>
      <label className="field">
        <span className="field-label">Proxy server address (optional)</span>
        <input value={input.endpoint} onChange={(e) => setInput({ ...input, endpoint: e.target.value })} placeholder="the SSH host" spellCheck={false} />
        <span className="field-help">Where this host connects to; leave empty to use the proxy server&apos;s host.</span>
      </label>
      <label className="field">
        <span className="field-label">Reach servers with their own LAN address</span>
        <select value={input.lanNetwork} onChange={(e) => setInput({ ...input, lanNetwork: e.target.value })}>
          <option value="">No: only this host&apos;s published ports</option>
          {status?.lanNetworks.map((n) => (
            <option key={n.name} value={n.name}>
              Through {n.name} ({n.driver} {n.subnet}
              {n.parent ? ` on ${n.parent}` : ""})
            </option>
          ))}
        </select>
        <span className="field-help">
          {status?.lanNetworks.length
            ? "For servers on a LAN network, each with its own address: this end joins that network and forwards to each server's own address and port. Published ports still work."
            : "No macvlan or ipvlan networks on the Docker host. Servers with published ports don't need one."}
        </span>
      </label>
      {input.lanNetwork && (
        <label className="field">
          <span className="field-label">This end&apos;s LAN address (optional)</span>
          <input value={input.lanAddress} onChange={(e) => setInput({ ...input, lanAddress: e.target.value })} placeholder="Docker chooses one" spellCheck={false} />
          <span className="field-help">A free address on that network, outside your router&apos;s DHCP range. Game servers see connections coming from it.</span>
        </label>
      )}
    </div>
  );

  const busyLabel = busy === "setup" ? "Setting up… (installing WireGuard can take a few minutes)" : undefined;

  return (
    <section className="card wireguard">
      <h2>4. WireGuard tunnel</h2>
      <p className="muted small">
        Instead of sending traffic to your public address, the proxy can reach this machine over a WireGuard tunnel. This
        host connects out to the proxy server, so nothing has to be forwarded at home, and it works behind carrier-grade NAT.
        hlm installs WireGuard on the proxy server and runs this end in a <code>hlm-wireguard</code> container on the Docker
        host (host networking), then points nginx at this end of the tunnel and the servers&apos; own ports.
      </p>
      {error && (
        <div className="alert error">
          {error.message}
          {error.output && <pre className="op-output">{error.output}</pre>}
        </div>
      )}
      {note && (
        <div className="alert success">
          {note.text}
          {note.output && <pre className="op-output">{note.output}</pre>}
        </div>
      )}
      {log.length > 0 && (
        <pre className="op-output wg-console" ref={consoleRef} aria-live="polite">
          {log.map((l, i) =>
            l.step ? <strong key={i}>{`▸ ${l.text}\n`}</strong> : <span key={i}>{`${l.text}\n`}</span>,
          )}
          {busy === "setup" && <span className="muted">…</span>}
        </pre>
      )}

      {status.configured ? (
        <>
          <div className="server-info">
            <ul>
              <li className="ok">
                Proxy server <code>{status.proxyAddress}</code>, listening on <code>{status.endpoint}</code>
              </li>
              <li className={status.home.running ? "ok" : "bad"}>
                This host <code>{status.homeAddress}</code>:{" "}
                {status.home.running
                  ? "the hlm-wireguard container is running"
                  : status.home.error || `the hlm-wireguard container is ${status.home.state}${status.home.exitCode ? ` (exit code ${status.home.exitCode})` : ""}; see docker logs hlm-wireguard`}
              </li>
              {status.lanNetwork && (
                <li className={status.home.lanAddress ? "ok" : "bad"}>
                  Reaches servers&apos; own addresses on <code>{status.lanNetwork}</code> ({status.lanSubnet})
                  {status.home.lanAddress ? (
                    <>
                      {" "}
                      from <code>{status.home.lanAddress}</code>
                    </>
                  ) : (
                    "; this end has no address there yet"
                  )}
                </li>
              )}
              <li className={status.inUse ? "ok" : "bad"}>
                {status.inUse ? "nginx relays through the tunnel" : "nginx doesn't use the tunnel: choose it as the home address under 3. What to relay"}
              </li>
              {check && (
                <li className={check.handshake && check.reachable !== false ? "ok" : "bad"}>
                  {check.handshake ? `Last handshake ${timeAgo(check.handshake)}` : "The two ends haven't connected yet"}
                  {check.reachable === true && "; the proxy server can reach this host"}
                  {check.reachable === false && "; the proxy server can't ping this host (a firewall here may block it)"}
                </li>
              )}
            </ul>
            {status.configuredAt && <p className="muted small no-margin">Set up {timeAgo(status.configuredAt)}.</p>}
          </div>
          {editing && form}
          <div className="btn-row">
            <button className="btn small" onClick={runCheck} disabled={disabled || !!busy}>
              {busy === "check" ? "Checking…" : "Check tunnel"}
            </button>
            {editing ? (
              <button className="btn small primary" onClick={setup} disabled={disabled || !!busy}>
                {busyLabel ?? "Save & set up again"}
              </button>
            ) : (
              <button className="btn small" onClick={() => setEditing(true)} disabled={disabled || !!busy}>
                Change
              </button>
            )}
            {!editing && (
              <button className="btn small" onClick={setup} disabled={disabled || !!busy} title="Reinstall both ends with the same keys">
                {busyLabel ?? "Set up again"}
              </button>
            )}
            <button className="btn small danger" onClick={remove} disabled={disabled || !!busy}>
              {busy === "remove" ? "Removing…" : "Remove tunnel"}
            </button>
          </div>
        </>
      ) : (
        <>
          {form}
          <label className="radio">
            <input type="checkbox" checked={input.useForRelays} onChange={(e) => setInput({ ...input, useForRelays: e.target.checked })} /> Relay
            through the tunnel once it&apos;s up (makes it the home address and re-applies nginx if it&apos;s applied)
          </label>
          {!hasServer && (
            <p className="muted small no-margin">Enter the proxy server under 2. Proxy server first (and test the connection).</p>
          )}
          <div className="btn-row">
            <button className="btn small primary" onClick={setup} disabled={disabled || !!busy || !hasServer}>
              {busyLabel ?? "Set up tunnel"}
            </button>
          </div>
        </>
      )}
    </section>
  );
}
