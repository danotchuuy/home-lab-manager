# 24 — Sharing game profiles

**Priority:** Low · **Depends on:** [19](19-game-profiles.md) (phase 2)

> **Status: done** (branch `feature/24-profile-sharing`). See the README's
> [Sharing profiles](../README.md#sharing-profiles). Where the result differs from this plan:
>
> - **`internal/profileshare`** does the fetching, indexes, update checks, diff and export; the store only keeps
>   origins (`.origins.json`, also in the configuration backups).
> - **Saving from a link is `POST /api/game-profiles/import`** (`{url}` or `{index, id}`, with the `sha256` that was
>   looked at) rather than an `origin` on the existing upload, so what's saved is always fetched by hlm and matches
>   what was shown; it's refused (409) if the link changed in between. A hand edit keeps the origin and shows
>   "changed here since".
> - **Index profiles' updates come from the index**: a profile imported from an index has an update when the index
>   lists a new SHA-256, and is fetched (and checked against it) only when reviewed. Index links are kept as a
>   setting; `GET …/indexes/browse` lists their profiles.
> - **Gist links** (`gist.github.com/user/id`) are turned into their raw form too.
> - **Addresses refused:** loopback, unspecified, link-local, multicast, and the AWS and Alibaba metadata addresses;
>   LAN addresses are allowed (a Gitea at home is a fine place for profiles). Environment proxies aren't used, so the
>   check sees where hlm really connects.

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

- [x] Fetching with the limits above
- [x] Origins file, update checks (on demand and daily), diff
- [x] Download and export
- [x] Index support
- [x] UI: import from a link, update badges and diff, download buttons, index browser
- [x] README and API table
