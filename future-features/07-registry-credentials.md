# 07 — Private registry credentials

**Priority:** Medium · **Depends on:** nothing

## Why

The README says: *"Only public images are supported for now: hlm doesn't send registry credentials."* That rules out
private Docker Hub repos, GHCR images from private repos, self-hosted registries (Gitea, Harbor, `registry:2`), and
avoids Docker Hub's anonymous pull rate limit (100 pulls / 6 h per IP), which an "Update all" can hit.

## What it does

**System → Registries**: add credentials for a registry host.

| Field | |
| --- | --- |
| Registry | `docker.io`, `ghcr.io`, `registry.example.com:5000` |
| Username | |
| Password / access token | Write-only |
| Test | Logs in and lists a repo's tags (optional repo field) |

Then installs, updates, update checks, image imports and the connection containers' updates use the matching
credentials automatically.

## Design

- Store in the secrets store under `"registries"`: `[]{Host, Username, Password}`.
- **Matching:** normalise the image ref's registry (`docker.io` for bare names, `index.docker.io` → `docker.io`) and
  pick the entry with that host.
- **Pull:** `internal/docker/images.go` `PullImage` gains an `auth` parameter → `X-Registry-Auth` header =
  base64url(JSON `{"username","password","serveraddress"}`).
- **Update check:** `RemoteDigest` uses Docker's `GET /distribution/{name}/json`, which also takes `X-Registry-Auth`.
  Pass it there too.
- **Image import** (`manager.ImportImage`) and `selfupdate` image pulls (if hlm's own image is private) — pass auth.
- **Test:** `POST /auth` on the Docker Engine API (`/auth` endpoint) with the credentials — Docker does the registry
  token dance.
- Optionally: **import from `~/.docker/config.json`** on the host (read-only, if mounted), including `credHelpers`?
  No — helpers need binaries; only plain `auths` entries.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `GET /api/registries` | admin | Hosts + usernames, no passwords |
| `POST /api/registries` | confirmed | Add |
| `PUT /api/registries/{host}` | confirmed | Change (blank password = keep) |
| `DELETE /api/registries/{host}` | admin | |
| `POST /api/registries/test` | admin | |

## Security

- A deployer key or member installing an image must not be able to exfiltrate credentials: credentials go only to the
  Docker daemon for the matching host, never into a container or response.
- Members who can update a server whose image is private will pull with the admin's credentials — that's fine (the
  image is already on the host) but document it.
- Restricted/confined installs (`deploy/confined.go`): deployers may only pull from registries an admin ticked as
  "allowed for deployer keys" — add that flag per registry.

## UI

- System → Registries list and form, Test button.
- Install / Add a Docker image: when a pull fails with 401/403/"denied", show "This image needs a registry login —
  add one under System → Registries".

## Testing

- Header encoding; host normalisation table (`nginx`, `library/nginx`, `docker.io/x/y`, `ghcr.io/x/y:tag`,
  `host:5000/x@sha256:…`).
- Docker client fake asserting the header on pull and distribution calls.
- Opt-in Docker test with a local `registry:2` using htpasswd.

## Tasks

- [ ] Store + host normalisation
- [ ] `X-Registry-Auth` on pull, distribution inspect, import, selfupdate
- [ ] `/auth` test call
- [ ] API + UI + friendly pull-denied message
- [ ] Deployer allow flag
- [ ] README: replace the "only public images" note
