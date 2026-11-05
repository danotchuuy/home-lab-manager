# 19 — Game profiles

**Priority:** Requested · **Depends on:** nothing

> **Status: phase 1 done** (branch `feature/19-game-profiles`): the built-in games are profiles in
> `internal/gameprofile/profiles/`, read by the status queries, the console and settings-file suggestions, and the web
> UI's schedule presets get their commands from the console status. Phases 2 (your own profiles) and 3 are to do.
> Where phase 1 differs from the plan:
>
> - **One way to recognise a game.** The console used to read only the repository name (split at `-`, `_`, `.`);
>   status queries and settings files read the name and tag. Profiles use the latter everywhere, so the console now
>   also recognises games named in the tag (`ich777/steamcmd:palworld`), and ARK SE images (`arkse`) get ARK's player
>   list for status, as the console already treated them as ARK.
> - **Also moved:** the save-before-stop command (`console.save_before_stop`), which only Palworld uses.

## Why

hlm knows things about particular games: how to recognise them from their image, which ports answer status queries,
their RCON port and console commands, how to read their player lists, which settings files they have, and what "say"
and "save" are for scheduled tasks. Today that knowledge is small tables of Go spread over five places (`gamequery`,
`console`, `configfiles`, and the web UI's schedule presets). Adding a game, or fixing one, means changing code in
several of them and waiting for a release.

A **plugin system that runs code** was considered and rejected:

- Go's `plugin` package needs the exact same toolchain and build as hlm, Linux and cgo (hlm builds without cgo), and
  can't unload plugins.
- External plugin processes (scripts, binaries, go-plugin style) would run next to something that controls the Docker
  socket, which is root on every host it manages: installing one would mean trusting it completely, and each is
  another thing to version and debug.
- Sandboxed scripting (WebAssembly, Starlark) is sound, but a lot of machinery for what is mostly data.

So: **declarative game profiles**. One YAML file per game says what's different about it; hlm's existing features read
the profile instead of their own tables. Profiles can only point existing behaviour at things (a port, a command, a
file name, a pattern), so a bad one can't do much harm.

## What a profile says

```yaml
id: palworld
name: Palworld
# Image words (the repository name and tag, split at anything that isn't a letter or digit) that identify it:
# any one set, all of its words. The most specific match wins.
match: [[palworld]]
query:
  console_first: true        # ask through the console (names, counts) before query ports
  a2s_ports: [27015]          # Steam query ports to try (container ports)
console:
  rcon_port: 25575            # when the image doesn't say
  say: Broadcast              # for scheduled tasks' warnings
  save: Save
  commands: [ShowPlayers, Info, Save, "Broadcast ", "KickPlayer ", "BanPlayer "]
  exec:                       # images that bring their own command sender
    - image: palworld-server-docker
      command: "rcon-cli {command}"
      why: This image has rcon-cli, which already knows the server's RCON password.
      off_when: { RCON_ENABLED: "false" }
  rest:                       # a built-in console driver that replaces RCON when the image has it
    driver: palworld
    commands: [ShowPlayers, Metrics, Info, Save, "Broadcast ", "KickPlayer ", "BanPlayer "]
  players:                    # reading the player list through the console
    command: ShowPlayers
    format: csv               # csv | summary | lines
    header: "name,"
settings_files:
  - names: [PalWorldSettings.ini]
    prefer: /Saved/Config/
```

`players.format`:

- `csv`: a header line starting with `header`, then one player per line, name in the first column.
- `summary`: `pattern` is a regular expression with named groups `online`, `max` and `names` (split at
  `names_separator`), e.g. Minecraft's "There are 2 of a max of 20 players online: alex, sam".
- `lines`: one player per line; `pattern` has a `name` group; lines equal to one of `ignore` are skipped.

Built-in drivers (`console.rest.driver`) stay Go: they're protocols, not data. Palworld's REST API is the only one.

## Phases

1. **Built-in profiles, no behaviour change.** Move every table into embedded profiles (`internal/gameprofile`),
   loaded and validated at start (an invalid built-in profile fails the tests). `gamequery`, `console` and
   `configfiles` ask the profile matched to a server's image; the console status gives the profile's `say` and `save`
   to the web UI, replacing its own table. Each built-in game behaves as before, checked by the existing tests plus
   new ones per profile.
2. **Your own profiles.** Profiles from `<data>/game-profiles/*.yaml` and imported on a Settings page, overriding
   built-in ones with the same `id`; each server shows the profile it uses; load errors are shown, not fatal.
3. **Gaps, if real ones appear.** E.g. a player list no pattern can read: consider a sandboxed hook for that one step.

## Phase 1 design

- `internal/gameprofile`: `Profile` and its parts, `Parse` (YAML, strict: unknown fields are errors), `Validate`
  (ids, match sets, ports 1–65535, formats, regular expressions compile with the right groups), the embedded
  `profiles/*.yaml`, `Default()` (the built-in set, parsed once), `Match(image)` (the profile whose match set has the
  most words, among those all present; ties by id), `Words(image)` and `Players.Parse(out)`.
- `gamequery`: candidates come from the profile (console first, Minecraft/Bedrock protocols and their port variable,
  A2S ports, a console candidate when it has `players`); `queryConsole` uses `players`; Palworld's REST extras when the
  profile's REST driver is `palworld`.
- `console`: `Detect`, quick commands, RCON port fallback, stdin and exec images from the profile; `Status` gains
  `say` and `save`.
- `configfiles`: suggestions from the profile's `settings_files`.
- Web: the schedule's presets use the console status's `say` and `save`.

## Testing

- Every built-in profile parses and validates; matching picks the expected profile for real image names (itzg,
  ich777's tags, thijsvanloef, …), most specific first (Bedrock over Minecraft).
- Player list parsing for each format, with the outputs the old parsers were tested with.
- The existing `gamequery`, `console` and `configfiles` tests pass unchanged.

## Tasks

- [x] Plan (this file)
- [x] `internal/gameprofile` with built-in profiles and tests
- [x] `gamequery`, `console`, `configfiles` read profiles
- [x] Console status `say`/`save`; schedule presets use them
- [x] README
- [ ] (phase 2) user profiles, import, per-server profile shown
- [ ] (phase 3) only if needed
