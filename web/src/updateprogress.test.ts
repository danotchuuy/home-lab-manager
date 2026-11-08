import assert from "node:assert/strict";
import { test } from "node:test";
import { pullText, summarizeUpdate } from "./updateprogress.ts";
import type { DeployEvent } from "./types";

const pull = (target: string, id: string, status: string, current?: number, total?: number): DeployEvent => ({
  stage: "pull",
  target,
  progress: { id, status, progressDetail: { current, total } },
});

test("an update's download shows how far it is, then its next step", () => {
  const events: DeployEvent[] = [
    { stage: "begin", target: "valheim", message: "Updating valheim" },
    { stage: "pull", target: "valheim", message: "Pulling lloesche/valheim-server" },
    pull("valheim", "lloesche/valheim-server", "Pulling from lloesche/valheim-server"),
    pull("valheim", "a", "Already exists"),
    pull("valheim", "b", "Downloading", 512 << 20, 1 << 30),
    pull("valheim", "c", "Downloading", 0, 1 << 30),
    { stage: "begin", target: "mc", message: "Updating mc" },
  ];
  let { items } = summarizeUpdate(events);
  assert.equal(pullText(items[0]), "Downloading image: 512.0 MiB of 2.0 GiB · 1 of 3 layers (25%)");
  assert.equal(items[1].message, "Updating mc");
  assert.equal(pullText(items[1]), undefined);

  events.push(pull("valheim", "b", "Pull complete"), pull("valheim", "c", "Download complete"), pull("valheim", "c", "Extracting"));
  ({ items } = summarizeUpdate(events));
  assert.equal(pullText(items[0]), "Extracting image…");

  events.push({ stage: "stop", target: "valheim", message: "Stopping valheim" });
  ({ items } = summarizeUpdate(events));
  assert.equal(pullText(items[0]), undefined);
  assert.equal(items[0].message, "Stopping valheim");

  events.push({ stage: "done", target: "valheim", message: "Updated" }, { stage: "summary", message: "1 updated" });
  const s = summarizeUpdate(events);
  assert.equal(s.items[0].state, "done");
  assert.equal(s.summary, "1 updated");
});
