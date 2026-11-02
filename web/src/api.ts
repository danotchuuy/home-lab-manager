import type {
  Account,
  APIKeyInfo,
  KeyRole,
  SSHKey,
  App,
  AuthConfig,
  AuthStatus,
  AuthUser,
  ClientSecret,
  OIDCInput,
  OIDCSettings,
  OIDCTestResult,
  AppSummary,
  BackupOverview,
  BackupPlan,
  BackupSettings,
  BackupSettingsInput,
  CategoryCount,
  ConfigBackupOverview,
  ConfigBackupPlan,
  DeployEvent,
  DeployRequest,
  FileContent,
  FileListing,
  FileMatch,
  IPCheck,
  ForwardStatus,
  UnifiConfig,
  UnifiInput,
  HostSnapshot,
  Job,
  HostInterface,
  NetworkInfo,
  ProxiedServer,
  PortSetting,
  PortSync,
  PortConflict,
  ServerPorts,
  ProxyInfo,
  ProxySettings,
  Adoption,
  FirewallStatus,
  FirewallSync,
  FirewallTest,
  UpstreamState,
  CloudflareTest,
  Connection,
  TunnelApply,
  TunnelEvent,
  TunnelRoute,
  TunnelRoutes,
  TunnelSetupInput,
  TunnelStatus,
  WireGuardCheck,
  WireGuardEvent,
  WireGuardInput,
  WireGuardResult,
  WireGuardStatus,
  SSHKeyInfo,
  NetworkRequest,
  UpdateCheckInfo,
  Role,
  Server,
  ServerGroup,
  Stats,
  StorageReport,
  SystemInfo,
  Status,
} from "./types";

const TOKEN_KEY = "hlm.token";
/** Where the token was kept before the short name changed from gsh to hlm. */
const LEGACY_TOKEN_KEY = "gsh.token";

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? localStorage.getItem(LEGACY_TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setToken(token: string) {
  try {
    localStorage.setItem(TOKEN_KEY, token);
  } catch {
    // Storage may be unavailable (private mode); the token just won't persist.
  }
}

export class ApiError extends Error {
  status: number;
  /** The decoded JSON error body, for endpoints that add details. */
  body?: Record<string, unknown>;
  constructor(status: number, message: string, body?: Record<string, unknown>) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

/** Dispatched when the API rejects our token, so the app can ask for one. */
export const UNAUTHORIZED_EVENT = "hlm:unauthorized";

async function request(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  // Required with a session cookie for requests that change something; see csrfHeader in the API.
  headers.set("X-HLM-Request", "1");
  if (init.body && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");

  const res = await fetch(path, { ...init, headers });
  if (!res.ok) {
    let message = res.statusText;
    let body: Record<string, unknown> | undefined;
    try {
      body = await res.json();
      if (typeof body?.error === "string") message = body.error;
    } catch {
      // Not JSON; keep the status text.
    }
    if (res.status === 401) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    throw new ApiError(res.status, message, body);
  }
  return res;
}

async function json<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await request(path, init);
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

/** Sends a request whose response is a stream of newline-delimited JSON events. */
async function stream<T = DeployEvent>(path: string, init: RequestInit, onEvent: (e: T) => void): Promise<void> {
  const res = await request(path, init);
  if (!res.body) throw new Error("streaming is not supported by this browser");
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += value;
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (line) onEvent(JSON.parse(line) as T);
    }
  }
  if (buf.trim()) onEvent(JSON.parse(buf) as T);
}

export const api = {
  status: () => json<Status>("/api/status"),

  categories: () => json<CategoryCount[]>("/api/catalog/categories"),
  searchApps: (params: { q?: string; category?: string; sort?: string; offset?: number; limit?: number }) => {
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined) qs.set(k, String(v));
    }
    return json<{ total: number; items: AppSummary[] }>(`/api/catalog/apps?${qs}`);
  },
  app: (id: string) => json<App>(`/api/catalog/apps/${encodeURIComponent(id)}`),
  /** Pulls an image and makes a template from its defaults; the "done" event's message is the template ID. */
  importImage: (image: string, onEvent: (e: DeployEvent) => void) =>
    stream("/api/catalog/import", { method: "POST", body: JSON.stringify({ image }) }, onEvent),
  deleteTemplate: (id: string) => json<void>(`/api/catalog/apps/${encodeURIComponent(id)}`, { method: "DELETE" }),
  refreshCatalog: () => json<{ apps: number }>("/api/catalog/refresh", { method: "POST" }),

  networks: () => json<NetworkInfo[]>("/api/networks"),
  createNetwork: (req: NetworkRequest) =>
    json<NetworkInfo>("/api/networks", { method: "POST", body: JSON.stringify(req) }),
  removeNetwork: (name: string) => json<void>(`/api/networks/${encodeURIComponent(name)}`, { method: "DELETE" }),
  hostInterfaces: () => json<HostInterface[]>("/api/networks/interfaces"),

  servers: () => json<Server[]>("/api/servers"),
  server: (id: string) => json<Server>(`/api/servers/${encodeURIComponent(id)}`),
  serverConfig: (id: string) =>
    json<{ app: App; request: DeployRequest }>(`/api/servers/${encodeURIComponent(id)}/config`),
  /** Resource usage of every running server and connection container, keyed by container ID. */
  allStats: () => json<Record<string, Stats>>("/api/stats"),
  /** Containers hlm runs for connections (WireGuard, cloudflared). */
  connections: () => json<Connection[]>("/api/connections"),
  connection: (id: string) => json<Connection>(`/api/connections/${encodeURIComponent(id)}`),
  connectionStats: (id: string) => json<Stats>(`/api/connections/${encodeURIComponent(id)}/stats`),
  connectionAction: (id: string, action: "start" | "stop" | "restart") =>
    json<Connection[]>(`/api/connections/${encodeURIComponent(id)}/${action}`, { method: "POST" }),
  /**
   * Restarts the container hlm runs in. hlm goes down with it, so the request
   * usually fails partway; that's expected, and hlm is back in a few seconds.
   */
  restartSelf: async (id: string) => {
    if (!confirm("Restart Home Lab Manager? This page loses its connection for a few seconds while hlm restarts.")) return;
    try {
      await json<Connection[]>(`/api/connections/${encodeURIComponent(id)}/restart`, { method: "POST" });
    } catch (e) {
      if (e instanceof ApiError) throw e;
    }
  },
  /** Recreates a connection container on its newest image, keeping its settings. */
  updateConnection: (id: string, onEvent: (e: DeployEvent) => void) =>
    stream(`/api/connections/${encodeURIComponent(id)}/update`, { method: "POST" }, onEvent),
  listFiles: (id: string, path: string) =>
    json<FileListing>(`/api/servers/${encodeURIComponent(id)}/files?${new URLSearchParams({ path })}`),
  readFile: (id: string, path: string) =>
    json<FileContent>(`/api/servers/${encodeURIComponent(id)}/files/content?${new URLSearchParams({ path })}`),
  /** Saves a file. expectedMtime guards against overwriting changes made since it was opened; omit to force. */
  writeFile: (id: string, path: string, content: string, expectedMtime?: string) =>
    json<FileContent>(`/api/servers/${encodeURIComponent(id)}/files/content`, {
      method: "PUT",
      body: JSON.stringify({ path, content, expectedMtime }),
    }),
  searchFiles: (id: string, path: string, q = "") =>
    json<{ matches: FileMatch[]; truncated: boolean }>(
      `/api/servers/${encodeURIComponent(id)}/files/search?${new URLSearchParams({ path, q })}`,
    ),
  backupSettings: () => json<BackupSettings>("/api/integrations/backup"),
  /** Checks bucket settings by listing the bucket and writing a test file; returns how many backups are there. */
  testBackupSettings: (cfg: BackupSettingsInput) =>
    json<{ backups: number }>("/api/integrations/backup/test", { method: "POST", body: JSON.stringify(cfg) }),
  saveBackupSettings: (cfg: BackupSettingsInput) =>
    json<BackupSettings>("/api/integrations/backup", { method: "PUT", body: JSON.stringify(cfg) }),
  deleteBackupSettings: () => json<void>("/api/integrations/backup", { method: "DELETE" }),
  serverBackups: (id: string) => json<BackupOverview>(`/api/servers/${encodeURIComponent(id)}/backups`),
  setBackupPlan: (id: string, plan: Omit<BackupPlan, "last">) =>
    json<BackupPlan>(`/api/servers/${encodeURIComponent(id)}/backups/plan`, { method: "PUT", body: JSON.stringify(plan) }),
  /** Starts a backup in the background; serverBackups shows its progress. */
  startBackup: (id: string) => json<void>(`/api/servers/${encodeURIComponent(id)}/backups`, { method: "POST" }),
  /** Starts putting a backup's files back; backupFirst saves the current ones as a new backup before. */
  restoreBackup: (id: string, key: string, backupFirst: boolean) =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/backups/restore`, {
      method: "POST",
      body: JSON.stringify({ key, backupFirst }),
    }),
  deleteBackup: (id: string, key: string) =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/backups?${new URLSearchParams({ key })}`, { method: "DELETE" }),
  /** URL to download a backup from. A plain link can't send headers, so the token goes in the query. */
  backupDownloadUrl: (id: string, key: string) => withKey(`/api/servers/${encodeURIComponent(id)}/backups/download`, key),
  configBackups: () => json<ConfigBackupOverview>("/api/system/backup"),
  setConfigBackupPlan: (plan: Omit<ConfigBackupPlan, "last">) =>
    json<ConfigBackupPlan>("/api/system/backup", { method: "PUT", body: JSON.stringify(plan) }),
  /** Starts a backup of hlm's configuration in the background. */
  startConfigBackup: () => json<void>("/api/system/backup", { method: "POST" }),
  deleteConfigBackup: (key: string) =>
    json<void>(`/api/system/backup?${new URLSearchParams({ key })}`, { method: "DELETE" }),
  configBackupDownloadUrl: (key: string) => withKey("/api/system/backup/download", key),
  stats: (id: string) => json<Stats>(`/api/servers/${encodeURIComponent(id)}/stats`),
  action: (id: string, action: "start" | "stop" | "restart") =>
    json<Server>(`/api/servers/${encodeURIComponent(id)}/${action}`, { method: "POST" }),
  remove: (id: string) => json<void>(`/api/servers/${encodeURIComponent(id)}`, { method: "DELETE" }),

  install: (req: DeployRequest, onEvent: (e: DeployEvent) => void) =>
    stream("/api/servers", { method: "POST", body: JSON.stringify(req) }, onEvent),
  /** Host ports the settings would use that are already in use; `server` is the one being edited. */
  checkPorts: (request: DeployRequest, server?: string) =>
    json<{ conflicts: PortConflict[]; error?: string }>("/api/servers/check-ports", {
      method: "POST",
      body: JSON.stringify({ request, server }),
    }),
  reconfigure: (id: string, req: DeployRequest, onEvent: (e: DeployEvent) => void) =>
    stream(`/api/servers/${encodeURIComponent(id)}/config`, { method: "PUT", body: JSON.stringify(req) }, onEvent),
  setAutostart: (id: string, enabled: boolean) =>
    json<Server>(`/api/servers/${encodeURIComponent(id)}/autostart`, {
      method: "PUT",
      body: JSON.stringify({ enabled }),
    }),

  authStatus: () => json<AuthStatus>("/api/auth/status"),
  /** Signs in with a local account, starting a session. */
  login: (username: string, password: string) =>
    json<AuthUser>("/api/auth/login", { method: "POST", body: JSON.stringify({ username, password }) }),
  logout: () => json<void>("/api/auth/logout", { method: "POST" }),
  authConfig: () => json<AuthConfig>("/api/auth/config"),
  saveAuthConfig: (c: AuthConfig) => json<AuthConfig>("/api/auth/config", { method: "PUT", body: JSON.stringify(c) }),
  accounts: () => json<Account[]>("/api/auth/accounts"),
  createAccount: (username: string, password: string, role: Role) =>
    json<Account>("/api/auth/accounts", { method: "POST", body: JSON.stringify({ username, password, role }) }),
  /** current is the password being replaced, which non-admins have to give. */
  changePassword: (username: string, password: string, current?: string) =>
    json<void>(`/api/auth/accounts/${encodeURIComponent(username)}/password`, { method: "PUT", body: JSON.stringify({ password, current }) }),
  setAccountRole: (username: string, role: Role) =>
    json<Account>(`/api/auth/accounts/${encodeURIComponent(username)}/role`, { method: "PUT", body: JSON.stringify({ role }) }),
  sshKeys: (username: string) => json<SSHKey[]>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys`),
  addSSHKey: (username: string, publicKey: string, name: string, current: string) =>
    json<SSHKey>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys`, {
      method: "POST",
      body: JSON.stringify({ publicKey, name, current }),
    }),
  deleteSSHKey: (username: string, id: string) =>
    json<void>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys/${encodeURIComponent(id)}`, { method: "DELETE" }),
  deleteAccount: (username: string) => json<void>(`/api/auth/accounts/${encodeURIComponent(username)}`, { method: "DELETE" }),
  apiKeys: () => json<APIKeyInfo[]>("/api/auth/keys"),
  /** Makes an API key; the response's key is the only time its secret is shown. */
  createAPIKey: (name: string, days: number, role: KeyRole) =>
    json<APIKeyInfo & { key: string }>("/api/auth/keys", { method: "POST", body: JSON.stringify({ name, days, role }) }),
  deleteAPIKey: (id: string) => json<void>(`/api/auth/keys/${encodeURIComponent(id)}`, { method: "DELETE" }),
  groups: () => json<ServerGroup[]>("/api/groups"),
  createGroup: (g: Omit<ServerGroup, "id">) => json<ServerGroup>("/api/groups", { method: "POST", body: JSON.stringify(g) }),
  updateGroup: (id: string, g: ServerGroup) =>
    json<ServerGroup>(`/api/groups/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(g) }),
  deleteGroup: (id: string) => json<void>(`/api/groups/${encodeURIComponent(id)}`, { method: "DELETE" }),
  setServerGroups: (server: string, groups: string[]) =>
    json<Server>(`/api/servers/${encodeURIComponent(server)}/groups`, { method: "PUT", body: JSON.stringify({ groups }) }),
  oidcSettings: () => json<OIDCSettings>("/api/auth/oidc"),
  testOIDC: (cfg: OIDCInput) => json<OIDCTestResult>("/api/auth/oidc/test", { method: "POST", body: JSON.stringify(cfg) }),
  saveOIDC: (cfg: OIDCInput) => json<OIDCSettings>("/api/auth/oidc", { method: "PUT", body: JSON.stringify(cfg) }),
  deleteOIDC: () => json<void>("/api/auth/oidc", { method: "DELETE" }),
  /** Makes a client secret for registering hlm with a provider; nothing is saved until the settings are. */
  generateClientSecret: () => json<ClientSecret>("/api/auth/oidc/secret", { method: "POST" }),
  unifiConfig: () => json<UnifiConfig>("/api/integrations/unifi"),
  /** Asks UniFi whether a LAN address is free; server excludes that server's own container. */
  checkIP: (ip: string, server?: string) =>
    json<IPCheck>(`/api/integrations/unifi/check-ip?${new URLSearchParams(server ? { ip, server } : { ip })}`),
  testUnifi: (cfg: UnifiInput) =>
    json<{ unifiOS: boolean; portForwards: number }>("/api/integrations/unifi/test", {
      method: "POST",
      body: JSON.stringify(cfg),
    }),
  saveUnifi: (cfg: UnifiInput) => json<UnifiConfig>("/api/integrations/unifi", { method: "PUT", body: JSON.stringify(cfg) }),
  deleteUnifi: () => json<void>("/api/integrations/unifi", { method: "DELETE" }),
  portSync: (id: string) => json<PortSync>(`/api/servers/${encodeURIComponent(id)}/ports/status`),
  serverPorts: (id: string) => json<ServerPorts>(`/api/servers/${encodeURIComponent(id)}/ports`),
  /** Saves a server's port options; `relay`, if given, adds it to the proxy's relays or removes it. */
  setServerPorts: (id: string, ports: Record<string, PortSetting>, relay?: boolean) =>
    json<ServerPorts>(`/api/servers/${encodeURIComponent(id)}/ports`, {
      method: "PUT",
      body: JSON.stringify({ ports, relay }),
    }),
  portForward: (id: string) => json<ForwardStatus>(`/api/servers/${encodeURIComponent(id)}/portforward`),
  syncPortForward: (id: string) =>
    json<ForwardStatus>(`/api/servers/${encodeURIComponent(id)}/portforward`, { method: "POST" }),
  removePortForward: (id: string) =>
    json<{ removed: number }>(`/api/servers/${encodeURIComponent(id)}/portforward`, { method: "DELETE" }),

  proxy: () =>
    json<{ configured: boolean; settings: ProxySettings; key: SSHKeyInfo | null; upstream: UpstreamState }>("/api/proxy"),
  checkProxyWAN: () => json<UpstreamState>("/api/proxy/wan-check", { method: "POST" }),
  unifiWAN: (wan: string) => json<{ wan: string; address: string }>(`/api/integrations/unifi/wan?wan=${wan}`),
  saveProxy: (st: ProxySettings) => json<ProxySettings>("/api/proxy", { method: "PUT", body: JSON.stringify(st) }),
  deleteProxy: () => json<void>("/api/proxy", { method: "DELETE" }),
  generateKey: (replace = false) =>
    json<SSHKeyInfo>("/api/proxy/key", { method: "POST", body: JSON.stringify({ replace }) }),
  importKey: (privateKey: string) =>
    json<SSHKeyInfo>("/api/proxy/key", { method: "PUT", body: JSON.stringify({ privateKey }) }),
  testProxy: (st: ProxySettings) => json<ProxyInfo>("/api/proxy/test", { method: "POST", body: JSON.stringify(st) }),
  installNginx: () => json<{ output: string }>("/api/proxy/install", { method: "POST" }),
  previewProxy: () => json<{ config: string; servers: ProxiedServer[]; path: string }>("/api/proxy/preview"),
  applyProxy: () =>
    json<{ config: string; output: string; servers: ProxiedServer[]; firewall?: FirewallSync }>("/api/proxy/apply", {
      method: "POST",
    }),
  planAdoption: () => json<Adoption>("/api/proxy/adopt"),
  adoptStreamBlock: () => json<{ adoption: Adoption; output: string }>("/api/proxy/adopt", { method: "POST" }),
  removeProxy: () => json<{ output: string; firewall?: FirewallSync }>("/api/proxy/remove", { method: "POST" }),
  wireGuard: () => json<WireGuardStatus>("/api/proxy/wireguard"),
  /** Sets up the tunnel, streaming steps and console output as they happen. */
  setupWireGuard: (input: WireGuardInput, onEvent: (e: WireGuardEvent) => void) =>
    stream<WireGuardEvent>("/api/proxy/wireguard", { method: "PUT", body: JSON.stringify(input) }, onEvent),
  removeWireGuard: () => json<WireGuardResult>("/api/proxy/wireguard", { method: "DELETE" }),
  checkWireGuard: () => json<WireGuardCheck>("/api/proxy/wireguard/check", { method: "POST" }),
  linode: () => json<FirewallStatus>("/api/integrations/linode"),
  /** Lists the firewalls a token can manage; an empty token uses the saved one. */
  testLinode: (token: string) =>
    json<FirewallTest>("/api/integrations/linode/test", { method: "POST", body: JSON.stringify({ token }) }),
  /** Saves the token (empty keeps the saved one) and the firewall to manage (0 for none). */
  saveLinode: (token: string, firewallId: number) =>
    json<FirewallStatus>("/api/integrations/linode", {
      method: "PUT",
      body: JSON.stringify({ provider: "linode", token, firewallId }),
    }),
  deleteLinode: (keepRules = false) =>
    json<void>(`/api/integrations/linode${keepRules ? "?keepRules=1" : ""}`, { method: "DELETE" }),
  syncProxyFirewall: () => json<FirewallStatus>("/api/proxy/firewall/sync", { method: "POST" }),

  tunnel: () => json<TunnelStatus>("/api/tunnel"),
  /** Lists the accounts and domains a Cloudflare token can use; an empty token tests the saved one. */
  testCloudflare: (token: string) =>
    json<CloudflareTest>("/api/integrations/cloudflare/test", { method: "POST", body: JSON.stringify({ token }) }),
  /** Saves the token (empty keeps the saved one) and the account the tunnel goes in. */
  saveCloudflare: (token: string, accountId: string) =>
    json<TunnelStatus>("/api/integrations/cloudflare", { method: "PUT", body: JSON.stringify({ token, accountId }) }),
  deleteCloudflare: () => json<void>("/api/integrations/cloudflare", { method: "DELETE" }),
  /** Creates the tunnel (the first time) and runs cloudflared, streaming steps and console output. */
  setupTunnel: (input: TunnelSetupInput, onEvent: (e: TunnelEvent) => void) =>
    stream<TunnelEvent>("/api/tunnel", { method: "PUT", body: JSON.stringify(input) }, onEvent),
  removeTunnel: () => json<TunnelStatus>("/api/tunnel", { method: "DELETE" }),
  controlTunnel: (action: "start" | "stop" | "restart") => json<TunnelStatus>(`/api/tunnel/${action}`, { method: "POST" }),
  tunnelRoutes: () => json<TunnelRoutes>("/api/tunnel/routes"),
  /** Replaces the routes, publishing them if the tunnel is set up. */
  setTunnelRoutes: (routes: TunnelRoute[]) =>
    json<TunnelApply>("/api/tunnel/routes", { method: "PUT", body: JSON.stringify({ routes }) }),
  applyTunnel: () => json<TunnelApply>("/api/tunnel/apply", { method: "POST" }),

  system: () => json<SystemInfo>("/api/system"),
  checkSelfUpdate: () => json<SystemInfo>("/api/system/update/check", { method: "POST" }),
  /** Installs the latest hlm release; hlm restarts when the stream ends. */
  applySelfUpdate: (onEvent: (e: DeployEvent) => void) =>
    stream("/api/system/update/apply", { method: "POST" }, onEvent),
  dismissSelfUpdate: () => json<void>("/api/system/update/dismiss", { method: "POST" }),

  host: () => json<HostSnapshot>("/api/host"),
  storage: () => json<StorageReport>("/api/host/storage"),
  refreshStorage: () => json<StorageReport>("/api/host/storage/refresh", { method: "POST" }),
  /** Deletes a deleted server's folder or volume from disk. */
  deleteDeletedData: (id: string, path: string, volume: boolean) =>
    json<StorageReport>(`/api/host/storage/deleted/${encodeURIComponent(id)}/delete`, {
      method: "POST",
      body: JSON.stringify({ path, volume }),
    }),
  /** Stops tracking a deleted server's folder or volume (all of them without a path); nothing is deleted. */
  forgetDeletedData: (id: string, path = "", volume = false) =>
    json<StorageReport>(`/api/host/storage/deleted/${encodeURIComponent(id)}/forget`, {
      method: "POST",
      body: JSON.stringify({ path, volume }),
    }),

  /** Installs and updates in progress, plus those that finished in the last minute. */
  jobs: () => json<Job[]>("/api/jobs"),

  updateInfo: () => json<UpdateCheckInfo>("/api/updates"),
  checkUpdates: () => json<UpdateCheckInfo>("/api/updates/check", { method: "POST" }),
  /** Updates every server that has an update available. */
  updateAll: (onEvent: (e: DeployEvent) => void) => stream("/api/updates/apply", { method: "POST" }, onEvent),

  update: (id: string, onEvent: (e: DeployEvent) => void) =>
    stream(`/api/servers/${encodeURIComponent(id)}/update`, { method: "POST" }, onEvent),

  /** URL for an EventSource of log lines. EventSource can't send headers, so the token goes in the query. */
  logsUrl: (id: string, tail = 500) => withToken(`/api/servers/${encodeURIComponent(id)}/logs`, tail),
  /** URL for an EventSource of cloudflared's log lines. */
  tunnelLogsUrl: (tail = 500) => withToken("/api/tunnel/logs", tail),
  connectionLogsUrl: (id: string, tail = 500) => withToken(`/api/connections/${encodeURIComponent(id)}/logs`, tail),
};

/** Adds a backup's key and, since a plain link can't send headers, the token to a download URL. */
function withKey(path: string, key: string): string {
  const qs = new URLSearchParams({ key });
  const token = getToken();
  if (token) qs.set("token", token);
  return `${path}?${qs}`;
}

/** Adds the tail length and, since EventSource can't send headers, the token to a log URL. */
function withToken(path: string, tail: number): string {
  const qs = new URLSearchParams({ tail: String(tail) });
  const token = getToken();
  if (token) qs.set("token", token);
  return `${path}?${qs}`;
}
