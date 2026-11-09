// Types mirroring the Go API's JSON.

export type ConfigType = "Port" | "Path" | "Variable" | "Device" | "Label";

/** A host port a server's settings would use that something else already uses. */
export interface PortConflict {
  port: number;
  proto: string;
  /** The container port it maps, e.g. "2456/udp". */
  container: string;
  /** The other container; absent for another program on the host. */
  usedBy?: string;
  running: boolean;
  message: string;
}

export interface ConfigItem {
  id: string;
  name: string;
  type: ConfigType;
  target: string;
  default?: string;
  options?: string[];
  value: string;
  mode?: string;
  description?: string;
  display?: "always" | "advanced";
  required: boolean;
  mask: boolean;
}

export interface Branch {
  tag: string;
  description?: string;
  repository?: string;
}

export interface AppSummary {
  id: string;
  name: string;
  author?: string;
  repo?: string;
  repository: string;
  icon?: string;
  overview?: string;
  categories: string[];
  downloads?: number;
  stars?: number;
  lastUpdate?: number;
  official?: boolean;
}

export interface App extends AppSummary {
  maintainer?: string;
  registry?: string;
  requires?: string;
  screenshots?: string[];
  support?: string;
  project?: string;
  readme?: string;
  templateUrl?: string;
  webui?: string;
  network?: string;
  privileged: boolean;
  extraParams?: string;
  postArgs?: string;
  branches?: Branch[];
  defaultTagDescription?: string;
  config: ConfigItem[];
  /** Set for templates made from a Docker image's own defaults. */
  fromImage?: string;
}

export interface CategoryCount {
  name: string;
  count: number;
}

export interface DeployRequest {
  templateId: string;
  name: string;
  tag?: string;
  values?: Record<string, string>;
  disabled?: string[];
  network?: string;
  ipAddress?: string;
  /** Paths, ports, variables, labels and devices added on top of the template. */
  extra?: ExtraConfig[];
  /** Overrides of the template's values; omitted means "use the template's". */
  extraParams?: string;
  postArgs?: string;
  privileged?: boolean;
  /** Overrides the template's restart policy; start on boot switches it. */
  restartPolicy?: string;
}

/** A server's settings exported to a file (<server>.hlm.json); see the README. */
export interface ServerExport {
  hlmExport: number;
  exported: string;
  hlmVersion?: string;
  server: string;
  template: App;
  request: DeployRequest;
  secrets: boolean;
  omitted?: string[];
  settings: Record<string, unknown>;
}

/** A server about to be imported or cloned: the settings form, filled in, and what to look out for. */
export interface TransferPreview {
  app: App;
  request: DeployRequest;
  /** Names of settings left out of the file (passwords). */
  omitted?: string[];
  warnings: string[];
  /** The template isn't in the catalog: it's added from the file. */
  templateMissing?: boolean;
  /** The other settings that come along, in words. */
  settings: string[];
  /** Data folders a clone copies. */
  copies?: { target: string; from: string; to: string }[];
}

export type CopyData = "none" | "now" | "backup";

export type ExtraConfigType = "Path" | "Port" | "Variable" | "Label" | "Device";

export interface ExtraConfig {
  type: ExtraConfigType;
  name?: string;
  target: string;
  value: string;
  mode?: string;
}

export interface NetworkInfo {
  name: string;
  driver: string;
  subnets: string[];
  gateways: string[];
  /** Whether servers on this network can have a fixed address. */
  staticIp: boolean;
  /** macvlan/ipvlan: containers get their own LAN address instead of publishing ports. */
  directIp: boolean;
  /** Blocks Docker hands out automatic addresses from (--ip-range). */
  ipRanges?: string[];
  /** Taken by running containers, and stopped containers' fixed addresses. */
  usedIps: { ip: string; container: string; stopped?: boolean }[];
  /** Host interface of a macvlan/ipvlan network. */
  parent?: string;
  /** Created in hlm, so it can be deleted here. */
  managed: boolean;
}

export interface HostInterface {
  name: string;
  subnets: string[];
  gateway?: string;
  default: boolean;
}

export interface NetworkRequest {
  name: string;
  driver: "bridge" | "macvlan" | "ipvlan";
  parent?: string;
  subnet: string;
  gateway?: string;
  ipRange?: string;
}

export interface PortSummary {
  PrivatePort: number;
  PublicPort?: number;
  Type: string;
}

export interface Server {
  id: string;
  /** Ends in @host for a server on another Docker host. */
  name: string;
  /** The Docker host it's on: "local", or another host's ID. */
  host: string;
  /** Labels a manager gave it. */
  tags?: string[];
  /** Pinned to the top of the list by the person looking. */
  favourite?: boolean;
  app: string;
  templateId: string;
  image: string;
  icon?: string;
  webui?: string;
  state: string;
  status: string;
  health?: string;
  created: string;
  ports: PortSummary[];
  network: string;
  ipAddress?: string;
  /** Docker restart policy; autostart is true when it starts the server on boot. */
  restartPolicy: string;
  autostart: boolean;
  update?: UpdateStatus;
  /** Left out for those who may only view the server: settings can hold passwords. */
  request?: DeployRequest;
  /** The caller's role on this server. */
  access?: ServerAccess;
  /** The server groups the caller can see it in. */
  groups?: string[];
  /** Settings left out of request because they look like secrets; managers see them. */
  hidden?: string[];
  /** Set when hlm stopped the server because it kept exiting. */
  crashLoop?: CrashLoop;
  /** The Compose stack (and service in it) the server is part of. */
  stack?: string;
  stackService?: string;
  /** The game profile its image matches. */
  gameProfile?: GameProfileRef;
  /** Set while hlm has it stopped because nobody played. */
  asleep?: { since: string; minutes: number };
  /** Set when its Docker host listed it but didn't answer for it in time (a backup copying its files, say). */
  unresponsive?: string;
}

export type PlayerAction =
  "kick" | "ban" | "unban" | "allow" | "disallow" | "admin" | "unadmin";
export type PlayerListName = "allow" | "ban" | "admin";

export interface PlayerEntry {
  name?: string;
  id?: string;
  reason?: string;
}

export interface PlayerList {
  file: string;
  path?: string;
  format: string;
  players: PlayerEntry[];
  error?: string;
  /** hlm can change the file itself now (a lines file, while stopped). */
  editable: boolean;
}

export interface PlayerRecord {
  time: string;
  by: string;
  action: PlayerAction;
  player: string;
  reason?: string;
  error?: string;
}

export interface PlayersOverview {
  /** Set when hlm can't manage this game's players. */
  unsupported?: string;
  game?: string;
  running: boolean;
  actions: PlayerAction[];
  /** Actions that take the player's ID, not their name. */
  needsId?: PlayerAction[];
  online: PlayerEntry[];
  onlineFrom?: "console" | "query";
  onlineError?: string;
  lists?: Partial<Record<PlayerListName, PlayerList>>;
  log: PlayerRecord[];
}

export interface WorkshopItem {
  id: string;
  /** Its mod name, for images that take those too. */
  name?: string;
  title?: string;
  updated?: string;
  size?: number;
  missing?: boolean;
  wrongGame?: boolean;
  /** Updated on Steam after the server started. */
  newer?: boolean;
}

export interface WorkshopOverview {
  game: string;
  appId: number;
  env: string;
  namesEnv?: string;
  items: WorkshopItem[];
  running: boolean;
  started?: string;
  warnings?: string[];
  timeoutNs: number;
}

export interface ModsOverview {
  /** Set when hlm can't manage this server's mods. */
  unsupported?: string;
  /** Set for servers whose image takes Steam Workshop items in a variable. */
  workshop?: WorkshopOverview;
  plugins?: boolean;
  loader: string;
  fit: { loaders: string[]; gameVersion: string };
  /** Why mods can't be added (the game version isn't known). */
  fitError?: string;
  dir: string;
  running: boolean;
  warnings?: string[];
  mods: InstalledMod[];
  checked?: string;
  timeoutNs: number;
}

export interface InstalledMod {
  file: string;
  disabled?: boolean;
  size: number;
  modified: string;
  /** hlm: installed through hlm; modrinth: put there another way, and recognised. */
  source: "hlm" | "modrinth" | "unknown";
  project?: string;
  slug?: string;
  title?: string;
  version?: string;
  versionNumber?: string;
  by?: string;
  installed?: string;
  update?: { version: string; versionNumber: string; blocked?: string };
}

export interface ModSearchHit {
  project_id: string;
  slug: string;
  title: string;
  description: string;
  author: string;
  downloads: number;
  server_side?: string;
  client_side?: string;
}

export type ModOp = "add" | "update" | "remove" | "disable" | "enable";

export interface ModChange {
  op: ModOp;
  project?: string;
  version?: string;
  file?: string;
  why?: string;
}

export interface ModPlan {
  steps: {
    op: "add" | "replace" | "remove" | "disable" | "enable";
    file?: string;
    title?: string;
    new?: {
      project: string;
      version: string;
      versionNumber: string;
      file: string;
      size: number;
      sha512: string;
    };
    fromVersion?: string;
    why?: string;
  }[];
  changes: ModChange[];
  warnings?: string[];
  running: boolean;
}

export type DiscordAction = "players" | "start" | "stop" | "restart" | "say";

export interface DiscordRule {
  roleId: string;
  roleName?: string;
  /** Server group IDs; none means every server the Discord server sees. */
  groups: string[];
  actions: DiscordAction[];
}

/** A Discord server the bot answers in. */
export interface DiscordGuild {
  id: string;
  name?: string;
  /** Server group IDs whose servers (on the status page) it sees; none means all of them. */
  groups: string[];
  /** Show addresses and who's playing. */
  details: boolean;
  rules: DiscordRule[];
}

export interface DiscordSettings {
  enabled: boolean;
  guilds: DiscordGuild[];
}

export interface DiscordGuildState {
  id: string;
  name?: string;
  commands: boolean;
  error?: string;
}

export interface DiscordState {
  status: "off" | "connecting" | "connected" | "error";
  error?: string;
  bot?: string;
  applicationId?: string;
  since?: string;
  guilds?: DiscordGuildState[];
}

export interface DiscordView {
  settings: DiscordSettings;
  hasToken: boolean;
  state: DiscordState;
  inviteUrl?: string;
}

export interface DiscordRole {
  id: string;
  name: string;
  position: number;
}

export interface DiscordTest {
  result: { bot?: string; inviteUrl?: string; guilds?: DiscordGuildState[] };
  error?: string;
}

export interface RestoreTestSettings {
  every: "" | "week" | "backups";
  /** With every "backups": after every Nth backup. */
  backups?: number;
  timeoutMinutes?: number;
}

export interface RestoreTestResult {
  key: string;
  trigger: "schedule" | "manual";
  started: string;
  finished: string;
  outcome: "passed" | "failed" | "skipped";
  seconds?: number;
  answer?: string;
  note?: string;
  error?: string;
  log?: string;
  files?: number;
  bytes?: number;
}

export interface RestoreTests {
  settings: RestoreTestSettings;
  results: RestoreTestResult[];
}

export interface IdleSettings {
  /** Minutes without players before it's stopped; 0 is off. */
  minutes: number;
  wakeOnConnect: boolean;
  wakeFromStatusPage: boolean;
}

export interface IdleStatus extends IdleSettings {
  /** The public status page is on and the server is on it (where "start it from the status page" shows a button). */
  onStatusPage?: boolean;
  idleSince?: string;
  asleep?: {
    since: string;
    minutes: number;
    listener?: string;
    listenerError?: string;
  };
  canStop: boolean;
  why?: string;
  canListen: boolean;
  listenWhy?: string;
}

export type GameProfileSource = "built-in" | "custom";

export interface GameProfileRef {
  id: string;
  name: string;
  source: GameProfileSource;
  /** hlm can install mods for the game. */
  mods?: boolean;
}

/** What hlm knows about a game (only the parts the UI shows). */
export interface GameProfile extends GameProfileRef {
  match: string[][];
  file?: string;
  /** A custom profile that takes a built-in one's place. */
  replaces?: boolean;
  /** Where your own profile was imported from, if from a link or index. */
  origin?: GameProfileOrigin;
}

export interface GameProfileOrigin {
  url: string;
  sha256: string;
  fetched: string;
  /** The index it was listed in. */
  index?: string;
  checked?: string;
  /** The SHA-256 the link (or index) has now, when it's different. */
  update?: string;
  updateError?: string;
  /** The file was changed here since it was fetched. */
  changedLocally?: boolean;
}

/** A diff line: " " the same, "-" only in the old text, "+" only in the new. */
export interface DiffLine {
  op: " " | "-" | "+";
  text: string;
}

/** A profile fetched from a link or index, before it's saved. */
export interface GameProfilePreview {
  url: string;
  sha256: string;
  id: string;
  name: string;
  yaml: string;
  index?: string;
  /** What saving it replaces: "built-in", "custom", or nothing (a new game). */
  replaces?: "built-in" | "custom";
  diff?: DiffLine[];
  same?: boolean;
}

/** Where a profile is fetched from: a link, or an index and an id. */
export type GameProfileLink = { url: string } | { index: string; id: string };

export interface ProfileIndexListing {
  url: string;
  name?: string;
  error?: string;
  profiles: {
    id: string;
    name: string;
    url: string;
    sha256: string;
    description?: string;
    inUse?: "built-in" | "custom";
    installed?: boolean;
    current?: boolean;
  }[];
}

export interface GameProfiles {
  /** Where your own profiles are kept. */
  dir: string;
  profiles: GameProfile[];
  /** Files that couldn't be used. */
  errors: { file: string; error: string }[];
}

/** A Compose stack's service as it runs. */
export interface StackMember {
  service: string;
  container: string;
  /** The container as a server: name@host on another Docker host. */
  server: string;
  id?: string;
  /** "missing" when the file has it but there's no container. */
  state: string;
  status?: string;
  health?: string;
  image: string;
}

/** A Compose stack (GET /api/stacks). */
export interface Stack {
  name: string;
  /** The Docker host it runs on ("local" for this one). */
  host: string;
  services: StackMember[];
  created: string;
  updated: string;
  busy?: boolean;
  /** The stored file can't be read any more. */
  error?: string;
  /** The Compose file on its host it was adopted from. */
  source?: string;
}

/** What applying a Compose file would do. */
export interface StackPlan {
  services: {
    service: string;
    container: string;
    image: string;
    ports: string[];
    mounts: string[];
    dependsOn?: string[];
    change: "create" | "update" | "unchanged" | "remove" | "adopt";
  }[];
  networks: string[];
  warnings: string[];
}

/** A Compose stack on a host, and what adopting it would do. */
export interface StackAdoptPlan {
  found: {
    name: string;
    host: string;
    /** The Compose file, and its folder. */
    source: string;
    dir: string;
    yaml: string;
    env?: string;
    /** The containers Compose made, by service. */
    containers: Record<string, StackMember>;
    warnings: string[];
  };
  plan: StackPlan;
}

/** A server hlm stopped because it exited too often in a short time. */
export interface CrashLoop {
  since: string;
  exits: number;
  windowMinutes: number;
  exitCode?: string;
  reason?: string;
}

export type HealthCheckKind = "" | "none" | "tcp" | "http" | "command";

/** A custom health check; timings of 0 (or left out) use the defaults. */
export interface HealthCheck {
  kind: HealthCheckKind;
  port?: number;
  path?: string;
  command?: string;
  /** Seconds. */
  interval?: number;
  /** Seconds. */
  timeout?: number;
  retries?: number;
  /** Minutes; left out for the default, 0 for none. */
  startPeriod?: number;
}

/** A Kubernetes-style probe, with Kubernetes's field names. */
export interface Probe {
  exec?: { command: string[] };
  httpGet?: {
    path?: string;
    port: number | string;
    host?: string;
    scheme?: "HTTP" | "HTTPS";
    httpHeaders?: { name: string; value: string }[];
  };
  tcpSocket?: { port: number | string; host?: string };
  initialDelaySeconds?: number;
  periodSeconds?: number;
  timeoutSeconds?: number;
  successThreshold?: number;
  failureThreshold?: number;
}

export interface Probes {
  startupProbe?: Probe;
  livenessProbe?: Probe;
  readinessProbe?: Probe;
}

export type ProbeKind = "startup" | "liveness" | "readiness";

/** How a running server's probes are doing. */
export interface ProbeStatus {
  started: boolean;
  ready: boolean;
  readySince?: string;
  restarting?: boolean;
  probes: Partial<
    Record<
      ProbeKind,
      {
        result?: "success" | "failure";
        successes: number;
        failures: number;
        message?: string;
        checked?: string;
        next?: string;
        waiting?: boolean;
      }
    >
  >;
}

/** A server's health settings (PUT /api/servers/{id}/health). */
export interface HealthSettings {
  crashLoop: { off?: boolean; exits?: number; minutes?: number };
  check: HealthCheck;
  /** Minutes of failing health before hlm restarts it; 0 or left out is off. */
  restartAfter?: number;
  /** "" for Docker's health check, "query" for the game's status query. */
  signal?: "" | "query";
  /** Kubernetes-style probes, run by hlm. */
  probes?: Probes;
}

export interface DockerHealthcheck {
  Test?: string[];
  Interval?: number;
  Timeout?: number;
  StartPeriod?: number;
  Retries?: number;
}

/** GET /api/servers/{id}/health. */
export interface ServerHealth {
  settings: HealthSettings;
  defaults: {
    exits: number;
    minutes: number;
    interval: number;
    timeout: number;
    retries: number;
    startPeriod: number;
  };
  /** Docker's view, while running: starting, healthy or unhealthy. */
  health?: string;
  crashLoop?: CrashLoop;
  recentExits: number;
  /** The container's check now (operators only). */
  currentCheck?: DockerHealthcheck;
  /** The container's check is the one hlm set. */
  custom: boolean;
  /** The container has the check the settings ask for. */
  applied: boolean;
  unhealthySince?: string;
  queryAvailable: boolean;
  /** Its probes' state, while it runs. */
  probes?: ProbeStatus;
}

/** A role on a server, each including the ones before. */
export type ServerAccess = GroupRole | "admin";
/** What a server group can give. */
export type GroupRole = "viewer" | "operator" | "manager";
/** A global role. */
export type Role = "admin" | "member";
/** Keys have one more role than accounts: they can create confined servers. */
export type KeyRole = Role | "deployer" | "metrics";
export type GrantKind = "local" | "oidc-user" | "oidc-group" | "key";

export interface ServerGrant {
  subject: { kind: GrantKind; name: string };
  role: GroupRole;
}

/** Servers and who may use them (GET /api/groups). Non-admins get the groups they're in, without grants. */
export interface ServerGroup {
  id: string;
  name: string;
  servers: string[];
  /** Docker hosts whose every server is in the group ("local" for this one). */
  hosts?: string[];
  grants: ServerGrant[];
}

/** A container hlm runs for a connection, such as a tunnel (GET /api/connections). */
export interface Connection {
  id: string;
  name: string;
  /** "wireguard" or "cloudflared". */
  kind: string;
  /** What it's for, e.g. "WireGuard tunnel". */
  title: string;
  /** The page that manages it, e.g. "/proxy". */
  page: string;
  image: string;
  state: string;
  status: string;
  health?: string;
  created: string;
  network: string;
  restartPolicy: string;
  update?: UpdateStatus;
  /** The container hlm itself runs in: it can't be stopped from here, and updates from the System page. */
  self?: boolean;
}

export interface Stats {
  /** Docker's figure: 100 is one full core. */
  cpuPercent: number;
  onlineCpus: number;
  memoryUsage: number;
  memoryLimit: number;
  networkRx: number;
  networkTx: number;
  blockRead: number;
  blockWrite: number;
  pids: number;
}

export interface PullProgress {
  id?: string;
  status?: string;
  progress?: string;
  progressDetail?: { current?: number; total?: number };
}

/** An install, update or reconfigure running on the backend, whoever started it. */
export interface Job {
  id: string;
  kind: "install" | "update" | "reconfigure";
  server: string;
  app?: string;
  icon?: string;
  image?: string;
  stage: string;
  message?: string;
  started: string;
  finished?: string;
  error?: string;
  pull: {
    layers: number;
    done: number;
    downloaded: number;
    total: number;
    extracting: boolean;
  };
}

export interface UpdateStatus {
  state: "current" | "available" | "unknown" | "error";
  reason?: string;
  checkedAt: string;
}

export interface UpdateCheckInfo {
  checking: boolean;
  lastCheck?: string;
  available: number;
  /** While checking: how many of the check's images have been looked up, of how many. */
  done?: number;
  total?: number;
}

export interface DeployEvent {
  /** begin, failed and summary only occur when updating several servers. */
  stage:
    | "pull"
    | "stop"
    | "create"
    | "start"
    | "done"
    | "error"
    | "warning"
    | "begin"
    | "failed"
    | "summary"
    // hlm's own updates:
    | "download"
    | "verify"
    | "log"
    | "install"
    | "restart"
    // imports and clones:
    | "created"
    | "template"
    | "settings"
    | "copy"
    // restore tests:
    | "restore"
    | "cleanup"
    | "skipped"
    // mod changes:
    | "folder"
    | "snapshot"
    | "apply"
    | "wait"
    | "rollback";
  message?: string;
  /** The server an event is about, when updating several servers. */
  target?: string;
  progress?: PullProgress;
  server?: Server;
  /** How far copying a server's data has got. */
  copy?: CopyProgress;
}

/** How far a copy of a server's data has got. */
export interface CopyProgress {
  files: number;
  bytes: number;
  /** How much there is in all; missing while it's measured (or if it can't be). */
  total?: number;
  /** Bytes a second, lately. */
  rate: number;
}

export interface Status {
  /** host is only sent to admins. */
  docker: {
    ok: boolean;
    host?: string;
    error?: string;
    version?: string;
    apiVersion?: string;
  };
  catalog: { apps: number; fetchedAt?: string; feedUpdated?: string };
  /** Only sent to admins. */
  appdataRoot?: string;
}

export interface HostSnapshot {
  available: boolean;
  reason?: string;
  hostname?: string;
  kernel?: string;
  uptime: number;
  sampledAt: string;
  interval: number;
  cpu: {
    cores: number;
    percent: number;
    load1: number;
    load5: number;
    load15: number;
  };
  memory: {
    total: number;
    used: number;
    available: number;
    cached: number;
    swapTotal: number;
    swapUsed: number;
  };
  network:
    | {
        name: string;
        rxRate: number;
        txRate: number;
        rxTotal: number;
        txTotal: number;
      }[]
    | null;
  filesystems:
    | {
        device: string;
        mount: string;
        fsType: string;
        total: number;
        used: number;
        free: number;
      }[]
    | null;
  diskIO: { device: string; readRate: number; writeRate: number }[] | null;
  history:
    | { t: string; cpu: number; memUsed: number; rx: number; tx: number }[]
    | null;
}

export interface DataFolder {
  path: string;
  target: string;
  volume?: boolean;
  size: number;
  error?: string;
  shared?: boolean;
  within?: string;
  /** For deleted servers: the folder or volume is gone. */
  missing?: boolean;
  /** For deleted servers: existing servers that use it now. */
  inUseBy?: string[];
}

/** A deleted server whose data is still on disk. */
export interface DeletedStorage {
  id: string;
  name: string;
  app?: string;
  icon?: string;
  image?: string;
  deletedAt: string;
  data: DataFolder[];
  dataTotal: number;
}

export interface ServerStorage {
  id: string;
  name: string;
  app?: string;
  icon?: string;
  image: string;
  imageSize: number;
  imageShared: boolean;
  writable: number;
  data: DataFolder[];
  dataTotal: number;
}

export interface StorageReport {
  docker?: {
    images: number;
    containers: number;
    volumes: number;
    buildCache: number;
  };
  dockerError?: string;
  servers: ServerStorage[];
  deleted?: DeletedStorage[];
  calculatedAt?: string;
  calculating: boolean;
  dataReason?: string;
}

export interface SelfUpdateStatus {
  current: string;
  mode: "binary" | "container";
  latest?: {
    version: string;
    name?: string;
    notes?: string;
    url?: string;
    published?: string;
  };
  available: boolean;
  canUpdate: boolean;
  reason?: string;
  target?: string;
  checkedAt?: string;
  checkError?: string;
  updating: boolean;
  lastResult?: {
    from: string;
    to: string;
    ok: boolean;
    error?: string;
    at: string;
  };
}

export interface SystemInfo {
  version: string;
  update: SelfUpdateStatus;
}

export interface FileEntry {
  name: string;
  type: "dir" | "file" | "symlink" | "other";
  size: number;
  mtime: string;
  mode: string;
  link?: string;
  isMount?: boolean;
}

export interface FileMount {
  path: string;
  source: string;
  type: "bind" | "volume";
  readOnly: boolean;
}

export interface FileListing {
  path: string;
  entries: FileEntry[];
  source: "host" | "docker";
  readOnly: boolean;
  truncated: boolean;
  mount?: FileMount;
  mounts: FileMount[];
}

export interface FileContent {
  path: string;
  content: string;
  size: number;
  mtime: string;
  mode: string;
  binary: boolean;
  readOnly: boolean;
  source: "host" | "docker";
}

export interface FileMatch {
  path: string;
  size: number;
  mtime: string;
}

export interface UnifiConfig {
  configured: boolean;
  url?: string;
  site?: string;
  authType?: "password" | "apikey";
  username?: string;
  hasPassword: boolean;
  hasApiKey: boolean;
  fingerprint?: string;
  interface?: "wan" | "wan2" | "both";
  forwardTo?: string;
  detectedHostIp?: string;
  error?: string;
}

/** Settings sent to save or test; empty password/apiKey keeps the saved one. */
export interface UnifiInput {
  url: string;
  site: string;
  authType: "password" | "apikey";
  username: string;
  password: string;
  apiKey: string;
  fingerprint: string;
  interface: "wan" | "wan2" | "both";
  forwardTo: string;
}

export interface UnifiPortForward {
  _id: string;
  name: string;
  enabled: boolean;
  src: string;
  dst_port: string;
  fwd: string;
  fwd_port: string;
  proto: string;
}

export interface ForwardRule {
  key: string;
  dstPort: string;
  fwdPort: string;
  proto: "tcp" | "udp" | "tcp_udp";
  fwd: string;
  state: "forwarded" | "outdated" | "missing" | "unknown";
  existing?: UnifiPortForward;
  conflicts?: UnifiPortForward[];
}

export interface ForwardStatus {
  configured: boolean;
  target?: string;
  note?: string;
  rules: ForwardRule[];
  stale: UnifiPortForward[];
  error?: string;
}

/** What UniFi knows about a LAN address. */
export interface IPCheck {
  ip: string;
  /** False when UniFi isn't set up, so nothing was checked. */
  checked: boolean;
  inUse: boolean;
  conflict?: string;
  warning?: string;
  network?: string;
  error?: string;
}

export interface SSHKeyInfo {
  public: string;
  fingerprint: string;
  created: string;
}

export interface ProxySettings {
  host: string;
  port: number;
  user: string;
  hostKey?: string;
  upstream: string;
  /**
   * "unifi": the gateway's public address, looked up and kept up to date.
   * "wireguard": this host's end of the WireGuard tunnel hlm set up.
   */
  upstreamSource: "manual" | "unifi" | "wireguard";
  unifiWan?: "wan" | "wan2";
  /** Where traffic goes at home: the internet port (router forwards it) or the server's LAN port (VPN). */
  upstreamPorts?: "internet" | "local";
  logMode: "noip" | "off";
  servers: string[];
  /** Serve the status page's sites on the proxy (through the WireGuard tunnel), with HTTPS as statusTls says. */
  statusPage?: boolean;
  statusTls?: "" | "letsencrypt" | "cloudflare" | "letsencrypt-cloudflare";
  statusEmail?: string;
  /** Also serve the status page on the proxy's own address (the one players use), over HTTP on statusPort (80). */
  statusOnAddress?: boolean;
  statusPort?: number;
  /** HTTPS there, as statusAddressName, with a Let's Encrypt certificate hlm gets through Cloudflare DNS. */
  statusAddressHttps?: boolean;
  statusAddressName?: string;
}

/** How serving the status page's sites on the proxy last went. */
export interface ProxyStatusSites {
  domains: string[];
  /** The status page on the proxy's own address, when it's served there. */
  address?: string;
  /** Why it's there without the HTTPS asked for. */
  addressError?: string;
  /** The HTTPS option used. */
  tls?: "" | "letsencrypt" | "cloudflare" | "letsencrypt-cloudflare";
  https: string[];
  failed?: Record<string, string>;
  /** Things to fix that don't stop the sites working. */
  notes?: string[];
  output?: string;
  error?: string;
}

export interface FirewallRule {
  action: string;
  protocol: string;
  ports?: string;
  label?: string;
  description?: string;
}

export interface FirewallStatus {
  configured: boolean;
  provider?: "linode";
  hasToken: boolean;
  firewallId?: number;
  firewallLabel?: string;
  firewallState?: string;
  inboundPolicy?: string;
  /** hlm's rules on the firewall now, and what they should be. */
  rules: FirewallRule[];
  wanted: FirewallRule[];
  inSync: boolean;
  otherRules: number;
  error?: string;
}

export interface FirewallSync {
  changed: boolean;
  rules: FirewallRule[];
  error?: string;
}

export interface FirewallChoice {
  id: number;
  label: string;
  status: string;
  linodes?: string[];
}

export interface FirewallTest {
  firewalls: FirewallChoice[];
  suggested?: number;
  linode?: string;
  note?: string;
}

/** The WireGuard tunnel between the proxy server and the Docker host. */
export interface WireGuardStatus {
  configured: boolean;
  /** The tunnel's network: the proxy has its first address, this host the second. */
  network: string;
  listenPort: number;
  /** host:port the home end connects to. */
  endpoint?: string;
  proxyAddress?: string;
  homeAddress?: string;
  proxyPublicKey?: string;
  homePublicKey?: string;
  configuredAt?: string;
  /** The container running this host's end. */
  home: {
    state?: string;
    running: boolean;
    exitCode?: number;
    error?: string;
    lanAddress?: string;
  };
  /** The proxy's home address is the tunnel. */
  inUse: boolean;
  image: string;
  /** LAN mode: the macvlan/ipvlan network the home end joins to reach servers' own LAN addresses. */
  lanNetwork?: string;
  lanSubnet?: string;
  /** The fixed address asked for there, if any. */
  lanAddress?: string;
  /** Networks LAN mode can use. */
  lanNetworks: {
    name: string;
    driver: string;
    subnet: string;
    parent?: string;
  }[];
  /** The tunnel's ends on other Docker hosts. */
  hosts?: WireGuardHostEnd[];
}

/** The WireGuard tunnel's end on another Docker host. */
export interface WireGuardHostEnd {
  host: string;
  homeAddress: string;
  homePublicKey: string;
  configuredAt?: string;
  lanNetwork?: string;
  lanSubnet?: string;
  lanAddress?: string;
  home: {
    state?: string;
    running: boolean;
    exitCode?: number;
    error?: string;
    lanAddress?: string;
  };
}

export interface WireGuardInput {
  network: string;
  listenPort: number;
  /** Empty: the proxy server's SSH host. */
  endpoint: string;
  /** Make the tunnel the proxy's home address (re-applying nginx if it's applied). */
  useForRelays: boolean;
  /** LAN mode's network, empty for the host's network; and an optional fixed address on it. */
  lanNetwork: string;
  lanAddress: string;
}

export interface WireGuardResult {
  status: WireGuardStatus;
  output: string;
  apply?: {
    config: string;
    output: string;
    servers: ProxiedServer[];
    firewall?: FirewallSync;
  };
  applyError?: string;
  firewall?: FirewallSync;
}

/** Progress while the tunnel is set up. */
export interface WireGuardEvent {
  /** step: what's being done; output: a console line; then done (with result) or error. */
  stage: "step" | "output" | "done" | "error";
  message?: string;
  output?: string;
  result?: WireGuardResult;
}

export interface WireGuardCheck {
  /** The last handshake the proxy end saw. */
  handshake?: string;
  /** The proxy could ping this host through the tunnel; absent if ping isn't installed. */
  reachable?: boolean;
  /** The last handshakes with the ends on other hosts, by host. */
  hosts?: Record<string, string>;
  output?: string;
}

export interface UpstreamState {
  /** The home address in the configuration on the proxy server. */
  applied?: string;
  appliedAt?: string;
  /** What UniFi last reported. */
  current?: string;
  checkedAt?: string;
  error?: string;
}

export interface ProxyInfo {
  os?: string;
  root: boolean;
  sudo: boolean;
  nginx?: string;
  stream: boolean;
  running: boolean;
  included: boolean;
  streamBlock: boolean;
  /** The file holding nginx's own stream block. */
  streamFile?: string;
  configured: boolean;
  packageManager?: string;
  /** hlm's file on the proxy: /etc/nginx/hlm-stream.conf, or gsh-stream.conf on proxies set up before the rename. */
  configFile?: string;
}

/** Adding hlm's include to nginx's own stream block. */
export interface Adoption {
  file: string;
  /** The block now, and after the change. */
  block: string;
  proposed: string;
  included: boolean;
  /** False when nginx has no stream block of its own. */
  found: boolean;
}

/** One of a server's ports: players connect to `port`, which reaches the server on `local`. */
export interface InternetPort {
  port: number;
  local: number;
  proto: string;
  /** Whether UniFi forwards it, the nginx proxy relays it, and the proxy's firewall opens it. */
  forward?: boolean;
  proxy?: boolean;
  firewall?: boolean;
  /** Other servers using the same internet port. */
  conflicts?: string[];
}

/** A server's ports with their options, and which integrations are set up. */
export interface ServerPorts {
  ports: InternetPort[];
  unifi: boolean;
  proxy: {
    configured: boolean;
    relayed: boolean;
    applied: boolean;
    route?: ProxyRoute;
  };
  firewall: boolean;
}

/** How the proxy reaches a server. */
export interface ProxyRoute {
  /** The home address source: through the WireGuard tunnel, or to your public address. */
  source: "manual" | "unifi" | "wireguard";
  /** The proxy server players connect to. */
  proxyHost: string;
  /** Where nginx sends the server's traffic: the tunnel's end on this host, the server's LAN address, or your public address. */
  target?: string;
  /** Traffic goes to the server's own ports, not its internet ports. */
  ownPorts: boolean;
  /** The target is the server's own LAN address (the tunnel's LAN mode), not this host. */
  serverAddress?: boolean;
  /** Why the server can't be relayed this way. */
  error?: string;
}

/** Whether a port's setting is in place in one integration. */
export interface SyncState {
  state: "synced" | "pending" | "removing" | "conflict" | "off";
  detail?: string;
}

/** Per-port sync states by "local/proto"; null where an integration isn't set up. */
export interface PortSync {
  unifi: Record<string, SyncState> | null;
  unifiError?: string;
  proxy: Record<string, SyncState> | null;
  proxyError?: string;
  firewall: Record<string, SyncState> | null;
  firewallError?: string;
}

export interface PortSetting {
  port: number;
  forward: boolean;
  proxy: boolean;
  firewall: boolean;
}

export interface ProxiedServer {
  server: string;
  ports: InternetPort[];
  /** The server's own LAN address the proxy sends to through the WireGuard tunnel, instead of the home address. */
  address?: string;
  skipped?: InternetPort[];
  error?: string;
}

/** A Cloudflare account an API token can use. */
export interface CloudflareAccount {
  id: string;
  name: string;
}

/** A domain on Cloudflare. */
export interface CloudflareZone {
  id: string;
  name: string;
  /** "active" once the domain uses Cloudflare's name servers. */
  status: string;
  account: CloudflareAccount;
}

export interface CloudflareTest {
  accounts: CloudflareAccount[];
  zones: CloudflareZone[];
}

/** One of the tunnel's connections to Cloudflare's edge. */
export interface TunnelConnection {
  id: string;
  colo_name: string;
  origin_ip?: string;
  opened_at: string;
  client_version?: string;
  is_pending_reconnect?: boolean;
}

/** The Cloudflare connection and the tunnel (GET /api/tunnel). */
export interface TunnelStatus {
  /** An API token is saved. */
  configured: boolean;
  accountId?: string;
  accountName?: string;
  zones: CloudflareZone[];
  tunnel?: {
    id: string;
    name: string;
    created: string;
    /** Cloudflare's view: healthy, degraded, down, inactive; "deleted" if it's gone there; empty if unknown. */
    health?: string;
    connections: TunnelConnection[];
    /** The hlm-cloudflared container. */
    connector: {
      state?: string;
      running: boolean;
      exitCode?: number;
      error?: string;
      lanAddress?: string;
      startedAt?: string;
    };
    lanNetwork?: string;
    lanSubnet?: string;
    lanAddress?: string;
  };
  image: string;
  lanNetworks: {
    name: string;
    driver: string;
    subnet: string;
    parent?: string;
  }[];
  /** Cloudflare couldn't be asked. */
  error?: string;
  /** Tunnels on other Docker hosts, each with cloudflared there. */
  hosts?: (NonNullable<TunnelStatus["tunnel"]> & { host: string })[];
}

export interface TunnelSetupInput {
  /** The Docker host cloudflared runs on: empty for this one. */
  host?: string;
  name: string;
  lanNetwork: string;
  lanAddress: string;
}

/** Sends a hostname to one of a server's TCP ports, or to another web server's address. */
export interface TunnelRoute {
  hostname: string;
  zoneId?: string;
  zone?: string;
  /** The game server it goes to; empty with `address`. */
  server?: string;
  /** Another web server's IP address or host name, e.g. 192.168.1.20 or nas.lan. */
  address?: string;
  /** The port on the host (published), the server's own port, or the other server's port. */
  port: number;
  scheme: "http" | "https";
  noTLSVerify?: boolean;
  /** Goes to Home Lab Manager itself (such as a status page site); hlm works out its port. */
  hlm?: boolean;
}

export interface TunnelRouteState extends TunnelRoute {
  url: string;
  /** The Docker host whose tunnel carries it. */
  via?: string;
  /** Where cloudflared sends requests, e.g. http://localhost:8080. */
  origin?: string;
  error?: string;
  state: "published" | "pending" | "conflict" | "error";
}

/** A TCP port a route can go to. */
export interface WebPort {
  port: number;
  /** The container's own port, for published ports. */
  container?: number;
  hostIp?: string;
}

export interface TunnelRoutes {
  routes: TunnelRouteState[];
  servers: {
    name: string;
    running: boolean;
    ports: WebPort[];
    webuiPort?: number;
    webuiScheme?: string;
  }[];
  /** The tunnel is set up, so routes are published when saved. */
  tunnel: boolean;
  /** Something isn't published yet. */
  pending: boolean;
}

export interface TunnelApply {
  routes: TunnelRouteState[];
  created: number;
  updated: number;
  removed: number;
}

export interface TunnelEvent {
  stage: "step" | "output" | "done" | "error";
  message?: string;
  status?: TunnelStatus;
  apply?: TunnelApply;
}

/** Where backups are stored: an S3-compatible bucket (SeaweedFS, MinIO, AWS, …). */
export interface BackupSettings {
  configured: boolean;
  endpoint?: string;
  region?: string;
  bucket?: string;
  accessKey?: string;
  hasSecret: boolean;
  virtualHost: boolean;
  /** The folder in the bucket backups go under. */
  prefix?: string;
  /** PEM certificates of a private CA trusted for the endpoint. */
  caCert?: string;
  caCerts?: CertInfo[];
  error?: string;
}

/** A certificate in BackupSettings.caCert. */
export interface CertInfo {
  subject: string;
  issuer: string;
  notAfter: string;
  isCA: boolean;
}

/** Bucket settings to test or save; an empty secretKey keeps the saved one. */
export interface BackupSettingsInput {
  endpoint: string;
  region: string;
  bucket: string;
  accessKey: string;
  secretKey: string;
  virtualHost: boolean;
  prefix: string;
  /** PEM certificates of a private CA to trust; empty for none. */
  caCert: string;
}

export interface BackupRun {
  trigger: "manual" | "schedule" | "restore";
  started: string;
  finished: string;
  key?: string;
  size: number;
  files: number;
  bytes: number;
  error?: string;
  warnings?: string[];
  /** For a restore: the backup of the files it replaced. */
  before?: string;
  sha256?: string;
  /** Files and folders exclude patterns left out. */
  excluded?: number;
  /** Reading the backup back after it was stored. */
  verify?: BackupVerification;
  /** Copying it to the plan's other storage. */
  copy?: { target: string; key?: string; error?: string };
}

/** How reading a backup back went. */
export interface BackupVerification {
  ok: boolean;
  at: string;
  seconds: number;
  files: number;
  bytes: number;
  /** A stored checksum was compared (older backups have none). */
  checksum: boolean;
  error?: string;
}

/** Keeping backups by age: the newest `last`, and one a day, week and month for that many. */
export interface BackupRetention {
  last: number;
  daily: number;
  weekly: number;
  monthly: number;
}

/** Backing up at a time of day; weekdays are 0 (Sunday) to 6, empty for every day. */
export interface BackupAt {
  time: string;
  weekdays?: number[];
}

/** What to back up for a server, and when. */
export interface BackupPlan {
  folders: string[];
  /** 0 means only when asked (unless at is set). */
  intervalHours: number;
  at?: BackupAt;
  /** How many backups to keep; 0 keeps them all. retention, if set, keeps by age instead. */
  keep: number;
  retention?: BackupRetention;
  stopServer: boolean;
  /** Console commands before and after, one per line. */
  preCommand?: string;
  postCommand?: string;
  exclude?: string[];
  /** "" (backups under 5 GB), "always" or "off". */
  verify?: "" | "always" | "off";
  copyTo?: { target: string; keep: number; retention?: BackupRetention };
  /** Latest verifications, by "<storage id>:<key>". */
  checks?: Record<string, BackupVerification>;
  last?: BackupRun;
  lastOk?: BackupRun;
  lastRestore?: BackupRun;
  /** Folders the game keeps its own backups in; each new one is uploaded. */
  gameFolders?: GameBackupFolder[];
  /** How many of each folder's uploaded game backups to keep (0: all); those still in the folder are kept either way. */
  gameKeep?: number;
  lastGame?: GameBackupSync;
}

/** A folder a game keeps its own backups in: a container path, where * stands for one folder's name. */
export interface GameBackupFolder {
  path: string;
  /** Only entries whose names match (e.g. *_backup_*). */
  match?: string;
}

/** Where the server's game profile says the game keeps its backups, found in this server. */
export interface GameBackupSuggestion extends GameBackupFolder {
  note?: string;
}

/** How an upload of a game's own backups went. */
export interface GameBackupSync {
  trigger: "manual" | "schedule";
  started: string;
  finished: string;
  found: number;
  uploaded: number;
  bytes: number;
  deleted?: number;
  error?: string;
  warnings?: string[];
}

/** A backup storage, by ID. */
export interface BackupTargetRef {
  id: string;
  name: string;
  encrypt?: boolean;
}

/** A backup storage (System → Backup storage). The primary is first. */
export interface BackupTarget extends BackupSettings {
  id: string;
  name: string;
  encrypt: boolean;
  /** An encryption key exists, so encrypted backups there can be read. */
  hasKey: boolean;
  /** The new encryption key, only in the answer to the save that made it. */
  newKey?: string;
}

/** A backup or restore in progress. */
export interface BackupProgress {
  /** game: uploading the game's own new backups. */
  kind: "backup" | "restore" | "game";
  trigger: "manual" | "schedule" | "restore";
  started: string;
  stage:
    | "preparing"
    | "commands"
    | "stopping"
    | "uploading"
    | "verifying"
    | "copying"
    | "restoring"
    | "cleaning"
    | "starting";
  files: number;
  bytes: number;
  /** Compressed bytes uploaded or downloaded so far. */
  transferred: number;
  /** The backup being restored. */
  key?: string;
}

export interface BackupObject {
  key: string;
  size: number;
  lastModified: string;
}

/** One server's backups, in the list of every server's (the Backups page). */
export interface ServerBackupsSummary {
  server: string;
  folders: string[];
  intervalHours?: number;
  at?: BackupAt;
  /** When the next scheduled backup is due. */
  next?: string;
  running?: BackupProgress;
  last?: BackupRun;
  lastOk?: BackupRun;
  /** Its backups in the primary storage, newest first (only when asked for). */
  stored: BackupObject[];
  storedSize: number;
  storedError?: string;
  gameFolders?: GameBackupFolder[];
  lastGame?: GameBackupSync;
  gameStored?: BackupObject[];
  gameSize?: number;
}

export interface AllBackups {
  configured: boolean;
  target: BackupTargetRef;
  servers: ServerBackupsSummary[];
  /** hlm's own settings backup, while one runs (admins only). */
  config?: BackupProgress;
  error?: string;
}

export interface BackupOverview {
  configured: boolean;
  plan: BackupPlan;
  running?: BackupProgress;
  /** Newest first. */
  backups: BackupObject[];
  error?: string;
  /** The primary storage. */
  target: BackupTargetRef;
  /** The backups in the storage the plan copies to. */
  copies?: { target: BackupTargetRef; backups: BackupObject[]; error?: string };
  /** The other storage the plan can copy to. */
  targets: BackupTargetRef[];
  /** The game's own backups hlm uploaded, newest first. */
  gameBackups: BackupObject[];
  /** Only when asked for. */
  gameSuggestions?: GameBackupSuggestion[];
}

/** When hlm backs up its own configuration. */
export interface ConfigBackupPlan {
  /** 0 means only when asked. */
  intervalHours: number;
  /** How many backups to keep; 0 keeps them all. retention, if set, keeps by age instead. */
  keep: number;
  retention?: BackupRetention;
  /** Adds the key that decrypts the stored credentials. */
  includeKey: boolean;
  last?: BackupRun;
}

export interface ConfigBackupOverview {
  /** Backup storage is set up. */
  configured: boolean;
  plan: ConfigBackupPlan;
  running?: BackupProgress;
  /** Newest first. */
  backups: BackupObject[];
  /** The secrets' key comes from HLM_SECRET_KEY, so it's never in the data directory. */
  keyFromEnv: boolean;
  error?: string;
}

/** Who is signed in (GET /api/auth/status). */
export interface AuthUser {
  /** "local" for a local account, "oidc" for single sign-on. */
  method: "local" | "oidc";
  sub: string;
  username?: string;
  name?: string;
  email?: string;
  groups?: string[];
  expires: string;
}

/** How to sign in, and who is (GET /api/auth/status). Reachable without signing in. */
export interface AuthStatus {
  required: boolean;
  /** There are local accounts to sign in with. */
  local: boolean;
  /** Single sign-on is set up, with the provider named oidcName. */
  oidc: boolean;
  oidcName?: string;
  /** HLM_TOKEN is set. */
  token: boolean;
  user?: AuthUser;
  /** The caller's global role, if they're let in at all. */
  role?: Role;
  /** The signed-in local account's two-factor. */
  twoFactor?: {
    enabled: boolean;
    recoveryLeft: number;
    required: boolean;
    passkeys?: number;
  };
  /** When the session's confirmation (see ConfirmPrompt) runs out, if it's confirmed. */
  confirmedUntil?: string;
}

/** Who has to use two-factor: "" (nobody), "admins" or "all". */
export type RequireTwoFactor = "" | "admins" | "all";

/** A secret being set up for an authenticator app. */
export interface TwoFactorSetup {
  secret: string;
  url: string;
  /** A QR code of url, as SVG, made by hlm. */
  qr: string;
}

/** How sign-in applies to everyone (GET /api/auth/config). */
export interface AuthConfig {
  required: boolean;
  requireTwoFactor?: RequireTwoFactor;
  sessionHours: number;
}

/** A local account (GET /api/auth/accounts). */
export interface Account {
  username: string;
  created: string;
  passwordChanged: string;
  role: Role;
  /** How many SSH keys can sign in as this account. */
  sshKeys: number;
  /** It uses an authenticator app; recoveryLeft of its recovery codes are unused. */
  twoFactor: boolean;
  recoveryLeft?: number;
  /** How many passkeys it has. */
  passkeys?: number;
}

/** A public key that can sign in as an account through the API. */
export interface SSHKey {
  id: string;
  name: string;
  type: string;
  fingerprint: string;
  added: string;
  lastUsed?: string;
  /** Addresses and ranges the key can be used from; missing is anywhere. */
  allowFrom?: string[];
}

/** An API key (GET /api/auth/keys); its secret is only shown when it's made. */
export interface APIKeyInfo {
  id: string;
  /** The start of the key, to tell keys apart. */
  prefix: string;
  name: string;
  created: string;
  expires?: string;
  expired?: boolean;
  lastUsed?: string;
  role: KeyRole;
  /** Addresses and ranges the key can be used from; empty is anywhere. */
  allowFrom: string[];
}

/** Single sign-on with an OpenID Connect provider such as Authelia (GET /api/auth/oidc). */
export interface OIDCSettings {
  configured: boolean;
  name: string;
  issuer?: string;
  clientId?: string;
  hasSecret: boolean;
  publicUrl?: string;
  redirectUrl?: string;
  scopes: string;
  allowedUsers: string[];
  allowedGroups: string[];
  /** Who of those is an admin. Both empty makes everyone one. */
  adminUsers: string[];
  adminGroups: string[];
  /** Someone has signed in with these settings' provider and client. */
  verified?: string;
  verifiedBy?: string;
  /** HLM_OIDC=off makes hlm ignore these settings. */
  disabled?: boolean;
  error?: string;
}

export interface OIDCInput {
  name: string;
  issuer: string;
  clientId: string;
  /** Empty keeps the saved secret. */
  clientSecret: string;
  publicUrl: string;
  scopes: string;
  allowedUsers: string[];
  allowedGroups: string[];
  adminUsers: string[];
  adminGroups: string[];
}

/** A client secret made by hlm (POST /api/auth/oidc/secret), with its digest for Authelia's configuration. */
export interface ClientSecret {
  secret: string;
  autheliaDigest: string;
}

/** The provider found by testing the settings. */
export interface OIDCTestResult {
  issuer: string;
  authorizationUrl: string;
  scopes?: string[];
  pkce: boolean;
}

/** HTTPS with a Let's Encrypt certificate (GET /api/system/https). */
export interface HTTPSStatus {
  enabled: boolean;
  domain?: string;
  email?: string;
  staging?: boolean;
  /** HTTPS is served and plain HTTP sent to it. */
  active: boolean;
  state: "off" | "waiting" | "issuing" | "active" | "error";
  error?: string;
  /** HLM_TLS=off overrides the settings. */
  off?: boolean;
  cloudflare: boolean;
  notAfter?: string;
  issued?: string;
  issuer?: string;
  certFor?: string;
}

/** How serious a notification is. */
export type Severity = "info" | "warning" | "error";

/** A kind of event targets can choose (in GET /api/notifications). */
export interface NotifyKind {
  name: string;
  area: string;
  severity: Severity;
  description: string;
}

export type TargetKind = "discord" | "ntfy" | "gotify" | "webhook" | "email";

/** A notification target as sent to the API. Secrets left blank keep their saved values. */
export interface NotifyTargetInput {
  id?: string;
  name: string;
  kind: TargetKind;
  enabled: boolean;
  events: string[];
  servers?: string[];
  groups?: string[];
  quietStart?: string;
  quietEnd?: string;
  url?: string;
  topic?: string;
  token?: string;
  headers?: { name: string; value: string }[];
  smtpHost?: string;
  smtpPort?: number;
  security?: "starttls" | "tls" | "none";
  username?: string;
  password?: string;
  from?: string;
  to?: string[];
}

/** The result of sending one event to one target. */
export interface NotifyDelivery {
  target: string;
  name: string;
  ok: boolean;
  error?: string;
  /** Waiting for the target's quiet hours to end. */
  held?: boolean;
  at: string;
}

/** A target as the API shows it: without its secrets. */
export interface NotifyTarget extends NotifyTargetInput {
  id: string;
  /** A secret URL's scheme and host. */
  urlHint?: string;
  hasUrl: boolean;
  hasToken: boolean;
  hasPassword: boolean;
  last?: NotifyDelivery;
}

/** GET /api/notifications */
export interface NotifySettings {
  targets: NotifyTarget[];
  baseUrl: string;
  diskPercent: number;
  /** The address links use when baseUrl is empty (the HTTPS address). */
  defaultBaseUrl?: string;
  kinds: NotifyKind[];
  /** Events dropped because too many were waiting. */
  dropped?: number;
}

/** An event and where it went (GET /api/notifications/history). */
export interface NotifyRecord {
  kind: string;
  severity: Severity;
  title: string;
  message?: string;
  server?: string;
  link?: string;
  time: string;
  /** A repeat of one sent recently, so not sent again. */
  suppressed?: boolean;
  deliveries: NotifyDelivery[];
}

/** How a server takes console commands. */
export type ConsoleMode = "" | "stdin" | "rcon" | "exec" | "rest";

/** A server's console settings; the password is never sent back. */
export interface ConsoleSettings {
  mode: ConsoleMode;
  /** RCON: an address to reach the port at instead of finding it; port is then the port there. */
  host?: string;
  /** RCON: the container's RCON port; 0 or empty finds it. */
  port?: number;
  /** RCON: a password to use instead of the variable's. Blank keeps the saved one. */
  password?: string;
  /** RCON: the container's environment variable holding the password. */
  passwordVar?: string;
  /** Exec: a command line run in the container, with {command} where the command goes. */
  exec?: string;
  quickCommands?: string[];
}

/** PUT /api/servers/{id}/console */
export interface ConsoleInput extends ConsoleSettings {
  clearPassword?: boolean;
}

/** GET /api/servers/{id}/console */
export interface ConsoleStatus {
  settings: ConsoleSettings;
  hasPassword: boolean;
  /** Whether commands can be sent now; reason says why not. */
  ready: boolean;
  reason?: string;
  /** How the server looks like it takes commands, and why. */
  detected?: ConsoleSettings;
  detectedBecause?: string;
  /** The container accepts input (-i). */
  stdinOpen: boolean;
  running: boolean;
  game?: string;
  quickCommands: string[];
  /** The game's commands for scheduled tasks (from its profile): warning players, and saving ("" if it has none). */
  schedule?: { say: string; save: string };
  envVars: string[];
}

/** POST /api/servers/{id}/console/command */
export interface ConsoleResult {
  mode: ConsoleMode;
  /** The reply over RCON or from the exec command; stdin commands' output is in the logs. */
  output: string;
  exitCode?: number;
}

/** When a scheduled task runs. */
export interface TaskWhen {
  kind: "daily" | "every" | "cron";
  /** daily: "04:00", and weekdays (0 is Sunday; empty is every day). */
  time?: string;
  weekdays?: number[];
  /** every: the interval in minutes. */
  every?: number;
  cron?: string;
}

export type StepKind =
  | "start"
  | "stop"
  | "restart"
  | "update"
  | "backup"
  | "command"
  | "wait"
  | "if_players";

export interface TaskStep {
  kind: StepKind;
  /** command: the console command; hidden from viewers. */
  command?: string;
  /** wait: seconds. */
  seconds?: number;
  /** if_players: carry on only if nobody ("none") or someone ("some") is playing. */
  players?: "none" | "some";
  continueOnError?: boolean;
}

export interface TaskStepResult {
  kind: StepKind;
  ok: boolean;
  skipped?: boolean;
  message?: string;
}

export interface TaskRun {
  trigger: "schedule" | "manual";
  started: string;
  finished: string;
  result: "ok" | "failed" | "skipped" | "missed";
  error?: string;
  steps?: TaskStepResult[];
}

export interface ScheduledTask {
  id: string;
  name: string;
  enabled: boolean;
  when: TaskWhen;
  steps: TaskStep[];
  /** Skip the task when the server is stopped. */
  onlyIfRunning: boolean;
  next?: string;
  runs?: TaskRun[];
}

/** A task that's running. */
export interface TaskProgress {
  taskId: string;
  taskName: string;
  trigger: string;
  started: string;
  /** Index of the step running. */
  step: number;
  steps: number;
}

/** GET /api/servers/{id}/tasks */
export interface ServerTasks {
  tasks: ScheduledTask[];
  running?: TaskProgress;
  /** hlm's time zone, which task times are in. */
  timezone: string;
}

/** GET /api/schedule/upcoming */
export interface UpcomingTask {
  server: string;
  taskId: string;
  taskName: string;
  when: string;
  next: string;
}

/** GET /api/system/timezone */
export interface TimezoneInfo {
  /** The configured zone; empty uses the system's. */
  timezone: string;
  system: string;
  effective: string;
}

/** How a server's updates are applied: "" follows the default. */
export type UpdateMode = "" | "manual" | "auto";

export interface UpdatePolicy {
  mode: UpdateMode;
  /** When an update may start ("03:00"–"06:00", hlm's time zone); empty is any time. */
  windowStart?: string;
  windowEnd?: string;
  /** A console command sent warnMinutes before updating. */
  warn?: string;
  warnMinutes?: number;
  /** Back up first, if the server has a backup plan. */
  backup: boolean;
  /** Wait while players are online (when the server's status query counts them). */
  onlyWhenEmpty?: boolean;
}

export interface AutoUpdateState {
  state: "waiting" | "busy" | "queued" | "updating" | "failed" | "players";
  message?: string;
  next?: string;
}

export interface AutoUpdateResult {
  started: string;
  finished: string;
  ok: boolean;
  error?: string;
  steps?: string[];
}

/** GET /api/servers/{id}/update-policy */
export interface ServerUpdatePolicy {
  /** The server's own policy; mode "" when it follows the default. */
  policy: UpdatePolicy;
  effective: UpdatePolicy;
  default: UpdatePolicy;
  state?: AutoUpdateState;
  last?: AutoUpdateResult;
  timezone: string;
}

export interface SelfUpdatePolicy {
  auto: boolean;
  windowStart?: string;
  windowEnd?: string;
}

/** GET /api/system/update-policy */
export interface SystemUpdatePolicy {
  default: UpdatePolicy;
  /** 0 means defaultCheckHours. */
  checkHours: number;
  defaultCheckHours: number;
  /** HLM_UPDATE_CHECK, which overrides checkHours, if set. */
  checkEnv?: string;
  self: SelfUpdatePolicy;
  selfLast?: AutoUpdateResult;
}

/** GET /api/updates/auto */
export interface AutoUpdates {
  default: UpdateMode;
  servers: Record<string, UpdateMode>;
  states: Record<string, AutoUpdateState>;
}

export type QueryProtocol =
  "" | "a2s" | "minecraft" | "bedrock" | "http-status" | "rcon" | "tcp" | "off";

/** One of a server's game's launch options (from its game profile), and whether it's on. */
export interface LaunchOption {
  id: string;
  name: string;
  description?: string;
  args: string;
  port?: number;
  proto?: string;
  on: boolean;
  /** Where its arguments go: "the variable GAME_PARAMS", or "its post arguments". */
  where: string;
}

/** How a server is asked for its status; empty finds a way. */
export interface QuerySettings {
  protocol: QueryProtocol;
  /** The container port to query. */
  port?: number;
}

/** What a server said to its game's status query. */
export interface QueryResult {
  protocol: QueryProtocol;
  address?: string;
  name?: string;
  map?: string;
  game?: string;
  version?: string;
  players: number;
  maxPlayers?: number;
  /** Only for operators and above. */
  playerNames?: string[];
  password?: boolean;
  /** Nanoseconds. */
  latency: number;
  /** False when the protocol doesn't count players (a TCP check). */
  hasPlayers: boolean;
}

export type GameStatusKind =
  "online" | "starting" | "not_responding" | "unknown" | "off" | "stopped";

/** GET /api/servers/{id}/query */
export interface GameStatus {
  status: GameStatusKind;
  settings: QuerySettings;
  /** How it's being asked: chosen, or found. */
  method?: QuerySettings;
  result?: QueryResult;
  error?: string;
  checked?: string;
  since?: string;
  /** The most players in each 5-minute step of the last day. */
  history?: { t: string; players: number }[];
}

/** GET /api/query, by server name. */
export interface GameSummary {
  status: GameStatusKind;
  players: number;
  maxPlayers?: number;
  hasPlayers: boolean;
}

/** A server's settings file, pinned or suggested. */
export interface ConfigFile {
  /** Absolute path in the container. */
  path: string;
  /** Empty uses the file name. */
  label?: string;
}

/** GET /api/servers/{id}/config-files */
export interface ConfigFiles {
  pinned: ConfigFile[];
  /** The game's usual settings files, found in its data folders, that aren't pinned. */
  suggested: ConfigFile[];
}

export type AuditResult = "ok" | "denied" | "error";
export type AuditActorKind =
  | "local"
  | "sso"
  | "key"
  | "ssh"
  | "token"
  | "anonymous"
  | "hlm"
  | "host"
  | "discord"
  | "";

/** One thing someone (or hlm) did. */
export interface AuditEntry {
  time: string;
  actor: string;
  actorKind: AuditActorKind;
  ip?: string;
  /** e.g. servers.restart, auth.login, notifications.targets.create */
  action: string;
  target?: string;
  server?: string;
  result: AuditResult;
  /** The HTTP status, for API requests. */
  status?: number;
  error?: string;
  details?: Record<string, string>;
}

/** A page of entries, newest first; next is the cursor for the following page. */
export interface AuditPage {
  entries: AuditEntry[];
  next?: string;
}

export interface AuditFilter {
  actor?: string;
  action?: string;
  target?: string;
  server?: string;
  result?: string;
  text?: string;
  since?: string;
  before?: string;
  limit?: number;
}

/** GET /api/system/audit */
export interface AuditSettings {
  retentionMonths: number;
  mirror: boolean;
  defaultRetention: number;
}

/** GET /api/registries: a private registry's login, without its password. */
export interface RegistryLogin {
  host: string;
  username: string;
  /** Confined installs (deployer keys, members who deploy) may use it. */
  deployers: boolean;
}

/** POST/PUT /api/registries, and the test. A blank password keeps (or tests) the saved one. */
export interface RegistryForm extends RegistryLogin {
  password: string;
  image?: string;
}

/** A range of resource history. */
export type HistoryRange = "1h" | "6h" | "24h" | "7d" | "30d";

/** GET /api/host/history, /api/servers/{id}/history: per-step averages and peaks; null where nothing was recorded. */
export interface MetricHistory {
  /** Seconds per step: 60 up to a day, 900 beyond. */
  step: number;
  /** Each step's start, epoch ms. */
  times: number[];
  /** cpu, mem, rx, tx, diskRead, diskWrite, players, and fs:<mount> (percent used) for the host. */
  series: Record<string, { avg: (number | null)[]; max: (number | null)[] }>;
}

/** GET/PUT /api/system/metrics */
export interface MetricsSettings {
  /** Addresses and ranges that may read /metrics without a key. */
  allowFrom: string[];
}

/** A Docker host servers can be on. Admins see how it's reached. */
export interface DockerHost {
  id: string;
  name: string;
  local?: boolean;
  status?: { id: string; ok: boolean; error?: string; checked: string };
  kind?: "tcp" | "ssh" | "agent";
  url?: string;
  ca?: string;
  cert?: string;
  hasKey?: boolean;
  ssh?: { host: string; port: number; user: string; hostKey?: string };
  socket?: string;
  appdataRoot?: string;
  address?: string;
  /** For a host with hlm's agent: its connection. */
  agent?: AgentStatus;
  /** How its agent's last update went. */
  agentUpdate?: AgentUpdate;
  /** How moving its agent to hlm's new address last went. */
  agentMove?: AgentMove;
}

/** Moving a host's agent to hlm's new address. */
export interface AgentMove {
  host: string;
  from?: string;
  to: string;
  /** pending: it's told when it next connects. */
  state: "moved" | "pending" | "failed";
  error?: string;
  at: string;
}

/** How a host's agent's last update to hlm's version went. */
export interface AgentUpdate {
  state: "updating" | "done" | "failed";
  from?: string;
  to: string;
  message?: string;
  at: string;
  /** hlm started it on its own. */
  auto?: boolean;
}

/** The hosts, and (for admins) what their agents are kept on. */
export interface HostsList {
  hosts: DockerHost[];
  sshKey?: SSHKeyInfo;
  /** hlm's version, which agents are updated to. */
  agentVersion?: string;
  agentAutoUpdate?: boolean;
  /** Why agents can't be updated (a development build of hlm). */
  agentUpdateBlocked?: string;
}

/** A host's agent's connection to hlm. */
export interface AgentStatus {
  connected: boolean;
  since?: string;
  lastSeen?: string;
  version?: string;
  remote?: string;
  /** hlm's address as the agent connects to it (older agents don't say). */
  server?: string;
}

/** A single-use token for a new host's agent. */
export interface AgentEnrolment {
  token: string;
  host: DockerHost;
  expires: string;
  version?: string;
}

/** A passkey on an account. */
export interface PasskeyInfo {
  id: string;
  name: string;
  /** The hostname it was added on, where it works. */
  rpId: string;
  synced?: boolean;
  created: string;
  lastUsed?: string;
}

/** A host to save: the TLS client key is only ever sent, never shown. */
export type DockerHostInput = DockerHost & { key?: string };

export interface DockerVersion {
  version: string;
  apiVersion: string;
  os: string;
  arch: string;
}

/** What hlm found on an SSH host, and how it would set Docker up there. */
export interface HostInspection {
  os: string;
  id: string;
  version: string;
  codename: string;
  arch: string;
  user: string;
  root: boolean;
  sudo: boolean;
  docker: boolean;
  running: boolean;
  dockerVersion?: string;
  inGroup: boolean;
  systemd: boolean;
  ready: boolean;
  distro?: string;
  script?: string;
  canSetUp: boolean;
  reason?: string;
  notes?: string[];
}

export type CleanupKind =
  "old-image" | "unused-image" | "helper" | "build-cache" | "volume";

/** Something disk cleanup can delete. */
export interface CleanupItem {
  kind: CleanupKind;
  id?: string;
  name: string;
  size: number;
  created?: string;
  default: boolean;
}

export interface CleanupPlan {
  host: string;
  items: CleanupItem[];
}

export interface CleanupRequest {
  images?: string[];
  containers?: string[];
  volumes?: string[];
  buildCache?: boolean;
}

export interface CleanupResult {
  freed: number;
  removed: CleanupItem[];
  failed: (CleanupItem & { error: string })[];
}

export interface CleanupSettings {
  keepOldImages: boolean;
  weekly: boolean;
  lastRun?: string;
  lastFreed: number;
  lastError?: string;
}

/** A server on the public status page, as an admin set it up. */
export interface StatusPageEntry {
  server: string;
  displayName?: string;
  showPlayers: boolean;
  showNames: boolean;
  showVersion: boolean;
  address?: string;
  hint?: string;
  slug?: string;
}

export interface StatusPageSettings {
  enabled: boolean;
  title?: string;
  message?: string;
  servers: StatusPageEntry[];
  allowFrom?: string[];
}

/** The status page under its own domain: some server groups' servers, behind its own passwords. */
export interface StatusSite {
  id?: string;
  hostname: string;
  /** Server group IDs; none shows every server on the page. */
  groups: string[];
  title?: string;
  message?: string;
  /** Kept off the proxy server while it serves the status page. */
  noProxy?: boolean;
  hasViewPassword?: boolean;
  hasDetailsPassword?: boolean;
  /** On save: a new password, "" to remove it, or left out to keep it. */
  viewPassword?: string;
  detailsPassword?: string;
}

export interface AddressSuggestion {
  address: string;
  source: "proxy" | "unifi";
}

/** The public status page: only these fields are ever sent. */
export interface PublicStatusServer {
  slug: string;
  name: string;
  game?: string;
  icon?: string;
  state: "online" | "starting" | "running" | "offline" | "asleep";
  /** It's asleep, and visitors may start it. */
  canWake?: boolean;
  players?: number;
  maxPlayers?: number;
  playerNames?: string[];
  version?: string;
  map?: string;
  address?: string;
  hint?: string;
  uptime7d?: number;
  uptime30d?: number;
}

export interface PublicStatusPage {
  title: string;
  message?: string;
  servers: PublicStatusServer[];
  updated: string;
  /** What the visitor's password (if any) lets them see. */
  access?: {
    level: number;
    viewLocked: boolean;
    detailsLocked: boolean;
    unlocked: boolean;
  };
}

/** A container on a Docker host that hlm doesn't manage. */
export interface UnmanagedContainer {
  /** Its name, with @host for another host: the name it keeps as a server. */
  id: string;
  host: string;
  name: string;
  image: string;
  icon?: string;
  state: string;
  status: string;
  created: string;
  ports: PortSummary[];
  network?: string;
  /** "source → destination" */
  mounts?: string[];
  unraid?: boolean;
  compose?: string;
  /** Its Compose project's file on the host (or folder), for adopting the whole stack. */
  composeFile?: string;
  adoptable: boolean;
  reason?: string;
}

/** How hlm would adopt a container: where its settings come from, and what would change. */
export interface AdoptPlan {
  container: UnmanagedContainer;
  source: "community-apps" | "unraid" | "container";
  template: string;
  templateId?: string;
  request: DeployRequest;
  changes: string[];
  notes?: string[];
  /** It's running, so adopting restarts it. */
  restart: boolean;
}

/** Why a moved server's network or address has to be chosen again on the new host. */
export interface NetworkChoice {
  /** Its network, subnets and fixed address where it is now. */
  network: string;
  subnets: string[];
  ipAddress?: string;
  /** The network's subnets on the new host, if it has it. */
  target?: string[];
  reason: string;
}

/** How a server would move to another Docker host. */
export interface MigratePreview extends TransferPreview {
  /** Its name there (name@host). */
  name: string;
  /** It runs now: it's stopped while its data is copied, then started there. */
  running: boolean;
  network?: NetworkChoice;
  /** Port forwards, web addresses and the like that are pointed at the new host. */
  follows: string[];
}
