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
  - Common `docker run` flags in `ExtraParams` are applied (`--restart`, `--memory`, `--ulimit`, `--gpus`, the
    `--health-*` flags and `--no-healthcheck`, …).
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
- `ssh://` in `DOCKER_HOST` isn't supported; add an SSH host on the Host page instead (below).

With a remote host, `HLM_APPDATA_ROOT` and template paths refer to the remote machine's filesystem, and game server
ports are published on the remote machine. Web UI links in the dashboard point to it, and the header shows which Docker
host is in use.

### More Docker hosts

One hlm can run servers on several machines: a NAS, a mini-PC, a GPU box. The host above (`DOCKER_HOST`, or the local
socket) is **this host**; add others on the **Host** page, under **Docker hosts**. Their servers are listed with this
host's, with a badge for their host, and have their console, files, backups, schedule, updates, health checks and
alerts as usual.

- **Names.** A server on another host is named `name@host`, where `host` is the short ID you give the host (`nas`).
  The container on that host is just `name`, so the same name can be used on two hosts. Servers on this host keep
  their plain names, so nothing changes for an existing install. Server groups, backups and other settings use the full
  name.
- **Install, import and clone** have a **Docker host** picker once there's more than one host. Paths in the form are
  under that host's data folder, port checks look at that host, and the networks are that host's. Cloning to another
  host copies the data folders across (through hlm), and keeps the name and ports if they're free there.
- **SSH (recommended).** hlm signs in with its SSH key (the same key the [proxy](#relaying-through-a-proxy-server-nginx-over-ssh)
  uses; the form shows it, to add to `~user/.ssh/authorized_keys`) as a user in the host's `docker` group, and talks to
  Docker's socket over the connection. The host's SSH key is checked against the fingerprint you confirmed the first
  time. If the SSH server doesn't allow forwarding (`AllowTcpForwarding no`), hlm runs `docker system dial-stdio`
  there instead, as the `docker` CLI does for `ssh://` hosts; that needs the `docker` command on the host.
- **No Docker on the host yet?** For an SSH host, **Check for Docker** (or a failed **Test connection**) signs in and
  shows the host's Linux, whether Docker is installed and running, and whether the user can use it. If not, hlm can
  set it up, following [Docker's install steps](https://docs.docker.com/engine/install/) for Ubuntu, Debian,
  Raspberry Pi OS, Fedora, CentOS Stream or RHEL: it adds Docker's apt or dnf repository (checking Docker's signing
  key on dnf), installs Docker CE with its Compose and Buildx plugins, starts Docker on boot and adds the SSH user to
  the `docker` group. If Docker is already installed it only does the last two. The form shows the exact commands
  first, and the output as they run. They run as root, so the user has to be root or have passwordless `sudo`;
  otherwise copy the commands and run them yourself. Other systems (Unraid, Alpine, Arch, …) aren't covered: install
  Docker there yourself.
- **TLS set up for you.** Choose **Docker's API over TCP with TLS**, then **Let hlm make the certificates and set
  Docker up for TLS, over SSH**. hlm signs in once (as root or a user with passwordless `sudo`; **Check for Docker**
  first, which also installs Docker if needed) and follows Docker's
  [Protect the Docker daemon socket](https://docs.docker.com/engine/security/protect-access/):
  - it makes a CA for that host, a server certificate for its Docker (for the address hlm uses, plus the host's name
    and addresses) and a client certificate for hlm, all lasting 10 years. The CA's key is thrown away once both are
    signed, so the certificates can't be used on another host, and nothing else can be signed with it;
  - it installs the server's certificate and key in `/etc/docker/hlm-tls/` (the key readable by root only, sent over
    the SSH connection's input, never on a command line), and adds a systemd drop-in,
    `/etc/systemd/system/docker.service.d/hlm-tls.conf`, that keeps the unit's own `dockerd` command and adds
    `-H tcp://0.0.0.0:2376 --tlsverify --tlscacert=… --tlscert=… --tlskey=…`;
  - it restarts Docker (and so its running containers). If Docker doesn't start, the drop-in is removed and Docker
    started as before. If `/etc/docker/daemon.json` already sets `hosts` or TLS options, nothing is changed;
  - it connects over TLS with its client certificate and saves the host. The client key stays in hlm's encrypted
    settings; the browser never sees it. Open the port in the host's firewall for hlm's machine.

  Setting TLS up again replaces all three certificates. To undo it, delete the drop-in and `/etc/docker/hlm-tls`, then
  `systemctl daemon-reload && systemctl restart docker`.
- **hlm's agent.** For a host hlm can't reach, or where you'd rather not open SSH or Docker's API to it (behind NAT,
  on another network, at a friend's): the host runs `hlm agent`, which connects *out* to hlm and serves it the host's
  Docker over that one connection. Choose **hlm's agent on the host**, give the host's ID, data folder and LAN address,
  and **Make the command**. Run the command there:

  ```sh
  docker run -d --name hlm-agent --restart unless-stopped \
    -v /var/run/docker.sock:/var/run/docker.sock -v hlm-agent:/data \
    -v /proc:/host/proc:ro -v /:/host/rootfs:ro \
    danotchuuy/home-lab-manager:latest agent --server https://hlm.example.com --token hlme_…
  ```

  (or `hlm agent --server … --token … --data DIR` with the binary). The host appears as soon as the agent connects;
  the form waits for it.
  - **Enrolment.** The token is single-use and lasts an hour; making another for the same host replaces it. The agent
    trades it for its own credential (only its hash is kept in hlm), saved in `agent.json` in its data folder (the
    `hlm-agent` volume, readable by its owner only). Running the same command again (to update the agent) keeps the
    credential; given a new token, the agent enrols again. Making a token needs a
    [confirmed](#confirming-its-you) admin session, like adding any host: `HLM_TOKEN` and API keys can't.
  - **The connection.** A WebSocket to `/api/agent/connect`, so it passes through reverse proxies and
    [Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel), carrying many streams at once (yamux); hlm's Docker
    calls, log and event streams and console input all go over it. The agent pings every 25 seconds and reconnects by
    itself (backing off up to a minute) when it drops or hlm restarts.
  - **Only hlm's Docker calls.** The agent passes on only the Docker API calls hlm makes (containers, images, networks,
    volumes, exec and archives for files and backups, events) and refuses the rest (swarm, plugins, secrets, image
    builds, exports, commits…), so even something that got hold of the connection couldn't use those. Starting
    containers is still allowed, which on any Docker host amounts to root there: only enrol hosts you'd give hlm that
    much.
  - **HTTPS.** The agent checks hlm's certificate (the system's authorities, plus `--ca FILE` for your own). It
    refuses a plain `http://` address unless given `--allow-http`, which the command includes when hlm is open over
    HTTP; then anyone who can see or change the traffic between them could take the host's Docker over, so only do
    that on a network you trust.
  - **Host metrics.** The agent also reports the host's CPU, memory, load, network and disks, read from the host's
    `/proc` and root filesystem (the two read-only mounts in the command, as for hlm itself; leave them out and only
    its Docker is used). hlm asks every 15 seconds: the **Host** page has a picker for this host or an agent's host,
    with the live charts and the kept history (by the minute for a day, by the quarter hour for 30 days), and
    [`/metrics`](#prometheus-metrics) has its `hlm_host_*` series with a `host` label. (Hosts reached over SSH or TLS
    report them too, see below.)
  - **Removing the host** disconnects its agent and its credential stops working; the agent says so and exits. Remove
    the `hlm-agent` container there too. The table shows each agent's connection: since when, and its version.
  - The agent's settings can also be given as `HLM_AGENT_SERVER`, `HLM_AGENT_TOKEN`, `HLM_AGENT_DATA` (default
    `/data`), `HLM_AGENT_SOCKET` (default `/var/run/docker.sock`), `HLM_AGENT_CA` and `HLM_AGENT_ALLOW_HTTP`, and the
    host's procfs and root as `--proc`/`HLM_HOST_PROC` and `--root`/`HLM_HOST_ROOT` (by default `/host/proc` and
    `/host/rootfs` when they're mounted, otherwise the agent's own `/proc`).
- **TCP with TLS.** For a daemon started with `--tlsverify`: its address (`tcp://nas.lan:2376`), the CA that signed its
  certificate and a client certificate and key. The daemon's certificate is always checked, and plain TCP isn't
  accepted. The client key is kept encrypted with hlm's settings and never shown again.
- **Data folder.** Where servers' data folders go on that host, like `HLM_APPDATA_ROOT` here.
- **LAN address.** Where the host's published ports are reached (the SSH host or the TCP address's host, unless you set
  another). Web UI links, game status checks and console connections (RCON, Palworld's REST API) use it, UniFi port
  forwards point at it (set an IP address, or a name hlm can look up), and Cloudflare Tunnel routes to it from this
  host.

If a host can't be reached, the Servers page says so and its servers are left out until it's back; their settings in
hlm are kept. Removing a host stops hlm managing its servers: they keep running there.

The **Host** page has a picker at the top for this host or another. For each host it shows:

- **CPU, memory, network and disks**, live and as [history](#resource-history), and in
  [`/metrics`](#prometheus-metrics) with a `host` label. A host with [hlm's agent](#more-docker-hosts) reports them
  every 15 seconds. On an SSH host, hlm runs a short script over its SSH connection every 15 seconds (it reads
  `/proc` and runs `df`; nothing is installed). On a TLS host, the same script runs every minute in a throwaway
  `busybox` container (labelled `io.hlm.helper=metrics`, no network) with the host's `/proc` and `/` mounted
  read-only, which is removed straight away. The live charts span 360 samples: 30 minutes for this host and agents,
  90 minutes for SSH hosts and 6 hours for TLS hosts.
- **Disk usage by server**, from Docker's figures and the size of each data folder. On another host, the folders are
  measured with `du` by one throwaway helper container per calculation (the host's `/` mounted read-only).
- **Deleted servers' data**, kept and shown per host. **Delete** runs a helper container there, with the same
  checks as here: never a system or top-level folder, the host's data folder itself, or a folder another container
  still uses.
- [Disk cleanup](#disk-cleanup), which already worked on every host.

A [Compose stack](#compose-stacks) can go on any host too, and a [server group](#roles-and-server-groups) can hold a whole
host.

Limits for now:

- Self-update runs on this host. [WireGuard](#wireguard-tunnel-to-this-host) and
  [Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel) can each have their own end, or tunnel, on another host,
  set up from the Proxy and Tunnel pages.
- Container IP addresses on another host's bridge networks can't be reached from here; hlm uses the host's published
  ports (or a server's own LAN address on a macvlan network) instead.

## Updates and start on boot

The server list shows each server's update status. **Check for updates** asks each image's registry for the current
digest of its tag and compares it with the local image (each image is checked once, however many servers use it).
hlm also checks every 12 hours, or as often as **System → Automatic updates** says (`HLM_UPDATE_CHECK`, if set, wins;
`0` turns checking off). A server is also marked as having an update when a newer image was already downloaded, e.g.
with `docker pull`, but the server still runs the old one.

**Update** (one server) and **Update all** pull the new image and recreate the servers one at a time with the same
settings. They keep going if you close the page, and a failure doesn't stop the remaining updates. Running servers are
restarted; stopped servers stay stopped.

<a id="connections"></a>**Connections.** Below the servers, the **Connections** section lists the containers hlm runs for
its tunnels: `hlm-wireguard` (the [WireGuard tunnel](#wireguard-tunnel-to-this-host)'s end) and `hlm-cloudflared` (the
[Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel)'s connector), on this host and on
[other Docker hosts](#more-docker-hosts) that have their own (named `name@host`), and, when hlm runs in Docker on the
host it manages, hlm's own container. They show CPU, memory and network use like servers, and **Check for updates** checks their
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

Images from private registries need a login: see [Private registries](#private-registries).

### Private registries

**System → Registries** keeps logins for registries that need a `docker login`: a private Docker Hub repository, GHCR
(`ghcr.io`), or your own registry (`registry.lan:5000`, Gitea, Harbor, `registry:2`). Add the registry's host, a
username and a password or access token; a read-only token is enough (on Docker Hub an access token with "Read-only",
on GitHub a classic token with `read:packages`). **Test** signs in through Docker, and with an image filled in also
looks that image up, to show the login can see it.

hlm then uses the login whenever it pulls or looks up an image on that registry: installs, updates and update checks,
**Add a Docker image**, the WireGuard and Cloudflare Tunnel containers, and hlm's own updates. An image's registry is the
first part of its name when that has a dot or a port (`ghcr.io/team/app`, `registry.lan:5000/app`) or is `localhost`;
anything else, like `itzg/minecraft-server`, is on Docker Hub (`docker.io`; `index.docker.io` means the same). A Docker
Hub login also raises Docker Hub's pull limit, which an "Update all" on a big install can run into.

When a registry refuses hlm, the error says the image needs a login.

- Passwords are kept in the encrypted secrets store (so they're in configuration backups), and only ever sent to the
  Docker daemon, for the image's own registry. They're never shown again or written to the activity log; leave the
  password blank when changing a login to keep it.
- Anyone who can update a server pulls its image with the saved login. That's by design (the image is already on the
  host), but it means an operator can pull newer versions of a private image.
- [Deployer keys](#deployer-keys) (and other confined installs) only use logins with **Allow deployer keys…** ticked.
- Adding or changing a login needs an admin who has recently confirmed it's them, like other sign-in settings; it can't
  be done with an API key.

## Editing server files

A server's **Files** tab browses its container, whether it's running or stopped, and opens text files in an editor. The
**Location** picker chooses where to start: any of the server's mounts (host folders and Docker volumes, with their
source shown) or the container's root. The choice is remembered per server; otherwise the first data folder opens.
**Find config files** lists the settings-like files (`.json`, `.ini`, `.cfg`, `.properties`, `.yml`, `.xml`, …) anywhere
under the current folder, or searches by name.

**Pinned settings files.** A server's settings files can be pinned, so they open in one click. They're listed under
**Config files** at the top of the Files tab and under **Game settings files** on the Settings tab. Managers pin a file
with **☆ Pin** in the editor; operators can open pinned files. For games it knows, hlm also suggests the usual file,
found in the server's data folders, ready to pin: Palworld's `PalWorldSettings.ini` (the one under
`Saved/Config`, not the defaults), Minecraft's `server.properties`, ARK's `GameUserSettings.ini` and `Game.ini`,
Enshrouded's `enshrouded_server.json`, V Rising's server settings, 7 Days to Die's `serverconfig.xml`, and more.

**The settings form.** INI-style settings files (`.ini`, `.cfg`, `.conf`, `.properties`, `.env`, …) open as a form: a
field per setting, with readable names ("Exp rate" for `ExpRate`), switches for True/False, number fields, hidden
password fields with **Show**, and suggestions for settings with a fixed set of values. It also reads Unreal Engine's
one-line settings, such as Palworld's `OptionSettings=(Difficulty=None,ExpRate=1.000000,…)`, as a field each.

- **Filter** finds settings by name or value, and **Only changed** lists what you've changed, each with what it was and
  **undo**.
- Only the values you change are rewritten. Comments, order, spacing, line endings, quoting and the case of True/False
  stay as they were, and a value with a comma or quotes in it is quoted so it doesn't break the line.
- **Add a setting** adds one the file doesn't have yet: choose where (one of the file's sections, a struct such as
  Palworld's `OptionSettings`, or a new section), its name and its value. It's written like the settings around it:
  in a struct as another `Name=Value` member (quoted if it needs to be); in a section as a line after its last setting,
  with the same indentation, `=` or ` = `, quoting and `export` as its neighbour; in a new section under a new header at
  the end. A name the place already has is refused, except Unreal's repeatable `+Name` lines. Each group also has
  **Add a setting to …** at its end. The new setting shows as "new" and is saved with **Save** like any other edit.
- **Form** and **Text** switch between the form and the text editor; your choice is remembered. Saving, reverting and
  the "this file changed since you opened it" check work the same in both.
- A file that doesn't read as settings (fewer lines of settings than other lines) opens as text only.

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
| **Player list through the [console](#sending-commands-to-a-server)** | Palworld (through its REST API, with the player limit, server name and version too; or `ShowPlayers` over RCON), Minecraft (`list`), ARK (`ListPlayers`): needs the console set up with RCON, Palworld's REST API, or a command run in the container |
| **TCP port check** | Anything else: only whether the port is open, no players |

**Finding a way:** hlm finds one by itself. It tries the game's usual way, then ports the template calls "query", the
game's usual Steam query port, and each UDP port and the one after it, then the console (for Palworld, the console
comes first). It looks again every couple of
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

### Game profiles

What hlm knows about particular games is kept as **game profiles**: one YAML file per game, built into hlm
(`internal/gameprofile/profiles/`). A profile says:

- **how to recognise the game**: words in the image's name and tag (`itzg/minecraft-server`, `ich777/steamcmd:valheim`).
  The most specific profile wins, so `minecraft-bedrock` (needing both words) beats `minecraft`;
- **status queries**: its own protocol (Minecraft, Bedrock) and the variable that moves its port, its usual Steam query
  ports, and whether to ask through the console first;
- **the console**: its usual RCON port, whether it reads commands from its console (stdin), quick commands, images that
  bring their own command sender (such as `rcon-cli` in `itzg/minecraft-server`), and a built-in REST driver (Palworld);
- **the player list** through the console: the command, and how to read its answer (`csv`, a `summary` pattern with the
  count, limit and names, or a pattern per player `lines`);
- **scheduled tasks' commands** to warn players and save, and whether to save before hlm stops it (Palworld);
- **settings files** to suggest (see [Editing server files](#editing-server-files)).

Built in: Minecraft (Java and Bedrock), Palworld, ARK, Rust, Terraria (and tShock), Factorio, Counter-Strike 2,
7 Days to Die, Valheim, Enshrouded, V Rising, Conan Exiles, Project Zomboid, Satisfactory, Nightingale and Soulmask.
Profiles only point hlm's own features at things (a port, a command, a file name, a pattern); they never run code.
Each server's page says which profile it uses (**Game profile: Palworld**).

**Your own profiles.** **System → Game profiles** lists every profile. **Add a game** starts one from a template,
**Edit a copy** starts from a built-in one, and **Import a file…** loads a `.yaml` file. Saving checks the profile
first (unknown fields, ports, patterns) and says what's wrong. A profile with the same `id` as a built-in one is used
instead of it; deleting yours brings the built-in one back. Your profiles are kept in `game-profiles/` in hlm's data
directory, one file each, and are in [hlm's configuration backups](#backing-up-hlms-own-configuration). You can also put files
there yourself and click **Read the folder again** (hlm reads them at start too). A file with a mistake is listed with
the reason and skipped; the others are used. Status queries try new profiles' ports when they next look for a way to
ask a server (within 2 minutes).

```yaml
id: mygame                  # lower-case letters, digits and dashes
name: My Game
# Words in the image's name and tag (split at anything that isn't a letter or digit) that identify it:
# all the words of any one set. The most specific match wins.
match: [[mygame], [my, game, server]]
query:
  console_first: false      # ask through the console (names, counts) before query ports
  protocol: minecraft       # optional: minecraft or bedrock, on port (or the port in port_env)
  port: 25565
  port_env: SERVER_PORT
  a2s_ports: [27015]        # Steam query ports to try (container ports)
console:
  rcon_port: 27015          # when the image doesn't say
  stdin: false              # the server reads commands from its console
  commands: [status, "say "]  # quick commands; a trailing space means something goes after
  say: say                  # scheduled tasks' warning…
  save: save                # …and save commands
  save_before_stop: false   # send save before hlm stops, restarts or recreates it
  exec:                     # images that bring their own command sender
    - image: someone/mygame-server
      command: "send-command {command}"
      why: This image has send-command.
      off_when: { RCON_ENABLED: "false" }
  rest: { driver: palworld, commands: [Info] }  # a built-in REST driver (palworld is the only one)
  players:                  # reading the player list through the console
    command: players
    format: lines           # csv (header), summary (pattern with online, max, names) or lines (pattern with name)
    pattern: '^\d+\. (?P<name>.+)$'
    ignore: [No players online]
  actions:                  # managing players (see Managing players)
    kick: "kick {player} {reason}"
    ban: "ban {player} {reason}"
settings_files:
  - names: [server.cfg]
    prefer: /cfg/           # among several files of that name, the one whose path contains this
    words: [mygame]         # only for images with these words too
player_lists:               # where the game keeps its lists: allow, ban, admin
  ban: { file: banned.txt, format: lines }   # lines or minecraft-json
mods:                       # installing mods from Modrinth (see Mods and plugins)
  start_timeout: 10m        # how long it gets to come back after a change (5m)
  modrinth:
    loader_env: TYPE        # the variable naming the mod loader…
    loader_default: VANILLA # …and its value when it isn't set
    version_env: VERSION    # the game version; else what the server reports
    loaders:                # the loader values that take mods: Modrinth loaders and the folder
      FABRIC: { modrinth: [fabric], dir: /data/mods }
      PAPER: { modrinth: [paper, spigot, bukkit], dir: /data/plugins, plugins: true }
    warn_env: [MODRINTH_PROJECTS]  # variables that make the image install mods itself
```

Every field but `id`, `name` and `match` is optional. The built-in profiles are good examples: **View** shows each
one.

#### Sharing profiles

- **Download** saves any profile as a `.yaml` file, and **Download all of yours** saves your own as a `.zip`, to give
  to someone or publish (a GitHub repository or gist works well).
- **Import from a link…** takes an HTTPS link to a profile: a GitHub file (its page link works too), a gist, or any
  web page serving the YAML. hlm fetches it and shows it before anything is saved: whether it adds a game, replaces a
  built-in profile or replaces one of yours, and the lines that change. **Save it** keeps it, only if the link still
  serves exactly what you looked at.
- **Updates.** hlm remembers where a profile came from and fetches the link again every day (and with **Check for
  updates**). A changed profile shows **Update available**; nothing changes until you **Review update** and accept it,
  since a profile decides which commands hlm sends. If you edited the profile here since importing it, it says so, and
  the update's lines marked "-" include your changes.
- **Profile indexes** are lists of profiles someone publishes, such as a community repository: an `index.yaml` with
  each profile's `id`, `name`, `url` (absolute, or relative to the index) and `sha256`, and optionally a
  `description`. Add an index's link under **Profile indexes** to browse it and import from it with **Look at it**.
  hlm uses a profile from an index only if its SHA-256 matches what the index lists, and offers an update when the
  index lists a new one. None is set up by default.

  ```yaml
  name: Our game profiles
  profiles:
    - id: mygame
      name: My Game
      url: profiles/mygame.yaml
      sha256: 3b1f…          # sha256sum profiles/mygame.yaml
      description: The dedicated server from someone/mygame-server.
  ```

hlm fetches links itself, not your browser: HTTPS only (redirects too, at most 3), 10 seconds, at most 64 KiB for a
profile (256 KiB for an index), no credentials, and never from hlm's own machine or link-local addresses (where cloud
metadata services are), checked on the address actually connected to. Where each profile came from is kept in
`game-profiles/.origins.json`, which is in the configuration backups too.

## Health checks and crash loops

**Crash loops.** With **Start on boot**, Docker restarts a server every time it exits, so a server that can't start (a
bad setting, a corrupt save) restarts forever: burning CPU, filling its log, maybe damaging more of its data, and
showing as running most of the time. hlm counts each server's exits that nobody asked for. If one exits **5 times
within 10 minutes**, hlm stops it, shows a red **Crash loop: stopped by hlm** badge with **View logs** and **Start
anyway**, and sends a `server.restart_loop` [notification](#notifications) with its last 50 lines of output. It stays
stopped until someone starts it (in hlm or with Docker); starting it forgets the exits counted so far. hlm remembers
which servers it stopped across its own restarts.

- What counts: any exit hlm didn't cause or see asked for, clean ones too (Docker restarts those as well).
- What doesn't: stops and restarts by people, hlm, backups and scheduled tasks (anything that goes through
  `docker stop`/`restart`/`kill`), and exits while the server is being installed, updated or reconfigured.
- The limit, or turning it off, is per server: **Settings → Health and crash loops**.
- Docker starts servers with Start on boot when the host reboots, even one hlm stopped; if it's still broken, it's
  stopped again after another 5 exits.

**Health.** Servers whose image has a Docker health check show **healthy**, **unhealthy** or **starting** next to
their state, and a `server.unhealthy` notification is sent when one starts failing. On **Settings → Health and crash
loops** a manager can give a server a check of its own, for images without one (or with one that doesn't suit):

| Check | Healthy when |
| --- | --- |
| The image's own | The image's check passes (or there's no check). |
| TCP port | A port in the container accepts connections (with `nc`, or `bash` if the image has no `nc`). |
| Web page | An HTTP GET of a path on a port in the container answers 2xx or 3xx (with `curl` or `wget`). |
| Command | A shell command run in the container (`/bin/sh -c`) exits with 0. |
| None | Never checked: turns off the image's check. |

Custom checks run every 60 seconds by default, may take 10 seconds, count as unhealthy after 3 failures in a row, and
don't count failures for 10 minutes after a start, while a game loads its world; each of these can be changed.
**Test now** runs the check once in the running server and shows its result and output, so you can tell whether the
image has the tools it needs. The check is part of the container, so it's set when the server is next recreated:
**Update image** or **Save & recreate** (the card says when the container doesn't have it yet). Templates can set one
with `--health-cmd`, `--health-interval`, `--health-timeout`, `--health-retries` and `--health-start-period` in their
`ExtraParams`, or turn the image's off with `--no-healthcheck`; a check set in hlm wins.

Game ports are often UDP, which these checks can't test. For servers whose [status query](#is-the-game-up-players-and-status)
works, the query can be the health signal instead.

**Restarting unhealthy servers** (off by default): hlm can restart a server once its health has been failing for a
number of minutes, going by Docker's health check or by the game's status query not answering. Each restart counts
towards the crash-loop limit, so a server that never recovers is stopped rather than restarted forever. Restarts wait
while the server is being updated or backed up; each one is in the [activity log](#activity-audit-log) and sends a
`server.unhealthy` notification.

**Who can do what.** Anyone who can see a server sees its health and crash-loop state. Operators can start a server
stopped for a crash loop, and see the commands of health checks (they can carry passwords). Changing the health
settings and running **Test now** takes a manager of the server: a check is a command run inside the server's own
container, which managers can already do through its files and console, and it can't reach anything the container
can't.

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
    command run in the container instead. Keep `gw_priority: 1` on hlm's `default` network, as there: without it,
    hlm's default route moves to the macvlan network, and its published port (e.g. `http://<host>:8080`) stops
    answering, since replies leave the wrong way.
  - **Published ports:** a server hlm can't reach directly is reached through its ports published on the Docker host.
    When hlm runs in a container, it uses its bridge network's gateway for that (the host, as seen from the container),
    which works even when hlm is also on a macvlan network, from which the host's own LAN address can't be reached.
  - **Connection:** it's kept open between commands and closed after 5 idle minutes.
- **Palworld's REST API:** Palworld replaced its RCON with a [REST API](https://docs.palworldgame.com/category/rest-api),
  turned on with `RESTAPIEnabled=True` (`REST_API_ENABLED=true` in most images) on port 8212. hlm signs in as `admin`
  with the server's admin password (from a variable such as `ADMIN_PASSWORD`, or entered by hand) and reaches the port
  the way it reaches RCON. It takes the commands people know from RCON and makes the API calls for them:

  | Command | API call |
  | --- | --- |
  | `ShowPlayers` | `GET /players`: `name,playeruid,steamid,level,ping`, one line per player |
  | `Metrics` | `GET /metrics`: players and limit, server FPS and frame time, uptime, in-game day, base camps |
  | `Info` | `GET /info`: server name, version, description, world |
  | `Settings` | `GET /settings`: the server's settings, as JSON |
  | `Save` | `POST /save` |
  | `Broadcast <message>` | `POST /announce` (spaces are fine, unlike over RCON) |
  | `KickPlayer <userId> [message]`, `BanPlayer <userId> [message]`, `UnBanPlayer <userId>` | `POST /kick`, `/ban`, `/unban`, with the user ID `ShowPlayers` lists as `steamid` |
  | `Shutdown <seconds> [message]` | `POST /shutdown`: the server counts down, then quits |
  | `DoExit` | `POST /stop`: stops it straight away, without saving |

  Palworld doesn't save its world when it's told to stop, so when its console can save (REST API, or RCON or
  `rcon-cli`), hlm sends `Save` first whenever it stops, restarts or recreates the server: **Stop**, **Restart**,
  updates, **Save & recreate**, backups that stop it, scheduled tasks and restarts for failing health. It waits up to 30
  seconds for that, then stops the server anyway. The API isn't meant to face the internet, so don't forward its port.
- **A command run in the container:** a command line run in the container for each command, with `{command}` replaced
  by what you type (or added at the end), such as `rcon-cli {command}`. It isn't run by a shell. Images like
  `itzg/minecraft-server` and Palworld's come with `rcon-cli`, which already knows the password.

**Console settings** suggest a setup when the server's image or settings make it clear: Palworld's REST API when a
Palworld server turns it on (`REST_API_ENABLED`) or exposes or publishes port 8212, `rcon-cli` or `send-command` for the
images that have them, RCON when there's a password variable and a port, and typing into the console for games known to
read it. **Test** checks the settings before saving them: it signs in over RCON or to the REST API (and shows the
server's name and version), looks for the command in the container, or checks the container takes input.

- Commands are at most 1 KiB; control characters other than tabs are removed. Each person can send at most 5 commands a
  second to a server.
- Commands you sent before are kept in your browser: ↑ and ↓ go through them.
- Shortcuts above the command line put common commands for the game (Minecraft, Palworld, ARK, Rust, Terraria,
  Factorio, Counter-Strike 2, 7 Days to Die) and your own into the command line, for you to finish and send.
- hlm logs each command, with who sent it, but not the reply.

Console commands can do whatever the game's admins can, and some games can run scripts or change files through them,
so giving someone the **operator** role on a server lets them administer the game.

## Managing players

A server's **Players** tab (operators and up; shown when its game has a [profile](#game-profiles)) lists who's on and
its allow, ban and admin lists, with buttons for what its game can do:

- **Online now**: each player, through the console's player list when it can (with their ID for games that act on IDs),
  or from the last [status check](#is-the-game-up-players-and-status). **Kick**, **Ban** and **Make admin** ask for
  an optional reason (kick and ban) and send the game's command through its [console](#sending-commands-to-a-server),
  as if typed there.
- **Bans**, **Allow list** and **Admins**: read from the game's own files, so changes made in the game show too, with
  **Unban**, **Take off the allow list** and **Remove admin**. **Add a player** adds someone who isn't online.
- **Operators can kick; banning and the lists take a manager.**
- **While a server is stopped**, games that keep their lists as plain files (Valheim's `adminlist.txt`,
  `bannedlist.txt`, `permittedlist.txt`) have the file changed directly, keeping its comments; the game reads it when
  it starts. Changes to Valheim's lists while it runs are refused, since it reads them only at start. Games with
  commands (Minecraft) need to be running for changes.
- **Names and IDs are checked before they go into a command:** a name is one word of up to 64 characters without `;`,
  quotes or backslashes, an ID only letters, digits, `_ . : -`, and a reason one line of up to 200 characters with the
  same limits. Anything else is refused, not cleaned up, so a crafted name can't add a second command.

| Game | Kick | Ban | Allow list | Admins | Lists read from |
| --- | --- | --- | --- | --- | --- |
| Minecraft (Java) | ✓ | ✓ | ✓ | ✓ (op) | `whitelist.json`, `banned-players.json`, `ops.json` |
| Minecraft (Bedrock) | ✓ | (none) | ✓ | ✓ (op) | `allowlist.json` |
| Palworld | ✓ (by ID) | ✓ (by ID) | | | |
| ARK | ✓ (by ID) | ✓ (by ID) | ✓ (by ID) | | |
| Factorio | ✓ | ✓ | ✓ | ✓ (promote) | |
| Valheim | | while stopped | while stopped | while stopped | `bannedlist.txt`, `permittedlist.txt`, `adminlist.txt` |

Other games can get them in their profile: `console.actions` (`kick`, `ban`, `unban`, `allow`, `disallow`, `admin`,
`unadmin`, each a command with `{player}`, `{id}` and `{reason}`), `console.players.id_column` (or an `id` group in a
`lines` pattern) for games that act on IDs, and `player_lists` (`allow`, `ban`, `admin`: `{file, format}` with format
`minecraft-json` or `lines`).

Each action is in the [activity log](#activity-audit-log) as `servers.players.<action>` with the player and reason, and
in the tab's **Done through hlm**. A ban sends the `server.player_banned` [notification](#notifications).

## Mods and plugins

A server's **Mods** tab (shown when its game has a [profile](#game-profiles) that says how it takes mods; Minecraft
Java with `itzg/minecraft-server` is built in) installs, updates and removes mods from [Modrinth](https://modrinth.com).
On Paper, Purpur, Spigot, Bukkit and Folia servers they're plugins, in `/data/plugins`; on Fabric, Quilt, Forge and
NeoForge, mods in `/data/mods`. Vanilla servers don't load mods: set the server's `TYPE` variable first.

- **Installed**: each mod's name and version, whether hlm installed it (and who, when), and whether an update is out.
  Files put in the folder some other way are recognised by their hash when Modrinth has them (**added outside hlm**),
  and can be updated like the rest. **Turn off** keeps a mod but stops it loading (it's renamed to `….jar.disabled`).
- **Add a mod**: search Modrinth. Only mods that run on servers, with a version for the server's loader and game
  version, are shown. The game version comes from the server's `VERSION` variable or, when that's `LATEST`, from what
  the server reports to its [status query](#is-the-game-up-players-and-status).
- **Before anything changes, hlm shows what it will do**: the files it adds, updates and removes, the mods it needs
  too (a mod's required dependencies are added, newest release that fits), and warnings (a mod that says it doesn't
  work with one you have, a dependency that's turned off).
- **Every change is a transaction.** hlm saves a copy of the folder, downloads the files, stops the server, makes the
  change, starts it and waits for it to answer its status query (up to 10 minutes for Minecraft). If the server stops,
  crashes or doesn't answer in time, hlm puts the folder back as it was, starts the server again, and sends the
  `server.mods_failed` [notification](#notifications). A stopped server is changed and left stopped.
- **Updates** are looked for once a day and with **Check for updates**; `server.mod_updates` says when there are new
  ones. A newer version that needs a different game version is shown, but not offered. **Update all** updates every mod
  that can be.
- **Managers** add, update, turn off and remove mods; operators can check for updates; viewers can see the list.

Mods are code that runs in the server's container, so they're treated like its image: files come only from
Modrinth's own download host (`cdn.modrinth.com`) over HTTPS, each is checked against the SHA-512 Modrinth lists
before it's installed, and file names are checked (a `.jar`, no paths). Each change is in the
[activity log](#activity-audit-log) as `servers.mods.apply`, with the files and their hashes. hlm doesn't need a
Modrinth account.

hlm records what it installed in `.hlm-mods.json` in the mods folder, so the record travels with the server's backups
and clones. If the image installs mods itself (`MODRINTH_PROJECTS`, `MODS`, `PLUGINS`, `REMOVE_OLD_MODS` and the
like), the tab warns that the two can undo each other's changes.

## Stopping idle servers

A game server holds its memory and CPU whether anyone is playing or not. On a server's **Schedule** tab, **When nobody's
playing** (managers) can stop it after a while without players, and start it again when someone wants to play:

- **Stop it when nobody has played for N minutes** (5 minutes to a day; 30 by default). hlm counts the time from its
  [status query](#is-the-game-up-players-and-status), so it needs a query that counts players; the card says when it can't. A query that
  fails, or a player joining, starts the count again. A minute before stopping, players get a warning through the
  game's `say` command (if its [console](#sending-commands-to-a-server) is set up); hlm checks again that nobody's on, then stops it,
  saving first for games that need it. It doesn't stop a server while a job (an install, update, backup or restore)
  or one of its scheduled tasks is running.
- The server shows as **Asleep** in the server list, on its page and on the status page. Starting it in any way (its
  Start button, a scheduled task, Docker on boot) wakes it; hlm notices.
- **Start it when someone connects to it.** While it's asleep, a small **wake listener** container takes its published
  ports. It's made from the server's own image with hlm's binary copied in, so it works on [other Docker
  hosts](#more-docker-hosts) too, and it runs as nobody, with no capabilities, no mounts and 64 MB of memory. When
  someone really tries to play, it exits and hlm starts the server; they connect again a minute or two later.
  - **Minecraft (Java):** the server list shows **Asleep** with your MOTD. A player joining sees "The server is
    starting: join again in a minute or two" and wakes it; list pings don't.
  - **Minecraft Bedrock:** the server list shows it as asleep, and a player joining wakes it; the LAN discovery pings
    clients send all the time don't.
  - **Other games:** any TCP connection that sends something, or any UDP packet, wakes it, except Steam's server
    queries (`A2S`), so server browsers refreshing don't start it. A port scan that only connects doesn't either.
  - It needs published ports (not the host's network, or a macvlan address of its own), a static Linux build of hlm
    (releases are) and an image for the same CPU architecture as hlm. The card says when one is missing.
  - With ports open to the internet, anyone who connects can start the server; that's all they can do.
- **Let visitors start it from the status page.** A **Start it** button beside it on the [public status
  page](#public-status-page) while it's asleep (if it's on the page). Each address can start servers three times, then
  once every 10 minutes.

Each sleep and wake is in the [activity log](#activity-audit-log) as `servers.idle.sleep` and `servers.idle.wake`, with
what woke it ("a Minecraft player joining to port 25565/tcp from 192.168.1.20"). [Notifications](#notifications):
`server.asleep` and `server.wake_failed`.

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

On a server's **Backups** tab, add folders by browsing the container (or with the data folder shortcuts). **Back up now**
runs a backup straight away. Then choose:

- **Schedule:** off; every 1 hour to 1 week; or **at a time of day** (say 04:00) on chosen weekdays, in hlm's time zone.
- **Which backups to keep.** Either the newest N (0 keeps all), or **by age**: the newest few, plus the newest backup of
  each of the last so many days, weeks (ISO weeks) and months that have one (by default 3, 7 days, 4 weeks and 6
  months). A backup any rule keeps is kept. Older ones are deleted after each successful backup.
- Whether to **stop the server while backing up**.

**More options:**

- **Leave out:** patterns for files not worth backing up, one per line:
  - `*.log` matches a name anywhere;
  - `cache/` (ending in a slash) matches only folders;
  - `/data/logs` matches a whole container path;
  - `mods/*.tmp` matches the end of a path.

  An excluded folder leaves out everything in it. SteamCMD's game files (`steamapps/`) are worth leaving out: they're
  downloaded again anyway. The last backup's summary says how many files and folders were left out.
- **Console commands before and after** (one per line), sent through the server's [console](#sending-commands-to-a-server)
  when it's running. For example, Minecraft's `save-off` and `save-all flush` before, and `save-on` after, give a
  consistent backup without stopping the server. A command that fails is noted on the backup, which goes ahead anyway.
- **Check each backup** (on by default for backups under 5 GB; or always, or off). After a backup is stored, hlm:
  - downloads it again;
  - checks its size and its SHA-256 checksum, which is stored beside it as `<backup>.sha256` (in `sha256sum`'s format);
  - decrypts it if it's encrypted;
  - reads every file in it through gzip, whose own check covers the contents;
  - counts the files.

  Nothing is written to disk. A backup that fails this counts as failed, and sends `backup.verify_failed`. **Verify**
  on any listed backup does the same check on demand. The result shows as **✓ verified** or **✗ damaged**.
- **Also copy each backup to** another backup storage (see below), with its own rule for which copies to keep there. After
  a backup is stored (and checked), it's streamed from the primary storage to the other one. The tab lists the copies
  there too, with **Restore**, **Verify**, **Download** and **Delete**. A copy that fails is a warning on the backup.

**More backup storage.** Under **System → Backup storage**, below the primary storage (where every backup goes), **Add
backup storage** adds others, such as an off-site bucket (Backblaze B2, Cloudflare R2, AWS S3, another SeaweedFS). They're
checked the same way when saved. The primary can only be disconnected once it's the only one.

**Encryption.** Any backup storage can **encrypt backups** before they're stored:
- AES-256-GCM, in chunks, so backups still stream without being staged on disk; a file that's cut short or changed is
  caught.
- The key is made when encryption is turned on, shown once to keep somewhere safe (**Show key** shows it again, after
  confirming it's you), and kept in hlm's encrypted settings, so configuration backups include it.
- hlm decrypts on restore, verify and download, so a downloaded backup is a plain `.tar.gz`.
- Without hlm: `hlm decrypt-backup KEY backup.tar.gz plain.tar.gz` (or `-key-file`). It runs in the image too:
  `docker run --rm -i danotchuuy/home-lab-manager decrypt-backup KEY < in > out`.
- Turning encryption off keeps the key, so older encrypted backups can still be read.

[Notifications](#notifications) also report a backup that fails its check (`backup.verify_failed`), and a server whose
scheduled backups haven't worked for a while (`backup.stale`): no successful backup for twice the schedule's longest gap,
and at least a day and a half.

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

### Restore tests

**Verify** checks a backup reads back whole, but not that the game can load it: a backup of the wrong folder, a world
saved mid-write, or a missing file the game needs all pass it. A **restore test** checks that for real. On the
Backups tab, **Test a restore** (managers) restores a backup into a **throwaway copy** of the server and starts it:

1. The copy, `hlm-restore-test-<server>`, runs the image the server runs now (by ID, so nothing is pulled) with the
   same settings and limits, but with its own empty volumes in place of the server's data folders, **no published
   ports**, no restart policy, and a network of its own made with `--internal`, which reaches neither the internet nor
   your LAN. It doesn't get the server's devices, extra privileges or connections. The server keeps running throughout.
2. The backup is restored into the copy, the copy is started, and hlm waits for its game to **answer its status
   query** on that network, the way the server is asked (up to 10 minutes; change it per server, up to 2 hours). For a
   server whose status query is off, the copy passes by running for a minute.
3. The result goes in the tab: **✓ Restores and starts**, with how long it took and what the game said, or **✗ Failed**
   with why (it stopped, or never answered) and the copy's last 50 log lines. A failure sends the
   `backup.restore_test_failed` [notification](#notifications).
4. The copy, its volumes and its network are removed whatever happened, and at hlm's start if it stopped mid-test.

Since the copy can't download anything, the backup has to hold everything the server needs to start. For example,
`itzg/minecraft-server` downloads Minecraft when it's missing, so a backup of only `/data/world` fails the test
("Failed to install vanilla Minecraft"), while one of `/data` passes.

Tests can run **on a schedule**: once a week, or after every N backups, each time testing the newest backup if it isn't
tested yet. One test runs at a time. A test is **skipped** (not failed) when the host has less free memory than the
server uses, and tried again a day later. Tests run only for servers on hlm's own Docker host for now.

### Backing up hlm's own configuration

Once backup storage is set up, hlm also backs up its own configuration there, **every day by default**, keeping the
last 14. **System → Backups of hlm's configuration** changes the schedule and which to keep (the newest N, or by age, as for
servers), runs a backup now, and
lists, downloads or deletes them. They're stored as `<folder>/_hlm/hlm-config-<UTC time>.tar.gz`, apart from the
servers' backups.

Each backup holds, named as they are in the data directory (`HLM_DATA_DIR`):

- `secrets/secrets.enc`: every integration's settings and credentials (UniFi, proxy and its SSH key, Linode, Cloudflare,
  backup storage), servers' port options and backup plans. Encrypted with AES-256-GCM.
- `templates.json` (templates made from images) and `deleted-servers.json`, when they exist.
- `game-profiles/`: your own [game profiles](#game-profiles), and where those imported from a link came from.
- `audit/`: the [activity log](#activity-audit-log), a file a month.
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

## Compose stacks

Some apps need several containers: an app with its database and cache, or a game server with a web map or a bot.
**Catalog → Add a Compose stack** takes a `docker-compose.yml` (pasted, or loaded from a file) and an optional `.env`.
**Check** shows what happens to each service: its image, ports, data and where it starts. Then **Create the stack**:

- hlm creates the stack's networks (`<stack>_default`, and any under `networks:`), where services reach each other by
  name.
- It pulls each service's image and creates its container, in `depends_on` order, waiting for
  `condition: service_healthy` (a health check passing) and `service_completed_successfully` (a one-off job finishing
  with exit code 0).
- Each service is one of hlm's servers, named `<stack>-<service>` (or its `container_name`). Its console, files,
  backups, schedule, port forwarding, metrics and alerts work as for any other server.
- On the Servers page, a stack is one card that opens to its services, with **Start**, **Stop** and **Restart** (in
  dependency order, stopping in reverse), **Update** (pull each image and recreate the services whose image changed)
  and **Edit**.
- **Edit** shows the file and `.env`. **Check** says which services an edit creates, recreates, leaves alone (their
  settings didn't change) or removes. Applying recreates only those; a service that fails to come back gets its old
  container back.
- **Delete stack** removes its containers and networks. Data folders and named volumes are kept, and listed on the
  Host page, as for deleted servers.
- **Docker host.** With [more Docker hosts](#more-docker-hosts), a new stack's form asks which one it goes on; its
  services, networks and relative data folders (under that host's data folder) are all there, and its services are
  named `<name>@<host>`. A stack stays on its host: to move it, create it on the other one and delete the old one.

What hlm reads from the file:

- **Variables.** `${VAR}`, `${VAR:-default}`, `${VAR:?message}` and the rest of Compose's forms, from the `.env`
  only: hlm's own environment is never used. `env_file: .env` passes the `.env`'s variables to a service.
- **Data folders.** Relative paths (`./data`) go in `<appdata>/<stack>/`, and can't leave it. Absolute paths and named
  volumes (`<stack>_<name>`, or the `name:` given) are used as they are.
- **Services:** `image`, `command`, `entrypoint`, `environment`, `env_file`, `ports` and `expose`, `volumes`, `tmpfs`,
  `networks` (with aliases), `network_mode` (including `service:<name>`), `depends_on`, `restart`, `healthcheck`,
  `labels`, `devices`, `cap_add`/`cap_drop`, `user`, `working_dir`, `hostname`, `privileged`, `init`, `tty`,
  `stdin_open`, `read_only`, `mem_limit`, `cpus`, `pids_limit`, `shm_size`, `ulimits`, `extra_hosts`, `dns`,
  `security_opt`, `sysctls`, `stop_grace_period`, `stop_signal`, `deploy.resources.limits` and `deploy.restart_policy`.
- YAML anchors and `<<` merges work. As in Compose, a merge isn't deep: a service's own `environment:` replaces a
  merged one.
- **Not supported** (refused, saying so): `build` (build the image elsewhere and push it), `extends`, `profiles`,
  `configs`/`secrets` (mount files instead), `include`, and swarm `deploy` settings other than resource limits. Keys
  that only matter to Docker Compose (like `logging`) are ignored, with a warning.

The file and `.env` are kept in hlm's encrypted settings (`.env` files usually hold passwords), so they're in
[configuration backups](#backing-up-hlms-own-configuration). Creating, editing and deleting stacks takes an admin.
Operators of every service in a stack can start, stop, restart and update it. Server groups give access to services
one by one, as to any server. A stack's services are changed and updated from the stack, not each server's
**Settings**.

## Export, import and clone

**Export settings** (a server's **Settings** tab) downloads `<server>.hlm.json`, which holds:
- the server's template, so it can be installed where the template isn't in the catalog;
- its settings: ports, data folders, variables, network, Docker options and whether it starts on boot;
- its scheduled tasks, backup plan, automatic-update policy, status-query, console, health and pinned-files settings.

Its data isn't in the file. Neither are its web addresses, port forwarding and server groups, since those reach outside
the server.

- **Paths under the appdata folder** (`HLM_APPDATA_ROOT`) are written as `${APPDATA}/…`, so they land in the other hlm's
  appdata folder.
- **Passwords and keys** (settings whose names look like passwords, tokens or keys, Docker options and the console's
  password) are left blank, and the file lists which. **Include passwords and keys** exports them too. That takes a
  manager of the server, at a browser, after confirming it's them; keep that file safe.
- Operators can export a server without its passwords.

**Catalog → Import a server** (admins) reads a file and shows the install form, filled in, so you can change anything
first. It warns when:
- passwords are blank;
- a server with that name, or one already using one of its data folders, exists here;
- a path is outside the appdata folder;
- it has a fixed IP address;
- its template isn't in the catalog. The template is then added from the file, as a custom template.

Installing gives the new server the file's other settings too. Files from a newer hlm's format are refused.

**Clone…** (admins, next to Export settings) makes a copy on this hlm. The copy:
- gets a new name;
- gets its own data folders: the folder under the appdata folder named after the server is renamed (`valheim` →
  `valheim-copy`), or else the path goes into a folder named after the copy. A folder outside the appdata folder that
  isn't named after the server stays shared, with a warning, so you can change it;
- gets free host ports: all of them move up by the same step, so a game port and the query port after it stay together.
  Servers with their own LAN address keep their ports; with host networking, you're warned to change the game's ports;
- doesn't keep a fixed IP address;
- gets all the other settings, passwords included.

For its data, choose:
- **Copy the data folders as they are now.** The data folders, and Docker volumes made for the image's `VOLUME` paths,
  are copied through Docker, the way a backup is made and restored, but without storing anything in between. **Stop the
  original while copying** (on by default) keeps a game from being caught halfway through a save. The original is
  started again as soon as the copy is done.
- **Copy one of its backups:** restores that backup into the copy.
- **Settings only:** the copy starts with empty data folders.

The settings form is shown before cloning, to change anything. The copy is started when it's ready, unless you untick
that. If copying fails, the half-made copy is removed; the folders it made stay on disk and are listed under
[deleted servers' data](#host-metrics) on the Host page.

**Restore as new server…** (a backup on the **Backups** tab, admins) is a clone that copies that backup, leaving the
original as it is: to look at an old world, or to try something on a copy.

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

**Other Docker hosts.** With [more Docker hosts](#more-docker-hosts), each host can have its own end of the tunnel:
under **Other Docker hosts** in the tunnel's card, **Set up _host_'s end**. hlm runs an `hlm-wireguard` container
there (host networking, as here), with its own key and its own address in the tunnel (the next free one: `.3`, `.4`, …),
and adds it to the proxy server's `gsh0` as another peer (which restarts the proxy's end briefly). It connects out to
the proxy too, so the host can be anywhere. nginx then relays each server to the tunnel address of the host it runs
on; a server on a host without an end is listed with an error and skipped. **Check tunnel** shows each end's last
handshake. An end can be set up again (same key and address) or removed; removing the whole tunnel removes them all.
The tunnel network can't change while other hosts have ends in it. An end can use **LAN mode** too: in its setup form
(**Set up _host_'s end…**, or **change**), choose one of that host's macvlan/ipvlan networks (listed from its Docker),
optionally with a fixed address there. It then reaches that host's servers with their own LAN address, as this host's
end does, and the proxy routes that network's subnet to it; only one end (this host's or another's) can reach a given
LAN. **Set up again** keeps an end's settings.

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
   domains you want to use (add **Zone → SSL and Certificates: Edit** to put the
   [status page's sites on the proxy server](#public-status-page) behind Cloudflare). **Test connection** lists the
   accounts and domains it can use; **Save** stores it encrypted, like the other credentials. If the token can use several accounts, choose the one the tunnel goes in. The token
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

**Other Docker hosts.** With [more Docker hosts](#more-docker-hosts), servers on other hosts are reached from this host's
cloudflared over the LAN, at their host's LAN address. A host that isn't on the same network (behind NAT with
[hlm's agent](#more-docker-hosts), say), or that you'd rather reach directly, can have a **tunnel of its own**: under
**Tunnels on other Docker hosts**, **Give _host_ its own tunnel**. hlm creates `hlm-<host>` in your Cloudflare account and
runs `hlm-cloudflared` on that host (host networking). A tunnel's connectors share its traffic, so each host needs its
own tunnel rather than another connector of the same one. That host's servers' hostnames then go through its tunnel,
reached on its `localhost` (each hostname shows "through _host_'s tunnel"), and their DNS records point at it;
hostnames for other web addresses and for hlm itself stay on this host's tunnel. **Remove** takes the host's tunnel
down and moves its hostnames back to this host's tunnel. Its container is listed with the
[connections](#connections) and can be updated there.

## Downloads in progress

Installs, updates and settings changes run on the backend, and the server list shows their progress to everyone, not
just the browser that started them. New servers appear under **Downloads in progress** with the image download (bytes,
layers and percentage) until they're created. Servers being updated show the same progress on their own card. Only one
operation can run on a server at a time. Progress covers the Docker image download; game files that a container
downloads itself on first start (e.g. through SteamCMD) show up in the server's console instead.

## Host metrics

The **Host** page shows the Docker host's CPU, memory, network and disk activity (current values plus 30 minutes of
history, sampled every 5 seconds, or longer [history](#resource-history) up to 30 days), its filesystems, Docker's own disk usage, and disk usage per server. Per-server usage
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

### Disk cleanup

Updates leave the old image behind (game images are often several GB), and deleted servers leave their images. The
Host page's **Clean up** finds what Docker can let go of, on this host or (with the picker) any other
[Docker host](#more-docker-hosts), and deletes what you tick:

| Item | What it is | Ticked to start with |
| --- | --- | --- |
| **Old images** | Untagged images no container uses: left when an update moved the tag to a newer image | Yes |
| **Leftover containers** | Stopped containers hlm left behind: its helpers, and a recreated server's `<name>-old-<time>` container (only when `<name>` exists again) | Yes |
| **Unused images** | Tagged images no container uses | No |
| **Build cache** | Docker's build cache | No |
| **Unused volumes** | Volumes no container uses, except deleted servers' data | No (asks again, naming them) |

Nothing a container uses (running or stopped, hlm's or not) is a candidate, images are deleted without force (Docker
refuses one in use, and that's reported), and what's asked for is checked again before anything is deleted. Deleted
servers' folders and volumes stay with **Deleted servers**. Image sizes include layers other images may share, so a
little less may be freed than shown.

Two settings, in the same card:

- **Delete a server's old image once an update has worked** (on by default): after an update (or a settings change that
  uses another image) succeeds, the previous image is deleted if no container uses it and it has no tags left. A failed
  update is rolled back first, so its image is kept.
- **Every week** (off by default): old images and leftover containers, the ticked-by-default items, on every host. The
  last run and what it freed are shown, and each run is in the activity log as `host.cleanup.weekly`.

## Resource usage

The server list shows each running server's CPU, memory and network use, and the server page adds disk I/O and process
count. Both refresh every 5 seconds. CPU is shown as a share of the whole host (the server page also shows cores in use),
so it compares directly with memory. Memory is shown against the container's limit, or the host's total memory when
there is no limit. Network and disk figures are totals since the container started.

### Resource history

hlm keeps a month of resource history itself, without a database, so "why was it laggy last night?" has an answer:

- **What:** for the host, CPU, memory, network, disk reads and writes, and each filesystem's space used; for each
  server, CPU, memory, network, disk reads and writes, and players online (when its [game status](#is-the-game-up-players-and-status) counts
  them). The host is sampled every 5 seconds and servers every 15.
- **How long:** each minute's average and peak for 24 hours, and each quarter hour's for 30 days.
- **Where:** the **Host** page's range buttons (Live, 6 h, 24 h, 7 days, 30 days), and **Resource history** on a server's
  page (anyone who can see the server). CPU charts show the peak as a dashed line beside the average, since a short
  spike is what players notice.
- **Storage:** `<data>/metrics/`. Each finished minute is appended to `minutes.jsonl` (a few kilobytes a minute), and
  the whole history is written to `snapshot.gob` every 6 hours and when hlm stops, which starts the log again. A restart
  loses at most the minute in progress. At 20 servers the snapshot is around 15 MB. History is kept by server name, so
  it carries on across updates and settings changes; a deleted server's goes 30 days after its last sample. A damaged
  snapshot is set aside (`snapshot.gob.bad`) and history starts again.

### Prometheus

`GET /metrics` serves hlm's state in Prometheus' text format, for Grafana dashboards and alerts:

- **Servers** (labels `server`, `app`, `image`, `host`: `local` or the [Docker host](#more-docker-hosts)'s ID): `hlm_server_up`, `hlm_server_cpu_ratio` (in cores),
  `hlm_server_memory_bytes`, `hlm_server_memory_limit_bytes`, `hlm_server_network_{receive,transmit}_bytes_total`,
  `hlm_server_block_{read,write}_bytes_total`, `hlm_server_restarts_total`, `hlm_server_update_available`, and from the
  [game status](#is-the-game-up-players-and-status) `hlm_server_query_up`, `hlm_server_players` and `hlm_server_players_max`.
- **Backups** (label `server`): `hlm_backup_last_success_timestamp_seconds`, `hlm_backup_last_size_bytes`,
  `hlm_backup_last_duration_seconds`.
- **Hosts** (label `host`: `local`, or the ID of a host with [hlm's agent](#more-docker-hosts)): `hlm_host_cpu_ratio`,
  `hlm_host_cpu_cores`, `hlm_host_load1`, `hlm_host_memory_{used,total}_bytes`, `hlm_host_filesystem_{size,avail}_bytes` (labels `mount`, `device`) and `hlm_host_network_{receive,transmit}_bytes_total`
  (label `interface`), when [host metrics](#host-metrics) are available.
- **hlm:** `hlm_build_info{version}`, and `hlm_https_cert_expiry_timestamp_seconds` with [HTTPS](#https) on.

Server figures are from the last 15-second sample, so scraping doesn't add load on Docker.

To scrape it, make an [API key](#api-keys) with the **Metrics** role. It can read `/metrics` and nothing else; every
`/api` request with it is refused. Admin keys and `HLM_TOKEN` work too, and so does anyone when sign-in isn't required.
**System → Prometheus** shows a scrape config to copy:

```yaml
scrape_configs:
  - job_name: hlm
    metrics_path: /metrics
    authorization:
      credentials: hlm_…   # the metrics key
    static_configs:
      - targets: ["192.168.11.5:8080"]
```

**System → Prometheus** can also let listed addresses or ranges (your Prometheus server) scrape without a key. That's
off until you list one, and needs a [confirmed](#confirming-its-you) admin, since server names and images are in the
metrics.

A starter Grafana dashboard is in [`docs/grafana/hlm-dashboard.json`](docs/grafana/hlm-dashboard.json)
(**Dashboards → New → Import**). It covers servers running, players, updates, backup age, CPU, memory, network,
restarts, and the host's CPU, memory and disk space, with a server picker.

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
refreshes a template; **Delete template** removes it (servers made from it keep running). Images from private
registries need a login under **System → Registries** first (see [Private registries](#private-registries)).

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

## Using the web UI

- **Theme.** The ◐ button in the header switches between the system's setting, light and dark; the choice is kept in
  the browser.
- **The server list** has a search (name, app, image, tags, host, address or port: every word has to match), filters
  for state (running, stopped, with a problem, with an update), server group, tag and Docker host, and a sort by name,
  state, CPU, memory or players. The view is in the page's address, so it can be bookmarked or shared.
- **Favourites.** ☆ pins a server to the top of the list. Favourites are each person's own (each account, single
  sign-on user or API key; everyone shares one set while sign-in isn't required) and follow them to other browsers.
- **Tags.** Managers can give a server up to 10 tags on its page (**+ Add tags**). Clicking a tag shows the servers
  with it.
- **Bulk actions.** Tick servers (or **Select all** for what's shown) to start, stop, restart, update or back them up
  together. Each server is checked against your role on it; ones you can't act on are skipped and reported, and the
  progress shows each server's outcome.
- **Connect.** **Copy address** on a server's card copies where players connect (the server's host and published
  port); its page has a **Connect** card with every port and **Copy all**, as one message to send to players.
- **Notices.** When an install or update finishes while you're on another page, a notice says so (click it to open
  the server).
- **Keyboard.** `/` searches the server list, `Ctrl K` (`⌘ K`) opens the command palette (go to a server or page, or
  start, stop or restart a server by typing, e.g. `restart valheim`), `g s` / `g c` / `g h` / `g y` go to Servers,
  Catalog, Host and System, and `?` lists the shortcuts.
- **Phones.** The layout fits narrow screens, with larger touch targets, and hlm can be added to the home screen (it has
  a web app manifest and icons).
- **Times** like "5 min ago" show the exact time when you hover over them.

## Public status page

A page for friends who ask "is it up?" and "what's the address?", without an hlm account. Turn it on under **System →
Status page** and add the servers to show; it's at `/status`.

- **Per server:** a display name (the app's name to start with; hlm's own server names aren't shown), the address
  players use, an optional note (such as where to find the password: never the password itself), and whether to show
  players online, their names, and the game's version and map. **Suggest** fills the address from the
  [proxy](#relaying-through-a-proxy-server-nginx-over-ssh) for ports it relays, or the home network's public address
  from [UniFi](#port-forwarding-with-unifi) for ports it forwards.
- **What it shows:** each server's state (online, starting, up when hlm can't ask the game, or offline), what's
  chosen above, and how much of the last 7 and 30 days it was up (once there's an hour of history; recorded with the
  [resource history](#resource-history)). An optional message goes at the top ("Maintenance on Saturday").
- **What it doesn't:** settings, logs, files, resource use, addresses inside your network, or anything else of hlm.
  The response is built from its own fields only.
- **Badges** for a README or Discord: `/api/status-page/<name>.svg` ("Viking world | 3/10 online"), and the same as
  JSON at `.json`. The settings show each server's URL and a Markdown snippet.
- **Sites (optional):** the page under its own domains, such as `friends.example.com` for a group of friends and
  `status.example.com` for everyone. Each site shows the servers on the page that are in the
  [server groups](#roles-and-server-groups) you tick (all of them if you tick none), can have its own title and
  message, and has **its own two passwords** (or none): on a site's domain only that site's passwords work, and a
  password given on one site doesn't open another. A site's domain reaches only the status page: every other path (the
  rest of the web UI and API, even with a token) answers 404 there, and `/` goes to `/status`. On any other address,
  such as hlm's own, the page shows every server with the page-wide passwords.
- **Sites on Cloudflare:** with [Cloudflare Tunnel](#web-addresses-with-cloudflare-tunnel) set up, each site has
  **Publish on Cloudflare**, which adds a tunnel route for its domain (and the DNS record) to hlm itself, and then
  shows whether it's published. On the Tunnel page the same route reads **Home Lab Manager (status page)**, a choice
  for any hostname. hlm works out where cloudflared reaches it: its own port, or the port its container publishes on
  the host (it says so if it doesn't publish one). It's sent as HTTP, which hlm accepts from the tunnel even with
  HTTPS on.
- **Sites on the proxy server:** with the [proxy server](#relaying-through-a-proxy-server-nginx-over-ssh) and its
  [WireGuard tunnel](#wireguard-tunnel-to-this-host) set up (and the tunnel chosen as the home address), tick **Serve the
  status page's sites here too** on the Proxy page and apply. nginx there answers each site's domain and passes only
  the status page's paths to hlm through the tunnel, so hlm's own port is never opened to the internet; everything
  else on those domains is 404 at the proxy as well as at hlm. **HTTPS** is one of:
  - **None:** plain HTTP; point each domain's DNS at the proxy server yourself.
  - **Let's Encrypt:** point each domain's DNS at the proxy server; certbot there gets a free certificate for each
    domain (and its timer renews them, reloading nginx), and plain HTTP redirects to HTTPS once a domain has one.
  - **Cloudflare:** for domains on the [Cloudflare account](#web-addresses-with-cloudflare-tunnel) (the tunnel itself
    isn't needed). hlm makes a proxied ("orange cloud") DNS record pointing each domain at the proxy server, and installs a
    Cloudflare Origin CA certificate for it there (valid 15 years; its key is made by hlm and readable only by root
    on the proxy, in `/etc/nginx/hlm-origin`). Visitors get Cloudflare's own certificate, and the proxy's firewall
    lets only Cloudflare's addresses reach ports 80 and 443. The token needs **Zone → SSL and Certificates: Edit**
    as well as DNS: Edit. Set the domain's **SSL/TLS mode to Full (strict)** in the Cloudflare dashboard (hlm warns if
    it's Off or Flexible, where Cloudflare would talk to the proxy unencrypted); port 80 serves the page as well, so
    the site works in any mode. A record that already exists for the name (yours, or a tunnel route's) is left alone
    and reported. When a domain leaves the list or you choose another option, hlm removes its record and certificate.

  hlm writes only `/etc/nginx/conf.d/hlm-status.conf` (nginx has to read `conf.d` in its `http` block, as Debian's,
  Ubuntu's and Fedora's do), checks it with `nginx -t` and puts the previous one back if it's refused. The proxy's Linode firewall,
  if hlm manages it, gets ports 80 and 443 opened (to Cloudflare alone, with that option). Each site's settings show
  whether the proxy serves it, and over HTTP or HTTPS.
- **Only from (optional):** addresses or ranges, such as a VPN's, that may see it. Behind a tunnel or proxy that's the
  tunnel's or proxy's address.
- **Passwords (optional), two of them** (page-wide, and per site): one to see the page at all, and one for the details: players' names, the
  addresses to connect to and your notes. Either can be used alone (with only the second, the page is public but the
  details aren't), and the details password opens the page too. A visitor types one once; a signed cookie (HttpOnly,
  30 days) remembers it in that browser until it's changed, and **Forget the password on this device** drops it.
  Passwords are stored as argon2id hashes and never shown again; wrong guesses are slowed down (five tries, then one
  every 12 seconds per address). With a view password, badges and the JSON only work in a browser that has given it.

It's off by default, and while it's off its endpoints answer 404. The page is rebuilt at most every 15 seconds, and each
address is limited to about two requests a second.

## Discord commands

A Discord bot lets people in your Discord servers check on the game servers and, if you allow it, start them, without
hlm accounts. One bot can answer in several Discord servers, each seeing its own game servers: friends in one, a club
in another. Set it up on **System → Discord** (admins; saving needs you to
[confirm it's you](#sign-in-accounts-and-api-keys)):

1. In the [Discord developer portal](https://discord.com/developers/applications), make an application, and under
   **Bot** choose **Reset Token**. Paste the token into hlm. The bot needs no privileged intents and no permissions.
2. In Discord, turn on **Developer Mode** (Settings → Advanced), right-click each server the bot should answer in,
   **Copy Server ID**, and add it in hlm with **Add a Discord server**. Turn the bot on and save.
3. Open the **invite link** hlm shows to add the bot to each server, then **Test**: each Discord server's line says
   whether its commands are there, and they appear when you type `/` in Discord.
4. For each Discord server, choose which game servers it **sees** (every server on the status page, or those in some
   [server groups](#roles-and-server-groups)), and what each of its roles may do.

| Command | Who | What it does |
| --- | --- | --- |
| `/status` | everyone in the Discord server | Every server: online, starting, offline or asleep, and how many are playing. |
| `/players <server>` | roles allowed **See who's playing** | How many are on, and who, with **Show addresses and who's playing** on. |
| `/start <server>` | roles allowed **Start** | Starts it (or wakes it, if hlm [put it to sleep](#stopping-idle-servers)) and answers once its status query does: "Valheim is up: 0/10 players". |
| `/stop <server>`, `/restart <server>` | roles allowed **Stop**, **Restart** | Stops it, or restarts it and waits as `/start` does. |
| `/say <server> <message>` | roles allowed **Say something** | Sends the game's [say command](#game-profiles) with `[Discord] name: message`. |

- **Discord sees only servers on the [status page](#public-status-page), as the page shows them**: their display
  names, never hlm's names for them, and players and versions only where the page shows them. The page doesn't have
  to be turned on. **Each Discord server sees only the game servers chosen for it**: `/status` lists those, and no
  command or rule reaches the others. Addresses and player names are shown only where that Discord server has
  **Show addresses and who's playing** on (what the page shows behind its details password).
- **Nothing is allowed by default** but `/status`. A rule gives one of that Discord server's roles (or **Everyone**)
  actions on every server it sees, or on some server groups. Server names autocomplete with only the servers you may
  use; asking for any other gets the same "you can't" as a server that doesn't exist.
- **Only in the Discord servers you added**: others and direct messages are refused. Each person can send 5 commands
  a minute.
- **hlm connects out to Discord** (its gateway), so nothing has to reach hlm from the internet. It reconnects by
  itself, and the System page shows whether it's connected. If Discord refuses the token, the bot stops until it's
  changed.
- **Everything is in the [activity log](#activity-audit-log)** as `discord:<username>` (via Discord): `servers.start`,
  `servers.stop`, `servers.restart` and `servers.say` with the message.
- The token is kept with hlm's other secrets and never shown again. **Disconnect** takes the commands out of every
  Discord server and forgets it.

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
| `server.restart_loop` | error | A server kept exiting (a [crash loop](#health-checks-and-crash-loops)), so hlm stopped it. The message includes its last 50 lines of output. |
| `server.unhealthy` | warning | A server's Docker health check starts failing, or hlm restarted a server whose health kept failing. |
| `server.online` | info | A server hlm saw start answers its game's [status query](#is-the-game-up-players-and-status): it's ready for players. |
| `server.not_responding` | warning | A running server stopped answering its status query, or never started answering. |
| `server.update_available` | info | A newer image is found for a server (once per image the server runs). |
| `server.updated`, `server.update_failed` | info, error | An update finishes or fails. |
| `hlm.update_available` | info | A new version of hlm is out (once per version). |
| `hlm.update_failed` | error | hlm couldn't [update itself automatically](#automatic-updates). |
| `backup.failed`, `backup.succeeded` | error, info | A server's or hlm's configuration backup fails or finishes. |
| `backup.verify_failed` | error | A backup was stored, but reading it back (after the backup, or **Verify**) showed it's damaged. |
| `backup.restore_test_failed` | error | A backup restored into a [test copy](#restore-tests) of its server, but the copy didn't start or answer. |
| `backup.stale` | warning | A server with scheduled backups hasn't had one work for twice its schedule's longest gap (at least a day and a half). |
| `restore.finished`, `restore.failed` | info, error | A restore finishes or fails. |
| `tunnel.down` | error | The WireGuard or Cloudflare Tunnel container stops on its own. |
| `wan.changed`, `proxy.update_failed` | info, error | The home address changed and the proxy was updated, or updating it failed. |
| `https.renew_failed`, `https.expiring` | error, warning | Getting or renewing hlm's certificate fails, and when it's getting close to expiring without being renewed. |
| `disk.low` | warning | A filesystem on the Docker host gets fuller than the threshold (90% by default), checked every 5 minutes. |
| `auth.locked_out` | warning | Sign-in for a local account is locked after repeated wrong passwords. |
| `server.asleep` | info | Nobody played on a server for a while, so hlm [stopped it](#stopping-idle-servers) until someone wants to play. |
| `server.wake_failed` | error | Someone wanted to play on a [sleeping server](#stopping-idle-servers), but starting it failed. |
| `server.player_banned` | info | Someone [banned a player](#managing-players) on a server through hlm. |
| `server.mods_failed` | error | A server didn't come back after a [change to its mods](#mods-and-plugins), so hlm put them back. |
| `server.mod_updates` | info | Newer versions of a server's [mods](#mods-and-plugins) are out (once per set of versions). |
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
existing account an admin, and `--reset-2fa` to turn off its [two-factor sign-in](#two-factor-sign-in) when its phone
and recovery codes are lost. For a binary install, run `hlm reset-password admin` with the same `HLM_DATA_DIR` (and `HLM_SECRET_KEY`, if set) as the server.

**Local accounts** need passwords of at least 10 characters. After five wrong passwords for a username, more attempts
are refused for 30 seconds, doubling with each further wrong password up to 15 minutes. Changing a password ends the
account's other sessions; removing an account ends all of them.

### Two-factor sign-in

Local accounts can add a second step to signing in: a [passkey](#passkeys), or a 6-digit code from an authenticator
app (Aegis, 2FAS, Google Authenticator, 1Password, Bitwarden, …), so a leaked password alone isn't enough. Single sign-on users get theirs from
their provider. API keys, SSH tokens and `HLM_TOKEN` aren't affected; they're already a separate credential, limited by
**allowed from**.

- **Setting it up.** **My account** (members) or **System → Sign-in** (admins, for their own account) → **Set up
  two-factor**, after confirming it's you.
  - hlm shows a QR code and the key as text. hlm draws the QR code itself, so the key isn't sent to any other site.
  - Enter the code the app shows to turn it on.
  - You get **10 recovery codes**, shown once: each signs in once in place of a code, if the phone is lost. Copy or
    download them.
  - **New recovery codes** replaces them, and **Set up again** moves it to a new phone.
- **Signing in** then takes the password, then the code (or a recovery code) within 5 minutes.
  - Codes are TOTP (RFC 6238: 30 seconds, SHA-1, 6 digits). One step of clock drift either way is allowed, and a code
    can't be used twice.
  - Wrong codes count towards the same lockout as wrong passwords.
  - Confirming it's you asks for a code too.
- **Requiring it.** **System → Sign-in → Require two-factor sign-in for local accounts**: for admins, or for every
  local account.
  - A passkey or the authenticator app counts.
  - It can only be turned on by someone who has it themselves (or who signed in with single sign-on), so it can't lock
    out the person turning it on.
  - Accounts that have to use it and don't yet set it up right after their password at the next sign-in: a passkey
    or the app.
  - They can't turn off their last second factor themselves while it's required.
- **Lost phone.**
  - Use a recovery code.
  - Or another passkey on the account.
  - Or an admin clicks **Reset** in the accounts table, which turns off the app and removes the account's passkeys
    (it then signs in with its password, or sets two-factor up again if required).
  - Or, on the host: `hlm reset-password <user> --reset-2fa` (the same).

### Passkeys

A passkey signs in with your fingerprint, face, device PIN or a security key, instead of a password. It's a key pair
kept by your device, your password manager or a security key; hlm keeps only the public half. It can't be phished:
browsers only use it on the site it was made for.

- **Adding one.** **My account** (members) or **System → Sign-in** (admins) → **Passkeys** → **Add a passkey**, after
  confirming it's you. Give it a name (hlm suggests one, like "Firefox on Linux"). An account can have up to 20. They
  can be renamed and removed there; the table shows where each works, when it was added and last used, and whether
  it's **synced** (in a password manager or a platform account, so on several devices).
- **Signing in.** **Sign in with a passkey** on the sign-in form needs no username or password: the authenticator
  has to verify you (PIN, fingerprint or face), so it's two factors in one. A passkey also works as the **second
  factor** after the password, in place of the app's code, and to **confirm it's you**. An account whose only second
  factor is a passkey confirms with it (its password alone isn't enough).
- **Where it works.** Browsers only allow passkeys on **HTTPS with a name** (see [HTTPS](#https)), or on
  `http://localhost`: not over plain HTTP on the network, and not on an IP address. A passkey belongs to the address
  hlm was opened on when it was added (its WebAuthn relying party ID), and works there; add one on each address you use
  hlm at. The forms say when passkeys aren't available on the current page.
- **Checks.** hlm asks for "none" attestation, as most sites do, so it doesn't care who made the authenticator; it
  checks the challenge (random, used once, valid for 5 minutes), the page's origin, the relying party, that you were
  present (and verified, where it stands alone), the signature (ES256, EdDSA or RS256) and the signature counter, which
  catches a copied security key. Failed attempts count towards the account's sign-in lockout.

### Roles and server groups

Everyone who signs in has a global role:

- **Admin** can do everything, including integrations, the proxy, the tunnel, the host page, accounts and groups.
- **Member** can use only the servers their groups give them and change their own password.
- **Deployer**, for API keys only, is a member that can also [create servers that can't reach the host](#deployer-keys).

**System → Server groups** puts servers in groups and says who may use them: local accounts, single sign-on users or
groups, and API keys. A server can be in several groups; the highest role applies. Servers can also be added on their
own **Settings** tab. Servers on [other Docker hosts](#more-docker-hosts) go in by their full name (`name@host`), and
with more than one host a group can also hold **whole Docker hosts**: every server on them, including ones installed
there later. Removing a host takes it out of every group, so a different machine given the same ID later doesn't
inherit its access.

| Role in a group | Can |
| --- | --- |
| **Viewer** | See the server, its stats, its game status and player count, its port and backup status and its scheduled tasks, but not its settings, logs, files or tasks' console commands, which can hold passwords |
| **Operator** | Also start, stop, restart and update it, [send it console commands](#sending-commands-to-a-server), run its scheduled tasks, read its logs, settings and files, and take and download backups. Settings that look like secrets (names with `TOKEN`, `SECRET`, `PASS`, `PWD`, `KEY`, `CREDENTIAL`, `AUTH` or `PRIVATE`, items the template masks, extra parameters and post arguments) are left out |
| **Manager** | Also change its ports and variables, edit files, set up its console, change its scheduled tasks and how it's updated, set start on boot and port forwarding, change the backup plan, restore and delete backups, and see its [activity](#activity-audit-log) |

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
password (and, with [two-factor](#two-factor-sign-in), a code), or, if you signed in with single sign-on, sends you to sign in there again. hlm checks the provider's
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
a member key only what server groups give it; a [deployer key](#deployer-keys) can also create servers; a metrics key
can only read [`/metrics`](#prometheus).

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
- **Private images:** only with registry logins an admin allowed for deployer keys (see
  [Private registries](#private-registries)).

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

## Activity (audit log)

hlm records who changed what, so a deleted server or a changed setting can be traced back. **System → Activity** lists
the entries newest first. It can search (who, what, server, address, details), narrow to an area (servers, sign-ins,
accounts and keys, groups, scheduled tasks, notifications, proxy and tunnel, system), a result and a time, and click a
row for its details.

**What's recorded:**
- **Every change made through hlm's API:** starting or deleting a server, a setting saved, a file edited, a console
  command sent, a backup restored, an account or API key created, and so on. Each entry says who did it (a local
  account, a single sign-on user like `sso:bob`, an API key like `key:ci`, an SSH key sign-in, `HLM_TOKEN`, or
  `anonymous` when sign-in isn't required), from which address, what to, and whether it worked. Commands from the
  [Discord bot](#discord-commands) are recorded as `discord:<username>`. Requests that were
  refused are recorded as **Denied**, and ones that failed (including an install or update that failed partway through)
  as **Failed**, with the error.
- **Sign-ins:** successful and refused ones (wrong password, locked out, not allowed by single sign-on), sign-outs,
  confirming it's you, and SSH key sign-ins. `hlm reset-password` on the host is recorded too.
- **What hlm does by itself,** as `hlm`: scheduled task runs, automatic updates, scheduled backups, and servers it
  stopped for a [crash loop](#health-checks-and-crash-loops) or restarted because their health was failing.
- **Downloads** of backups and of hlm's configuration backups, and exports of the log itself.

Reads (opening a page, listing files) aren't recorded.

**What isn't in an entry:** request bodies and secrets. An entry has the names of what changed: which settings a
reconfigure changed (not their values), the file that was saved, the backup restored, the account or key created. The
command text is kept for console commands, as that's the point, but secret-looking parts (`password=…`, tokens,
passwords in URLs) are masked.

**Where it's kept:** one JSON Lines file a month, `<data>/audit/audit-YYYY-MM.jsonl`, written as each entry happens.
- **Retention:** 12 months by default (**Keeping the log** changes it; shortening it deletes the older months, and takes
  [confirming it's you](#confirming-its-you)).
- **Tamper-resistance:** entries can't be changed or deleted through hlm.
- **Backups:** the files are included in [hlm's configuration backups](#backing-up-hlms-own-configuration).
- **Export:** CSV or JSON Lines, with the current filters.
- **hlm's own log:** **Also write each entry to hlm's own log** sends them to journald, Loki and the like.

**Who sees what:**
- Admins see everything.
- A server's **Settings** tab shows its last changes to its managers.
- **Your account** shows your recent sign-ins.

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
| `POST /api/auth/login`                         | `{username, password}`: sign in with a local account (send `X-HLM-Request: 1`). Needs no sign-in. For an account with two-factor, the answer is `{twoFactor: true, code, passkey}` (which second factors it has; or `{twoFactorSetup: true}` when it has to set one up) and a short-lived cookie for the next step. |
| `POST /api/auth/login/passkey/begin`, `POST /api/auth/login/passkey` | [Sign in with a passkey](#passkeys) alone: the WebAuthn request options, then the browser's assertion (`{id, clientDataJSON, authenticatorData, signature, userHandle}`, base64url). Needs no sign-in. |
| `POST /api/auth/login/2fa/passkey/begin`, `.../2fa/passkey` | A passkey as the second factor, after the password. |
| `POST /api/auth/login/2fa/setup/passkey/begin`, `.../setup/passkey` | During sign-in, for an account that has to use two-factor: add a passkey (`{name, clientDataJSON, attestationObject, transports}`) and finish signing in. |
| `POST /api/auth/login/2fa`                     | `{code}`: finish signing in with an authenticator or recovery code.               |
| `POST /api/auth/login/2fa/setup`, `.../enable` | During sign-in, for an account that has to use two-factor: get the secret (`{secret, url, qr}`), then `{code}` to turn it on and finish signing in (`{recoveryCodes}`). |
| `GET /api/auth/accounts/{username}/passkeys`   | An account's passkeys (its owner, or an admin): `[{id, name, rpId, synced, created, lastUsed}]`. |
| `POST /api/auth/accounts/{username}/passkeys/begin`, `POST .../passkeys` | Add a passkey to your own account: the WebAuthn creation options (a confirmed session), then `{name, clientDataJSON, attestationObject, transports}`. |
| `PUT/DELETE /api/auth/accounts/{username}/passkeys/{id}` | Rename your passkey (`{name}`), or remove one (confirmed: your own, keeping a required second factor; or, as an admin, anyone's). |
| `POST /api/auth/accounts/{username}/2fa/setup` | Start setting up your own two-factor: `{secret, url, qr}` (a [confirmed](#confirming-its-you) session). |
| `POST /api/auth/accounts/{username}/2fa/enable` | `{code}`: turn it on; answers the recovery codes, once.                         |
| `POST /api/auth/accounts/{username}/2fa/recovery` | New recovery codes for your own account (confirmed).                          |
| `DELETE /api/auth/accounts/{username}/2fa`     | Turn off your own authenticator app (unless it's your required, last second factor), or, as an admin, reset someone's two-factor: the app and passkeys (confirmed). |
| `POST /api/auth/logout`                        | Ends the session (send `X-HLM-Request: 1`).                                       |
| `GET/PUT /api/auth/config`                     | `{required, sessionHours, requireTwoFactor}` (`requireTwoFactor`: empty, `admins` or `all`). Requiring sign-in needs a signed-in session; requiring two-factor, one that has it (or single sign-on). |
| `GET/POST /api/auth/accounts`                  | Local accounts; `POST {username, password, role}` adds one.                       |
| `PUT /api/auth/accounts/{username}/password`   | `{password}`: sets a new password, ending the account's other sessions. Members change only their own, and send `current` too. |
| `PUT /api/auth/accounts/{username}/role`       | `{role}`: `admin` or `member`.                                                    |
| `DELETE /api/auth/accounts/{username}`         | Removes an account.                                                               |
| `GET/POST /api/auth/accounts/{username}/ssh-keys`, `PUT/DELETE …/ssh-keys/{id}` | [SSH keys](#ssh-keys) of an account, for an admin or the account itself. `POST {publicKey, name?, allowFrom?}` and `PUT {allowFrom}` need a [confirmed](#confirming-its-you) session. |
| `POST /api/auth/confirm`                       | Confirm a password session for 10 minutes: `{password, code?}` (the code for accounts with two-factor). Single sign-on confirms at `/auth/login?confirm=1`. |
| `POST /api/auth/confirm/passkey/begin`, `POST /api/auth/confirm/passkey` | Confirm with a passkey instead (it has to verify you). |
| `GET/PUT /api/system/https`, `POST /api/system/https/renew` | [HTTPS](#https) settings and certificate status; `PUT {enabled, domain, email?, staging?}` needs a [confirmed](#confirming-its-you) session. |
| `POST /api/auth/download-ticket`               | `{url}` of a backup download returns a URL that works once, for a minute, from the same address: for downloads when signed in with a token. |
| `POST /api/auth/ssh/challenge`, `POST /api/auth/ssh/login` | Sign in with an SSH key: `{username}` returns `{challenge}`; `{challenge, signature}` returns `{token, expires}`. |
| `GET/POST /api/auth/keys`, `PUT/DELETE /api/auth/keys/{id}` | [API keys](#api-keys); `POST {name, days, role, allowFrom?}` returns the key, once; `PUT {allowFrom}`. Both need a [confirmed](#confirming-its-you) session. |
| `GET/POST /api/groups`, `PUT/DELETE /api/groups/{id}` | [Server groups](#roles-and-server-groups): `{name, servers, hosts, grants}` (`hosts`: Docker host IDs, `local` for this one, whose every server is in the group). `GET` gives members their own groups, without grants. |
| `PUT /api/servers/{id}/groups`                 | `{groups}`: the IDs of the groups a server is in.                                 |
| `GET/PUT/DELETE /api/auth/oidc`                | [Single sign-on](#single-sign-on-with-authelia-openid-connect) settings; the client secret is never returned. |
| `POST /api/auth/oidc/test`                     | Fetches the provider's discovery document for (unsaved) settings.                 |
| `POST /api/auth/oidc/secret`                   | A new client secret, with its digest for Authelia's configuration; nothing is saved. |
| `GET /auth/login?return=`, `GET /auth/callback` | Browser sign-in: off to the provider and back.                                   |
| `GET /api/catalog/apps?q=&category=&sort=`     | Search templates (default category `GameServers`; `category=` for all).           |
| `GET /api/catalog/apps/{id}?host=`             | Full template, with paths localized for a [Docker host](#more-docker-hosts) (this host by default). |
| `GET /api/catalog/categories`                  | Categories with counts.                                                           |
| `POST /api/catalog/import`                     | `{image}`: pull an image and make a template from its defaults. Streams NDJSON; the final `done` event's message is the template ID. |
| `DELETE /api/catalog/apps/{id}`                | Delete a template made from an image.                                             |
| `POST /api/catalog/refresh`                    | Re-download the feed now.                                                         |
| `GET /api/networks?host=`                      | Docker networks with subnets, addresses in use, and whether static IPs are allowed. `host` (here and for `POST`/`DELETE`) is a [Docker host](#more-docker-hosts); this host by default. |
| `GET /api/servers`                             | Managed servers.                                                                  |
| `POST /api/servers/check-ports`               | Host ports `{request, server?, host?}` would use that are already in use (`conflicts`); `server` is the one being reconfigured, `host` the Docker host a new one goes on. |
| `POST /api/servers`                            | Install: `{templateId, name, tag?, values?, disabled?, network?, ipAddress?, extra?, extraParams?, postArgs?, privileged?}`, plus `group?` for a [deployer key](#deployer-keys) and `host?` for another [Docker host](#more-docker-hosts). Streams NDJSON progress. |
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
| `GET /api/audit?actor=&action=&target=&server=&result=&text=&since=&before=&limit=` | The [activity log](#activity-audit-log), newest first: `{entries, next}`, where `next` is the `before` of the following page. Admins only. |
| `GET /api/audit/export?format=csv\|jsonl&…`    | The matching entries as a file. Admins only.                                      |
| `GET/PUT /api/system/audit`                    | `{retentionMonths, mirror}`; `PUT` needs a [confirmed](#confirming-its-you) session. |
| `GET /api/servers/{id}/activity`               | A server's entries (a manager).                                                   |
| `GET /api/auth/activity`                       | Your own recent sign-ins.                                                         |
| `GET /api/registries`                          | [Registry logins](#private-registries): `[{host, username, deployers}]`, never passwords. Admins only. |
| `POST /api/registries`                         | `{host, username, password, deployers}`: adds one; needs a [confirmed](#confirming-its-you) session. |
| `PUT/DELETE /api/registries/{host}`            | Changes (a blank password keeps the saved one; confirmed) or removes one (an admin). |
| `POST /api/registries/test`                    | `{host, username, password, image?}`: signs in through Docker (a blank password tests the saved one) and looks up `image`; `{status, digest?}`, or 400 with the registry's answer. |
| `POST /api/servers/{id}/console/command`       | `{command}`: sends it; returns `{mode, output, exitCode?}`. Over stdin, the answer is in the logs instead. |
| `GET /api/servers/{id}/files?path=`            | List a directory in the server's container, plus its data folders.                |
| `GET /api/servers/{id}/files/content?path=`    | Read a text file (up to 1 MiB).                                                   |
| `PUT /api/servers/{id}/files/content`          | `{path, content, expectedMtime}`: save a file; 409 if it changed since it was read. |
| `GET /api/servers/{id}/backups`                | The server's backup plan, a running backup's progress, and its backups in the bucket (newest first). |
| `PUT /api/servers/{id}/backups/plan`           | `{folders, intervalHours, at?: {time, weekdays}, keep, retention?: {last, daily, weekly, monthly}, stopServer, exclude?, preCommand?, postCommand?, verify?: ""\|"always"\|"off", copyTo?: {target, keep, retention?}}`: what to back up, when, and how long to keep it. |
| `POST /api/servers/{id}/backups/verify?key=&target=` | Read a backup back and check it: `{ok, files, bytes, checksum, error}`. Operators. |
| `GET /api/servers/{id}/backups/restore-tests` | The server's [restore tests](#restore-tests): `{settings: {every, backups, timeoutMinutes}, results: [{key, trigger, started, finished, outcome, seconds, answer, note, error, log, files, bytes}]}`, newest first. |
| `PUT /api/servers/{id}/backups/restore-tests` | `{every: "" \| "week" \| "backups", backups, timeoutMinutes}`: when to test. Managers. |
| `POST /api/servers/{id}/backups/restore-test` | `{key}` (the newest if empty): test a restore now; streams progress as JSON lines, ending with `done` or `error`. Managers. |
| `POST /api/servers/{id}/backups`               | Start a backup now (202); progress shows in `GET .../backups`.                     |
| `POST /api/servers/{id}/backups/restore`       | `{key, target?, backupFirst}`: start restoring a backup (202); progress shows in `GET .../backups`. |
| `GET /api/servers/{id}/backups/download?key=&target=` | Download one of the server's backups (decrypted).                           |
| `GET /api/host/cleanup?host=`                 | What [disk cleanup](#disk-cleanup) can delete on a Docker host (`items`: kind, ID, name, size, ticked by default); nothing is deleted. Admins. |
| `POST /api/host/cleanup?host=`                | `{images, containers, volumes, buildCache}`: delete those (if they're still candidates); answers `{freed, removed, failed}`. Admins. |
| `GET/PUT /api/system/cleanup`                  | `{keepOldImages, weekly}`, plus the last weekly run (`lastRun`, `lastFreed`, `lastError`). Admins. |
| `PUT /api/servers/{id}/tags`                   | `{tags}`: a server's tags (up to 10; letters, digits, spaces, `.`, `_`, `-`). Managers. Server lists include `tags`. |
| `PUT /api/servers/{id}/favourite`              | `{favourite}`: pin the server for the caller (their own favourites). Viewers. Server lists include `favourite`. |
| `POST /api/servers/bulk`                       | `{servers, action: "start"\|"stop"\|"restart"\|"update"\|"backup"}`: one action on several servers in turn, checking the caller's role on each. Streams NDJSON (each event's `target` is the server; `summary` at the end). |
| `GET /api/status-page`                         | The [public status page](#public-status-page) (no sign-in; 404 while it's off). |
| `GET /api/status-page/{name}.json`, `.svg`     | One server of the page, as JSON or a badge.                                       |
| `POST /api/status-page/{slug}/wake`            | Start a [sleeping server](#stopping-idle-servers) whose idle settings allow it (send `X-HLM-Request: 1`); 404 for anything else. Three per address, then one every 10 minutes. |
| `POST /api/status-page/unlock`, `.../lock`     | `{password}`: give the page's view or details password (sets the `hlm_status` cookie; send `X-HLM-Request: 1`), or forget it. |
| `GET/PUT /api/system/status-page`              | The status page's settings `{enabled, title, message, servers: [{server, displayName, showPlayers, showNames, showVersion, address, hint}], allowFrom, sites: [{id, hostname, groups, title, message}]}`, plus `viewPassword`/`detailsPassword` on `PUT`, page-wide and on each site (a new one; `""` removes it; left out keeps it; a site keeps its passwords by its `id`). Answers `hasViewPassword`/`hasDetailsPassword` (and the same on each of `sites`), never the passwords. Admins. |
| `GET /api/system/status-page/suggest?server=`  | Public addresses hlm can suggest for a server (from the proxy and UniFi). Admins. |
| `GET /api/hosts`                               | [Docker hosts](#more-docker-hosts) and whether they could be reached. Admins also get how each is reached (never a client key) and hlm's SSH public key. |
| `POST /api/hosts/test`                         | Connect to a host as given, without saving it: its Docker version, or (for SSH) the host key to confirm (`hostKey`, `hostKeyFingerprint`). Admins. |
| `POST /api/hosts`, `PUT /api/hosts/{id}`       | `{id, name, kind: "ssh"\|"tcp"\|"agent" (PUT only; agents' hosts are added by enrolment), ssh?: {host, port, user, hostKey}, socket?, url?, ca?, cert?, key?, appdataRoot, address?}`: add or change a host, after connecting to it. An empty `key` keeps the saved one. [Confirmed](#confirming-its-you) session. |
| `POST /api/hosts/agent-enrolment`              | `{id, name, appdataRoot, address}`: a single-use, one-hour token for a new host's [agent](#more-docker-hosts) (`token`, `host`, `expires`). Confirmed session. |
| `POST /api/agent/enrol`                        | Called by `hlm agent`: `{token, version}` → `{host, credential}`. No sign-in; the token is the proof. Rate-limited. |
| `GET /api/agent/connect`                       | The agent's WebSocket, signed in with `Authorization: Bearer <credential>`. |
| `DELETE /api/hosts/{id}`                       | Stop managing a host's servers (they keep running there). Confirmed session.      |
| `POST /api/hosts/{id}/check`                   | Connect to a saved host now. Admins.                                              |
| `POST /api/hosts/ssh-key`                      | Make hlm's SSH key, if there isn't one. Admins.                                   |
| `POST /api/hosts/inspect`                      | A host (as for `test`, SSH only): its Linux, whether Docker is installed, running and usable, and the setup commands hlm would run (`script`, `canSetUp`, `reason`). Admins. |
| `POST /api/hosts/setup-tls`                    | `{…host, ssh: {host, port, user, hostKey}, port?}`: make a CA and certificates, set Docker's API up with TLS on the host over SSH, then save it as a TCP host. Streams NDJSON. Confirmed session. |
| `POST /api/hosts/setup-docker`                 | Install Docker CE on an SSH host (or start it, or add the user to the `docker` group), streaming the output as NDJSON. [Confirmed](#confirming-its-you) session. |
| `GET /api/stacks`, `GET /api/stacks/{name}`   | [Compose stacks](#compose-stacks) and their services' state (the services the caller can see); admins also get the file and `.env`. |
| `POST /api/stacks/preview`                     | `{name, yaml, env, existing, host}`: what creating (or, with `existing`, changing) the stack would do, per service. Admins. |
| `POST /api/stacks`, `PUT /api/stacks/{name}`   | `{name, yaml, env, host}`: create a stack (on `host`, this one by default), or apply a changed file (it stays on its host). Streams NDJSON. Admins. |
| `POST /api/stacks/{name}/{action}`             | `start`, `stop`, `restart` or `update` the whole stack. Streams NDJSON. Operators of every service. |
| `DELETE /api/stacks/{name}`                    | Remove the stack's containers and networks (data is kept). Admins.              |
| `GET /api/servers/{id}/export?secrets=0\|1`    | The server's settings as an [export file](#export-import-and-clone). Operators; `secrets=1` (passwords included) takes a manager in a [confirmed](#confirming-its-you) browser session. |
| `POST /api/servers/import/preview?host=`       | Read an export file: the install form's template and settings (for a [Docker host](#more-docker-hosts)), warnings, what was left out and what comes along. Admins. |
| `POST /api/servers/import`                     | `{export, request, host?}`: install a server from an export file with the form's settings, and give it the file's other settings. Streams NDJSON. Admins. |
| `POST /api/servers/{id}/clone/preview`         | `{name, host?}`: a clone's settings (new data folders and free ports), what it copies, and warnings; `host` puts it on another [Docker host](#more-docker-hosts). Admins. |
| `POST /api/servers/{id}/clone`                 | `{name, host?, request?, copyData: "none"\|"now"\|"backup", backupKey?, stopSource, start}`: clone the server. Streams NDJSON. Admins. |
| `DELETE /api/servers/{id}/backups?key=&target=` | Delete one of the server's backups (and its checksum) from the bucket.          |
| `GET /api/system/backup`                       | Configuration backups: their plan, a running backup, and the backups in the bucket. |
| `PUT /api/system/backup`                       | `{intervalHours, keep, retention?, includeKey}`: when to back up hlm's configuration, and which backups to keep. |
| `POST /api/system/backup`                      | Back up hlm's configuration now (202).                                            |
| `GET /api/system/backup/download?key=`         | Download a configuration backup.                                                  |
| `DELETE /api/system/backup?key=`               | Delete a configuration backup.                                                    |
| `GET\|PUT\|DELETE /api/integrations/backup`     | Bucket settings, including `caCert` (PEM) for a private CA (the secret key is never returned; empty keeps the saved one). |
| `POST /api/integrations/backup/test`           | Check bucket settings: list, write and delete a test file.                        |
| `GET /api/integrations/backup/targets`         | Every backup storage (the primary first), without secret keys: `{id, name, encrypt, hasKey, endpoint, bucket, …}`. |
| `POST /api/integrations/backup/targets`, `PUT .../targets/{target}` | Add or change a backup storage (`{name, endpoint, bucket, accessKey, secretKey, prefix, region, virtualHost, caCert, encrypt}`), checked first. Turning on encryption returns the new key once as `newKey`. Needs a [confirmed](#confirming-its-you) session. |
| `DELETE /api/integrations/backup/targets/{target}` | Forget a backup storage (not its backups); the primary only when it's the only one. |
| `GET /api/integrations/backup/targets/{target}/key` | A storage's encryption key, to keep safe. Needs a confirmed session. |
| `GET /api/servers/{id}/files/search?path=&q=`  | Find files by name, or config files when `q` is empty.                            |
| `GET/PUT /api/servers/{id}/config-files`       | The server's [pinned settings files](#editing-server-files) and the usual ones of its game that hlm found (`{pinned, suggested}`); `PUT {pinned: [{path, label?}]}` takes a manager. |
| `GET /api/servers/{id}/stats`                  | CPU, memory, network and disk usage.                                              |
| `POST /api/networks`                           | Create a network: `{name, driver: bridge\|macvlan\|ipvlan, parent?, subnet, gateway?, ipRange?}`. |
| `DELETE /api/networks/{name}`                  | Delete a network created in hlm that no container uses.                           |
| `GET /api/networks/interfaces`                 | The host's interfaces with their subnets and default gateway, for suggestions.    |
| `GET /api/stats`                               | Usage of every running server, keyed by container ID.                             |
| `GET /api/game-profiles`                       | Every [game profile](#game-profiles) in use, with your own profiles' `origin` (`url`, `sha256`, `fetched`, `index`, `checked`, `update`, `updateError`, `changedLocally`), and files that couldn't be used (admins). |
| `GET /api/game-profiles/{id}/yaml`             | A profile as written (`?builtin=1`: the built-in one). |
| `POST /api/game-profiles`                      | Save a profile sent as YAML; one imported from a link keeps its origin. |
| `DELETE /api/game-profiles/{id}`               | Delete your own profile. |
| `POST /api/game-profiles/reload`               | Read the profiles folder again. |
| `POST /api/game-profiles/fetch`                | `{url}`, or `{index, id}`: fetch a profile without saving it: `{url, sha256, id, name, yaml, index, replaces, diff: [{op, text}], same}`. |
| `POST /api/game-profiles/import`               | The same, with the `sha256` that was looked at: save it, remembering where it came from (409 if it changed since). |
| `POST /api/game-profiles/check`                | Fetch every linked profile again and note updates; answers like `GET /api/game-profiles`. |
| `GET /api/game-profiles/export`                | Your own profiles, as a `.zip`. |
| `GET`, `PUT /api/game-profiles/indexes`        | `{indexes: [link]}`: the profile indexes. |
| `GET /api/game-profiles/indexes/browse`        | Each index's profiles: `[{url, name, error, profiles: [{id, name, url, sha256, description, inUse, installed, current}]}]`. |
| `GET /api/integrations/discord`                | The [Discord bot](#discord-commands)'s settings, without the token: `{settings: {enabled, guilds: [{id, name, groups, details, rules: [{roleId, roleName, groups, actions}]}]}, hasToken, state: {status, error, bot, applicationId, since, guilds: [{id, name, commands, error}]}, inviteUrl, actions}`. |
| `PUT /api/integrations/discord`                | Save them (confirmed): the same `settings` fields, and `token` to replace the bot's (checked with Discord). |
| `POST /api/integrations/discord/test`          | Checks the token, and adds the commands to each Discord server again: `{result: {bot, inviteUrl, guilds: [{id, name, commands, error}]}, error}`. |
| `GET /api/integrations/discord/roles?guild=`   | One Discord server's roles, for rules. |
| `DELETE /api/integrations/discord`             | Takes the commands out of every Discord server, forgets the token and turns the bot off (confirmed). |
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
| `POST /api/proxy/wireguard/check`              | The last handshake the proxy saw and whether it can ping this host (`reachable`), and each other host's end's last handshake (`hosts`). |
| `PUT /api/proxy/wireguard/hosts/{host}`        | Set up (or again) the tunnel's end on another Docker host `{lanNetwork?, lanAddress?}`: a peer on the proxy and a container there; re-applies nginx if it relays through the tunnel. Streams NDJSON like `PUT /api/proxy/wireguard`. |
| `GET /api/proxy/wireguard/hosts/{host}/networks` | That host's macvlan/ipvlan networks, for its end's LAN mode.                    |
| `DELETE /api/proxy/wireguard/hosts/{host}`     | Remove that host's end.                                                           |
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
| `PUT /api/tunnel`                              | Create the tunnel (the first time) and run cloudflared `{host?, name, lanNetwork, lanAddress}`, then publish the hostnames. With `host`, another Docker host gets a tunnel of its own (named `hlm-<host>` by default) with cloudflared there. Streams NDJSON: `step` and `output` events, then `done` with `status` (and `apply`) or `error`. |
| `POST /api/tunnel/{start,stop,restart}?host=`  | Control the `hlm-cloudflared` container (this host's, or `host`'s).               |
| `GET /api/tunnel/logs?tail=&host=`             | cloudflared's log as server-sent events.                                           |
| `DELETE /api/tunnel?host=`                     | Remove the container, hlm's DNS records and the tunnel (this host's, or `host`'s, whose hostnames move back to this host's tunnel); the hostnames are kept. |
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
| `GET /api/host?host=`                          | Host CPU, memory, network, filesystems and disk I/O, with recent history: this host's, or (`host=`) another [Docker host's](#more-docker-hosts). When they can't be had, `available` is false and `reason` says why. |
| `GET /api/host/history?range=1h\|6h\|24h\|7d\|30d&host=` | The host's (or, with `host=`, an agent host's) [resource history](#resource-history): `{step, times, series: {name: {avg, max}}}`, `null` where nothing was recorded. Admins only. |
| `GET /api/servers/{id}/history?range=…`        | A server's resource history (a viewer).                                           |
| `GET /metrics`                                 | [Prometheus](#prometheus) metrics: a metrics or admin key, or an allowed address. |
| `GET/PUT /api/system/metrics`                  | `{allowFrom}`: who may scrape without a key; `PUT` needs a [confirmed](#confirming-its-you) session. |
| `GET /api/host/storage?host=`                  | A Docker host's disk usage per server, deleted servers' data and Docker's totals (last background calculation; this host by default). |
| `POST /api/host/storage/refresh?host=`         | Recalculate disk usage now, on every host (runs in the background), and return the host's last report. |
| `POST /api/host/storage/deleted/{id}/delete`   | Delete a deleted server's folder or volume `{path, volume}` from disk.             |
| `POST /api/host/storage/deleted/{id}/forget`   | Stop tracking a deleted server's folder or volume `{path, volume}`, or all of them without a `path`. |
| `GET /api/jobs`                                | Installs and updates in progress, plus those that finished in the last minute.    |
| `GET /api/updates`                             | When updates were last checked and how many are available.                        |
| `GET/PUT /api/servers/{id}/update-policy`      | A server's [automatic updates](#automatic-updates): `{mode, windowStart?, windowEnd?, warn?, warnMinutes?, backup}` (`mode` is `auto`, `manual`, or empty to follow the default), plus what it follows, the update's state and the last result. `PUT` takes a manager; the warning is only shown to operators. |
| `GET/PUT /api/servers/{id}/health`             | A server's [health settings](#health-checks-and-crash-loops): `{crashLoop: {off?, exits?, minutes?}, check: {kind, port?, path?, command?, interval?, timeout?, retries?, startPeriod?}, restartAfter?, signal?}` (`kind` is empty for the image's check, `none`, `tcp`, `http` or `command`; `signal` is empty for Docker's health or `query`), plus Docker's health, the crash loop it was stopped for, recent exits and whether the container has the check yet. `PUT` takes a manager; commands are only shown to operators. |
| `GET /api/servers/{id}/players`                | A server's [players](#managing-players) (operators): `{game, running, actions, needsId, online: [{name, id}], onlineFrom, onlineError, lists: {allow, ban, admin: {file, path, format, players: [{name, id, reason}], error, editable}}, log}`, or `{unsupported}` for a game without player actions or lists. |
| `POST /api/servers/{id}/players/{action}`      | `{name, id, reason}`: `kick` (operators), `ban`, `unban`, `allow`, `disallow`, `admin`, `unadmin` (managers). `{output}` is the console's answer. |
| `GET /api/servers/{id}/mods`                   | A server's [mods](#mods-and-plugins): `{plugins, loader, fit: {loaders, gameVersion}, fitError, dir, running, warnings, mods: [{file, disabled, size, source, project, title, version, versionNumber, by, installed, update: {version, versionNumber, blocked}}], checked, timeoutNs}`, or `{unsupported}`. |
| `GET /api/servers/{id}/mods/search?q=`         | Modrinth projects that fit the server (managers). |
| `POST /api/servers/{id}/mods/check`            | Looks for updates now (operators); answers like `GET …/mods`. |
| `POST /api/servers/{id}/mods/plan`             | `{changes: [{op, project, version, file}]}` with `op` `add`, `update`, `remove`, `disable` or `enable` (managers): `{steps, changes, warnings, running}`, where `changes` has the versions chosen and dependencies added. |
| `POST /api/servers/{id}/mods/apply`            | The same body (usually a plan's `changes`); streams the change's progress as JSON lines, ending with `done` or `error` (managers). |
| `GET/PUT /api/servers/{id}/idle`               | A server's [idle settings](#stopping-idle-servers) `{minutes, wakeOnConnect, wakeFromStatusPage}`, plus `idleSince`, `asleep` (`since`, `minutes`, `listener`, `listenerError`), `canStop`/`why` and `canListen`/`listenWhy`. `PUT` takes a manager. |
| `POST /api/servers/{id}/health/test`           | Run a health check `{check}` (or the container's own, without one) in the running server once: `{healthy, exitCode, output}`. Managers only. |
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
HLM_DOCKER_TESTS=1 go test ./internal/files ./internal/backup ./internal/manager
# Also install Docker CE in throwaway Ubuntu, Debian, Fedora and CentOS containers (downloads a few hundred MB):
HLM_DOCKER_TESTS=1 HLM_INSTALL_TESTS=1 go test ./internal/hosts -run DockerInstallSteps
# (HLM_DOCKER_TESTS=1 alone also runs the TLS setup script in a busybox container.)
# Also build every game server template in a real feed with default values:
curl -o /tmp/feed.json https://assets.ca.unraid.net/feed/applicationFeed.json
HLM_FEED_FILE=/tmp/feed.json go test ./internal/deploy -run WholeFeed -v
```
