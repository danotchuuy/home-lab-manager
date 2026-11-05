import assert from "node:assert/strict";
import { test } from "node:test";
import {
  allTags,
  DEFAULT_VIEW,
  matches,
  sortServers,
  viewFromParams,
  viewToParams,
  type ListView,
} from "./serverlist.ts";
import type { Server } from "./types.ts";

function server(name: string, extra: Partial<Server> = {}): Server {
  return {
    id: name + "-id",
    name,
    host: "local",
    app: "Game",
    templateId: "t",
    image: "img:latest",
    state: "running",
    status: "",
    created: "",
    ports: [],
    network: "bridge",
    restartPolicy: "no",
    autostart: false,
    ...extra,
  } as Server;
}

const view = (v: Partial<ListView>): ListView => ({ ...DEFAULT_VIEW, ...v });

const list = [
  server("valheim", {
    app: "Valheim",
    tags: ["survival", "Friends"],
    image: "lloesche/valheim-server",
    groups: ["games"],
  }),
  server("minecraft@nas", {
    host: "nas",
    app: "Minecraft",
    tags: ["friends"],
    ports: [{ PrivatePort: 25565, PublicPort: 25565, Type: "tcp" }],
  }),
  server("plex", { app: "Plex", state: "exited" }),
  server("ark", {
    app: "ARK",
    state: "exited",
    crashLoop: { exits: 5 } as unknown as Server["crashLoop"],
    favourite: true,
  }),
  server("factorio", { update: { state: "available" } as Server["update"] }),
];

const names = (v: Partial<ListView>) => list.filter((s) => matches(s, view(v))).map((s) => s.name);

test("search looks at the name, app, image, tags, host and ports, every word", () => {
  assert.deepEqual(names({ q: "valheim" }), ["valheim"]);
  assert.deepEqual(names({ q: "LLOESCHE" }), ["valheim"]);
  assert.deepEqual(names({ q: "friends" }), ["valheim", "minecraft@nas"]);
  assert.deepEqual(names({ q: "friends nas" }), ["minecraft@nas"]);
  assert.deepEqual(names({ q: "25565" }), ["minecraft@nas"]);
  assert.deepEqual(names({ q: "nothing-like-it" }), []);
});

test("filters by state, problems, updates, group, tag and host", () => {
  assert.deepEqual(names({ state: "running" }), ["valheim", "minecraft@nas", "factorio"]);
  assert.deepEqual(names({ state: "stopped" }), ["plex", "ark"]);
  assert.deepEqual(names({ state: "problem" }), ["ark"]);
  assert.deepEqual(names({ state: "update" }), ["factorio"]);
  assert.deepEqual(names({ group: "games" }), ["valheim"]);
  assert.deepEqual(names({ tag: "FRIENDS" }), ["valheim", "minecraft@nas"]);
  assert.deepEqual(names({ host: "nas" }), ["minecraft@nas"]);
  assert.deepEqual(names({ host: "local", state: "stopped", tag: "" }), ["plex", "ark"]);
});

test("sorts favourites first, then by the key, then by name", () => {
  const u = {
    stats: {
      "valheim-id": { cpuPercent: 50, memoryUsage: 4e9 },
      "factorio-id": { cpuPercent: 120, memoryUsage: 1e9 },
    } as never,
    games: { valheim: { players: 3 }, factorio: { players: 7 } } as never,
  };
  const order = (sort: ListView["sort"]) => sortServers(list, view({ sort }), u).map((s) => s.name);
  assert.deepEqual(order("name"), ["ark", "factorio", "minecraft@nas", "plex", "valheim"]);
  assert.deepEqual(order("cpu"), ["ark", "factorio", "valheim", "minecraft@nas", "plex"]);
  assert.deepEqual(order("memory"), ["ark", "valheim", "factorio", "minecraft@nas", "plex"]);
  assert.deepEqual(order("players"), ["ark", "factorio", "valheim", "minecraft@nas", "plex"]);
  assert.deepEqual(order("state"), ["ark", "factorio", "minecraft@nas", "valheim", "plex"]);
});

test("the view round-trips through the URL, without defaults", () => {
  const v = view({ q: "val", state: "problem", tag: "pvp", sort: "cpu" });
  assert.equal(viewToParams(v).toString(), "q=val&state=problem&tag=pvp&sort=cpu");
  assert.deepEqual(viewFromParams(viewToParams(v)), v);
  assert.equal(viewToParams(DEFAULT_VIEW).toString(), "");
  assert.deepEqual(viewFromParams(new URLSearchParams("state=bogus&sort=bogus")), DEFAULT_VIEW);
});

test("tags are listed once whatever their case", () => {
  assert.deepEqual(allTags(list), ["Friends", "survival"]);
});
