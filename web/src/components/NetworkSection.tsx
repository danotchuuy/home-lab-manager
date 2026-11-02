import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
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
  templateNetwork?: string;
  network: string;
  ipAddress: string;
  onNetwork: (n: string) => void;
  onIPAddress: (ip: string) => void;
}

/** Picks the Docker network for the server and, where Docker allows it, a fixed IP. */
export function NetworkSection({ serverName, templateNetwork, network, ipAddress, onNetwork, onIPAddress }: Props) {
  const [networks, setNetworks] = useState<NetworkInfo[]>([]);
  const [error, setError] = useState<string>();
  const [creating, setCreating] = useState<NetworkKind | false>(false);

  const reload = useCallback(() => api.networks().then(setNetworks, (e) => setError(e.message)), []);
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

  async function removeSelected() {
    if (!selected || !confirm(`Delete the network ${selected.name}?`)) return;
    try {
      await api.removeNetwork(selected.name);
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
            onChange={(e) => onIPAddress(e.target.value)}
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
                <button type="button" className="link-button" onClick={() => onIPAddress(freeIP)}>
                  Use the next free address, {freeIP}
                </button>{" "}
              </>
            )}
            {staticAllowed ? (
              <>
                Leave empty for an automatic address.{" "}
                Subnet {selected!.subnets.join(", ")}
                {selected!.gateways.length > 0 && `, gateway ${selected!.gateways.join(", ")}`}.
                {selected!.usedIps.length > 0 && (
                  <> In use: {selected!.usedIps.map((u) => `${u.ip} (${u.container})`).join(", ")}.</>
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

// --- IPv4 helpers for suggestions ---

function ipToInt(ip: string): number {
  return ip.split(".").reduce((n, o) => (n << 8) + Number(o), 0) >>> 0;
}

function intToIp(n: number): string {
  return [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join(".");
}

function parseCidr(cidr: string): { base: number; bits: number } | undefined {
  const m = /^(\d+\.\d+\.\d+\.\d+)\/(\d+)$/.exec(cidr.trim());
  if (!m) return undefined;
  const bits = Number(m[2]);
  if (bits < 0 || bits > 32) return undefined;
  const mask = bits === 0 ? 0 : (~0 << (32 - bits)) >>> 0;
  return { base: (ipToInt(m[1]) & mask) >>> 0, bits };
}

/**
 * The first address in the network's first IPv4 subnet that isn't its
 * network address, a gateway, broadcast or used by another container. Only
 * running containers are known, so a stopped server's fixed address can
 * still clash; Docker reports that when the server starts.
 */
function nextFreeIP(n: NetworkInfo, server?: string): string | undefined {
  const c = n.subnets.map(parseCidr).find((x) => x);
  if (!c || c.bits > 30) return undefined;
  const taken = new Set([...n.gateways, ...n.usedIps.filter((u) => u.container !== server).map((u) => u.ip)]);
  const size = 2 ** (32 - c.bits);
  // A LAN network's DHCP pool is usually at the bottom, so start near the top there.
  const order = n.directIp ? [...Array(size - 2).keys()].map((i) => size - 2 - i) : [...Array(size - 2).keys()].map((i) => i + 1);
  for (const i of order.slice(0, 4096)) {
    const ip = intToIp(c.base + i);
    if (!taken.has(ip)) return ip;
  }
  return undefined;
}

/** An address near the top of the network's subnet, as a placeholder. */
function exampleIP(n: NetworkInfo): string {
  const c = n.subnets[0] && parseCidr(n.subnets[0]);
  if (!c || c.bits > 30) return "192.168.1.200";
  const size = 2 ** (32 - c.bits);
  return intToIp(c.base + size - 10);
}

/**
 * Suggests a block at the top of a subnet for Docker's automatic addresses:
 * routers usually hand out DHCP addresses from the bottom.
 */
function suggestRange(subnet: string): string {
  const c = parseCidr(subnet);
  if (!c || c.bits > 27) return "";
  const size = 2 ** (32 - c.bits);
  return `${intToIp(c.base + size - 32)}/27`;
}

function CreateNetwork({
  existing,
  kind,
  onKind: setKind,
  onCreated,
  onCancel,
}: {
  existing: NetworkInfo[];
  /** The kind of network, kept by the parent so its dropdown can name it. */
  kind: NetworkKind;
  onKind: (k: NetworkKind) => void;
  onCreated: (n: NetworkInfo) => void;
  onCancel: () => void;
}) {
  const [ifaces, setIfaces] = useState<HostInterface[]>([]);
  const [name, setName] = useState("");
  const [parent, setParent] = useState("");
  const [subnet, setSubnet] = useState("");
  const [gateway, setGateway] = useState("");
  const [ipRange, setIPRange] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    api.hostInterfaces().then(setIfaces, () => setIfaces([]));
  }, []);

  // Fill in suggestions for the chosen type and interface.
  useEffect(() => {
    if (kind === "bridge") {
      setName((n) => n || (existing.some((x) => x.name === "games") ? "" : "games"));
      setSubnet(freePrivateSubnet(existing));
      setGateway("");
      setIPRange("");
      return;
    }
    setName((n) => n || (existing.some((x) => x.name === "lan") ? "" : "lan"));
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
      onCreated(await api.createNetwork(req));
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
