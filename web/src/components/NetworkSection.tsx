import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import { exampleIP, ipToInt, nextFreeIP, parseCidr, suggestRange } from "../netaddr";
import type { HostInterface, IPCheck, NetworkInfo, NetworkRequest } from "../types";

const BUILTIN = ["bridge", "host", "none"];

/** Select values for making a network, and the one shown while its form is open. */
const NEW_ANY = "\u0000new";
const NEW_PENDING = "\u0000pending";

type NetworkKind = "macvlan" | "bridge";

/** How the dropdown names a network of each kind while it's being made. */
const NEW_LABEL: Record<NetworkKind, string> = {
  bridge: "New private network",
  macvlan: "New LAN network",
};

interface Props {
  /** The server being edited, so its own address isn't reported as taken. */
  serverName?: string;
  /** The Docker host whose networks these are. */
  host?: string;
  templateNetwork?: string;
  network: string;
  ipAddress: string;
  onNetwork: (n: string) => void;
  onIPAddress: (ip: string) => void;
}

/** Picks the Docker network for the server and, where Docker allows it, a fixed IP. */
export function NetworkSection({ serverName, host, templateNetwork, network, ipAddress, onNetwork, onIPAddress }: Props) {
  const [networks, setNetworks] = useState<NetworkInfo[]>([]);
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState<NetworkKind | false>(false);

  const reload = useCallback(() => api.networks(host).then(setNetworks, (e) => setError(e.message)), [host]);
  useEffect(() => {
    reload();
  }, [reload]);

  // What "template default" resolves to, mirroring the backend: a template
  // network that doesn't exist here (like Unraid's br0) falls back to bridge.
  const wanted = (templateNetwork || "bridge").trim();
  const templateAvailable = BUILTIN.includes(wanted.toLowerCase()) || networks.some((n) => n.name === wanted);
  const effective = network || (templateAvailable ? wanted : "bridge");
  const selected = networks.find((n) => n.name === effective);
  // Nothing to address until the new network exists.
  const staticAllowed = !!selected?.staticIp && !creating;
  const anyStatic = networks.some((n) => n.staticIp);

  // Clear an address that the newly selected network can't take.
  useEffect(() => {
    // Not while a new network's form is open, so cancelling keeps the address.
    if (networks.length > 0 && !selected?.staticIp && ipAddress) onIPAddress("");
  }, [networks.length, selected?.staticIp, ipAddress, onIPAddress]);

  const custom = networks.filter((n) => !BUILTIN.includes(n.name));
  const freeIP = staticAllowed ? nextFreeIP(selected!, serverName) : undefined;

  const check = useIPCheck(selected?.directIp ? ipAddress : "", selected, serverName);

  // A new server on a LAN network gets the next free address filled in, so
  // servers there are numbered in order. It follows the chosen network until
  // the user types an address or clears it; a server being edited keeps its
  // own. A cloned server's address that's taken is replaced.
  const touched = useRef(false);
  const autoFilled = useRef<string | undefined>(undefined);
  const isNew = serverName === undefined;
  useEffect(() => {
    if (!isNew || touched.current || creating || networks.length === 0) return;
    const cur = ipAddress.trim();
    const mine = cur === "" || cur === autoFilled.current;
    if (!selected?.directIp || !freeIP) {
      // Off a LAN network, don't leave its address behind.
      if (cur !== "" && cur === autoFilled.current) {
        autoFilled.current = undefined;
        onIPAddress("");
      }
      return;
    }
    const takenByOther = cur !== "" && selected.usedIps.some((u) => u.ip === cur);
    if ((mine || takenByOther) && cur !== freeIP) {
      autoFilled.current = freeIP;
      onIPAddress(freeIP);
    }
  }, [isNew, creating, networks.length, selected, freeIP, ipAddress, onIPAddress]);

  async function removeSelected() {
    if (!selected || !confirm(`Delete the network ${selected.name}?`)) return;
    try {
      await api.removeNetwork(selected.name, host);
      onNetwork("");
      await reload();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <fieldset className="config-section">
      <legend>Network</legend>
      {error && <div className="alert error">{error}</div>}
      <div className="field-grid">
        <div className="field">
          <span className="field-label">
            Network
            {!creating && (
              <button type="button" className="link-button" onClick={() => setCreating("macvlan")}>
                + New network
              </button>
            )}
          </span>
          <select
            // While the form is open, the select shows the network being made;
            // picking anything else closes the form.
            value={creating ? NEW_PENDING : network}
            onChange={(e) => {
              const v = e.target.value;
              if (v === NEW_ANY) setCreating("macvlan");
              else if (v !== NEW_PENDING) {
                setCreating(false);
                onNetwork(v);
              }
            }}
          >
            <option value="">
              Template default ({templateAvailable ? wanted : `${wanted} not found, using bridge`})
            </option>
            <option value="bridge">bridge (Docker default, ports published on the host)</option>
            <option value="host">host (shares the host's network)</option>
            {custom.map((n) => (
              <option key={n.name} value={n.name}>
                {n.name} ({n.directIp ? `LAN via ${n.parent ?? n.driver}` : n.driver}
                {n.subnets.length > 0 ? `, ${n.subnets.join(", ")}` : ""})
              </option>
            ))}
            {creating ? (
              <option value={NEW_PENDING}>{NEW_LABEL[creating]} (fill it in below)</option>
            ) : (
              <option value={NEW_ANY}>New network…</option>
            )}
          </select>
          <span className="field-help">
            {creating
              ? "Fill in the new network below and create it; the server then uses it. Cancel keeps the network chosen before."
              : selected?.directIp
              ? "The server gets its own address on your LAN. Ports aren't published on the host; players connect to the server's IP directly."
              : effective === "host"
                ? "The server uses the host's ports directly; port mappings below are ignored."
                : "Ports below are published on the host."}
            {selected?.managed && selected.usedIps.length === 0 && (
              <>
                {" "}
                <button type="button" className="link-button" onClick={removeSelected}>
                  Delete this network
                </button>
              </>
            )}
          </span>
        </div>

        <label className="field">
          <span className="field-label">Fixed IP address</span>
          <input
            value={ipAddress}
            onChange={(e) => {
              touched.current = true;
              onIPAddress(e.target.value);
            }}
            disabled={!staticAllowed}
            placeholder={
              creating
                ? "Create the network first"
                : staticAllowed
                  ? `Automatic, or e.g. ${freeIP ?? exampleIP(selected!)}`
                  : "Pick a network that supports fixed IPs"
            }
            spellCheck={false}
          />
          <span className="field-help">
            {check && <IPCheckResult check={check} />}
            {staticAllowed && freeIP && freeIP !== ipAddress.trim() && (
              <>
                <button
                  type="button"
                  className="link-button"
                  onClick={() => {
                    touched.current = true;
                    onIPAddress(freeIP);
                  }}
                >
                  Use the next free address, {freeIP}
                </button>{" "}
              </>
            )}
            {staticAllowed ? (
              <>
                {isNew && selected!.directIp && autoFilled.current === ipAddress.trim() && ipAddress.trim() !== ""
                  ? "Filled in with the next free address, in order; change it or clear it for an automatic one. "
                  : "Leave empty for an automatic address. "}
                Subnet {selected!.subnets.join(", ")}
                {selected!.gateways.length > 0 && `, gateway ${selected!.gateways.join(", ")}`}.
                {selected!.usedIps.length > 0 && (
                  <>
                    {" "}
                    In use:{" "}
                    {selected!.usedIps.map((u) => `${u.ip} (${u.container}${u.stopped ? ", stopped" : ""})`).join(", ")}.
                  </>
                )}
                {selected!.directIp && " Use an address no other device on your LAN has (outside your router's DHCP range)."}
              </>
            ) : creating ? (
              <>Create the network first, then pick an address on it.</>
            ) : anyStatic ? (
              <>Docker only allows fixed IPs on networks you create. Pick one above to set an address.</>
            ) : (
              <>
                Docker doesn't allow fixed IPs on its default networks.{" "}
                {!creating && (
                  <button type="button" className="link-button" onClick={() => setCreating("bridge")}>
                    Create a network for fixed IPs
                  </button>
                )}
              </>
            )}
          </span>
        </label>
      </div>

      {creating && (
        <CreateNetwork
          host={host}
          kind={creating}
          onKind={setCreating}
          existing={networks}
          onCancel={() => setCreating(false)}
          onCreated={async (n) => {
            setCreating(false);
            await reload();
            onNetwork(n.name);
          }}
        />
      )}
    </fieldset>
  );
}

function CreateNetwork({
  existing,
  host,
  kind,
  onKind: setKind,
  onCreated,
  onCancel,
}: {
  existing: NetworkInfo[];
  /** The Docker host it's made on, whose interfaces are suggested. */
  host?: string;
  /** The kind of network, kept by the parent so its dropdown can name it. */
  kind: NetworkKind;
  onKind: (k: NetworkKind) => void;
  onCreated: (n: NetworkInfo) => void;
  onCancel: () => void;
}) {
  const [ifaces, setIfaces] = useState<HostInterface[]>([]);
  const [ifaceError, setIfaceError] = useState<string>();
  const [reading, setReading] = useState(true);
  const [name, setName] = useState("");
  const [parent, setParent] = useState("");
  const [subnet, setSubnet] = useState("");
  const [gateway, setGateway] = useState("");
  const [ipRange, setIPRange] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    setReading(true);
    setIfaceError(undefined);
    api
      .hostInterfaces(host)
      .then(setIfaces, (e: Error) => {
        setIfaces([]);
        setIfaceError(e.message);
      })
      .finally(() => setReading(false));
  }, [host]);

  // Fill in suggestions for the chosen type and interface.
  useEffect(() => {
    if (kind === "bridge") {
      setName((n) => n || (existing.some((x) => x.name === "games") ? "" : "games"));
      setSubnet(freePrivateSubnet(existing));
      setGateway("");
      setIPRange("");
      return;
    }
    // Unraid's name, so templates that ask for br0 use it.
    setName((n) => n || (existing.some((x) => x.name === "br0") ? "" : "br0"));
    const iface = ifaces.find((i) => i.name === parent) ?? ifaces[0];
    if (!iface) return;
    if (!parent) setParent(iface.name);
    const sub = iface.subnets[0] ?? "";
    setSubnet(sub);
    setGateway(iface.gateway ?? "");
    setIPRange(suggestRange(sub));
  }, [kind, parent, ifaces, existing]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    e.stopPropagation();
    setBusy(true);
    setError(undefined);
    const req: NetworkRequest = {
      name,
      driver: kind,
      subnet,
      gateway: gateway || undefined,
      parent: kind === "macvlan" ? parent : undefined,
      ipRange: ipRange || undefined,
    };
    try {
      onCreated(await api.createNetwork(req, host));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    // A div rather than a nested <form>, which HTML doesn't allow inside the
    // install form; Enter in a field still submits it.
    <div
      className="create-network"
      onKeyDown={(e) => {
        if (e.key === "Enter" && (e.target as HTMLElement).tagName === "INPUT") {
          e.preventDefault(); // don't submit the install form around this
          submit(e as unknown as FormEvent);
        } else if (e.key === "Escape") {
          onCancel();
        }
      }}
    >
      <div className="segmented" role="radiogroup" aria-label="Network type">
        <button type="button" role="radio" aria-checked={kind === "macvlan"} className={kind === "macvlan" ? "on" : ""} onClick={() => setKind("macvlan")}>
          LAN network
          <span>Servers get their own address on your network, like Unraid's br0</span>
        </button>
        <button type="button" role="radio" aria-checked={kind === "bridge"} className={kind === "bridge" ? "on" : ""} onClick={() => setKind("bridge")}>
          Private network
          <span>Fixed addresses on this host only; ports are still published</span>
        </button>
      </div>

      <div className="field-grid">
        <label className="field">
          <span className="field-label">Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} spellCheck={false} />
        </label>
        {kind === "macvlan" && (
          <label className="field">
            <span className="field-label">Host interface</span>
            {ifaces.length > 0 ? (
              <select value={parent} onChange={(e) => setParent(e.target.value)}>
                {ifaces.map((i) => (
                  <option key={i.name} value={i.name}>
                    {i.name} ({i.subnets.join(", ")}){i.default ? " · default route" : ""}
                  </option>
                ))}
              </select>
            ) : (
              <input value={parent} onChange={(e) => setParent(e.target.value)} placeholder="e.g. eth0" spellCheck={false} />
            )}
          </label>
        )}
        <label className="field">
          <span className="field-label">Subnet</span>
          <input value={subnet} onChange={(e) => setSubnet(e.target.value)} placeholder="192.168.1.0/24" spellCheck={false} />
        </label>
        <label className="field">
          <span className="field-label">Gateway{kind === "bridge" && " (optional)"}</span>
          <input
            value={gateway}
            onChange={(e) => setGateway(e.target.value)}
            placeholder={kind === "bridge" ? "Automatic" : "Your router, e.g. 192.168.1.1"}
            spellCheck={false}
          />
        </label>
        {kind === "macvlan" && (
          <label className="field">
            <span className="field-label">Automatic address range (optional)</span>
            <input value={ipRange} onChange={(e) => setIPRange(e.target.value)} placeholder="e.g. 192.168.1.224/27" spellCheck={false} />
            <span className="field-help">
              Servers without a fixed IP get addresses from here. Keep it outside your router's DHCP pool.
            </span>
          </label>
        )}
      </div>

      {kind === "macvlan" && reading && <p className="muted small">Reading the host&apos;s network interfaces…</p>}
      {kind === "macvlan" && ifaceError && (
        <p className="muted small">
          hlm couldn&apos;t read this host&apos;s interfaces ({ifaceError}), so fill them in: <code>ip route</code> on
          the host shows them.
        </p>
      )}
      {kind === "macvlan" && (
        <p className="muted small">
          On a LAN network, the server's ports are reachable at its own address from other devices, but not from this
          host itself (a Linux limitation).
        </p>
      )}
      {error && <div className="alert error">{error}</div>}
      <div className="form-footer">
        <button type="button" className="btn ghost small" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <div className="spacer" />
        <button type="button" className="btn primary small" onClick={(e) => submit(e as unknown as FormEvent)} disabled={busy || !name || !subnet}>
          {busy ? "Creating…" : "Create network"}
        </button>
      </div>
    </div>
  );
}

/** A 172.x.0.0/24 subnet not used by an existing network. */
function freePrivateSubnet(existing: NetworkInfo[]): string {
  const used = existing.flatMap((n) => n.subnets);
  for (let i = 30; i < 32; i++) {
    for (let j = 0; j < 256; j++) {
      const s = `172.${i}.${j}.0/24`;
      if (!used.some((u) => u.startsWith(`172.${i}.${j}.`) || u.startsWith(`172.${i}.0.0/16`))) return s;
    }
  }
  return "10.250.0.0/24";
}

/** A LAN address is checked with UniFi only once it's a full IPv4 address in the network's subnet. */
function inSubnet(ip: string, n?: NetworkInfo): boolean {
  if (!/^\d{1,3}(\.\d{1,3}){3}$/.test(ip) || !n) return false;
  return n.subnets.some((s) => {
    const c = parseCidr(s);
    if (!c) return false;
    const mask = c.bits === 0 ? 0 : (~0 << (32 - c.bits)) >>> 0;
    return ((ipToInt(ip) & mask) >>> 0) === c.base;
  });
}

type CheckState = { state: "checking" } | { state: "done"; result: IPCheck };

/** Checks a LAN address with UniFi, a moment after the user stops typing. */
function useIPCheck(ip: string, network: NetworkInfo | undefined, server?: string): CheckState | undefined {
  const [state, setState] = useState<CheckState>();
  const value = ip.trim();
  const valid = inSubnet(value, network);
  useEffect(() => {
    if (!valid) {
      setState(undefined);
      return;
    }
    let cancelled = false;
    setState({ state: "checking" });
    const t = setTimeout(() => {
      api.checkIP(value, server).then(
        (result) => !cancelled && setState(result.checked || result.error ? { state: "done", result } : undefined),
        () => !cancelled && setState(undefined),
      );
    }, 500);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [value, valid, server]);
  return state;
}

function IPCheckResult({ check }: { check: CheckState }) {
  if (check.state === "checking") return <span className="ip-check muted">Checking with UniFi…</span>;
  const r = check.result;
  if (r.error) return <span className="ip-check warn">Couldn&apos;t check with UniFi: {r.error}</span>;
  if (r.inUse) return <span className="ip-check bad">✕ {r.conflict}. Choose another address.</span>;
  return (
    <>
      <span className="ip-check good">✓ Not in use on your network{r.network ? ` (${r.network})` : ""}, according to UniFi.</span>
      {r.warning && <span className="ip-check warn">⚠ {r.warning}.</span>}
    </>
  );
}

/**
 * Sets up a LAN network (macvlan, named br0 like Unraid's) on a Docker host,
 * with its interface, subnet and gateway read from that host; or says it's
 * there already.
 */
export function LANNetworkSetup({ host, onClose }: { host: string; onClose: () => void }) {
  const [networks, setNetworks] = useState<NetworkInfo[]>();
  const [error, setError] = useState<string>();
  const [kind, setKind] = useState<NetworkKind>("macvlan");
  const [created, setCreated] = useState<NetworkInfo>();

  useEffect(() => {
    api.networks(host).then(setNetworks, (e: Error) => setError(e.message));
  }, [host]);

  if (error) return <div className="alert error small">{error}</div>;
  if (!networks) return <p className="muted small">Loading the host&apos;s networks…</p>;
  const lan = created ?? networks.find((n) => n.name === "br0") ?? networks.find((n) => n.directIp);
  if (lan) {
    return (
      <div className="lan-network-setup">
        <div className={`alert ${created ? "success" : "info"} small`}>
          {created ? "Created" : "This host has"} the LAN network <strong>{lan.name}</strong> ({lan.driver}
          {lan.parent ? ` on ${lan.parent}` : ""}, {lan.subnets.join(", ")}
          {lan.gateways.length > 0 ? `, gateway ${lan.gateways.join(", ")}` : ""}). Choose it in a server&apos;s Network
          section to give the server its own address on your LAN.
        </div>
        <div className="btn-row">
          <button className="btn small" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    );
  }
  return (
    <div className="lan-network-setup">
      <p className="small no-margin">
        A LAN network gives each server on it its own address on your network, like Unraid&apos;s <code>br0</code>{" "}
        (Docker&apos;s macvlan driver). hlm read the host&apos;s interfaces to suggest the settings; nothing else on
        the host changes.
      </p>
      <CreateNetwork host={host} kind={kind} onKind={setKind} existing={networks} onCancel={onClose} onCreated={setCreated} />
    </div>
  );
}
