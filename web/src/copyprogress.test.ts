import assert from "node:assert/strict";
import { test } from "node:test";
import { describeCopy, timeLeft } from "./copyprogress.ts";

test("a copy's progress says how much of how much, how fast and how long", () => {
  const d = describeCopy({ files: 120, bytes: 2 ** 30, total: 4 * 2 ** 30, rate: 50 * 2 ** 20 });
  assert.equal(d.percent, 25);
  assert.equal(d.copied, "1.0 GiB of 4.0 GiB (120 files)");
  assert.equal(d.speed, "50.0 MiB/s");
  assert.equal(d.left, "3.0 GiB to go · about 1 min left");
});

test("before the total is known, it says how much so far", () => {
  const d = describeCopy({ files: 1, bytes: 5 * 2 ** 20, rate: 0 });
  assert.equal(d.percent, undefined);
  assert.equal(d.copied, "5.0 MiB copied (1 file)");
  assert.equal(d.speed, "");
});

test("never more than all of it", () => {
  const d = describeCopy({ files: 2, bytes: 110, total: 100, rate: 10 });
  assert.equal(d.percent, 100);
  assert.match(d.left, /^0 B to go/);
});

test("time left reads naturally", () => {
  assert.equal(timeLeft(3), "a few seconds");
  assert.equal(timeLeft(42), "about 40 s");
  assert.equal(timeLeft(150), "about 3 min");
  assert.equal(timeLeft(3600), "about 1 h");
  assert.equal(timeLeft(5400), "about 1 h 30 min");
  assert.equal(timeLeft(Infinity), "");
});
