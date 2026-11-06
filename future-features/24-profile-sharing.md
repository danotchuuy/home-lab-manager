# 24 — Sharing game profiles

**Priority:** Low · **Depends on:** [19](19-game-profiles.md) (phase 2)

## Why

Someone who writes a good profile for a game hlm doesn't know should be able to give it to others, and someone who
uses one should hear when it's improved. Today a profile can be imported from a file or pasted, but not fetched from
where it's published or kept up to date.

## What it does

On **System → Game profiles**:

- **Import from a link…**: an HTTPS URL to a profile (a raw GitHub file, a gist, any web page serving YAML). hlm
  fetches it, shows it and what it changes (a new game, or replacing a built-in or your own profile with the same id),
  and saves it on confirmation.
- Profiles imported this way remember their link. **Check for updates** fetches each again (and daily, with the image
  update check) and shows **Update available** with a diff; nothing changes until you accept.
- **Download** on any profile, and **Download all of yours** (a `.zip`), to share them.
- An optional **index**: a URL to a list of profiles (`index.yaml` with id, name, URL and a SHA-256 for each), so a
  community repository can be browsed in hlm and its profiles imported with one click. None is configured by default.

## Design

- `gameprofile.Store` keeps origins beside the files: `game-profiles/.origins.json` with `{file: {url, sha256,
  fetched}}`. Profiles' JSON gains `origin` (`url`, `fetched`, `update`: the new SHA-256 if different).
- Fetching: HTTPS only, a 10-second timeout, at most 64 KiB, at most 3 redirects (each HTTPS), a plain `User-Agent`
  naming hlm; the body must parse and validate as a profile before anything is shown. GitHub `blob` URLs are turned
  into their `raw` form.
- Fetches go through hlm's own HTTP client, never the browser, so the page doesn't need to allow other origins.
- **Index:** `index.yaml` is `{profiles: [{id, name, url, sha256, description}]}`; a profile fetched from it must match
  its SHA-256. Settings hold index URLs (admin).
- Updates are only offered, never applied by themselves: a changed profile changes which commands are suggested and
  sent, so a person looks first. The diff is line-based on the YAML.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `POST /api/game-profiles/fetch` | admin | `{url}` → the profile and what it would replace, not saved |
| `POST /api/game-profiles` | admin | (exists) gains an optional `origin` |
| `POST /api/game-profiles/check` | admin | Fetch every profile with an origin again |
| `GET /api/game-profiles/export` | admin | `.zip` of your profiles |
| `GET/PUT /api/game-profiles/indexes` | admin | Index URLs |

## Security

- **Server-side requests to user-given URLs.** Only admins can make them, and admins can already reach anything hlm can
  through other features, but keep it narrow: HTTPS only, no credentials sent, small responses, timeouts, and refuse
  addresses in loopback and link-local ranges (169.254.0.0/16, fd00:ec2::/32 etc., where cloud metadata services
  live) after resolving, including on redirects.
- Profiles stay data (see 19); an update is shown as a diff and needs confirmation.

## Testing

- A test HTTPS server: fetch, too large, slow, not YAML, invalid profile, redirect to HTTP or to 127.0.0.1 refused,
  GitHub blob URL rewriting.
- Origins: saved on import, an update detected by hash, accepted, origin kept when edited by hand ("changed locally"
  shown, and the update shows both).
- Index: listing, SHA-256 mismatch refused.
- Export: the zip holds exactly your files.

## Tasks

- [ ] Fetching with the limits above
- [ ] Origins file, update checks (on demand and daily), diff
- [ ] Download and export
- [ ] Index support
- [ ] UI: import from a link, update badges and diff, download buttons, index browser
- [ ] README and API table
