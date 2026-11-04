# 18 — Settings files: pinned files and a form editor

**Priority:** Requested · **Depends on:** nothing

> **Status: done** (branch `feature/config-files`). See the README's [Editing server files](../README.md#editing-server-files).

## Why

Editing game settings meant finding the file in the Files tab each time, then editing raw text. Palworld keeps all its
settings on one very long line, `OptionSettings=(Difficulty=None,DayTimeSpeedRate=1.000000,…)`, where changing one value
in a text editor is error-prone: a stray comma or quote breaks the line.

## What it does

- **Pinned settings files per server:** managers pin files; they open in one click from the Files tab and the Settings
  tab. hlm suggests the usual file for known games, found in the server's data folders.
- **A form for INI-style files:** a field per setting (Unreal structs split into a field each), with switches, number
  fields, password fields and value suggestions; filter and "only changed"; only edited values are rewritten.

## Design

- `internal/configfiles`: pins stored per server name in the secrets store (`configfiles`), validated (absolute paths,
  at most 20, labels up to 64 characters); suggestions by game, from words in the image name (tag included), found with
  the existing file search in each data folder (tmpfs skipped), exact names only, the game's usual folder preferred,
  cached for 10 minutes.
- API: `GET /api/servers/{id}/config-files` (operator), `PUT` (manager).
- `web/src/settingsform.ts`: a parser that keeps each value's position in the text, so an edit replaces only those
  characters; tested with Node's built-in test runner (`npm test`, now part of `make test`).
