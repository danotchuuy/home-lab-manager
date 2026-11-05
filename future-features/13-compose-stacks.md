# 13 — Docker Compose stacks

**Priority:** Low · **Depends on:** nothing (benefits from 10's export format)

> **Status: done** (branch `feature/13-compose-stacks`). See the README's
> [Compose stacks](../README.md#compose-stacks). Where the result differs from this plan:
>
> - **YAML:** `gopkg.in/yaml.v3`, as recommended (decided with the user).
> - **Storage:** the file and `.env` are kept in the secrets store (`"stacks"`), not `stacks/<name>.yml` in the data
>   directory. `.env` files usually hold passwords, and the secrets store is encrypted and already in configuration
>   backups.
> - **No per-service form:** the editor is the YAML and `.env`. **Check** shows each service's image, ports, resolved
>   data paths, order and change before applying.
> - **Services aren't template servers:** their containers are built from the Compose file by `internal/compose` (not
>   `deploy.Spec`, which is template-based) and carry `io.hlm.managed` plus `io.hlm.stack`, `io.hlm.stack.service` and
>   `io.hlm.stack.hash` labels. The per-server update, reconfigure, export and clone refuse them ("change it from the
>   stack"), and the server page hides those settings.
> - **Edits** recreate a service only when its hash (of the container it would get) changed; the old container is
>   kept, renamed, until the new one starts, as in `manager.recreate` (implemented in `internal/stack`).
> - **RBAC:** no stack-level grant. Server groups hold the services like any server; stack actions need operator on
>   every service, and creating, editing and deleting stacks is admin-only (deployer keys can't).
> - **Testing:** parity with `docker compose config` was checked by hand for the fixture (environment and merge
>   behaviour, command splitting, entrypoint, restart, network and volume names), not as an automated test, since the
>   Compose plugin isn't a test dependency.

## Why

hlm installs one container per server. Many self-hosted apps need several: an app plus Postgres/Redis, a game server
plus a web map renderer or a Discord bot, Pterodactyl-style setups. Today users have to install each part separately
and wire up the network by hand, and hlm doesn't know they belong together.

## What it does

**Catalog → Add a Compose stack**: paste or upload a `docker-compose.yml` (plus optional `.env`). hlm:

1. Parses it and shows a form: stack name, each service's ports, volumes (mapped under `<appdata>/<stack>/`) and
   environment variables, like the install form.
2. Creates a dedicated network `<stack>_default` (plus any declared networks), volumes, then the containers in
   `depends_on` order (waiting for `condition: service_healthy` where given).
3. Shows the stack as **one card** on the Servers page, expandable to its services. Each service is a normal managed
   container with `io.hlm.*` labels plus `io.hlm.stack=<name>` and `io.hlm.stack.service=<svc>`, so Console, Files,
   Backups, ports, hostnames all work per service.

Stack actions: start/stop/restart all (in dependency order / reverse), update all images, edit the YAML (diff, then
recreate only changed services), delete (keeps data like servers do).

## Scope (supported Compose subset)

`services.*`: `image`, `command`, `entrypoint`, `environment`, `env_file` (from the uploaded `.env` only), `ports`,
`volumes` (bind/named), `networks`, `depends_on` (list and long form), `restart`, `healthcheck`, `labels`, `devices`,
`cap_add`/`cap_drop`, `user`, `working_dir`, `mem_limit`/`deploy.resources.limits`, `ulimits`, `privileged`,
`network_mode: service:x`. Top-level `networks`, `volumes`. Variable interpolation `${VAR:-default}`.

Not supported (rejected with a clear message): `build`, `extends`, `configs`/`secrets` (v2), `profiles`, swarm `deploy`
fields other than resource limits.

## Design

- **No dependency on the compose binary.** A YAML parser is needed: the web UI already has a YAML worker; for Go, use
  `gopkg.in/yaml.v3` (discuss — it'd be a new dependency) or reject YAML and accept the JSON form (`docker compose
  config --format json`). Recommend yaml.v3.
- `internal/compose`: parse → normalized model → per-service `deploy.Spec` (reuse `deploy` for ports, mounts, appdata
  mapping, extra params semantics) → manager creates containers.
- Store the stack definition in `HLM_DATA_DIR/stacks/<name>.yml` (added to config backup) and the resolved form in
  labels, like single servers (state lives in Docker).
- Recreate logic per service reuses `manager.recreate` (rename-old-then-rollback).
- Restricted/deployer installs: same rules as `deploy/restricted.go` applied to every service.

## API

| Endpoint | Policy | |
| --- | --- | --- |
| `POST /api/stacks/preview` | admin | Parse + validate → form model, warnings |
| `POST /api/stacks` | admin | Install (job stream) |
| `GET /api/stacks`, `GET /api/stacks/{name}` | signedIn (filtered by access) | |
| `PUT /api/stacks/{name}` | admin | New YAML → plan of changes → apply |
| `POST /api/stacks/{name}/{action}` | Operator on all services | start/stop/restart/update |
| `DELETE /api/stacks/{name}` | admin | |

Server groups can contain a stack (grant applies to all its services).

## Testing

- Parse tables over real-world compose files (testdata): interpolation, depends_on order, unsupported fields.
- Spec mapping parity with `docker compose config` output for the fixtures.
- Opt-in Docker test: two busybox services with depends_on + healthcheck.

## Tasks

- [x] Decide YAML dependency
- [x] `internal/compose` parser + model + validation
- [x] Service → `deploy.Spec` mapping
- [x] Ordered create/start/stop, network/volume creation
- [x] Stack labels + grouping in server list
- [x] Edit with change plan
- [x] RBAC grouping
- [x] UI: add stack, stack card, YAML editor with diff
- [x] README section
