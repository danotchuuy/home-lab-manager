import assert from "node:assert/strict";
import { test } from "node:test";
import { probesFromYaml, probesToYaml } from "./probesyaml.ts";

test("probes round-trip through YAML", () => {
  const p = probesFromYaml(
    "livenessProbe:\n  tcpSocket:\n    port: 53\n  periodSeconds: 30\n",
  );
  assert.deepEqual(p, {
    livenessProbe: { tcpSocket: { port: 53 }, periodSeconds: 30 },
  });
  assert.deepEqual(probesFromYaml(probesToYaml(p)), p);
  assert.equal(probesToYaml({}), "");
  assert.deepEqual(probesFromYaml("  "), {});
});

test("probes YAML is checked", () => {
  assert.throws(
    () => probesFromYaml("readiness:\n  exec: {}\n"),
    /Unknown key readiness/,
  );
  assert.throws(() => probesFromYaml("- a\n"), /YAML map/);
});
