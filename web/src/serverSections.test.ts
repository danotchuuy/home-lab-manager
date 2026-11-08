import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { test } from "node:test";

// "Find a setting" on a server's page scrolls to each section by its ID, so
// every section it lists has to have an element with that ID.
test("every findable server section has its element", () => {
  const src = new URL("./", import.meta.url);
  const page = readFileSync(new URL("pages/ServerDetail.tsx", src), "utf8");
  const list = page.slice(page.indexOf("const sections: {"), page.indexOf("const tabLabel"));
  const ids = [...list.matchAll(/\{ id: "([a-z-]+)"/g)].map((m) => m[1]);
  assert.ok(ids.length > 10);
  let all = "";
  for (const f of readdirSync(src, { recursive: true }) as string[]) {
    if (f.endsWith(".tsx")) all += readFileSync(new URL(f, src), "utf8");
  }
  for (const id of ids) assert.ok(all.includes(`id="${id}"`), `no element with id="${id}"`);
});
