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
}

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
  usedIps: { ip: string; container: string }[];
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
  name: string;
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
}

/** A role on a server, each including the ones before. */
export type ServerAccess = GroupRole | "admin";
/** What a server group can give. */
export type GroupRole = "viewer" | "operator" | "manager";
/** A global role. */
export type Role = "admin" | "member";
/** Keys have one more role than accounts: they can create confined servers. */
export type KeyRole = Role | "deployer";
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
    | "install"
    | "restart";
  message?: string;
  /** The server an event is about, when updating several servers. */
  target?: string;
  progress?: PullProgress;
  server?: Server;
}

export interface Status {
  /** host is only sent to admins. */
  docker: { ok: boolean; host?: string; error?: string; version?: string; apiVersion?: string };
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
  cpu: { cores: number; percent: number; load1: number; load5: number; load15: number };
  memory: { total: number; used: number; available: number; cached: number; swapTotal: number; swapUsed: number };
  network: { name: string; rxRate: number; txRate: number; rxTotal: number; txTotal: number }[] | null;
  filesystems: { device: string; mount: string; fsType: string; total: number; used: number; free: number }[] | null;
  diskIO: { device: string; readRate: number; writeRate: number }[] | null;
  history: { t: string; cpu: number; memUsed: number; rx: number; tx: number }[] | null;
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
  docker?: { images: number; containers: number; volumes: number; buildCache: number };
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
  latest?: { version: string; name?: string; notes?: string; url?: string; published?: string };
  available: boolean;
  canUpdate: boolean;
  reason?: string;
  target?: string;
  checkedAt?: string;
  checkError?: string;
  updating: boolean;
  lastResult?: { from: string; to: string; ok: boolean; error?: string; at: string };
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
  home: { state?: string; running: boolean; exitCode?: number; error?: string; lanAddress?: string };
  /** The proxy's home address is the tunnel. */
  inUse: boolean;
  image: string;
  /** LAN mode: the macvlan/ipvlan network the home end joins to reach servers' own LAN addresses. */
  lanNetwork?: string;
  lanSubnet?: string;
  /** The fixed address asked for there, if any. */
  lanAddress?: string;
  /** Networks LAN mode can use. */
  /** Networks LAN mode can use. */
  lanNetworks: { name: string; driver: string; subnet: string; parent?: string }[];
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
  apply?: { config: string; output: string; servers: ProxiedServer[]; firewall?: FirewallSync };
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
  proxy: { configured: boolean; relayed: boolean; applied: boolean; route?: ProxyRoute };
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
    connector: { state?: string; running: boolean; exitCode?: number; error?: string; lanAddress?: string; startedAt?: string };
    lanNetwork?: string;
    lanSubnet?: string;
    lanAddress?: string;
  };
  image: string;
  lanNetworks: { name: string; driver: string; subnet: string; parent?: string }[];
  /** Cloudflare couldn't be asked. */
  error?: string;
}

export interface TunnelSetupInput {
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
}

export interface TunnelRouteState extends TunnelRoute {
  url: string;
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
  servers: { name: string; running: boolean; ports: WebPort[]; webuiPort?: number; webuiScheme?: string }[];
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
}

/** What to back up for a server, and when. */
export interface BackupPlan {
  folders: string[];
  /** 0 means only when asked. */
  intervalHours: number;
  /** How many backups to keep; 0 keeps them all. */
  keep: number;
  stopServer: boolean;
  last?: BackupRun;
  lastRestore?: BackupRun;
}

/** A backup or restore in progress. */
export interface BackupProgress {
  kind: "backup" | "restore";
  trigger: "manual" | "schedule" | "restore";
  started: string;
  stage: "preparing" | "stopping" | "uploading" | "restoring" | "cleaning" | "starting";
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

export interface BackupOverview {
  configured: boolean;
  plan: BackupPlan;
  running?: BackupProgress;
  /** Newest first. */
  backups: BackupObject[];
  error?: string;
}

/** When hlm backs up its own configuration. */
export interface ConfigBackupPlan {
  /** 0 means only when asked. */
  intervalHours: number;
  /** How many backups to keep; 0 keeps them all. */
  keep: number;
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
}

/** How sign-in applies to everyone (GET /api/auth/config). */
export interface AuthConfig {
  required: boolean;
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
export type ConsoleMode = "" | "stdin" | "rcon" | "exec";

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

export type StepKind = "start" | "stop" | "restart" | "update" | "backup" | "command" | "wait";

export interface TaskStep {
  kind: StepKind;
  /** command: the console command; hidden from viewers. */
  command?: string;
  /** wait: seconds. */
  seconds?: number;
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
