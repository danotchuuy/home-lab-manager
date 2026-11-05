import { useState, type ReactNode } from "react";
import type { DockerHost, Server } from "../types";
import { bareName } from "../util";
import { toast } from "./Shell";

/** Copies text, also where the clipboard API isn't offered (plain HTTP on the LAN). */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Fall back below.
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.appendChild(area);
  area.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  area.remove();
  return ok;
}

export function CopyButton({ text, label, children }: { text: string; label: string; children?: ReactNode }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className="btn ghost small copy-button"
      aria-label={label}
      title={text}
      onClick={async (e) => {
        e.preventDefault();
        const ok = await copyText(text);
        if (ok) {
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } else toast("Couldn't copy: select it and copy it yourself", "error");
      }}
    >
      {done ? "Copied ✓" : (children ?? "Copy")}
    </button>
  );
}

export interface ConnectAddress {
  address: string; // host:port
  port: number; // the container's port
  proto: string;
}

/**
 * Where players connect to a server: its own LAN address on a macvlan
 * network, or the published ports at its Docker host's address (this page's
 * host for the local one).
 */
export function connectAddresses(s: Server, hosts?: DockerHost[]): ConnectAddress[] {
  const direct = !!s.ipAddress && s.network !== "bridge" && s.ports.every((p) => !p.PublicPort);
  let host = window.location.hostname;
  if (s.host && s.host !== "local") host = hosts?.find((h) => h.id === s.host)?.address || "";
  if (!host) return [];
  const fmt = (h: string, port: number) => (h.includes(":") ? `[${h}]:${port}` : `${h}:${port}`);
  const seen = new Set<string>();
  const out: ConnectAddress[] = [];
  for (const p of s.ports) {
    const port = direct ? p.PrivatePort : p.PublicPort;
    if (!port) continue;
    const address = fmt(direct ? s.ipAddress! : host, port);
    if (seen.has(address + p.Type)) continue;
    seen.add(address + p.Type);
    out.push({ address, port: p.PrivatePort, proto: p.Type });
  }
  return out;
}

/** Server page: the addresses to give players, each to copy, and all of them as one message. */
export function ConnectCard({ server, hosts }: { server: Server; hosts?: DockerHost[] }) {
  const list = connectAddresses(server, hosts);
  if (list.length === 0) return null;
  const name = bareName(server.name);
  const message = [`${name} (${server.app})`, ...list.map((a) => `${a.address} (${a.proto.toUpperCase()})`)].join("\n");
  return (
    <div className="card connect-card">
      <div className="section-head">
        <h2>Connect</h2>
        <CopyButton text={message} label={`Copy how to connect to ${name}`}>
          Copy all
        </CopyButton>
      </div>
      <ul className="connect-list">
        {list.map((a) => (
          <li key={a.address + a.proto}>
            <code>{a.address}</code>{" "}
            <span className="muted small">
              {a.proto.toUpperCase()} · container port {a.port}
            </span>{" "}
            <CopyButton text={a.address} label={`Copy ${a.address}`} />
          </li>
        ))}
      </ul>
      <p className="muted small no-margin">
        Addresses on your network. From the internet, players use your public address (or a port forward, proxy or
        tunnel) with the same ports.
      </p>
    </div>
  );
}
