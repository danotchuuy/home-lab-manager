import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { parse, parseDocument } from "yaml";

// The example docker-compose.yml people copy should stay valid.
const path = new URL("../../docker-compose.yml", import.meta.url);

test("the example docker-compose.yml parses and has only Compose's top-level sections", () => {
  const text = readFileSync(path, "utf8");
  const doc = parseDocument(text);
  assert.deepEqual(doc.errors.map(String), []);
  const top = Object.keys(parse(text));
  const allowed = ["services", "volumes", "networks", "configs", "secrets", "name", "include", "version"];
  assert.deepEqual(top.filter((k) => !allowed.includes(k) && !k.startsWith("x-")), []);
  assert.ok(parse(text).services?.gsh, "the gsh service is there");
});

test("its commented-out macvlan lines are valid when uncommented", () => {
  const text = readFileSync(path, "utf8")
    .replace(/^ {4}# networks:$/m, "    networks:")
    .replace(/^ {4}# {3}default:$/m, "      default:")
    .replace(/^ {4}# {5}gw_priority: 1$/m, "        gw_priority: 1")
    .replace(/^ {4}# {3}br0:$/m, "      br0:")
    .replace(/^ {4}# {5}ipv4_address: (.*)$/m, "        ipv4_address: $1")
    .replace(/^# networks:$/m, "networks:")
    .replace(/^# {3}br0:$/m, "  br0:")
    .replace(/^# {5}external: true$/m, "    external: true");
  const c = parse(text);
  assert.deepEqual(c.services.gsh.networks, { default: { gw_priority: 1 }, br0: { ipv4_address: "192.168.1.5" } });
  assert.deepEqual(c.networks, { br0: { external: true } });
});
