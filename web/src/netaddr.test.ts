import assert from "node:assert/strict";
import { test } from "node:test";
import { nextFreeIP, suggestRange } from "./netaddr.ts";
import type { NetworkInfo } from "./types";

const br0 = (over: Partial<NetworkInfo> = {}): NetworkInfo => ({
  name: "br0",
  driver: "macvlan",
  subnets: ["192.168.1.0/24"],
  gateways: ["192.168.1.1"],
  staticIp: true,
  directIp: true,
  usedIps: [],
  managed: true,
  ...over,
});

test("LAN servers are numbered in order from the start of the automatic range", () => {
  const n = br0({ ipRanges: ["192.168.1.224/27"] });
  assert.equal(nextFreeIP(n), "192.168.1.224");
  n.usedIps = [
    { ip: "192.168.1.224", container: "a" },
    { ip: "192.168.1.225", container: "b", stopped: true },
  ];
  assert.equal(nextFreeIP(n), "192.168.1.226");
  // A gap is filled first.
  n.usedIps = [{ ip: "192.168.1.225", container: "b" }];
  assert.equal(nextFreeIP(n), "192.168.1.224");
  // The server being edited doesn't take its own address.
  n.usedIps = [{ ip: "192.168.1.224", container: "me" }];
  assert.equal(nextFreeIP(n, "me"), "192.168.1.224");
});

test("without a range, the block hlm would suggest, then down from the top", () => {
  const n = br0();
  assert.equal(nextFreeIP(n), suggestRange("192.168.1.0/24").replace("/27", ""));
  assert.equal(nextFreeIP(n), "192.168.1.224");
  // The block is full (.255 is broadcast): below it, from the top down.
  n.usedIps = Array.from({ length: 31 }, (_, i) => ({ ip: `192.168.1.${224 + i}`, container: `s${i}` }));
  assert.equal(nextFreeIP(n), "192.168.1.223");
});

test("a range that ends at the broadcast address skips it", () => {
  const n = br0({ ipRanges: ["192.168.1.252/30"] });
  n.usedIps = [
    { ip: "192.168.1.252", container: "a" },
    { ip: "192.168.1.253", container: "b" },
    { ip: "192.168.1.254", container: "c" },
  ];
  assert.equal(nextFreeIP(n), "192.168.1.251");
});

test("private networks count up from the bottom, past the gateway", () => {
  const n = br0({ driver: "bridge", directIp: false, subnets: ["172.30.0.0/24"], gateways: ["172.30.0.1"] });
  assert.equal(nextFreeIP(n), "172.30.0.2");
});
