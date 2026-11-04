import assert from "node:assert/strict";
import { test } from "node:test";
import { flipBool, humanize, parseSettings, setValue } from "./settingsform.ts";

const palworld = `[/Script/Pal.PalGameWorldSettings]
OptionSettings=(Difficulty=None,DayTimeSpeedRate=1.000000,bEnablePlayerToPlayerDamage=False,ServerName="My \\"Pal\\" Server",ServerPassword="",AdminPassword="hunter2",PublicPort=8211,RCONEnabled=True,RCONPort=25575,CrossplayPlatforms=(Steam,Xbox,PS5,Mac),BanListURL="https://api.palworldgame.com/api/banlist.txt")
`;

test("palworld's struct line becomes one field per setting", () => {
  const f = parseSettings(palworld)!;
  assert.ok(f);
  const by = Object.fromEntries(f.map((x) => [x.key, x]));
  assert.equal(f.length, 11);
  assert.equal(by.Difficulty.value, "None");
  assert.equal(by.Difficulty.group, "OptionSettings");
  assert.equal(by.Difficulty.section, "/Script/Pal.PalGameWorldSettings");
  assert.equal(by.DayTimeSpeedRate.kind, "number");
  assert.equal(by.bEnablePlayerToPlayerDamage.kind, "bool");
  assert.equal(by.ServerName.value, 'My "Pal" Server');
  assert.equal(by.ServerName.quoted, true);
  assert.equal(by.AdminPassword.secret, true);
  assert.equal(by.ServerPassword.value, "");
  assert.equal(by.CrossplayPlatforms.value, "(Steam,Xbox,PS5,Mac)");
  assert.equal(by.BanListURL.value, "https://api.palworldgame.com/api/banlist.txt");
  assert.equal(by.RCONEnabled.value, "True");
});

test("editing a field changes only its value", () => {
  let text = palworld;
  const field = (k: string) => parseSettings(text)!.find((x) => x.key === k)!;
  text = setValue(text, field("RCONEnabled"), flipBool(field("RCONEnabled").value));
  text = setValue(text, field("ServerName"), 'New, "quoted" (name)');
  text = setValue(text, field("DayTimeSpeedRate"), "2.000000");
  text = setValue(text, field("Difficulty"), "Hard, really");
  assert.equal(
    text,
    `[/Script/Pal.PalGameWorldSettings]
OptionSettings=(Difficulty="Hard, really",DayTimeSpeedRate=2.000000,bEnablePlayerToPlayerDamage=False,ServerName="New, \\"quoted\\" (name)",ServerPassword="",AdminPassword="hunter2",PublicPort=8211,RCONEnabled=False,RCONPort=25575,CrossplayPlatforms=(Steam,Xbox,PS5,Mac),BanListURL="https://api.palworldgame.com/api/banlist.txt")
`,
  );
  // And it still reads back the same way.
  const f = parseSettings(text)!;
  assert.equal(f.length, 11);
  assert.equal(f.find((x) => x.key === "ServerName")!.value, 'New, "quoted" (name)');
  assert.equal(f.find((x) => x.key === "Difficulty")!.value, "Hard, really");
});

test("plain INI, .properties and .env files", () => {
  const ini = `; comment
[Server]
Name = My server ; not a comment here
MaxPlayers=16
Enabled = true

[Other]
Name="second"
`;
  const f = parseSettings(ini)!;
  assert.deepEqual(
    f.map((x) => [x.section, x.key, x.value, x.kind]),
    [
      ["Server", "Name", "My server ; not a comment here", "text"],
      ["Server", "MaxPlayers", "16", "number"],
      ["Server", "Enabled", "true", "bool"],
      ["Other", "Name", "second", "text"],
    ],
  );
  // Same key in two sections: separate fields.
  assert.notEqual(f[0].id, f[3].id);
  const edited = setValue(ini, f[1], "20");
  assert.equal(edited, ini.replace("MaxPlayers=16", "MaxPlayers=20"));

  const props = "#Minecraft server properties\nmotd=A Minecraft Server\nmax-players=20\nonline-mode=true\nlevel-seed=\n";
  const p = parseSettings(props)!;
  assert.deepEqual(
    p.map((x) => [x.key, x.value]),
    [
      ["motd", "A Minecraft Server"],
      ["max-players", "20"],
      ["online-mode", "true"],
      ["level-seed", ""],
    ],
  );
  assert.equal(setValue(props, p[3], "12345"), props.replace("level-seed=\n", "level-seed=12345\n"));

  const env = 'export TOKEN="abc"\nPORT=8080\n';
  const e = parseSettings(env)!;
  assert.deepEqual(
    e.map((x) => [x.key, x.value, x.secret]),
    [
      ["TOKEN", "abc", true],
      ["PORT", "8080", false],
    ],
  );
});

test("files that aren't settings aren't forced into a form", () => {
  assert.equal(parseSettings("This is a readme.\nIt has sentences, not settings.\nNothing = here though.\n"), undefined);
  assert.equal(parseSettings(""), undefined);
  assert.equal(parseSettings("; only comments\n"), undefined);
  // A struct that doesn't parse stays one text field.
  const odd = "Weird=(not a struct)\n";
  const f = parseSettings(odd)!;
  assert.equal(f.length, 1);
  assert.equal(f[0].value, "(not a struct)");
});

test("booleans keep their case, keys read as words", () => {
  assert.equal(flipBool("True"), "False");
  assert.equal(flipBool("false"), "true");
  assert.equal(flipBool("TRUE"), "FALSE");
  assert.equal(humanize("bEnablePlayerToPlayerDamage"), "Enable player to player damage");
  assert.equal(humanize("DayTimeSpeedRate"), "Day time speed rate");
  assert.equal(humanize("RCONPort"), "RCON port");
  assert.equal(humanize("max-players"), "Max players");
  assert.equal(humanize("PalEggDefaultHatchingTime"), "Pal egg default hatching time");
  assert.equal(humanize("+ActiveGameModsIds"), "Active game mods ids");
});
