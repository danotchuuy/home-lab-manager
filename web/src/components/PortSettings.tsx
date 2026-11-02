import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api } from "../api";
import type { InternetPort, PortSetting, PortSync, ServerPorts, SyncState } from "../types";
import { Spinner } from "./common";

const keyOf = (p: InternetPort) => `${p.local}/${p.proto}`;

/** A port's options as edited: the internet port as typed, and the three switches. */
interface Draft {
  port: string;
  forward: boolean;
  proxy: boolean;
  firewall: boolean;
}

type Flag = "forward" | "proxy" | "firewall";

const SYNC_LABEL: Record<string, Record<SyncState["state"], string>> = {
  unifi: { synced: "forwarded", pending: "not yet", removing: "still forwarded", conflict: "conflict", off: "" },
  proxy: { synced: "relayed", pending: "not yet", removing: "still relayed", conflict: "conflict", off: "" },
  firewall: { synced: "open", pending: "not yet", removing: "still open", conflict: "conflict", off: "" },
};

/** A port's state in one integration, under its checkbox. */
function SyncBadge({ kind, state }: { kind: keyof typeof SYNC_LABEL; state?: SyncState }) {
  if (!state || !SYNC_LABEL[kind][state.state]) return null;
  return (
    <span className={`sync-badge sync-${state.state}`} title={state.detail}>
      {SYNC_LABEL[kind][state.state]}
    </span>
  );
}

/** One line on how an integration's ports compare with what's in place. */
function syncSummary(name: string, states: Record<string, SyncState> | null, error: string | undefined, fix: string) {
  if (error) return { ok: false, text: `${name}: couldn't check (${error})` };
  if (!states) return null;
  const list = Object.values(states);
  const waiting = list.filter((s) => s.state === "pending" || s.state === "removing").length;
  const conflicts = list.filter((s) => s.state === "conflict").length;
  const parts: string[] = [];
  if (waiting) parts.push(`${waiting} change${waiting === 1 ? "" : "s"} waiting for ${fix}`);
  if (conflicts) parts.push(`${conflicts} conflict${conflicts === 1 ? "" : "s"}`);
  if (parts.length) return { ok: false, text: `${name}: ${parts.join(", ")}` };
  return { ok: true, text: list.some((s) => s.state === "synced") ? `${name}: in sync` : `${name}: no ports included` };
}

function draftsOf(ports: InternetPort[]): Record<string, Draft> {
  return Object.fromEntries(
    ports.map((p) => [
      keyOf(p),
      { port: String(p.port), forward: p.forward ?? true, proxy: p.proxy ?? true, firewall: p.firewall ?? true },
    ]),
  );
}

/**
 * Everything about how a server is reached from the internet: each port's
 * internet port, and whether UniFi forwards it, the nginx proxy relays it
 * and the proxy's Linode firewall opens it. `onSaved` lets the UniFi rules
 * below reload.
 */
export function PortSettings({
  server,
  rulesVersion,
  onSaved,
  onForwardingNeeded,
}: {
  server: string;
  rulesVersion: number;
  onSaved: () => void;
  /** Called with whether the UniFi rules card below is of use: not through the tunnel, unless rules are left over. */
  onForwardingNeeded?: (needed: boolean) => void;
}) {
  const admin = useAdmin();
  const [info, setInfo] = useState<ServerPorts>();
  const [draft, setDraft] = useState<Record<string, Draft>>({});
  const [relay, setRelay] = useState(false);
  const [shift, setShift] = useState("");
  const [busy, setBusy] = useState<"save" | "apply">();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  // Set after saving changes that only reach the proxy server once applied.
  const [needsApply, setNeedsApply] = useState(false);
  // Whether each port is in place on UniFi, the proxy and the firewall.
  const [sync, setSync] = useState<PortSync>();
  const [checking, setChecking] = useState(false);

  const loadSync = useCallback(async () => {
    setChecking(true);
    try {
      setSync(await api.portSync(server));
    } catch {
      // The table still works without it.
    } finally {
      setChecking(false);
    }
  }, [server]);

  useEffect(() => {
    loadSync();
  }, [loadSync, rulesVersion]);

  const show = useCallback((s: ServerPorts) => {
    setInfo(s);
    setDraft(draftsOf(s.ports));
    setRelay(s.proxy.relayed);
  }, []);

  useEffect(() => {
    api
      .serverPorts(server)
      .then(show)
      .catch((e) => setError((e as Error).message));
  }, [server, show]);

  // Through the WireGuard tunnel players come in via the proxy, so the
  // gateway forwards nothing; UniFi only shows up to clear old rules.
  const route = info?.proxy.route;
  const tunnel = !!info?.proxy.configured && relay && route?.source === "wireguard";
  const leftoverRules = Object.values(sync?.unifi ?? {}).some((s) => s.state === "synced" || s.state === "removing");
  const showUnifi = !!info?.unifi && (!tunnel || leftoverRules);
  useEffect(() => {
    if (info) onForwardingNeeded?.(!tunnel || leftoverRules);
  }, [info, tunnel, leftoverRules, onForwardingNeeded]);

  if (!info) return <div className="card">{error ? <div className="alert error">{error}</div> : <Spinner />}</div>;
  if (info.ports.length === 0) {
    return (
      <div className="card">
        <h2>Ports</h2>
        <p className="muted small no-margin">This server doesn&apos;t publish any ports.</p>
      </div>
    );
  }

  const ports = info.ports;
  const saved = draftsOf(ports);
  const changed = relay !== info.proxy.relayed || ports.some((p) => JSON.stringify(draft[keyOf(p)]) !== JSON.stringify(saved[keyOf(p)]));
  const moved = ports.some((p) => p.port !== p.local);
  const proxyOn = info.proxy.configured && relay;
  // Through the tunnel to the server's own LAN address (LAN mode) rather than this host.
  const lanTarget = tunnel && !!route?.serverAddress;

  const set = (key: string, change: Partial<Draft>) => setDraft((d) => ({ ...d, [key]: { ...d[key], ...change } }));
  const setAll = (flag: Flag, on: boolean) =>
    setDraft((d) => Object.fromEntries(Object.entries(d).map(([k, v]) => [k, { ...v, [flag]: on }])));

  async function save(values: Record<string, Draft>) {
    const body: Record<string, PortSetting> = {};
    for (const p of ports) {
      const d = values[keyOf(p)];
      const v = d.port.trim();
      const n = Number(v);
      if (!v || !Number.isInteger(n) || n < 1 || n > 65535) {
        setError(`Enter a port from 1 to 65535 for ${keyOf(p)}.`);
        return;
      }
      body[keyOf(p)] = { port: n, forward: d.forward, proxy: d.proxy, firewall: d.firewall };
    }
    setBusy("save");
    setError(undefined);
    setNotice(undefined);
    try {
      const affectsProxy =
        info!.proxy.configured &&
        (relay !== info!.proxy.relayed ||
          (relay &&
            ports.some((p) => {
              const a = values[keyOf(p)];
              const b = saved[keyOf(p)];
              return a.port !== b.port || a.proxy !== b.proxy || a.firewall !== b.firewall;
            })));
      const next = await api.setServerPorts(server, body, info!.proxy.configured ? relay : undefined);
      show(next);
      const steps = [showUnifi ? "update the UniFi rules below" : "", affectsProxy ? "apply the proxy" : ""].filter(Boolean);
      setNotice(`Saved.${steps.length ? ` To use the changes, ${steps.join(" and ")}.` : ""}`);
      if (affectsProxy) setNeedsApply(true);
      onSaved();
      loadSync();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function applyProxy() {
    setBusy("apply");
    setError(undefined);
    setNotice(undefined);
    try {
      const r = await api.applyProxy();
      const fw = r.firewall?.error ? ` The firewall couldn't be updated: ${r.firewall.error}` : r.firewall?.changed ? " The firewall was updated." : "";
      setNotice(`Applied: the proxy server now relays the selected servers' ports.${fw}`);
      setNeedsApply(false);
      show(await api.serverPorts(server));
      loadSync();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  function applyShift() {
    const n = Number(shift);
    if (!Number.isInteger(n)) return;
    setDraft((d) => Object.fromEntries(ports.map((p) => [keyOf(p), { ...d[keyOf(p)], port: String(p.local + n) }])));
  }

  const reset = () => save(Object.fromEntries(ports.map((p) => [keyOf(p), { ...draft[keyOf(p)], port: String(p.local) }])));

  // A column's header checkbox: on when every port is on.
  const header = (flag: Flag, label: string, disabled = false) => {
    const all = ports.every((p) => draft[keyOf(p)]?.[flag]);
    return (
      <th>
        <label className="col-toggle">
          <input type="checkbox" checked={all && !disabled} disabled={disabled} onChange={(e) => setAll(flag, e.target.checked)} />
          {label}
        </label>
      </th>
    );
  };

  return (
    <div className="card port-settings">
      <h2>Ports</h2>
      {tunnel ? (
        <>
          <p className="muted small no-margin">
            Players connect to the proxy server, <code>{route!.proxyHost}</code>, on the <strong>proxy port</strong>. It
            relays their traffic through the WireGuard tunnel to {route!.target ? <code>{route!.target}</code> : "this host"}{" "}
            on the {lanTarget ? "server's own" : "host"} port. Nothing is forwarded on your router, so your home address stays
            private. Give servers that use the same ports different proxy ports.
          </p>
          <div className="route-line small">
            Player → <code>{route!.proxyHost}:{"<proxy port>"}</code> → WireGuard tunnel →{" "}
            <code>{route!.target ?? "this host"}:{lanTarget ? "<server port>" : "<host port>"}</code>
          </div>
          {route!.error && <div className="alert error">This server can&apos;t be relayed through the tunnel: it {route!.error}.</div>}
          {leftoverRules && (
            <div className="alert warn">
              UniFi still forwards some of these ports from your public address. Through the tunnel that isn&apos;t
              needed and exposes your home address: untick UniFi for them, save, and click Update rules below.
            </div>
          )}
        </>
      ) : (
        <p className="muted small no-margin">
          How players reach this server from the internet. The <strong>internet port</strong> is the one they connect to;
          UniFi and the proxy send it to the server&apos;s own port, so two servers of the same game (e.g. each on its own
          LAN IP) can share your public address on different ports. Players connect with the new port directly; a
          game&apos;s server browser may still list the server&apos;s own port.
        </p>
      )}

      <div className="port-channels">
        {info.proxy.configured ? (
          <label className="radio">
            <input type="checkbox" checked={relay} onChange={(e) => setRelay(e.target.checked)} /> Relay this server through
            the proxy server (nginx)
          </label>
        ) : (
          <span className="muted small">
            No proxy server is set up; <Link to="/proxy">set one up</Link> to relay servers through it.
          </span>
        )}
        {!info.unifi && !tunnel && (
          <span className="muted small">
            UniFi isn&apos;t connected; <Link to="/system">connect it</Link> to forward ports on your gateway.
          </span>
        )}
        {info.proxy.configured && !info.firewall && (
          <span className="muted small">
            No Linode firewall is managed; <Link to="/system#linode">choose one</Link> to open relayed ports in it.
          </span>
        )}
      </div>

      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}

      <div className="sync-summary">
        {[
          showUnifi ? syncSummary("UniFi", sync?.unifi ?? null, sync?.unifiError, "Update rules") : null,
          syncSummary("Proxy", sync?.proxy ?? null, sync?.proxyError, "Apply proxy"),
          syncSummary("Firewall", sync?.firewall ?? null, sync?.firewallError, "Apply proxy"),
        ]
          .filter((s) => s !== null)
          .map((s) => (
            <span key={s!.text} className={`sync-line ${s!.ok ? "ok" : "bad"}`}>
              {s!.text}
            </span>
          ))}
        <button type="button" className="link-button small" onClick={loadSync} disabled={checking}>
          {checking ? "checking…" : "check again"}
        </button>
        {changed && <span className="muted small">Shows the saved settings; save to update it.</span>}
      </div>

      <div className="table-scroll">
        <table className="data-table">
          <thead>
            <tr>
              <th>{tunnel && !lanTarget ? "Host port" : "Server port"}</th>
              <th>Protocol</th>
              <th>{tunnel ? "Proxy port" : "Internet port"}</th>
              {tunnel && <th>Goes to</th>}
              {showUnifi && header("forward", "UniFi")}
              {info.proxy.configured && header("proxy", "Proxy", !relay)}
              {info.firewall && header("firewall", "Firewall", !relay)}
            </tr>
          </thead>
          <tbody>
            {ports.map((p) => {
              const k = keyOf(p);
              const d = draft[k];
              if (!d) return null;
              return (
                <tr key={k}>
                  <td>
                    <code>{p.local}</code>
                  </td>
                  <td>{p.proto.toUpperCase()}</td>
                  <td>
                    <input
                      className="port-input"
                      type="number"
                      min={1}
                      max={65535}
                      value={d.port}
                      onChange={(e) => set(k, { port: e.target.value })}
                      aria-label={`${tunnel ? "Proxy" : "Internet"} port for ${k}`}
                    />
                    {p.conflicts?.length ? <div className="small form-error">Also used by {p.conflicts.join(", ")}</div> : null}
                  </td>
                  {tunnel && (
                    <td>
                      {d.proxy ? <code>{`${route!.target ?? "this host"}:${p.local}`}</code> : <span className="muted small">not relayed</span>}
                    </td>
                  )}
                  {showUnifi && (
                    <td>
                      <input type="checkbox" checked={d.forward} onChange={(e) => set(k, { forward: e.target.checked })} aria-label={`Forward ${k} with UniFi`} />
                      <SyncBadge kind="unifi" state={sync?.unifi?.[k]} />
                    </td>
                  )}
                  {info.proxy.configured && (
                    <td>
                      <input
                        type="checkbox"
                        checked={d.proxy && relay}
                        disabled={!relay}
                        onChange={(e) => set(k, { proxy: e.target.checked })}
                        aria-label={`Relay ${k} through the proxy`}
                      />
                      <SyncBadge kind="proxy" state={sync?.proxy?.[k]} />
                    </td>
                  )}
                  {info.firewall && (
                    <td>
                      <input
                        type="checkbox"
                        checked={d.firewall && d.proxy && relay}
                        disabled={!relay || !d.proxy}
                        onChange={(e) => set(k, { firewall: e.target.checked })}
                        aria-label={`Open ${k} in the firewall`}
                        title={!d.proxy ? "Only relayed ports can be opened in the firewall" : undefined}
                      />
                      <SyncBadge kind="firewall" state={sync?.firewall?.[k]} />
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {info.firewall && proxyOn && (
        <p className="muted small">
          The firewall column opens relayed ports in the proxy server&apos;s Linode firewall; untick it for ports you open
          there yourself.
        </p>
      )}

      <div className="form-footer">
        <label className="shift-ports">
          <span className="small muted">Shift {tunnel ? "proxy" : "internet"} ports by</span>
          <input type="number" value={shift} onChange={(e) => setShift(e.target.value)} placeholder="10" className="port-input" />
          <button type="button" className="btn small" onClick={applyShift} disabled={!shift || !!busy}>
            Shift
          </button>
        </label>
        <div className="spacer" />
        {moved && (
          <button type="button" className="btn small ghost" onClick={reset} disabled={!!busy}>
            Use server ports
          </button>
        )}
        {info.proxy.configured && (needsApply || (!info.proxy.applied && info.proxy.relayed)) && !changed && (
          admin ? (
            <button type="button" className="btn small" onClick={applyProxy} disabled={!!busy}>
              {busy === "apply" ? "Applying…" : "Apply proxy"}
            </button>
          ) : (
            <span className="muted small">An admin has to apply this on the Proxy page.</span>
          )
        )}
        <button type="button" className="btn small primary" onClick={() => save(draft)} disabled={!!busy || !changed}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
