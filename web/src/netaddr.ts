// IPv4 helpers for suggesting network settings and addresses. Pure
// functions, so they can be tested on their own.
import type { NetworkInfo } from "./types";

export function ipToInt(ip: string): number {
  return ip.split(".").reduce((n, o) => (n << 8) + Number(o), 0) >>> 0;
}

export function intToIp(n: number): string {
  return [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join(".");
}

export function parseCidr(cidr: string): { base: number; bits: number } | undefined {
  const m = /^(\d+\.\d+\.\d+\.\d+)\/(\d+)$/.exec(cidr.trim());
  if (!m) return undefined;
  const bits = Number(m[2]);
  if (bits < 0 || bits > 32) return undefined;
  const mask = bits === 0 ? 0 : (~0 << (32 - bits)) >>> 0;
  return { base: (ipToInt(m[1]) & mask) >>> 0, bits };
}

/**
 * The next free address on a network: not its network address, broadcast
 * or a gateway, and not taken by another container (running, or a stopped
 * one's fixed address). On a LAN network, addresses are handed out in order
 * from the start of the network's automatic range (the block hlm suggests
 * when it's made, kept out of the router's DHCP pool), or from the top /27
 * of the subnet if it has none, then downwards from the top of the subnet;
 * on other networks, from the bottom.
 */
export function nextFreeIP(n: NetworkInfo, server?: string): string | undefined {
  const c = n.subnets.map(parseCidr).find((x) => x);
  if (!c || c.bits > 30) return undefined;
  const taken = new Set([...n.gateways, ...n.usedIps.filter((u) => u.container !== server).map((u) => u.ip)]);
  const size = 2 ** (32 - c.bits);
  const inSubnet = (i: number) => i >= 1 && i <= size - 2;
  const order: number[] = [];
  if (n.directIp) {
    // The automatic range, in order; otherwise the block hlm would suggest.
    const range = (n.ipRanges ?? []).map(parseCidr).find((r) => r && r.bits >= c.bits && ((r.base - c.base) >>> 0) < size);
    const start = range ? (range.base - c.base) >>> 0 : c.bits <= 27 ? size - 32 : 1;
    const len = range ? 2 ** (32 - range.bits) : c.bits <= 27 ? 32 : size;
    for (let i = start; i < start + len; i++) if (inSubnet(i)) order.push(i);
    // Then the rest, from the top down (DHCP pools are usually at the bottom).
    for (let i = size - 2; i >= 1 && order.length < 4096; i--) if (i < start || i >= start + len) order.push(i);
  } else {
    for (let i = 1; i <= size - 2 && order.length < 4096; i++) order.push(i);
  }
  for (const i of order) {
    const ip = intToIp(c.base + i);
    if (!taken.has(ip)) return ip;
  }
  return undefined;
}

/** An address near the top of the network's subnet, as a placeholder. */
export function exampleIP(n: NetworkInfo): string {
  const c = n.subnets[0] && parseCidr(n.subnets[0]);
  if (!c || c.bits > 30) return "192.168.1.200";
  const size = 2 ** (32 - c.bits);
  return intToIp(c.base + size - 10);
}

/**
 * Suggests a block at the top of a subnet for Docker's automatic addresses:
 * routers usually hand out DHCP addresses from the bottom.
 */
export function suggestRange(subnet: string): string {
  const c = parseCidr(subnet);
  if (!c || c.bits > 27) return "";
  const size = 2 ** (32 - c.bits);
  return `${intToIp(c.base + size - 32)}/27`;
}
