import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { test } from "node:test";
import { sectionInfo, sectionPath, SYSTEM_SECTIONS, SYSTEM_TABS } from "./systemSections.ts";

test("every System section has its own ID, on a tab", () => {
  const ids = SYSTEM_SECTIONS.map((s) => s.id);
  assert.equal(new Set(ids).size, ids.length);
  const tabs = new Set(SYSTEM_TABS.map((t) => t.id));
  for (const s of SYSTEM_SECTIONS) assert.ok(tabs.has(s.tab), s.id);
  // Every tab has something on it.
  for (const t of tabs) assert.ok(SYSTEM_SECTIONS.some((s) => s.tab === t), t);
});

test("links into the System page reach a section", () => {
  // Every /system#… link in the app names a section, so it opens its tab.
  const src = new URL("./", import.meta.url);
  const ids = new Set<string>();
  for (const f of readdirSync(src, { recursive: true }) as string[]) {
    if (!f.endsWith(".tsx")) continue;
    for (const m of readFileSync(new URL(f, src), "utf8").matchAll(/\/system#([a-z-]+)/g)) ids.add(m[1]);
  }
  assert.ok(ids.size > 0);
  for (const id of ids) assert.ok(sectionInfo(id), `/system#${id}`);
});

test("a section's path opens its tab", () => {
  assert.equal(sectionPath(sectionInfo("linode")!), "/system?tab=network#linode");
  assert.equal(sectionPath(sectionInfo("version")!), "/system#version");
});
