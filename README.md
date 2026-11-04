# Home Lab Manager

A Go server for managing and hosting Docker-based game servers. Its short name is `hlm`. It was called Game Server
Hoster (`game-server-hoster`), short name `gsh`, before; existing installs carry on (see
[Renamed from Game Server Hoster](#renamed-from-game-server-hoster) and [Short name changed from gsh to
hlm](#short-name-changed-from-gsh-to-hlm)).

It installs game servers from [Unraid Community Applications](https://ca.unraid.net/) templates, such as
[Enshrouded](https://ca.unraid.net/apps/enshrouded-1qb963l15ejrfx), on any Docker host. You don't need Unraid. The
backend talks to the Docker socket directly, and a React web UI lets you browse templates, fill in their settings, and
then start, stop, update and watch the servers.

## How it works

```
 browser ──► web UI (React + Vite)
               │  /api (JSON, NDJSON progress streams, SSE logs)
               ▼
           hlm (Go) ──► Docker Engine API over /var/run/docker.sock
               │
               └──► CA app feed (assets.ca.unraid.net), cached on disk
```

- **Catalog** (`internal/catalog`): downloads the CA feed (about 4,800 apps), skips plugins and deprecated or
  blacklisted entries, and normalizes the template fields. It caches the feed in `$HLM_DATA_DIR/cache` and refreshes it
  every 6 hours. The UI shows all categories by default.
- **Deploy** (`internal/deploy`): turns a template plus the user's values into a container definition:
  - `Port` entries become port bindings.
  - `Path` entries become bind mounts.
  - `Variable` entries become environment variables.
  - `Device` and `Label` entries become devices and labels.
  - Common `docker run` flags in `ExtraParams` are applied (`--restart`, `--memory`, `--ulimit`, `--gpus`, …).
  - Flags it doesn't support are skipped and reported as warnings.
  - Unraid's `/mnt/user/appdata/...` default paths are moved under `HLM_APPDATA_ROOT`.
- **Manager** (`internal/manager`): pulls images, then creates, starts, stops, updates and removes containers. All state
  lives in Docker. Each managed container carries `io.hlm.*` labels that record its template and the settings it was
  created with. Containers without those labels are never touched.
- **Cloudflare Tunnel** (`internal/cftunnel`, `internal/cloudflare`): creates a tunnel with the Cloudflare API, runs
  `cloudflared` in a container, and publishes servers' web interfaces on your own hostnames (ingress rules plus proxied
  CNAME records). See [Web addresses with Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel).
- **Backups** (`internal/backup`, `internal/s3`): streams chosen folders from a server's container as a tar.gz to
  S3-compatible storage such as SeaweedFS, on demand or on a schedule, and backs up hlm's own configuration there. See [Backups to S3](#backups-to-s3-seaweedfs-minio-).
- **Notifications** (`internal/notify`): sends alerts about crashes, failed backups, updates, a full disk and more to
  Discord, ntfy, Gotify, a webhook or email. See [Notifications](#notifications).
- **Docker client** (`internal/docker`): a small client for the Docker Engine API that uses only the Go standard library.
  It negotiates the API version with the daemon.

A server's **Console** tab follows its output live and shows terminal colors and styles (ANSI codes: the 16 standard
colors, 256-color and 24-bit color, bold, dim, italic, underline, strikethrough and inverse). Colors carry over from one
line to the next, as in a terminal, and other escape codes (cursor movement, clearing the line, window titles) are
removed instead of showing up as junk.

Updating or reconfiguring a server recreates its container. The old container is renamed and kept until the new one
starts; if the new one fails, the old one is put back.

## Running

Requirements: Go 1.26+, Node 22+, and access to a Docker daemon.

```sh
make run                   # builds the UI into the binary, serves it on http://localhost:8080
```

`make build` produces `bin/hlm`, a single binary with the web UI embedded.

For development, run the backend and the Vite dev server (with hot reload) side by side:

```sh
make dev-api               # Go API on :8080
make dev-web               # UI on :5173, proxies /api to :8080
```

Both listen on all network interfaces, so you can open `http://<machine-ip-or-hostname>:5173` from other computers.
Vite prints the network URLs when it starts. To keep them local, set `HLM_DEV_HOST=localhost` and
`HLM_LISTEN=127.0.0.1:8080`. Anyone who can reach the dev servers can control Docker on this machine, so set `HLM_TOKEN`
if the network isn't trusted.

### In Docker

```sh
HLM_TOKEN=change-me docker compose up -d
```

This runs the published image, `danotchuuy/home-lab-manager:latest` from Docker Hub. Until the first release is published,
or to run your own changes, replace the `image:` line in `docker-compose.yml` with `build: .` and add `--build`.

Set `HLM_APPDATA_ROOT` in `docker-compose.yml` to a directory **on the Docker host**. The Docker daemon creates game
server bind mounts on its own machine, not inside the hlm container.

Game server images are often several GB (SteamCMD-based images also download the game on first start), so leave plenty
of free disk space.

## Updating hlm

The **System** page (the version number in the top-right corner) shows the running version and the latest release.
Where hlm looks depends on how it runs:

- **In Docker**, the latest release is the highest version tag (like `v1.2.0`) of the release image on its registry,
  `danotchuuy/home-lab-manager` on Docker Hub unless `HLM_UPDATE_IMAGE` says otherwise. So pushing an image with
  `make docker-push VERSION=v1.2.0` is enough; no GitHub release is needed. Prereleases (`v1.2.0-rc1`) and other tags
  (`latest`, commit builds) are ignored. If a GitHub release exists for that version, its notes are shown too.
- **As a binary**, it's the latest GitHub release, since that's where the binaries are published.

When a new release is out, a banner says so, and **Update** installs it:

- **Binary install:** hlm downloads the release binary for its platform, checks it against the release's
  `checksums.txt`, replaces itself (keeping the previous binary as `hlm.old`) and restarts in place. The user running
  hlm must be able to write to the binary's directory.
- **Docker:** hlm pulls the new image and starts a short-lived helper container from it. The helper stops hlm's
  container, recreates it on the new image with the same settings (mounts, environment, ports, networks, labels and
  restart policy), and watches it for 20 seconds. If the new container doesn't stay up, the old one is put back. A
  container following a tag like `:latest` keeps following it; one pinned to a version (e.g. `:v1.2.0`) moves to the
  new version's tag.

Game servers keep running throughout, and hlm won't update itself while a server is being installed or updated. hlm
checks for new releases every 12 hours (`HLM_UPDATE_CHECK`), and can install them by itself
([automatic updates](#automatic-updates)). Development builds (`go run`, or builds without a
version) can't update themselves.

### Publishing a release

Push a version tag and the release workflow (`.github/workflows/release.yml`) builds and publishes everything hlm's
updater needs:

```sh
git tag v0.1.0
git push origin v0.1.0
```

It runs the tests, then publishes a GitHub release with Linux binaries (`hlm_linux_amd64`, `hlm_linux_arm64`, and
`gsh_linux_*` copies for [older installs](#short-name-changed-from-gsh-to-hlm)) and `checksums.txt`, and pushes a
multi-arch image to Docker Hub as `danotchuuy/home-lab-manager`, tagged with the version and `latest`. Before the first
release, add two repository secrets on GitHub (Settings → Secrets and variables → Actions):
`DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`, a Docker Hub access token with read & write access. To push somewhere other
than `<owner>/<repo>`, set the repository variable `DOCKERHUB_IMAGE`. Keep the Docker Hub repository public so hlm can
pull it without logging in. To build the same files locally, run `make release VERSION=v0.1.0`.

To build and push the image yourself instead, log in to the registry, then run `make docker-push`:

```sh
echo "$DOCKERHUB_TOKEN" | docker login -u danotchuuy --password-stdin   # a Docker Hub access token
make docker-push                                   # tags the image with the current git version
make docker-push VERSION=v0.2.0                    # or a given version
make docker-push IMAGE=registry.example.com/hlm PLATFORMS=linux/amd64
```

It builds for amd64 and arm64 (`PLATFORMS`) with its own buildx builder, `hlm-builder`, and pushes to `IMAGE`
(`danotchuuy/home-lab-manager` on Docker Hub by default). Only a release version (`vX.Y.Z`) is also tagged `latest`,
which installs following that tag update to; development builds such as `v0.2.0-3-gabc1234-dirty` only get their own tag.

### Renamed from Game Server Hoster

The project was called Game Server Hoster (`game-server-hoster`). The name changed first; the short name, `gsh`, changed
to `hlm` later (see [below](#short-name-changed-from-gsh-to-hlm)). The data directory works as before, and existing
servers stay managed.

- **Image:** releases are published as `danotchuuy/home-lab-manager`. `danotchuuy/game-server-hoster` no longer gets
  new releases; an install still on it can't update itself, so change its image to `danotchuuy/home-lab-manager`.
- **Compose:** `docker-compose.yml` now names the container `home-lab-manager`. Running `docker compose up -d` with it
  replaces a `game-server-hoster` container; the data directory and settings carry over.
- **Cloudflare:** DNS records hlm made are commented `Managed by Home Lab Manager`. Records with the old comment are
  still recognized as hlm's, and get the new comment when the routes are next applied.
- Comments hlm writes elsewhere (nginx and WireGuard configs, Linode firewall rules) say Home Lab Manager from their next
  update; nothing depends on them.

### Short name changed from gsh to hlm

The short name changed from `gsh` to `hlm`: the binary, the `HLM_*` settings, the `io.hlm.*` labels, and the names hlm
gives things it creates. Installs from before keep working without changes:

- **Settings:** each `HLM_*` variable falls back to its `GSH_*` name, so `GSH_TOKEN`, `GSH_SECRET_KEY`,
  `GSH_DATA_DIR` and the rest still apply. The image no longer sets `HLM_LISTEN`/`HLM_DATA_DIR` itself (its defaults,
  `:8080` and `/data`, are built in), so a `GSH_*` value isn't overridden by one.
- **Compose:** `docker-compose.yml` keeps the service's name (`gsh`) and its volume (`gsh-data`), so `docker compose up
  -d` carries on with the same container and data. It passes `GSH_TOKEN` and `GSH_SECRET_KEY` from your `.env` when the
  `HLM_*` ones aren't set.
- **Binary installs:** releases include `gsh_linux_*` copies of the binaries (and their checksums), which is what an
  older install's updater downloads. The binary keeps its file name when it updates itself.
- **Containers and networks:** those labelled `io.gsh.*` are read as `io.hlm.*`, so servers and networks made before
  stay managed. A server gets the new labels when it's next recreated (an update or a settings change). Neither
  `io.hlm.*` nor `io.gsh.*` can be set as an additional label.
- **Tunnels:** on start, hlm renames `gsh-wireguard` and `gsh-cloudflared` to `hlm-wireguard` and `hlm-cloudflared`.
  Renaming doesn't restart them. The WireGuard volume keeps its old name until the tunnel's home end is next
  recreated, and the tunnel's interface stays `gsh0` on both ends: renaming it would mean taking the tunnel down.
- **Proxy server:** if nginx includes `/etc/nginx/gsh-stream.conf`, hlm keeps writing the relays there instead of
  `/etc/nginx/hlm-stream.conf`, so nginx's own files don't change.
- **Linode firewall and UniFi:** rules labelled `gsh-…` (Linode) or named `gsh:<server> …` (UniFi) are still hlm's.
  They're relabelled `hlm-…` and `hlm:<server> …` when the proxy is next applied or the server's port forwarding is
  next synced.
- **Backups:** backup settings saved without a folder keep using `gsh` (new setups default to `hlm`), and backups of
  hlm's configuration in `<folder>/_gsh/` are listed, restored and pruned with the new ones in `<folder>/_hlm/`.
- **Servers' MAC addresses** on LAN networks are derived as before, so they don't change.
- **The web UI** keeps your sign-in token and other preferences saved under the old names in your browser.

## Connecting to Docker

hlm uses the same environment variables as the `docker` CLI, so you can either use a local socket or point it at a
Docker host.

**Local socket (default).** With `DOCKER_HOST` unset, hlm uses `/var/run/docker.sock`. When hlm runs in a container,
mount the socket: `-v /var/run/docker.sock:/var/run/docker.sock`. For a socket somewhere else, such as rootless Docker,
set `DOCKER_HOST=unix:///run/user/1000/docker.sock`.

**Remote Docker host.** Set `DOCKER_HOST=tcp://host:port`. Always use TLS for a daemon reachable over a network:

```sh
DOCKER_HOST=tcp://game-host.lan:2376
DOCKER_TLS_VERIFY=1          # verify the daemon's certificate against ca.pem
DOCKER_CERT_PATH=/certs      # holds ca.pem, cert.pem, key.pem (defaults to ~/.docker)
```

The daemon must be started with TLS and client verification (`dockerd --tlsverify --tlscacert=ca.pem
--tlscert=server-cert.pem --tlskey=server-key.pem -H tcp://0.0.0.0:2376`). Docker's
[guide to protecting the daemon socket](https://docs.docker.com/engine/security/protect-access/) shows how to make the
certificates. `docker-compose.yml` has a commented-out "Option B" for this setup.

- Setting `DOCKER_CERT_PATH` without `DOCKER_TLS_VERIFY` encrypts the connection but doesn't check the daemon's
  certificate, as the docker CLI does.
- Plain `tcp://` without TLS works, but anyone who can reach that port has root on the host. hlm logs a warning when you
  use it.
- `ssh://` hosts aren't supported.

With a remote host, `HLM_APPDATA_ROOT` and template paths refer to the remote machine's filesystem, and game server
ports are published on the remote machine. Web UI links in the dashboard point to it, and the header shows which Docker
host is in use.

## Updates and start on boot

The server list shows each server's update status. **Check for updates** asks each image's registry for the current
digest of its tag and compares it with the local image (each image is checked once, however many servers use it).
hlm also checks every 12 hours, or as often as **System → Automatic updates** says (`HLM_UPDATE_CHECK`, if set, wins;
`0` turns checking off). A server is also marked as having an update when a newer image was already downloaded, e.g.
with `docker pull`, but the server still runs the old one.

**Update** (one server) and **Update all** pull the new image and recreate the servers one at a time with the same
settings. They keep going if you close the page, and a failure doesn't stop the remaining updates. Running servers are
restarted; stopped servers stay stopped.

**Connections.** Below the servers, the **Connections** section lists the containers hlm runs for its tunnels:
`hlm-wireguard` (the [WireGuard tunnel](#wireguard-tunnel-to-this-host)'s end on this host) and `hlm-cloudflared` (the
[Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel)'s connector), and, when hlm runs in Docker on the host it
manages, hlm's own container. They show CPU, memory and network use like servers, and **Check for updates** checks their
images too. **Update** pulls the new image and has the tunnel's own setup recreate the container with the same settings,
so the connection drops for a few seconds; it isn't part of **Update all**. **Restart**, **Stop** and **Start** control
the container. Clicking a card opens its own page, like a server's: live resource usage, the container's console output,
and a **Details** tab with the container's settings (for WireGuard, also the tunnel's addresses and **Check tunnel**).
From there, **Manage** goes to the Proxy or Tunnel page. hlm's own container can be restarted (the page reconnects when
hlm is back) but not stopped, since that would take hlm down with it. Its **Update** goes to the System page, where hlm
updates itself.

### Automatic updates

Updates can be applied without anyone clicking **Update**. Each server's **Settings** tab has an **Automatic updates**
section; **System → Automatic updates** sets the default for servers that don't choose (waiting for **Update**, unless
you change it).

- **When:** as soon as an update is found, or only starting within a window, such as 03:00 to 06:00 in
  [hlm's time zone](#scheduled-tasks). A window can run past midnight. Outside it, the update waits, and the server list
  says when it's due.
- **Warn players first:** a console command, such as `say Updating in 5 minutes`, sent through the server's
  [console](#sending-commands-to-a-server) if it's set up and the server is running, then a wait of up to 30 minutes.
- **Wait while players are online:** holds the update until nobody's playing, going by the server's
  [player count](#is-the-game-up-players-and-status). If the count isn't known (the server doesn't answer a status
  query), this doesn't hold it up.
- **Back up first** (on by default): runs the server's [backup plan](#backups-to-s3-seaweedfs-minio-), if it has one,
  and waits for it. If the backup fails, the update isn't applied.

The update itself is the same as clicking **Update**: if the new container doesn't start, the old one is put back. Just
before applying an update, hlm checks the registry again, in case it was applied by hand in the meantime.

- **One at a time:** only one update runs at a time, and others queue. A server that's being installed, updated,
  reconfigured, backed up or restored, or running a scheduled task, is updated once it's free.
- **Failures:** after a failed automatic update, hlm waits a day before trying that server again (update it by hand
  sooner if you like). The Settings tab shows the last automatic update and how it went. The usual
  [notifications](#notifications) are sent (`server.updated`, `server.update_failed`, and `backup.failed` for a
  backup that stopped the update).
- **hlm itself:** **System → Automatic updates** can also install new versions of hlm automatically, optionally only
  within a window. It never does so while a server is being installed or updated, or while servers have updates to
  apply first. If it fails, hlm waits a day before trying again and sends `hlm.update_failed`.
- **Notifications instead:** to be told about updates rather than have them applied, leave servers on manual and send
  `server.update_available` to a notification target.

**Start on boot** sets the container's restart policy without recreating it: `always` when on, `no` when off. The
setting is kept across updates and edits.

Only public images are supported for now: hlm doesn't send registry credentials, so images that need a `docker login`
can't be pulled or checked for updates.

## Editing server files

A server's **Files** tab browses its container, whether it's running or stopped, and opens text files in an editor. The
**Location** picker chooses where to start: any of the server's mounts (host folders and Docker volumes, with their
source shown) or the container's root. The choice is remembered per server; otherwise the first data folder opens.
**Find config files** lists the settings-like files (`.json`, `.ini`, `.cfg`, `.properties`, `.yml`, `.xml`, …) anywhere
under the current folder, or searches by name.

The editor works on raw text and checks `.json`, `.yaml`/`.yml` and `.xml` files as you type. A syntax error shows below
the text and marks the line, but doesn't stop you saving, since some files are templates or are read by lenient parsers.
Tab and Shift+Tab indent and outdent the selected lines, or the cursor's line for Shift+Tab, using the file's own
indentation (YAML always gets spaces). Enter keeps the current line's indentation. Saving keeps a file's CRLF or LF line
endings; a file that mixes them is saved with whichever it has more of.

Keys, values and comments are colored in JSON, YAML, XML and INI-style files (`.ini`, `.cfg`, `.conf`, `.cnf`,
`.properties`, `.toml`, `.env`). Files over 256 KiB and lines over 1,000 characters stay plain. Each line is colored on
its own, so a multi-line XML comment or YAML text block can be colored as if it were settings; the file isn't affected.

- Files in data folders (bind mounts and volumes) are read and written on the host directly when hlm can see the Docker
  host's filesystem. Everything else, such as the container's own files, a remote Docker host, or a symlink that points
  outside the data folder, goes through Docker's archive API, which resolves paths the way the container does. Saves
  also go through Docker when the host's view is read-only, as with `docker-compose.yml`'s `/:/host/rootfs:ro` mount, or
  when hlm lacks permission to write the file.
- **Word wrap** (or Alt+Z) wraps long lines instead of scrolling sideways; the line numbers stay with their lines. The
  choice is remembered in the browser.
- Saving keeps the file's owner and permissions, so servers running as non-root users can still read their settings.
- If the file changed after you opened it (game servers often rewrite their settings), saving stops and offers to load
  the new version or overwrite it.
- Only files up to 1 MiB can be opened or saved; larger ones are greyed out in listings and search results, and the API
  refuses them (413). DLL files can't be opened at all, even through a symlink with another name (415). Other binary
  files open read-only, and read-only mounts stay read-only.

Many games only read their settings at startup and some rewrite them on shutdown, so stop the server, edit, then start
it again.

## Is the game up? Players and status

A running container doesn't mean players can join: SteamCMD servers spend minutes downloading the game, and a game can
hang while its container keeps running. So hlm asks each running server how it's doing, in its game's own protocol:

| How | Games |
| --- | --- |
| **Steam server query** (A2S, UDP) | Most Steam games: Valheim, Enshrouded, ARK, Rust, Counter-Strike 2, 7 Days to Die, V Rising, Conan Exiles… |
| **Minecraft Java server list ping** (TCP) | Minecraft Java, modded or not |
| **Minecraft Bedrock ping** (UDP) | Minecraft Bedrock |
| **Player list through the [console](#sending-commands-to-a-server)** | Palworld (`ShowPlayers`), Minecraft (`list`), ARK (`ListPlayers`): needs the console set up with RCON or a command run in the container |
| **TCP port check** | Anything else: only whether the port is open, no players |

**Finding a way:** hlm finds one by itself. It tries the game's usual way, then ports the template calls "query", the
game's usual Steam query port, and each UDP port and the one after it, then the console. It looks again every couple of
minutes until something answers. A server's status card has **Query settings** for managers to choose the protocol and
port instead, **Test** them first, or turn querying **off**.

The server list and the server's page show:

- **Online**: the game answers. Also shown are players (`3 / 10`), the server's name, map and version, and whether
  it's password protected. Operators and above also see players' names.
- **Starting**: the container is running, but the game isn't answering yet.
- **Not responding**: it answered before and has stopped (three queries in a row failed), or it hasn't answered after
  30 minutes with a chosen protocol.
- **Status unknown**: no way to ask it was found after 30 minutes. It may use a protocol hlm doesn't know; choose one,
  or turn querying off.

A sparkline shows the most players online in each 5-minute step of the last day (kept while the server runs; it starts
over when the server or hlm restarts).

Running servers are asked every 30 seconds (every 15 while starting or failing, and every minute through the console),
from hlm: straight at the container on its networks, or through ports published on the Docker host. Servers with their
own LAN address on a macvlan network need [hlm connected to that network](#sending-commands-to-a-server) too.

**Alerts:** [`server.online`](#notifications) says a server hlm saw start is ready for players, handy after a long
download. `server.not_responding` says a running server stopped answering or never came up.

**Player counts drive automation:** a [scheduled task](#scheduled-tasks) step can carry on only if nobody (or someone)
is playing, and [automatic updates](#automatic-updates) can wait while players are online.

## Sending commands to a server

Under a server's **Console** output there's a command line. Commands go to the server in one of three ways, chosen in
**Console settings** (managers of the server; operators can then use it):

- **Typed into the server's console (stdin):** the command is written to the server program's input, as if typed at
  its own console, and its answer shows up in the output. hlm creates every server with its input open (`-i`), as
  Unraid does, so this works for servers that read commands there, such as Terraria, Factorio, or Minecraft run
  directly. A server whose Docker options turn input off (`--interactive=false`) can't take commands this way.
- **RCON:** hlm signs in to the game's RCON port and shows the reply under the command. It works with games that speak
  the Source RCON protocol: Minecraft, Palworld, ARK, Rust (legacy RCON), Counter-Strike 2, Conan Exiles and others.
  - **Password:** from one of the container's variables (such as `RCON_PASSWORD` or `ADMIN_PASSWORD`), or entered by
    hand for games that keep it in a settings file. One entered by hand is stored encrypted and never shown again; it's
    hidden from replies, should a game echo it.
  - **Port:** the RCON port inside the container. hlm finds it from a variable like `RCON_PORT`, a usual RCON port the
    container exposes or publishes, or the game's usual one.
  - **Address:** hlm connects straight to the container on its networks, or else to the port published on the Docker
    host, and remembers what worked. If it can't reach the server, enter the address by hand.
  - **Servers with their own LAN address** (a macvlan or ipvlan network, such as Unraid's `br0`): a Docker host can't
    reach its own macvlan containers, so hlm, running on that host, fails with "no route to host". Connect hlm to
    that network too, with a free address on it (see the commented `networks` lines in `docker-compose.yml`), or use a
    command run in the container instead.
  - **Connection:** it's kept open between commands and closed after 5 idle minutes.
- **A command run in the container:** a command line run in the container for each command, with `{command}` replaced
  by what you type (or added at the end), such as `rcon-cli {command}`. It isn't run by a shell. Images like
  `itzg/minecraft-server` and Palworld's come with `rcon-cli`, which already knows the password.

**Console settings** suggest a setup when the server's image or settings make it clear: `rcon-cli` or `send-command`
for the images that have them, RCON when there's a password variable and a port, and typing into the console for games
known to read it. **Test** checks the settings before saving them: it signs in over RCON, looks for the command in the
container, or checks the container takes input.

- Commands are at most 1 KiB; control characters other than tabs are removed. Each person can send at most 5 commands a
  second to a server.
- Commands you sent before are kept in your browser: ↑ and ↓ go through them.
- Shortcuts above the command line put common commands for the game (Minecraft, Palworld, ARK, Rust, Terraria,
  Factorio, Counter-Strike 2, 7 Days to Die) and your own into the command line, for you to finish and send.
- hlm logs each command, with who sent it, but not the reply.

Console commands can do whatever the game's admins can, and some games can run scripts or change files through them,
so giving someone the **operator** role on a server lets them administer the game.

## Scheduled tasks

A server's **Schedule** tab runs **tasks** at set times: a nightly restart that warns players first, a save every half
hour, a weekly update, a nightly backup, or starting and stopping the server at set times. Each task has a schedule
and a list of steps, run in order:

| Step | What it does |
| --- | --- |
| **Start / Stop / Restart the server** | Starting a running server, or stopping a stopped one, is skipped. |
| **Update, if there's an update** | Checks the image's registry, and updates (as **Update** does) only if there's a newer image. |
| **Back up** | Runs the server's [backup plan](#backups-to-s3-seaweedfs-minio-) and waits for it to finish. |
| **Send a console command** | Through the server's [console](#sending-commands-to-a-server), which has to be set up. |
| **Wait** | 1 second to an hour, e.g. between a warning and the restart. |
| **Only carry on if…** | …nobody's playing, or someone is, from the server's [player count](#is-the-game-up-players-and-status). If not (or the count isn't known), the rest of the task is skipped. |

A step that fails stops the task, unless it's marked **carry on if it fails** (handy for warnings, which shouldn't stop
a restart if the console is down). **Skip the task when the server is stopped** (on by default) keeps a nightly restart
from starting a server someone stopped.

**When:**
- **At a time of day**, on chosen weekdays.
- **Every so often**: every 5 minutes to a week, counted from when the task was saved.
- **On a cron schedule**: five fields (minute hour day-of-month month day-of-week), with `*`, ranges, steps, lists and
  names (`30 4 * * mon-fri`, `0 */6 * * *`), or `@daily` and the like.

Times are in **hlm's time zone**, set under **System → Time zone and schedule** (the system's by default). It also
applies to notifications' quiet hours. When the clocks go forward, a time that doesn't exist that day runs when they
did (02:30 runs at 03:00); when they go back, a time that happens twice runs once.

**Add a task** offers ready-made ones (nightly restart with warnings, save every 30 minutes, weekly update, nightly
backup, start every evening, stop every night). They use the right commands for the game when the console knows which
it is, e.g. `Broadcast` and `Save` for Palworld, `say` and `save-all` for Minecraft.

- **Run now** runs a task straight away.
- A server runs one task at a time. A task that comes due while the server is being installed, updated, reconfigured,
  backed up or restored, or running another task, waits; after 10 minutes it gives up and counts as failed.
- If hlm wasn't running when a task was due, it isn't run late: it counts as **missed**, unless hlm is back within 15
  minutes.
- Each task keeps its last 5 runs, with how each step went. A failed or missed run sends the
  [`task.failed`](#notifications) notification.
- Restarts and stops by a task aren't reported as crashes.
- The server list shows each server's next task, and **System → Time zone and schedule** lists what's coming up on
  every server.

Tasks are kept by server name, encrypted with the other settings (commands can hold passwords), and deleted with the
server. Viewers see a server's tasks but not their commands; operators can also run them; managers can change them.

## Backups to S3 (SeaweedFS, MinIO, …)

A server's **Backups** tab copies folders you choose from its container to S3-compatible storage, such as SeaweedFS,
MinIO, Garage or AWS S3. Set up the bucket once under **System → Backup storage**:

- **Endpoint URL**: the S3 API's address, e.g. `http://seaweedfs.lan:8333` for SeaweedFS (`weed s3`, or
  `weed server -s3`). Plain `http://` works, which is fine on a trusted LAN.
- **Bucket**: it must already exist (for SeaweedFS: `echo 's3.bucket.create -name game-backups' | weed shell`).
- **Access key / secret key**: for SeaweedFS, an identity in its `-s3.config` file with `Read`, `Write` and `List`
  (or `Admin`) on the bucket. The secret is stored encrypted, like the other integrations' credentials.
- **Folder in the bucket** (default `hlm`) and **Region** (default `us-east-1`; SeaweedFS and MinIO accept any).
- Buckets are addressed path-style (`host/bucket`), which SeaweedFS and MinIO expect. Tick **Virtual-hosted buckets** for
  providers that need `bucket.host`.
- **Private CA certificate** (optional): for an `https://` endpoint whose certificate your own CA signed, paste the CA's
  certificate or upload its file (PEM `.crt`/`.pem`, or DER `.cer`). It's trusted only for backup storage, on top of the
  system's public CAs, so nothing else in hlm is affected. Only certificates are accepted; a pasted private key is
  refused. The System page shows which CA is trusted and when it expires. Without it, an untrusted certificate fails with
  a message saying so.

**Test connection** and **Save** list the bucket, then write and delete a small test file, so a read-only key is caught
right away.

On a server's **Backups** tab, add folders by browsing the container (or with the data folder shortcuts). Then choose a
**schedule** (off, or every 1 hour to 1 week), how many **backups to keep** (older ones are deleted after each successful
backup; 0 keeps all), and whether to **stop the server while backing up**. **Back up now** runs one straight away.

- Each backup is one `.tar.gz`, streamed from the container to the bucket in 16 MiB parts (S3 multipart upload), so
  nothing is staged on disk and backups of up to about 160 GB work. It's stored as
  `<folder>/<server>/<server>-<UTC time>.tar.gz`.
- Entries are named by their container path (`data/saves/world.sav` for `/data/saves/world.sav`), so a downloaded
  backup can also be extracted by hand at `/` in the container, or under the matching data folders on the host.
- Folders in data folders are read from the host when hlm can see it, as in the Files tab. Other folders (the
  container's own files, or a remote Docker host) are copied with Docker's archive API, which keeps other requests about
  that container waiting until the copy finishes. Data folders mounted inside a chosen folder are included.
- Games write saves while they run. A file that changes during the backup is reported, and the backup still completes.
  For a consistent copy, turn on **stop the server while backing up**: a running server is stopped first and started
  again afterwards.
- A scheduled backup that fails is retried after an hour. Each backup's result (and any files that were skipped or
  changed) is shown on the tab. **Download** fetches a backup through hlm; **Delete** removes it from the bucket.
- Plans and backups are kept by server name, so they carry over when a server is updated or reconfigured.

**Restore** puts a backup's files back in the container. A running server is stopped first and started again
afterwards, so the game doesn't overwrite the restored saves when it shuts down.

- Files in the backup replace the current ones, keeping the owner and permissions they had when backed up. Files that
  were created since are left in place, and nothing is deleted.
- **Back up the current files first** (on by default) saves the server's chosen folders as a new backup before
  restoring, so a restore can be undone by restoring that one. Old backups aren't deleted for it, and if it fails,
  nothing is restored.
- Files are written through Docker's archive API, which works whether the server is running or stopped and writes into
  its data folders. Files that would land in a read-only mount are skipped and listed as warnings.

### Backing up hlm's own configuration

Once backup storage is set up, hlm also backs up its own configuration there, **every day by default**, keeping the
last 14. **System → Backups of hlm's configuration** changes the schedule and how many to keep, runs a backup now, and
lists, downloads or deletes them. They're stored as `<folder>/_hlm/hlm-config-<UTC time>.tar.gz`, apart from the
servers' backups.

Each backup holds, named as they are in the data directory (`HLM_DATA_DIR`):

- `secrets/secrets.enc`: every integration's settings and credentials (UniFi, proxy and its SSH key, Linode, Cloudflare,
  backup storage), servers' port options and backup plans. Encrypted with AES-256-GCM.
- `templates.json` (templates made from images) and `deleted-servers.json`, when they exist.
- `servers.json`: every server's name, template, image, start on boot and settings. Servers live in Docker rather than
  the data directory, so this is what it takes to install them again on a new host. Their game data is in their own
  backups.
- `README.txt`, explaining the above.

The feed cache isn't included (it's downloaded again), and neither is game data kept under `<data dir>/appdata`.

**The encryption key.** `secrets.enc` can only be read with the key that encrypted it: `HLM_SECRET_KEY`, or else
`secrets/secret.key` in the data directory. The key file is left out by default, because with it anyone who can read
the bucket can read every stored credential, including the bucket's own keys. Either keep a copy of the key somewhere
safe (or set `HLM_SECRET_KEY` and keep that), or tick **Include the encryption key** so each backup can be restored on
its own. With `HLM_SECRET_KEY`, there's no key file to include.

To restore, stop hlm, extract a backup into its data directory (`/data` in the Docker image), put back the key if it
wasn't included, and start hlm again.

## A server's ports

**Host ports.** In the install and settings forms, each port field is the port on this host that Docker publishes the
game's container port on (shown next to it, e.g. `container 2456/udp`). That's how servers of the same game are told
apart: the game listens on 2456 in each container, and you publish one on host port 2456 and the next on 2466. With
host networking the container port is used on the host directly. On a LAN (macvlan/ipvlan) network nothing is published
and the field is ignored.

On bridge and host networks, hlm checks those host ports as you edit and refuses to install or save if one is already
taken by another container (running or stopped, since a stopped one takes its ports back when it starts) or, when the
Docker host is this machine, by another program listening on it. The port's field shows what uses it. Docker's own
listeners for published ports count as the container's, so a server's current ports never clash with themselves.

Each server's **Port forwarding** tab has a **Ports** table with everything about how the server is reached from the
internet:

- **Internet port**: the port players connect to. By default it's the server's own port. Changing it lets two servers of
  the same game, each on its own LAN IP and both listening on 2456, share your one public address: leave the first on
  2456 and move the second to 2466. **Shift internet ports by** moves every port by the same amount, which keeps ranges
  together. The server keeps listening on its own port. UniFi rules forward the internet port to it (e.g. WAN
  `2466-2468` → `192.168.1.62:2456-2458`), and the proxy listens on the internet port. Players connect with the new port
  directly; a game's server browser may still show the server's own port, because the game announces that itself.
- **UniFi**: whether the gateway forwards the port. Unticked ports get no rule, and **Update rules** removes rules made
  for them before.
- **Proxy**: whether the nginx proxy relays the port. **Relay this server through the proxy server** above the table adds
  the server to the proxy's relays or removes it (the same list as on the Proxy page).
- **Firewall**: whether a relayed port is opened in the proxy server's Linode firewall. Untick it for ports you open
  there yourself.

**Through the WireGuard tunnel.** When the server is relayed and the proxy's home address is the
[WireGuard tunnel](#wireguard-tunnel-to-this-host), the table describes that path instead: players connect to the proxy
server on the **proxy port**, and a **Goes to** column shows where it ends up (the tunnel's end on this host and the
host port, e.g. `10.77.77.2:2456`, or a LAN-mode server's own address and port). A line above the table spells out the
route. The UniFi column and the UniFi rules card are hidden, since nothing needs forwarding at home. If the gateway
still has rules for the server, they reappear with a warning, because those rules expose your home address: untick
them and update the rules.

Columns only appear for what's set up, and each column's header ticks or unticks all its ports. Internet ports that
another server also exposes are flagged. After saving, click **Update rules** (UniFi) and **Apply proxy** (nginx and the
firewall) to put the changes in place. Deleting a server forgets its port settings.

Each port shows whether it's actually in place: **forwarded**, **relayed** or **open** when it is; **not yet** when it's
waiting for Update rules or Apply proxy; **still forwarded/relayed/open** when it was turned off but is still in place;
**conflict** when another rule or relayed server already uses the internet port. Hover a badge for details. A line above
the table sums it up for UniFi, the proxy and the firewall. UniFi and the firewall are checked against the gateway's and
Linode's actual rules; the proxy against the configuration hlm last applied, since hlm is the only thing that writes
it.

## Port forwarding with UniFi

hlm can open servers' ports to the internet through a UniFi gateway's port forwarding, so players outside your network
can connect.

1. On the **System** page, under **UniFi port forwarding**, enter the controller's address (your UniFi console, e.g.
   `https://192.168.1.1`, or `https://host:8443` for a self-hosted Network controller) and sign-in details:
   - a **local account**: create a local-access-only admin for hlm (accounts with two-factor sign-in can't be used), or
   - an **API key** (UniFi OS: Control Plane → Integrations). Not every Network version accepts API keys for port
     forwarding; **Test connection** tells you, and a local account always works.
2. UniFi consoles use a self-signed certificate. hlm shows its SHA-256 fingerprint; compare it with the one your browser
   shows for the console and trust it. hlm then accepts exactly that certificate (it doesn't turn checks off).
3. Each server's **Port forwarding** tab lists the rules it needs, from its published ports (ranges and TCP+UDP pairs are
   merged; see [A server's ports](#a-servers-ports) to choose which are forwarded and on which internet port), and creates or updates them with one
   click. Traffic goes to this host's LAN address (detected, or set
   **Forward to**), or to the server's own address for servers on a LAN network, which then need a fixed IP.

**Address checks.** When UniFi is set up, a fixed IP on a LAN network is checked against what UniFi knows: devices
connected now, fixed-IP reservations (even for devices that are offline) and the gateway's own address. The form shows the
result as you type, and installs and setting changes are refused if the address is taken; an address inside the DHCP
range only gets a warning, since the router could hand it out later. If UniFi can't be reached, the change goes ahead with
a warning. Servers on LAN networks also get a stable MAC address derived from their name, so UniFi sees the same device
after every update instead of a new one, and a server's own address is never reported as taken.

Rules hlm creates are named `hlm:<server> <ports>/<proto>`; it only changes or removes those, and points out conflicts
with other rules on the same ports. Deleting a server removes its rules, and **Update rules** cleans up after a server's
ports change.

**Credentials** are stored encrypted (AES-256-GCM) in `<data>/secrets/secrets.enc`, readable only by the user hlm runs
as, and are never sent back to the browser. The key is `<data>/secrets/secret.key`, created on first use; to keep the key
away from the data (e.g. out of backups), set `HLM_SECRET_KEY` to a passphrase or a base64-encoded 32-byte key instead.
Changing the key makes stored credentials unreadable, so re-enter them afterwards. A saved password is only reused for
the same controller address and username.

These UniFi endpoints aren't an official, documented API, so a Network update could change them; the code is tested
against a simulated controller, not real hardware.

## Relaying through a proxy server (nginx over SSH)

The **Proxy** page puts another machine, typically a small VPS, in front of your game servers. Players connect to the
proxy, and its nginx relays their TCP and UDP traffic to your home network. Players never see your home address, and the
proxy keeps no record of their IP addresses.

1. **SSH key.** hlm generates an Ed25519 key the first time you open the page, and stores the private key encrypted (see
   the secrets store above). Add the public key to `~/.ssh/authorized_keys` on the proxy for the user hlm signs in as;
   the page has a ready-made command. You can import your own (passphrase-less) private key instead.
2. **Proxy server.** Enter its host, SSH port and user: `root`, or a user that can run `sudo` without a password. The
   first connection shows the server's host key fingerprint to compare with `ssh-keygen -lf
   /etc/ssh/ssh_host_ed25519_key.pub` on the server. After you trust it, hlm refuses any other key.
3. **Install nginx** if needed, with its stream module (apt on Debian/Ubuntu, dnf on Fedora/RHEL).
4. **What to relay.** Set the **home address** the proxy sends traffic to: your public IP or a dynamic-DNS name (with the
   ports forwarded at home, e.g. by the UniFi integration), or a VPN address. With UniFi set up, you can instead choose
   **Use the internet address from UniFi** (WAN or WAN2), and with hlm's [WireGuard tunnel](#wireguard-tunnel-to-this-host)
   set up, **Through the WireGuard tunnel**; see below. Then pick the servers.
   The proxy listens on each server's [internet ports](#a-servers-ports) (you can also leave ports out there). By default it sends traffic to the same port at
   the home address, where your router forwards it. For a VPN address that reaches this machine directly, choose **Send
   to the servers' own ports** instead. An internet port two servers share is only relayed for the first.
5. **Save & apply.** hlm writes `/etc/nginx/hlm-stream.conf` and the first time adds one `stream { include …; }` block to
   `nginx.conf`, backing it up as `nginx.conf.hlm-bak`. It then runs `nginx -t` and reloads nginx. If the test fails,
   the previous files are put back and nginx's message is shown. **Remove relays** empties hlm's file again.

Logging choices: **without IP addresses** (time, protocol, port, bytes and duration only) or **none**. Either way, the
relays' error log only keeps critical errors, because nginx's error messages include the client's address. hlm never
touches other nginx configuration, except to adopt an existing stream block (below). Remember to allow the game ports
in the proxy's firewall. A hostname as the home address is resolved when nginx loads its configuration, so apply again
(or reload nginx) after it changes.

**Adopting an existing stream block.** nginx allows only one `stream { }` block, so if the proxy server already has one
(in `nginx.conf` or any file it includes), hlm can't add its own. **Test connection** points this out, and **Save &
apply** stops with the same message. Click **Review the change** to see the block as it is and as it would be: hlm adds
`include /etc/nginx/hlm-stream.conf;` as the first line inside it and changes nothing else. **Add the include** keeps a
backup (`<file>.hlm-bak`), checks the result with `nginx -t`, and puts the file back if the check fails. It also
refuses if the file changed since you reviewed it. When started from a failed apply, it then applies. The block's own
servers keep working and keep their own logging; hlm's relays still log without client addresses. A port one of them
already listens on can't also be relayed by hlm (nginx refuses, and the previous configuration is kept). **Remove
relays** empties hlm's file but leaves the include in place, which is harmless.

### Linode Cloud Firewall

If the proxy server is a Linode protected by a Cloud Firewall, hlm can keep the firewall open for exactly the relayed
ports. On the **System** page, under **Linode firewall**, enter a personal access token (Cloud Manager → My Profile → API
Tokens) with **Firewalls: Read/Write** and **Linodes: Read Only**, test it, and save it. Then pick the firewall to manage
under **Firewall to manage** and click **Use this firewall**. The list shows which Linodes each firewall is assigned to,
and hlm preselects the one on the Linode whose address matches the proxy server's host. If the proxy is already applied,
the ports open straight away. The token is stored encrypted, like the other credentials, and never sent back to the
browser. **Change token** replaces it and keeps the chosen firewall. Section **6. Firewall** on the Proxy page shows the
firewall's state and links back to these settings.

From then on, **Save & apply** also opens the relayed ports in the firewall, and **Remove relays** closes them again.
hlm only manages inbound rules labelled `hlm-tcp-N`/`hlm-udp-N` (and `hlm-wireguard` for the
[WireGuard tunnel](#wireguard-tunnel-to-this-host)'s port, which stays open while the tunnel is set up, relays or not) (consecutive ports become ranges, split to fit Linode's
limit of 15 ports per rule). It adds them after the firewall's own rules and never changes those rules or the default
policies, so keep a rule allowing SSH. Linode allows at most 25 rules per firewall; if hlm's rules wouldn't fit, the
error says so. If the firewall can't be updated (e.g. the token expired), nginx is still applied and the page shows the
error along with an **Update firewall** button. Choosing a different firewall moves hlm's rules to the new one, choosing
**None** removes them, and **Disconnect** removes them before forgetting the token.

### WireGuard tunnel to this host

Section **4. WireGuard tunnel** on the Proxy page connects the proxy server and the Docker host with WireGuard, so nginx
relays to the host directly instead of to your public address. The host connects out to the proxy, so nothing has to be
forwarded at home, and it works behind carrier-grade NAT or a changing IP. Save the proxy server first (hlm sets up its
end over SSH), then click **Set up tunnel**:

- **On the proxy server**, hlm installs `wireguard-tools` if needed (apt or dnf), writes
  `/etc/wireguard/gsh0.conf` (mode 600) and starts it with `wg-quick@gsh0`, enabled at boot. It listens on UDP
  51820 by default. With a Linode firewall set up, hlm opens that port (rule `hlm-wireguard`); otherwise open it
  yourself.
- **On the Docker host**, hlm runs the `hlm-wireguard` container (`lscr.io/linuxserver/wireguard`, or
  `HLM_WIREGUARD_IMAGE`) with host networking and `NET_ADMIN`, restarting unless stopped. It brings up a `gsh0`
  interface on the host, connects to the proxy and keeps the connection open (`PersistentKeepalive = 25`). Its
  configuration, private key included, lives in the `hlm-wireguard` volume. With a remote `DOCKER_HOST`, this runs on
  that host, where the game servers are. hlm runs its own small entrypoint in the image (any image with `sh`, `ip` and
  `wg-quick` works) instead of the image's VPN-client init. That init deletes the default route when a tunnel fails, and
  on the host's network that route belongs to the host. Containers made by older versions of hlm are recreated this way
  when hlm starts.
- **Your networks are protected**: before bringing the tunnel up, hlm checks the tunnel network against the routes on
  the proxy server and on the Docker host. It refuses one that overlaps a network either machine uses or that holds its
  default gateway. If the gateway is cut off anyway, the container takes `gsh0` down again and reports why.
- **Addresses** come from the tunnel network (`10.77.77.0/24` by default): the proxy gets the first (`10.77.77.1`), the
  host the second (`10.77.77.2`). Pick another private range if that one is in use on either network. Only those two
  addresses are routed through the tunnel; neither machine's other traffic changes.
- **nginx**: with **Relay through the tunnel** ticked (the default), the home address becomes **Through the WireGuard
  tunnel**: nginx's `proxy_pass` goes to `10.77.77.2` on each server's own port (the port published on the host, not
  the internet port), and if the relays are applied, hlm applies them again straight away. Otherwise choose it under
  **3. What to relay** and apply.

While it runs, the card shows a console with each step and its output as it happens: the package install and
`wg-quick` on the proxy server, the image pull, and the `hlm-wireguard` container's startup log (up to "All tunnels are
now active"). It stays up afterwards, so you can read it back.

Replacing this host's end (**Set up again**, or **Update** under Connections on the Servers page) stops
`hlm-wireguard` gracefully first, so `wg-quick` takes the tunnel down cleanly. With host networking, a `gsh0` left on
the host by a container that was killed or crashed is deleted before the new one starts (by a short-lived
`hlm-wireguard-cleanup` container, since hlm may not run on the host's network itself); otherwise `wg-quick` would refuse
to start. If the tunnel still doesn't come up, the update fails with the container's error.

Keys are generated by hlm the first time and stored encrypted; **Change** and **Set up again** keep them. **Check
tunnel** shows the last handshake the proxy saw and whether it can ping the host through the tunnel. **Remove tunnel**
takes both ends down, removes the container and volume, closes the firewall port, and forgets the keys. It refuses while
applied relays go through the tunnel: choose another home address and apply, or remove the relays, first.

**Servers with their own LAN address.** Games often insist on their own ports, so several servers of one game are
easiest to run on a macvlan or ipvlan network, each with its own LAN address and the same ports. On the host's network,
the tunnel can't reach those addresses (a host can't talk to its own macvlan containers), so choose the network under
**Reach servers with their own LAN address**. The tunnel's end then runs differently:

- `hlm-wireguard` joins that network, with an address Docker picks or one you give (choose a free one outside your
  router's DHCP range), and Docker's default `bridge` network. It forwards IP traffic (`net.ipv4.ip_forward=1` in its
  own network namespace only).
- The proxy routes the network's subnet (e.g. `192.168.1.0/24`) through the tunnel as well as `10.77.77.2`.
- nginx sends each such server's traffic to its own LAN address and its own port, e.g. `192.168.1.61:2456` and
  `192.168.1.62:2456`. Servers that share ports still need different internet ports (set them under the server's
  ports), since the proxy has one address.
- Traffic for `10.77.77.2` (servers with published ports) is passed on to the host through the bridge's gateway, so
  those keep working.
- Everything forwarded is masqueraded, so game servers see connections from `hlm-wireguard`'s LAN address and reply
  through the tunnel, with no routes to add on your router.

In this mode the proxy server can reach that whole subnet through the tunnel, not just the game servers. Keep it on a
network meant for them if that matters to you.

Limits: the host's kernel and the proxy's need WireGuard (built into Linux 5.6 and later), and LAN mode needs `iptables`
in the image (the default image has it). A server with its own LAN address on a network other than the one chosen is
listed with an error and skipped. Ports published on one specific host address are only reachable there. A host firewall (e.g. ufw)
may need to allow the game ports on `gsh0` for servers using host networking; Docker's own published ports are
unaffected.

### Keeping the home address up to date from UniFi

If your ISP changes your public IP from time to time, let UniFi supply it. hlm reads the gateway's WAN address from UniFi
every 5 minutes (`HLM_WAN_CHECK`). When it changes, hlm applies the configuration again with the new address, using the
same `nginx -t` check and rollback. This only happens after you've applied the configuration yourself once, and stops
after **Remove relays**. The nginx section shows the address on the proxy, the address UniFi last reported, and any
error. **Check now** runs a check straight away. If UniFi reports a private or carrier-grade NAT address (100.64.0.0/10),
your gateway isn't reachable from the internet, so hlm reports an error and leaves the proxy alone. Use a VPN address
instead in that case.

## Web addresses with Cloudflare Tunnel

The **Tunnel** page gives servers with a web interface (admin panels, live maps, file managers) an address on your own
domain, like `panel.example.com`, through a [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/).
hlm runs Cloudflare's connector, `cloudflared`, on the Docker host. It connects out to Cloudflare, so nothing has to be
forwarded at home, it works behind carrier-grade NAT, and visitors never see your home address. Cloudflare serves each
hostname over HTTPS with its own certificate. The tunnel only carries web traffic (HTTP and HTTPS) from browsers; game
traffic, which is usually UDP, still needs the [proxy](#relaying-through-a-proxy-server-nginx-over-ssh) or
[port forwarding](#port-forwarding-with-unifi). Your domain must use Cloudflare's name servers.

1. **Cloudflare account.** Create an API token in the Cloudflare dashboard (My Profile → API Tokens → Create Token →
   Custom token) with **Account → Cloudflare Tunnel: Edit**, **Zone → DNS: Edit** and **Zone → Zone: Read**, for the
   domains you want to use. **Test connection** lists the accounts and domains it can use; **Save** stores it encrypted,
   like the other credentials. If the token can use several accounts, choose the one the tunnel goes in. The token
   doesn't need Account Settings: Read: hlm finds the account through the token's domains, and it only offers accounts
   where the token can actually manage tunnels. If a permission is missing, the error names that one permission.
2. **Tunnel.** **Set up tunnel** creates a remotely managed tunnel (named `hlm` unless you choose another name) and runs
   the `hlm-cloudflared` container (`cloudflare/cloudflared:latest`, or `HLM_CLOUDFLARED_IMAGE`) with
   `tunnel --no-autoupdate run`, host networking and restart unless stopped. Its token goes in the container's
   environment (`TUNNEL_TOKEN`), so anyone with access to Docker can read it. A console shows each step, the image pull
   and cloudflared's startup log, up to its first connection. hlm refuses to use a tunnel with the same name that it
   didn't create.
3. **Hostnames.** Each hostname sends visitors to one of a server's TCP ports (a published host port, or the server's
   own port on the host's network or its own LAN address), or to [another web server](#web-addresses-with-cloudflare-tunnel)
   by address. A server's web UI port, from its template, is picked first.
   Choose **HTTPS** for servers that only speak HTTPS, and tick **self-signed** if their certificate is self-signed.
   **Save & publish** sets the tunnel's ingress rules (anything else gets a 404) and adds a proxied `CNAME` record to
   `<tunnel id>.cfargotunnel.com`, commented `Managed by Home Lab Manager`. Hostnames can be added before the tunnel
   exists; they're published when it's set up.

**Other web servers.** A hostname can also go to a web server hlm doesn't run, such as a NAS, a router's admin page or
Home Assistant: choose **Other server…** and enter its IP address or host name (`192.168.1.20`, `nas.lan`) and port.
cloudflared resolves the name and connects from where it runs: with host networking, `localhost` is the Docker host and
LAN addresses are reached through the host; on a LAN network (below), it reaches the LAN directly, but not the Docker
host's own address. Leave the subdomain empty to publish on the domain itself (`example.com`). Deleting a game server
leaves these hostnames alone.

Each hostname shows where cloudflared sends it (e.g. `http://localhost:8080`) and whether it's **published**, **not
yet** published, or has a **conflict** or an **error**. A server's **Port forwarding** tab lists its hostnames too.

- hlm only changes or removes DNS records it made. A hostname that already has another `A`, `AAAA` or `CNAME` record is
  shown as a conflict and isn't published; delete that record in Cloudflare or choose another hostname.
- Cloudflare's free certificate covers one level of subdomain: `panel.example.com`, but not `a.panel.example.com`.
- Published hostnames are open to anyone. Make sure the web interface has a password, or put it behind
  [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/).
- Every 5 minutes hlm checks whether a server's address or ports changed (after it was reconfigured, say) and publishes
  again. A server with its own LAN address has none while stopped, so its hostname keeps the last published address.
- Deleting a server removes its hostnames and their DNS records.

**Servers with their own LAN address.** A host can't reach its own macvlan/ipvlan containers, so to publish those choose
the network under **Reach servers with their own LAN address**, optionally with a fixed address for cloudflared
(outside your router's DHCP range). `hlm-cloudflared` then runs on that network and Docker's default bridge: it reaches
those servers at their own address, and published ports through the bridge's gateway (e.g. `http://172.17.0.1:8080`).
Ports published only on `127.0.0.1` can't be reached in this mode.

The **Tunnel** card shows the container's state and Cloudflare's view of the tunnel (healthy, degraded, down or not yet
connected, with the Cloudflare data centers it's connected to), and has **Start**, **Stop** and **Restart**. **Change**
and **Set up again** recreate the container (pulling the latest image) and keep the tunnel. The **cloudflared log**
follows the container's output live. **Remove tunnel** removes the container, the DNS records hlm made and the tunnel in
Cloudflare; the hostnames are kept for the next tunnel. The token can only be forgotten (**Disconnect**) once the tunnel
is removed. With a remote `DOCKER_HOST`, cloudflared runs on that host, where the servers are.

## Downloads in progress

Installs, updates and settings changes run on the backend, and the server list shows their progress to everyone, not
just the browser that started them. New servers appear under **Downloads in progress** with the image download (bytes,
layers and percentage) until they're created. Servers being updated show the same progress on their own card. Only one
operation can run on a server at a time. Progress covers the Docker image download; game files that a container
downloads itself on first start (e.g. through SteamCMD) show up in the server's console instead.

## Host metrics

The **Host** page shows the Docker host's CPU, memory, network and disk activity (current values plus 30 minutes of
history, sampled every 5 seconds), its filesystems, Docker's own disk usage, and disk usage per server. Per-server usage
is split into data folders (bind mounts and volumes, measured like `du -s`), the container's writable layer, and its
image. Data folders are measured in the background every 10 minutes or when you click **Recalculate**, since large game
installs take a while to scan. A folder mounted inside another of the same server's folders is only counted once, and
folders shared between servers are marked.

**Deleted servers.** Deleting a server keeps its data, so hlm records the server's data folders and volumes (with their
sizes at the time) in `<data>/deleted-servers.json`. The Host page lists them under **Deleted servers**, measured along
with everything else, and shows folders that are already gone or that another server uses now (e.g. after a reinstall).
For each one:

- **Delete** removes it from disk for good, after asking. Volumes are removed through Docker, which refuses while a
  container uses one. Folders are deleted by a short-lived `busybox` container, because hlm may not be able to write
  there itself (it sees the host read-only in a container, and game files often belong to another user). hlm refuses
  to delete:
  - a folder that isn't in the record;
  - a system or top-level folder;
  - the appdata root or anything containing it;
  - a folder that any container, managed or not, still uses or mounts something inside.
- **Stop tracking** only removes it from the list. **Stop tracking all** does that for the whole server.

These figures come from the host's `/proc` and filesystems:

- **hlm running directly on the host:** works out of the box.
- **hlm in a container:** mount the host's `/proc` and root filesystem read-only and point hlm at them, as
  `docker-compose.yml` does: `-v /proc:/host/proc:ro -v /:/host/rootfs:ro` with `HLM_HOST_PROC=/host/proc` and
  `HLM_HOST_ROOT=/host/rootfs`. Disks mounted after hlm starts appear once it restarts. Adding `,rslave` to the root
  mount shows them right away, but only works where `/` is a shared mount on the host; elsewhere (Unraid, Docker
  Desktop, WSL) Docker refuses with "path / is mounted on / but it is not a shared or slave mount".
- **Remote Docker host:** the host can't be read from here, so the page only shows Docker's figures (image and container
  layer sizes per server, and Docker's totals).

## Resource usage

The server list shows each running server's CPU, memory and network use, and the server page adds disk I/O and process
count. Both refresh every 5 seconds. CPU is shown as a share of the whole host (the server page also shows cores in use),
so it compares directly with memory. Memory is shown against the container's limit, or the host's total memory when
there is no limit. Network and disk figures are totals since the container started.

## Any Docker image

Images without a Community Applications template can be added from the catalog with **Add a Docker image**: enter a
reference such as `itzg/minecraft-server:latest`, and hlm pulls it and builds a template from the defaults inside it:

- **Exposed ports** become ports, published on the same port of the host by default (each can be switched off).
- **Volumes** become data folders under the appdata root (`<appdata>/<image>/<path>`).
- **Environment variables** become variables, without the usual base-image noise (`PATH`, `HOME`, …). Build details such
  as `*_VERSION`, `LANG` or `JAVA_*` are under advanced settings, and names containing password, secret or token are
  masked.
- **Labels** fill in the name, description and links (OCI `org.opencontainers.image.*`, plus Unraid's icon and WebUI
  labels when present).

Variables left at the image's default aren't set on the container, so a newer version of the image can change its own
defaults; only the ones you change are passed. These templates are listed under the **Custom** category, saved in
`<data>/templates.json`, and work like any other: installs, settings changes and updates. **Read the image again**
refreshes a template; **Delete template** removes it (servers made from it keep running). Images that need
`docker login` can't be pulled.

## Additional settings

As in Unraid's container editor, the install and settings forms can add things a template doesn't include:

- **Additional settings:** extra paths (bind mounts), ports or port ranges, variables, labels and devices. An extra
  variable with the same name as one in the template overrides it.
- **Docker options:**
  - *Extra parameters* are raw `docker run` flags (`-e`, `-v`, `-p`, `--memory`, `--cpus`, `--ulimit`, `--cap-add`,
    `--device`, `--gpus`, `--restart`, …). Unsupported flags are skipped with a warning.
  - *Post arguments* are the container command.
  - *Privileged* runs the container in privileged mode.

  These start with the template's values. A server only stores them if you change them, so a template update still
  reaches servers that kept the template's values.

In the API these are `extra` (a list of `{type, name?, target, value, mode?}`) and `extraParams`, `postArgs` and
`privileged` on the install or config request. Leaving one out uses the template's value; an empty string clears it.

## Networks and static IPs

Each server can join any Docker network, chosen in the **Network** section of the install or settings form (or with
`network` in the API). A static IP can be set (`ipAddress`) on networks that support one.

| Network                          | Ports                         | Static IP | Players connect to         |
|----------------------------------|-------------------------------|-----------|----------------------------|
| `bridge` (Docker default)        | Published on the host         | No        | Host IP : host port        |
| User-defined bridge with subnet  | Published on the host         | Yes       | Host IP : host port        |
| macvlan / ipvlan                 | Not published                 | Yes       | Server's own LAN IP : container port |
| `host`                           | Uses the host's ports directly | No        | Host IP : container port   |

Docker only allows a static IP on a user-defined network that was created with a subnet. You can create one from the
form: click **+ New network** (or **Create a network for fixed IPs**) in the Network section and pick:

- **LAN network** (macvlan): each server gets its own address on your network, like Unraid's `br0`. hlm suggests the
  host interface, subnet and gateway from the host's routing table, and a block at the top of the subnet for automatic
  addresses (keep it outside your router's DHCP pool).
- **Private network** (bridge): fixed addresses on this host only; ports are still published as usual.

Networks created in hlm can be deleted from the same place once no server uses them. The equivalent commands, for
reference:

```sh
docker network create --subnet 172.30.0.0/24 --gateway 172.30.0.1 games
```

To give each server its own address on your LAN, like Unraid's `br0`, create a macvlan network on the host's network
interface. Use a range your router's DHCP doesn't hand out:

```sh
docker network create -d macvlan \
  --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.192/27 \
  -o parent=eth0 br0
```

Unraid templates that ask for `br0` use a network with that name if one exists; otherwise they fall back to `bridge`
with a warning. hlm checks that a static IP is inside the network's subnet, isn't the gateway, and isn't used by another
container. It can't see other devices on your LAN, so keep macvlan addresses outside your DHCP pool.

With macvlan, the Docker host itself can't reach the containers (a Linux kernel limitation); other machines on the LAN
can. Web UI links for macvlan servers use the server's own IP.

## Notifications

**System → Notifications** sends alerts to places you choose when something needs attention. Add one or more
**targets**:

| Kind | What it needs |
| --- | --- |
| **Discord** | A channel's webhook URL (channel settings → Integrations → Webhooks). |
| **ntfy** | The server (e.g. `https://ntfy.sh`), a topic, and an access token if the topic is protected. |
| **Gotify** | The server's address and an application token. |
| **Webhook** | Any URL, plus optional headers (e.g. `Authorization`). hlm POSTs JSON: `{kind, severity, title, message, server, link, time, fields}`. |
| **Email** | An SMTP server (STARTTLS on 587, TLS on 465, or none on a trusted network), optional sign-in, From and To. |

Webhook URLs (which carry their token), tokens, header values and passwords are stored encrypted, like the other
integrations' credentials, and never shown again: leave them empty when editing to keep them. **Send a test** checks a
target before or after saving it.

Each target chooses its **events** (warnings and errors by default):

| Event | Severity | When |
| --- | --- | --- |
| `server.crashed` | error | A server exits with an error, or runs out of memory, without anyone stopping it. The message includes its last lines of output. |
| `server.unhealthy` | warning | A server's Docker health check starts failing. |
| `server.online` | info | A server hlm saw start answers its game's [status query](#is-the-game-up-players-and-status): it's ready for players. |
| `server.not_responding` | warning | A running server stopped answering its status query, or never started answering. |
| `server.update_available` | info | A newer image is found for a server (once per image the server runs). |
| `server.updated`, `server.update_failed` | info, error | An update finishes or fails. |
| `hlm.update_available` | info | A new version of hlm is out (once per version). |
| `hlm.update_failed` | error | hlm couldn't [update itself automatically](#automatic-updates). |
| `backup.failed`, `backup.succeeded` | error, info | A server's or hlm's configuration backup fails or finishes. |
| `restore.finished`, `restore.failed` | info, error | A restore finishes or fails. |
| `tunnel.down` | error | The WireGuard or Cloudflare Tunnel container stops on its own. |
| `wan.changed`, `proxy.update_failed` | info, error | The home address changed and the proxy was updated, or updating it failed. |
| `https.renew_failed`, `https.expiring` | error, warning | Getting or renewing hlm's certificate fails, and when it's getting close to expiring without being renewed. |
| `disk.low` | warning | A filesystem on the Docker host gets fuller than the threshold (90% by default), checked every 5 minutes. |
| `auth.locked_out` | warning | Sign-in for a local account is locked after repeated wrong passwords. |
| `task.failed` | error | A [scheduled task](#scheduled-tasks) failed, or was missed because hlm wasn't running. |

- **Which servers:** a target can be limited to some servers and [server groups](#roles-and-server-groups). Alerts that
  aren't about a server (a full disk, a new hlm version) always go to it.
- **Repeats are held back:** the same alert about the same thing isn't sent again for a while: 15 minutes for crashes
  and tunnels, an hour for health checks and lockouts, 6 hours for failed backups, renewals and proxy updates, a day for
  disk space and expiring certificates. A success clears it, so the next failure after a successful backup is sent
  straight away. Updates, restores and successful backups are always sent.
- **Quiet hours:** a target can have quiet hours (e.g. 22:00 to 07:00, in [hlm's time zone](#scheduled-tasks)). Errors are still sent; the
  rest are sent when the quiet hours end. Alerts held this way are lost if hlm restarts first.
- **What counts as a crash:** hlm follows Docker's events. A container that exits after being stopped, restarted or
  killed (by hlm, `docker stop`, a backup that stops the server, or Docker shutting down) didn't crash, and neither did
  one that exited cleanly with code 0. Exits during an install or update aren't reported either; the update reports its
  own result. hlm only sees what happens while it's running.
- **Links:** messages link to the page they're about. Set **hlm's address** (how you open it in a browser) for that;
  when [HTTPS](#https) is on, its address is used by default.
- **Recent alerts** lists the last 200 events and where each went. It's kept, with what was sent recently, in
  `<data>/notifications.json`.

Messages leave out passwords and tokens that errors sometimes carry (in URLs and query strings), but a crash report
includes the server's last lines of output, so send them only to places you'd trust with the server's logs.

## Sign-in, accounts and API keys

By default anyone who can reach hlm can use it. **System → Sign-in** sets up who may:

- **Local accounts:** a username and password kept by hlm (hashed with argon2id). They work whether or not single
  sign-on is set up, so keep one for when your provider is down.
- **Single sign-on** through an OpenID Connect provider: Authelia, Authentik, Keycloak, Pocket ID and the like (see
  below).
- **API keys** for scripts and other programs (see [below](#api-keys)).
- **`HLM_TOKEN`**, if set, works like an API key that's configured on the host.

**Requiring sign-in.** Add an account (or set up single sign-on), sign in with it, then click **Require sign-in**. It
can only be turned on while you're signed in, so there's a way in that's known to work. From then on the API refuses
requests without a sign-in, an API key or `HLM_TOKEN`; the web UI asks you to sign in. Sign-ins last 24 hours by
default (adjustable there), as a signed, HttpOnly cookie that survives restarts. **Sign out** is at the top right.

While sign-in is required, hlm won't remove the last way in: the only local account when there's no working single
sign-on, or single sign-on when there are no local accounts.

**Locked out?** On the host, `docker exec home-lab-manager hlm reset-password admin` prints a new random password for
`admin` (creating the account, as an admin, if there isn't one), ending its existing sessions. Add `--admin` to make an
existing account an admin. For a binary install, run `hlm reset-password admin` with the same `HLM_DATA_DIR` (and `HLM_SECRET_KEY`, if set) as the server.

**Local accounts** need passwords of at least 10 characters. After five wrong passwords for a username, more attempts
are refused for 30 seconds, doubling with each further wrong password up to 15 minutes. Changing a password ends the
account's other sessions; removing an account ends all of them.

### Roles and server groups

Everyone who signs in has a global role:

- **Admin** can do everything, including integrations, the proxy, the tunnel, the host page, accounts and groups.
- **Member** can use only the servers their groups give them and change their own password.
- **Deployer**, for API keys only, is a member that can also [create servers that can't reach the host](#deployer-keys).

**System → Server groups** puts servers in groups and says who may use them: local accounts, single sign-on users or
groups, and API keys. A server can be in several groups; the highest role applies. Servers can also be added on their
own **Settings** tab.

| Role in a group | Can |
| --- | --- |
| **Viewer** | See the server, its stats, its game status and player count, its port and backup status and its scheduled tasks, but not its settings, logs, files or tasks' console commands, which can hold passwords |
| **Operator** | Also start, stop, restart and update it, [send it console commands](#sending-commands-to-a-server), run its scheduled tasks, read its logs, settings and files, and take and download backups. Settings that look like secrets (names with `TOKEN`, `SECRET`, `PASS`, `PWD`, `KEY`, `CREDENTIAL`, `AUTH` or `PRIVATE`, items the template masks, extra parameters and post arguments) are left out |
| **Manager** | Also change its ports and variables, edit files, set up its console, change its scheduled tasks and how it's updated, set start on boot and port forwarding, change the backup plan, and restore and delete backups |

Some things always take an admin, because they amount to control of the host or of other servers: deleting servers,
installing them (except for [deployer keys](#deployer-keys)), importing templates, and changing a server's paths, devices, network, extra items or Docker options.
Hostnames in the tunnel and connection containers (cloudflared, WireGuard, hlm itself) are admin-only too. Servers
someone can't see answer 404, as if they didn't exist.

- **Treat operators as able to see a server's secrets.** Hiding secret-looking settings is a courtesy, not a
  boundary: an operator can read the server's files and logs, which usually hold the same passwords, and its
  environment through `/proc`. Don't give operator, or above, on a server whose secrets matter more than the server,
  such as an identity provider whose keys other systems trust.
- **A group only contains what the server itself can reach.** Operators and managers run code in the container, so if
  an admin gave a server `--privileged`, the Docker socket or a mount shared with another server, whoever manages it can
  use those too. Keep such servers out of groups, or give viewer only.
- **Sign-in has to be required for this to mean anything.** While it isn't, anyone who can reach hlm is an admin, and
  `HLM_TOKEN` is always an admin.
- **After upgrading,** existing accounts and API keys are admins, as they could do everything before. New accounts and
  keys are members unless you choose otherwise. The first account is always an admin.
- **With single sign-on,** set **Admin users** and **Admin groups**. With neither, everyone let in is an admin; with
  either, everyone else is a member. A change in the provider's groups applies at the next sign-in; roles and grants
  set in hlm apply at once, though a log stream that's already open keeps going until it reconnects.
- hlm won't demote or remove the last admin way in while sign-in is required. If it's lost anyway,
  `hlm reset-password <user> --admin` makes the account an admin.
- Changes made through the API are logged with who made them and from which address.

### Confirming it's you

Changes that could give someone lasting access take a person at a browser who has confirmed it's them in the last 10
minutes: making API keys and changing where they work from, adding SSH keys, adding accounts and setting their
passwords and roles, changing server groups, and changing sign-in and single sign-on settings. hlm asks for your
password, or, if you signed in with single sign-on, sends you to sign in there again. hlm checks the provider's
`auth_time` to make sure it really asked, so a provider that ignores `prompt=login` can't confirm; use a local account
then.

API keys, SSH tokens and `HLM_TOKEN` can't make these changes, even with the admin role, so a stolen key can't make
itself permanent. They can still revoke: delete keys, SSH keys, accounts and groups. `HLM_TOKEN` can also add the
first account, so a new install can be set up, and `hlm reset-password` on the host is the way back in if every admin
is locked out. While sign-in isn't required, everyone is an admin anyway, so nothing asks.

### HTTPS

hlm serves plain HTTP until you turn HTTPS on under **System → HTTPS**. Passwords, session cookies and tokens then
travel encrypted. It's optional: left off, hlm works as before, and its session cookies aren't marked `Secure`, because
browsers don't send `Secure` cookies back over plain HTTP.

The certificate is a free one from Let's Encrypt, proved with a DNS record that hlm makes through the
[Cloudflare connection](#web-addresses-with-cloudflare-tunnel) and removes after, so hlm needn't be reachable from the
internet. You need:

- a name for hlm in a Cloudflare zone, like `hlm.home.example.com`, that points at the server on your network: a local
  DNS entry (in your router or Pi-hole), or a Cloudflare record with its private address (`192.168.11.101`). Let's
  Encrypt doesn't issue certificates for IP addresses, so open hlm by the name.
- the Cloudflare connection saved, with a token that can edit that zone's DNS (which tunnel hostnames need anyway).

hlm then gets a certificate, renews it when a third of its life is left, and serves HTTPS on the same port as HTTP
(`https://hlm.home.example.com:8080`). Only once there is a certificate is plain HTTP sent to HTTPS, so hlm stays
reachable while it's being issued or if renewing fails, until the certificate expires. The redirect is temporary, so
turning HTTPS off works straight away; connections already open close within two minutes. Cookies are `Secure`
whenever the request came over HTTPS. **Use Let's Encrypt's staging server** for trying it out: it allows many more
attempts, but browsers don't trust its certificates.

Things to change when you turn it on:

- Anything that reaches hlm at `http://`, like a tunnel route to `http://…:8080` or a script, gets a redirect. Point
  it at `https://` and the name instead.
- With single sign-on, the redirect URL registered with the provider (`http://…/auth/callback`) has to become
  `https://`, and the **Public URL** in hlm's single sign-on settings too.
- The certificate's name is public: Let's Encrypt logs every certificate it issues.

If HTTPS goes wrong and you can't get in, start hlm with `HLM_TLS=off` to serve plain HTTP only, whatever is saved.
Another ACME CA, like `step-ca` for a private CA, works with `HLM_ACME_DIRECTORY` and `HLM_ACME_CA`.

### Single sign-on with Authelia (OpenID Connect)

hlm sends the browser to the provider, which signs the person in (with two-factor, if the provider asks for it) and
sends them back. It works without a reverse proxy in front, including through a
[Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel).

1. **System → Single sign-on:** enter the provider's issuer URL (for Authelia, its own address), the client ID and the
   address you open hlm at. hlm generates the client secret (64 random characters); **generate a new one** replaces
   it, or type in one the provider gave you.
2. **Register hlm with the provider** as a confidential client with that secret and the redirect URI
   `<hlm's address>/auth/callback`, e.g. `https://hlm.example.com/auth/callback`. For Authelia, the card shows the
   block to add under `identity_providers.oidc.clients`, with the secret's PBKDF2-SHA512 digest already filled in, so
   the secret itself never goes in Authelia's configuration; restart Authelia afterwards. Other providers take the
   secret itself (**copy** it from the card).
3. **Back in hlm,** optionally limit sign-in to some users (usernames or emails) or groups (the `groups` scope is
   needed for those). **Test** checks the provider can be reached; **Save** stores the settings,
   with the client secret encrypted like other credentials.
4. **Sign in with the provider to check** the settings work.

hlm uses the authorization code flow with PKCE and checks the ID token (signature, issuer, audience, expiry, nonce).
Names, emails and groups also come from the provider's userinfo endpoint, where Authelia puts them by default. Who may
sign in is checked again on every request, so narrowing it applies to people already signed in. Signing out of hlm
leaves the provider signed in, so signing in again may not ask for a password. **Remove** signs everyone out.

If the provider is down, sign in with a local account. `HLM_OIDC=off` makes hlm ignore the single sign-on settings.

### API keys

**System → API keys** makes keys for scripts and other programs, such as Home Assistant. Give each a name, a
[role](#roles-and-server-groups) and, optionally, an expiry (30 days, 90 days or a year). The key (`hlm_<id>_<secret>`) is shown once; hlm keeps only a hash
of it. Send it as a bearer token:

```sh
curl -H "Authorization: Bearer hlm_…" https://hlm.example.com/api/servers
```

Keys work whether or not sign-in is required, and don't need the `X-HLM-Request` header that cookie sign-ins do. Send
them only in the `Authorization` header; a token in the URL (`?token=`) isn't accepted, since URLs end up in logs and
browser history. An admin key can do everything except the changes that [need you at a browser](#confirming-its-you);
a member key only what server groups give it; a [deployer key](#deployer-keys) can also create servers.

**Allowed from** limits a key to addresses and ranges, like OpenSSH's `from=` (`10.0.3.115, 192.168.11.0/24`). It's
checked on every request, against the address the connection comes from; `X-Forwarded-For` is ignored, since anyone can
send it. So behind a proxy or Cloudflare Tunnel, requests come from the proxy's address: limit those keys to it, or
don't limit them. Click a key's addresses in the list to change them. The list shows when each key was last used (updated at most hourly); **Revoke** stops a
key straight away.

### Deployer keys

A key with the **Deployer** role can create servers without being able to reach the host, for a CI job or a script you
don't want holding an admin key. Make the key, then give it the **manager** role in a server group; its servers go in
that group. Send `group` with the group's ID if it manages more than one:

```sh
curl -H "Authorization: Bearer hlm_…" -d '{"templateId":"…","name":"valheim-2","values":{"SERVER_PASS":"…"}}' \
  https://hlm.example.com/api/servers
```

hlm checks the finished container, so these limits apply whatever the template or request says. If the template needs
more, the install fails and says what, and an admin has to create the server.

- **Folders:** each of the template's data folders becomes a new folder under `<appdata root>/<server name>/`, and no
  others can be mounted. A name whose folder already has data is refused, so a server can't pick up old files.
- **Refused:** privileged mode, added capabilities, devices and GPUs, kernel settings, other container runtimes,
  relaxed security profiles, and hlm's own labels.
- **Network:** only `bridge` or `none`, so no host networking, LAN addresses or networks shared with other containers.
- **Ports:** host ports 1024 and up.

These stay in force whenever the server is recreated, including by an update after its template has changed, and for
the key's own settings changes. An admin can lift them on a server by giving it something a confined server can't have.

Confined isn't isolated from everything:

- It can still reach your network and anything listening on the host, like any container on `bridge`. Keep the Docker
  API off TCP, and require sign-in on hlm.
- Root in the container can create setuid files in the server's folder. Someone with a shell on the host could run
  them, so keep the appdata root out of reach of untrusted host users.
- Nothing limits how many servers a key creates or the disk and memory they use.
- The folder check also looks on disk only when hlm can see the Docker host's filesystem. Otherwise it relies on hlm's
  records of running and deleted servers, so it can't see folders it didn't know about.

### SSH keys

A local account can have SSH public keys, so a script can use the API as that account, with its role and groups, without
holding its password or a long-lived key. Add one under **System → Sign-in** (admins, for any account) or **System → Your
account**. Adding one takes [confirming it's you](#confirming-its-you), so it can't be done with an API key or token. Set
**Allowed from** to the addresses the key is used from, as for API keys; it's checked at sign-in and on every request
with the token.

`hlm ssh-login` signs in and prints a token, using the agent at `SSH_AUTH_SOCK` if there is one (only the `.pub` file
is needed then), or the private key file:

```sh
token=$(hlm ssh-login -url http://192.168.11.101:8080 -user alice -key ~/.ssh/id_ed25519.pub)
curl -H "Authorization: Bearer $token" http://192.168.11.101:8080/api/servers
```

The `hlm` binary from a release works as the client on any Linux machine with `ssh-keygen`. Without it, ask for a challenge, sign it with `ssh-keygen`, which can use `ssh-agent` (`-U`) or a hardware key, and swap the signature for a token:

```sh
HLM=https://hlm.example.com
challenge=$(curl -s -d '{"username":"alice"}' $HLM/api/auth/ssh/challenge | jq -r .challenge)
signature=$(printf %s "$challenge" | ssh-keygen -Y sign -n hlm-login -f ~/.ssh/id_ed25519)
token=$(jq -n --arg c "$challenge" --arg s "$signature" '{challenge: $c, signature: $s}' |
  curl -s -d @- $HLM/api/auth/ssh/login | jq -r .token)
curl -H "Authorization: Bearer $token" $HLM/api/servers
```

A challenge lasts two minutes, works once and doesn't survive a restart of hlm. Failed sign-ins count towards the same
limit as wrong passwords, for each username and address, and are logged with the address. A token lasts an hour and stops working as soon as its key or account is
removed or the account's password changes. Changing a password doesn't remove the account's keys, so if an account may
have been taken over, check them as well. Keys can be ed25519, ECDSA or RSA of at least 2048 bits. They're for local
accounts; single sign-on users don't have one in hlm, so they use API keys.

## Configuration

| Variable           | Default                       | Description                                                |
|--------------------|-------------------------------|------------------------------------------------------------|
| `HLM_LISTEN`       | `:8080`                       | HTTP listen address.                                       |
| `DOCKER_HOST`      | `unix:///var/run/docker.sock` | Docker daemon address (`unix://` or `tcp://`).             |
| `DOCKER_TLS_VERIFY`| (none)                        | Use TLS and verify the daemon's certificate.               |
| `DOCKER_CERT_PATH` | `~/.docker` with TLS verify   | Directory with `ca.pem`, `cert.pem`, `key.pem`; enables TLS. |
| `HLM_DATA_DIR`     | `./data`                      | Feed cache and default appdata location.                   |
| `HLM_APPDATA_ROOT` | `$HLM_DATA_DIR/appdata`       | Host directory that replaces Unraid's `/mnt/user/appdata`. |
| `HLM_TOKEN`        | (none)                        | If set, API requests need `Authorization: Bearer <token>`. |
| `HLM_TLS`          | (none)                        | `off` ignores the [HTTPS](#https) setting and serves plain HTTP only. |
| `HLM_ACME_DIRECTORY` | Let's Encrypt               | Another ACME CA's directory URL for HTTPS certificates, e.g. step-ca. |
| `HLM_ACME_CA`      | (none)                        | PEM file of extra CA certificates to trust for that ACME CA. |
| `HLM_ACME_RESOLVER`| `1.1.1.1:53`                  | DNS server checked for the challenge record before the CA is asked to look. |
| `HLM_WEB_DIR`      | (none)                        | Directory of built frontend files to serve (`web/dist`).   |
| `HLM_FEED_URL`     | CA feed                       | Override the application feed URL.                         |
| `HLM_FEED_REFRESH` | `6h`                          | How often to refresh the feed.                             |
| `HLM_HOST_PROC`    | `/proc`                       | The Docker host's procfs, for the Host page.               |
| `HLM_HOST_ROOT`    | (none)                        | Where the host's root filesystem is mounted, when hlm runs in a container. |
| `HLM_UPDATE_CHECK` | `12h`                         | How often to check servers and hlm itself for updates; `0` disables. When set, it wins over the setting under System → Automatic updates. |
| `HLM_WAN_CHECK`    | `5m`                          | How often to look up the home address from UniFi for the proxy; `0` disables. |
| `HLM_LINODE_API`   | `https://api.linode.com/v4`   | Linode API address (only changed for testing).            |
| `HLM_WIREGUARD_IMAGE` | `lscr.io/linuxserver/wireguard:latest` | Image for this host's end of the proxy's WireGuard tunnel. |
| `HLM_CLOUDFLARED_IMAGE` | `cloudflare/cloudflared:latest` | Image for the Cloudflare Tunnel's connector.          |
| `HLM_CLOUDFLARE_API` | `https://api.cloudflare.com/client/v4` | Cloudflare API address (only changed for testing). |
| `HLM_SECRET_KEY`   | (key file)                    | Key for encrypting stored credentials; see Port forwarding with UniFi. |
| `HLM_UPDATE_REPO`  | `danotchuuy/home-lab-manager` | GitHub repository hlm's own releases come from.            |
| `HLM_UPDATE_IMAGE` | `<repo>` (Docker Hub)         | Image repository hlm's releases are pushed to.             |
| `HLM_UPDATE_API`   | `https://api.github.com`      | GitHub API base URL (e.g. for GitHub Enterprise).          |
| `HLM_DEBUG`        | (none)                        | Log every API request.                                     |
| `HLM_OIDC`         | (none)                        | `off` ignores the [single sign-on](#single-sign-on-with-authelia-openid-connect) settings. |

Each variable also reads its `GSH_*` name from before the [short name changed](#short-name-changed-from-gsh-to-hlm),
when the `HLM_*` one isn't set.

> **Security:** access to the Docker socket is root on the host. Anyone who can reach this API can run any container.
> Set `HLM_TOKEN` or [require sign-in](#sign-in-accounts-and-api-keys), and keep the port off the public
> internet. With a sign-in cookie, requests that change something must send the `X-HLM-Request` header (the web UI
> does), so other websites can't make them on your behalf.

## API

| Method & path                                  | Description                                                                       |
|------------------------------------------------|-----------------------------------------------------------------------------------|
| `GET /api/status`                              | Docker and catalog status.                                                        |
| `GET /api/auth/status`                         | How to sign in, and who is signed in. Needs no sign-in.                           |
| `POST /api/auth/login`                         | `{username, password}`: sign in with a local account (send `X-HLM-Request: 1`). Needs no sign-in. |
| `POST /api/auth/logout`                        | Ends the session (send `X-HLM-Request: 1`).                                       |
| `GET/PUT /api/auth/config`                     | `{required, sessionHours}`. Requiring sign-in needs a signed-in session.          |
| `GET/POST /api/auth/accounts`                  | Local accounts; `POST {username, password, role}` adds one.                       |
| `PUT /api/auth/accounts/{username}/password`   | `{password}`: sets a new password, ending the account's other sessions. Members change only their own, and send `current` too. |
| `PUT /api/auth/accounts/{username}/role`       | `{role}`: `admin` or `member`.                                                    |
| `DELETE /api/auth/accounts/{username}`         | Removes an account.                                                               |
| `GET/POST /api/auth/accounts/{username}/ssh-keys`, `PUT/DELETE …/ssh-keys/{id}` | [SSH keys](#ssh-keys) of an account, for an admin or the account itself. `POST {publicKey, name?, allowFrom?}` and `PUT {allowFrom}` need a [confirmed](#confirming-its-you) session. |
| `POST /api/auth/confirm`                       | Confirm a password session for 10 minutes: `{password}`. Single sign-on confirms at `/auth/login?confirm=1`. |
| `GET/PUT /api/system/https`, `POST /api/system/https/renew` | [HTTPS](#https) settings and certificate status; `PUT {enabled, domain, email?, staging?}` needs a [confirmed](#confirming-its-you) session. |
| `POST /api/auth/download-ticket`               | `{url}` of a backup download returns a URL that works once, for a minute, from the same address: for downloads when signed in with a token. |
| `POST /api/auth/ssh/challenge`, `POST /api/auth/ssh/login` | Sign in with an SSH key: `{username}` returns `{challenge}`; `{challenge, signature}` returns `{token, expires}`. |
| `GET/POST /api/auth/keys`, `PUT/DELETE /api/auth/keys/{id}` | [API keys](#api-keys); `POST {name, days, role, allowFrom?}` returns the key, once; `PUT {allowFrom}`. Both need a [confirmed](#confirming-its-you) session. |
| `GET/POST /api/groups`, `PUT/DELETE /api/groups/{id}` | [Server groups](#roles-and-server-groups): `{name, servers, grants}`. `GET` gives members their own groups, without grants. |
| `PUT /api/servers/{id}/groups`                 | `{groups}`: the IDs of the groups a server is in.                                 |
| `GET/PUT/DELETE /api/auth/oidc`                | [Single sign-on](#single-sign-on-with-authelia-openid-connect) settings; the client secret is never returned. |
| `POST /api/auth/oidc/test`                     | Fetches the provider's discovery document for (unsaved) settings.                 |
| `POST /api/auth/oidc/secret`                   | A new client secret, with its digest for Authelia's configuration; nothing is saved. |
| `GET /auth/login?return=`, `GET /auth/callback` | Browser sign-in: off to the provider and back.                                   |
| `GET /api/catalog/apps?q=&category=&sort=`     | Search templates (default category `GameServers`; `category=` for all).           |
| `GET /api/catalog/apps/{id}`                   | Full template, with paths localized.                                              |
| `GET /api/catalog/categories`                  | Categories with counts.                                                           |
| `POST /api/catalog/import`                     | `{image}`: pull an image and make a template from its defaults. Streams NDJSON; the final `done` event's message is the template ID. |
| `DELETE /api/catalog/apps/{id}`                | Delete a template made from an image.                                             |
| `POST /api/catalog/refresh`                    | Re-download the feed now.                                                         |
| `GET /api/networks`                            | Docker networks with subnets, addresses in use, and whether static IPs are allowed. |
| `GET /api/servers`                             | Managed servers.                                                                  |
| `POST /api/servers/check-ports`               | Host ports `{request, server?}` would use that are already in use (`conflicts`); `server` is the one being reconfigured. |
| `POST /api/servers`                            | Install: `{templateId, name, tag?, values?, disabled?, network?, ipAddress?, extra?, extraParams?, postArgs?, privileged?}`, plus `group?` for a [deployer key](#deployer-keys). Streams NDJSON progress. |
| `GET /api/servers/{id}`                        | One server (`id` is the container name or ID).                                    |
| `POST /api/servers/{id}/start\|stop\|restart`  | Lifecycle actions.                                                                |
| `POST /api/servers/{id}/update`                | Pull the latest image and recreate. Streams NDJSON.                               |
| `GET /api/servers/{id}/config`                 | Template plus the saved settings, for an edit form.                               |
| `PUT /api/servers/{id}/config`                 | Recreate with new settings. Streams NDJSON.                                       |
| `GET /api/servers/{id}/logs?tail=500`          | Server-sent events, one log line per event.                                       |
| `GET/PUT /api/servers/{id}/console`            | How the server takes [console commands](#sending-commands-to-a-server): `{mode, host?, port?, password?, passwordVar?, exec?, quickCommands?}` (`mode` is `stdin`, `rcon`, `exec` or empty), plus whether it's ready, what was detected, and quick commands. The password is never returned; `PUT` with it empty keeps it, and with `clearPassword` removes it. `PUT` takes a manager. |
| `POST /api/servers/{id}/console/test`          | Checks console settings as given, without saving them (a manager).               |
| `GET/POST /api/servers/{id}/tasks`, `PUT/DELETE /api/servers/{id}/tasks/{task}` | [Scheduled tasks](#scheduled-tasks): `{name, enabled, onlyIfRunning, when: {kind, time?, weekdays?, every?, cron?}, steps: [{kind, command?, seconds?, continueOnError?}]}`. `GET` (viewers; commands only for operators) also gives the running task and the time zone; changes take a manager. |
| `POST /api/servers/{id}/tasks/{task}/run`      | Runs a task now (an operator); 409 if the server is busy.                          |
| `GET /api/schedule/upcoming`                   | The next task runs on the servers you can see, soonest first.                      |
| `GET/PUT /api/system/timezone`                 | hlm's time zone: `{timezone}` (empty for the system's); `PUT` takes an admin.      |
| `POST /api/servers/{id}/console/command`       | `{command}`: sends it; returns `{mode, output, exitCode?}`. Over stdin, the answer is in the logs instead. |
| `GET /api/servers/{id}/files?path=`            | List a directory in the server's container, plus its data folders.                |
| `GET /api/servers/{id}/files/content?path=`    | Read a text file (up to 1 MiB).                                                   |
| `PUT /api/servers/{id}/files/content`          | `{path, content, expectedMtime}`: save a file; 409 if it changed since it was read. |
| `GET /api/servers/{id}/backups`                | The server's backup plan, a running backup's progress, and its backups in the bucket (newest first). |
| `PUT /api/servers/{id}/backups/plan`           | `{folders, intervalHours, keep, stopServer}`: what to back up and when.            |
| `POST /api/servers/{id}/backups`               | Start a backup now (202); progress shows in `GET .../backups`.                     |
| `POST /api/servers/{id}/backups/restore`       | `{key, backupFirst}`: start restoring a backup (202); progress shows in `GET .../backups`. |
| `GET /api/servers/{id}/backups/download?key=`  | Download one of the server's backups.                                             |
| `DELETE /api/servers/{id}/backups?key=`        | Delete one of the server's backups from the bucket.                               |
| `GET /api/system/backup`                       | Configuration backups: their plan, a running backup, and the backups in the bucket. |
| `PUT /api/system/backup`                       | `{intervalHours, keep, includeKey}`: when to back up hlm's configuration.          |
| `POST /api/system/backup`                      | Back up hlm's configuration now (202).                                            |
| `GET /api/system/backup/download?key=`         | Download a configuration backup.                                                  |
| `DELETE /api/system/backup?key=`               | Delete a configuration backup.                                                    |
| `GET\|PUT\|DELETE /api/integrations/backup`     | Bucket settings, including `caCert` (PEM) for a private CA (the secret key is never returned; empty keeps the saved one). |
| `POST /api/integrations/backup/test`           | Check bucket settings: list, write and delete a test file.                        |
| `GET /api/servers/{id}/files/search?path=&q=`  | Find files by name, or config files when `q` is empty.                            |
| `GET /api/servers/{id}/stats`                  | CPU, memory, network and disk usage.                                              |
| `POST /api/networks`                           | Create a network: `{name, driver: bridge\|macvlan\|ipvlan, parent?, subnet, gateway?, ipRange?}`. |
| `DELETE /api/networks/{name}`                  | Delete a network created in hlm that no container uses.                           |
| `GET /api/networks/interfaces`                 | The host's interfaces with their subnets and default gateway, for suggestions.    |
| `GET /api/stats`                               | Usage of every running server, keyed by container ID.                             |
| `GET /api/integrations/unifi`                  | UniFi settings, without credentials.                                              |
| `PUT /api/integrations/unifi`                  | Save UniFi settings (tested first). Empty `password`/`apiKey` keeps the saved one. |
| `POST /api/integrations/unifi/test`            | Test settings without saving. An untrusted certificate returns `certFingerprint`. |
| `GET /api/integrations/unifi/check-ip?ip=&server=` | Whether a LAN address is free according to UniFi (`server` excludes that server itself). |
| `GET /api/integrations/unifi/wan?wan=`         | The gateway's public address on `wan` (default) or `wan2`.                         |
| `DELETE /api/integrations/unifi`               | Forget the UniFi settings and credentials.                                        |
| `GET /api/servers/{id}/ports`                  | The server's ports with their options (`port` = internet port, `local`, `forward`, `proxy`, `firewall`), clashing servers, and which integrations are set up. |
| `PUT /api/servers/{id}/ports`                  | Set port options `{"ports": {"2456/udp": {"port": 2466, "forward": true, "proxy": true, "firewall": true}}, "relay": true}` (keyed by the server's own port; `relay` adds the server to the proxy or removes it). |
| `GET /api/servers/{id}/ports/status`           | Whether each port is in place on UniFi, the proxy and the firewall (`synced`, `pending`, `removing`, `conflict` or `off`, with a `detail`). |
| `GET /api/servers/{id}/portforward`            | The server's wanted rules and their state on the gateway.                         |
| `POST /api/servers/{id}/portforward`           | Create or update the server's rules and remove outdated ones.                     |
| `DELETE /api/servers/{id}/portforward`         | Remove the server's rules.                                                        |
| `GET /api/proxy`                               | Proxy settings, the SSH public key and the home address state (`upstream`).       |
| `PUT /api/proxy`                               | Save proxy settings `{host, port, user, hostKey, upstream, upstreamSource, unifiWan, upstreamPorts, logMode, servers}`; `upstreamSource` is `manual`, `unifi` or `wireguard`, `upstreamPorts` is `internet` or `local`. |
| `POST /api/proxy/key`                          | Generate the SSH key (`{"replace": true}` for a new one).                         |
| `PUT /api/proxy/key`                           | Import a private key `{privateKey}`.                                               |
| `POST /api/proxy/test`                         | Connect and inspect the proxy (nginx, stream module, sudo). Untrusted host keys return `hostKey`. |
| `POST /api/proxy/install`                      | Install nginx with its stream module.                                             |
| `GET /api/proxy/preview`                       | The nginx configuration that would be applied.                                    |
| `POST /api/proxy/apply`                        | Write it, test it with `nginx -t` and reload (rolled back if the test fails); also updates the firewall (`firewall` in the result). |
| `GET /api/proxy/adopt`                         | nginx's own stream block (`file`, `block`) and how it would look with hlm's include (`proposed`). |
| `POST /api/proxy/adopt`                        | Add hlm's include to that block (backed up, checked with `nginx -t`, rolled back on failure). |
| `POST /api/proxy/remove`                       | Remove all relays and close their ports in the firewall.                          |
| `GET /api/proxy/wireguard`                     | The WireGuard tunnel: addresses, public keys and the host end's container.        |
| `PUT /api/proxy/wireguard`                     | Set up or update the tunnel (streams NDJSON: `step` and `output` events, then `done` with the result or `error`) `{network, listenPort, endpoint, useForRelays, lanNetwork, lanAddress}` on both ends; `lanNetwork` (a macvlan/ipvlan network) lets it reach servers' own LAN addresses; `useForRelays` makes it the home address and re-applies nginx if applied. |
| `POST /api/proxy/wireguard/check`              | The last handshake the proxy saw and whether it can ping this host (`reachable`). |
| `DELETE /api/proxy/wireguard`                  | Take the tunnel down on both ends and forget it (refused while applied relays use it). |
| `GET /api/integrations/linode`                 | The Linode connection, the firewall hlm manages, its rules for the relays and whether they're up to date. |
| `POST /api/integrations/linode/test`           | List the firewalls a token `{token}` can manage (empty: the saved token), suggesting the proxy server's. |
| `PUT /api/integrations/linode`                 | Save `{provider: "linode", token, firewallId}`: empty `token` keeps the saved one, `firewallId` 0 manages no firewall. Opens the relayed ports on the chosen one. |
| `DELETE /api/integrations/linode`              | Remove hlm's rules and forget the connection (`?keepRules=1` leaves the rules).   |
| `POST /api/proxy/firewall/sync`                | Bring the firewall's rules in line with the applied relays.                      |
| `POST /api/proxy/wan-check`                    | Look up the home address from UniFi now and re-apply if it changed.               |
| `GET /api/tunnel`                              | The Cloudflare connection, its domains, the tunnel (Cloudflare's health and connections, the `hlm-cloudflared` container) and the LAN networks it can join. |
| `POST /api/integrations/cloudflare/test`       | List the accounts and domains a token `{token}` can use (empty: the saved token). |
| `PUT /api/integrations/cloudflare`             | Save `{token, accountId}`: empty `token` keeps the saved one, empty `accountId` picks the token's only account. |
| `DELETE /api/integrations/cloudflare`          | Forget the token (refused while the tunnel is set up).                            |
| `PUT /api/tunnel`                              | Create the tunnel (the first time) and run cloudflared `{name, lanNetwork, lanAddress}`, then publish the hostnames. Streams NDJSON: `step` and `output` events, then `done` with `status` (and `apply`) or `error`. |
| `POST /api/tunnel/{start,stop,restart}`        | Control the `hlm-cloudflared` container.                                          |
| `GET /api/tunnel/logs?tail=`                   | cloudflared's log as server-sent events.                                           |
| `DELETE /api/tunnel`                           | Remove the container, hlm's DNS records and the tunnel; the hostnames are kept.   |
| `GET /api/tunnel/routes`                       | The hostnames with where they go and their states, and the servers' TCP ports.    |
| `PUT /api/tunnel/routes`                       | Replace the hostnames `{routes: [{hostname, server, port, scheme, noTLSVerify}]}` and publish them if the tunnel is set up. Use `address` (an IP address or host name) instead of `server` for a web server hlm doesn't run. |
| `POST /api/tunnel/apply`                       | Publish the hostnames now.                                                        |
| `GET /api/system`                              | hlm's version and update status.                                                  |
| `GET/PUT /api/notifications`                   | [Notification](#notifications) targets (without secrets), the event kinds, and `PUT {baseUrl, diskPercent}`. |
| `POST /api/notifications/targets`, `PUT/DELETE /api/notifications/targets/{id}` | Add, change or remove a target: `{kind, name, enabled, events, servers?, groups?, quietStart?, quietEnd?, url?, topic?, token?, headers?, smtpHost?, smtpPort?, security?, username?, password?, from?, to?}`. Secrets left empty keep their saved values. Adding and changing need a [confirmed](#confirming-its-you) session. |
| `POST /api/notifications/test`                 | Sends a test message to a target as given (with `id`, empty secrets come from the saved target). |
| `GET /api/notifications/history`               | The last 200 events, newest first, and where each was sent.                        |
| `POST /api/system/update/check`                | Check GitHub for a newer hlm release.                                             |
| `POST /api/system/update/apply`                | Install the latest release. Streams NDJSON; hlm restarts afterwards.             |
| `POST /api/system/update/dismiss`              | Clear the result of the last update.                                              |
| `GET /api/host`                                | Host CPU, memory, network, filesystems and disk I/O, with 30 minutes of history.  |
| `GET /api/host/storage`                        | Disk usage per server and Docker's totals (last background calculation).          |
| `POST /api/host/storage/refresh`               | Recalculate disk usage now (runs in the background).                              |
| `POST /api/host/storage/deleted/{id}/delete`   | Delete a deleted server's folder or volume `{path, volume}` from disk.             |
| `POST /api/host/storage/deleted/{id}/forget`   | Stop tracking a deleted server's folder or volume `{path, volume}`, or all of them without a `path`. |
| `GET /api/jobs`                                | Installs and updates in progress, plus those that finished in the last minute.    |
| `GET /api/updates`                             | When updates were last checked and how many are available.                        |
| `GET/PUT /api/servers/{id}/update-policy`      | A server's [automatic updates](#automatic-updates): `{mode, windowStart?, windowEnd?, warn?, warnMinutes?, backup}` (`mode` is `auto`, `manual`, or empty to follow the default), plus what it follows, the update's state and the last result. `PUT` takes a manager; the warning is only shown to operators. |
| `GET /api/query`                               | Each running server you can see: its [game status](#is-the-game-up-players-and-status) and player count. |
| `GET /api/servers/{id}/query`                  | A server's game status: the last answer (players' names only for operators), how it's queried, errors, and a day of player counts. |
| `PUT /api/servers/{id}/query/settings`         | `{protocol, port?}` (`protocol` is empty to find a way, or `a2s`, `minecraft`, `bedrock`, `rcon`, `tcp`, `off`); a manager. |
| `POST /api/servers/{id}/query/test`            | Queries with the given settings without saving them; with an empty protocol, says what it found (a manager). |
| `GET /api/updates/auto`                        | Which of the servers you can see update automatically, and what their updates are waiting for. |
| `GET/PUT /api/system/update-policy`            | The default policy, `checkHours`, and hlm's own (`self: {auto, windowStart?, windowEnd?}`); admins only. |
| `POST /api/updates/check`                      | Check every server for image updates now.                                         |
| `POST /api/updates/apply`                      | Update every server that has an update. Streams NDJSON; events carry `target`.    |
| `GET /api/connections`                        | The containers hlm runs for connections (`hlm-wireguard`, `hlm-cloudflared`), with their update status. Their usage is in `GET /api/stats`. |
| `GET /api/connections/{id}`                   | One connection container, by name or ID.                                         |
| `GET /api/connections/{id}/stats`             | Its resource usage, like a server's.                                             |
| `GET /api/connections/{id}/logs`              | Its console output as server-sent events (`tail`, `follow=0`), like a server's.  |
| `POST /api/connections/{id}/{start,stop,restart}` | Control a connection container.                                              |
| `POST /api/connections/{id}/update`            | Pull its image and recreate it with the same settings. Streams NDJSON progress.   |
| `PUT /api/servers/{id}/autostart`              | `{"enabled": true}` to start the server on boot.                                  |
| `DELETE /api/servers/{id}`                     | Remove the container. Data folders are kept.                                      |

`values` is keyed by each config item's `id` from the template, e.g. `port:15637/udp` or `variable:SERVER_NAME`.

Port values can be a single port (`27015`), a range (`27015-27020`), or either one with a host address to bind
(`0.0.0.0:27015`). For a port range in the template, such as Valheim's `2456-2458/udp`, give either a host range of the
same size or just its first port: `"port:2456-2458/udp": "3456"` maps 2456-2458 to 3456-3458. Docker forwards each
port separately, so ranges over 1,000 ports get a warning; consider host networking for those.

Example: install the Enshrouded server with its query port moved to 25637:

```sh
curl -N -X POST localhost:8080/api/servers -d '{
  "templateId": "enshrouded-79ccd9847c",
  "name": "enshrouded",
  "values": {"port:15637/udp": "25637"}
}'
```

## Tests

```sh
make test
# Also run the Docker-backed tests (a busybox container, and SeaweedFS for backups):
HLM_DOCKER_TESTS=1 go test ./internal/files ./internal/backup
# Also build every game server template in a real feed with default values:
curl -o /tmp/feed.json https://assets.ca.unraid.net/feed/applicationFeed.json
HLM_FEED_FILE=/tmp/feed.json go test ./internal/deploy -run WholeFeed -v
```
