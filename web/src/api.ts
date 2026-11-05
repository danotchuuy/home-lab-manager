import type {
  Account,
  AuditFilter,
  AuditPage,
  AuditSettings,
  RegistryForm,
  HistoryRange,
  MetricHistory,
  MetricsSettings,
  Stack,
  StackPlan,
  TwoFactorSetup,
  BackupTarget,
  BackupVerification,
  ServerExport,
  TransferPreview,
  CopyData,
  HealthCheck,
  HealthSettings,
  ServerHealth,
  RegistryLogin,
  AutoUpdates,
  APIKeyInfo,
  KeyRole,
  HTTPSStatus,
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
  ConfigFile,
  ConfigFiles,
  ConsoleInput,
  ConsoleResult,
  ConsoleStatus,
  ConfigBackupOverview,
  ConfigBackupPlan,
  DeployEvent,
  DeployRequest,
  FileContent,
  FileListing,
  FileMatch,
  GameStatus,
  GameSummary,
  QueryResult,
  QuerySettings,
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
  AgentEnrolment,
  DockerHost,
  DockerHostInput,
  DockerVersion,
  HostInspection,
  AddressSuggestion,
  StatusPageSettings,
  StatusSite,
  ProxyStatusSites,
  CleanupPlan,
  CleanupRequest,
  CleanupResult,
  CleanupSettings,
  NetworkRequest,
  UpdateCheckInfo,
  Role,
  NotifyRecord,
  NotifySettings,
  NotifyTarget,
  NotifyTargetInput,
  ScheduledTask,
  Server,
  ServerGroup,
  ServerTasks,
  ServerUpdatePolicy,
  SystemUpdatePolicy,
  UpdatePolicy,
  SelfUpdatePolicy,
  TimezoneInfo,
  UpcomingTask,
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

/**
 * Asks the person to confirm it's them, for changes that need it; resolves
 * true once they have, so the request can be sent again. Set by the app.
 */
let confirmer: ((method: string, twoFactor: boolean) => Promise<boolean>) | undefined;

export function setConfirmer(f: typeof confirmer) {
  confirmer = f;
}

async function request(path: string, init: RequestInit = {}, confirmed = false): Promise<Response> {
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
    if (res.status === 403 && body?.code === "confirm_required" && confirmer && !confirmed) {
      if (await confirmer(String(body.method ?? "local"), body.twoFactor === true)) return request(path, init, true);
    }
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

/**
 * Follows a stream of server-sent events with fetch rather than EventSource,
 * which can't send the Authorization header. Resolves when the stream ends;
 * ended is true if the server said so with an "end" event.
 */
export async function followEvents(
  path: string,
  on: { open: () => void; message: (data: string) => void },
  signal: AbortSignal,
): Promise<{ ended: boolean }> {
  const res = await request(path, { signal, headers: { Accept: "text/event-stream" } });
  if (!res.body) throw new Error("streaming is not supported by this browser");
  on.open();
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return { ended: false };
    buf += value;
    let end: number;
    while ((end = buf.indexOf("\n\n")) >= 0) {
      const block = buf.slice(0, end);
      buf = buf.slice(end + 2);
      let event = "message";
      const data: string[] = [];
      for (const line of block.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ")) data.push(line.slice(6));
        else if (line === "data:") data.push("");
      }
      if (event === "end") return { ended: true };
      if (data.length > 0) on.message(data.join("\n"));
    }
  }
}

/**
 * Starts a download. A session cookie goes with a plain link; a token can't,
 * so it gets a link that works once instead, rather than putting the token in
 * the URL.
 */
export async function download(path: string) {
  if (!getToken()) {
    window.location.href = path;
    return;
  }
  const { url } = await json<{ url: string }>("/api/auth/download-ticket", { method: "POST", body: JSON.stringify({ url: path }) });
  window.location.href = url;
}

export interface StatusPageSettingsView {
  settings: StatusPageSettings;
  sites: StatusSite[];
  hasViewPassword: boolean;
  hasDetailsPassword: boolean;
}

export type BulkAction = "start" | "stop" | "restart" | "update" | "backup";

/** ?host= for requests about a Docker host other than the local one. */
function hostQuery(host?: string): string {
  return host && host !== "local" ? `?host=${encodeURIComponent(host)}` : "";
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
  /** A template, with its paths for the host it's going on. */
  app: (id: string, host?: string) =>
    json<App>(`/api/catalog/apps/${encodeURIComponent(id)}${hostQuery(host)}`),
  /** Pulls an image and makes a template from its defaults; the "done" event's message is the template ID. */
  importImage: (image: string, onEvent: (e: DeployEvent) => void) =>
    stream("/api/catalog/import", { method: "POST", body: JSON.stringify({ image }) }, onEvent),
  deleteTemplate: (id: string) => json<void>(`/api/catalog/apps/${encodeURIComponent(id)}`, { method: "DELETE" }),
  refreshCatalog: () => json<{ apps: number }>("/api/catalog/refresh", { method: "POST" }),

  networks: (host?: string) => json<NetworkInfo[]>(`/api/networks${hostQuery(host)}`),
  createNetwork: (req: NetworkRequest, host?: string) =>
    json<NetworkInfo>(`/api/networks${hostQuery(host)}`, { method: "POST", body: JSON.stringify(req) }),
  removeNetwork: (name: string, host?: string) =>
    json<void>(`/api/networks/${encodeURIComponent(name)}${hostQuery(host)}`, { method: "DELETE" }),
  hostInterfaces: () => json<HostInterface[]>("/api/networks/interfaces"),

  servers: () => json<Server[]>("/api/servers"),
  server: (id: string) => json<Server>(`/api/servers/${encodeURIComponent(id)}`),
  serverConfig: (id: string) =>
    json<{ app: App; request: DeployRequest; hidden?: string[] }>(`/api/servers/${encodeURIComponent(id)}/config`),
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
  restoreBackup: (id: string, key: string, backupFirst: boolean, target = "") =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/backups/restore`, {
      method: "POST",
      body: JSON.stringify({ key, backupFirst, target }),
    }),
  deleteBackup: (id: string, key: string, target = "") =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/backups?${new URLSearchParams({ key, target })}`, {
      method: "DELETE",
    }),
  /** Reads a backup back and checks it. */
  verifyBackup: (id: string, key: string, target = "") =>
    json<BackupVerification>(`/api/servers/${encodeURIComponent(id)}/backups/verify?${new URLSearchParams({ key, target })}`, {
      method: "POST",
    }),
  /** URL to download a backup from. A plain link can't send headers, so the token goes in the query. */
  backupDownloadUrl: (id: string, key: string, target = "") =>
    withKey(`/api/servers/${encodeURIComponent(id)}/backups/download`, key) +
    (target ? `&${new URLSearchParams({ target })}` : ""),
  backupTargets: () => json<BackupTarget[]>("/api/integrations/backup/targets"),
  /** Adds a backup storage (without an id) or changes one; a blank secret key keeps the saved one. */
  saveBackupTarget: (t: Partial<BackupTarget> & { secretKey?: string }) =>
    json<BackupTarget>(`/api/integrations/backup/targets${t.id ? `/${encodeURIComponent(t.id)}` : ""}`, {
      method: t.id ? "PUT" : "POST",
      body: JSON.stringify(t),
    }),
  deleteBackupTarget: (id: string) =>
    json<void>(`/api/integrations/backup/targets/${encodeURIComponent(id)}`, { method: "DELETE" }),
  backupTargetKey: (id: string) => json<{ key: string }>(`/api/integrations/backup/targets/${encodeURIComponent(id)}/key`),
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

  /** A server's settings as a file; with secrets, its passwords too (a manager, after confirming it's them). */
  exportServer: (id: string, secrets: boolean) =>
    json<ServerExport>(`/api/servers/${encodeURIComponent(id)}/export${secrets ? "?secrets=1" : ""}`),
  previewImport: (exp: ServerExport, host?: string) =>
    json<TransferPreview>(`/api/servers/import/preview${hostQuery(host)}`, { method: "POST", body: JSON.stringify(exp) }),
  importServer: (exp: ServerExport, request: DeployRequest, onEvent: (e: DeployEvent) => void, host?: string) =>
    stream("/api/servers/import", { method: "POST", body: JSON.stringify({ export: exp, request, host }) }, onEvent),
  previewClone: (id: string, name: string, host?: string) =>
    json<TransferPreview>(`/api/servers/${encodeURIComponent(id)}/clone/preview`, {
      method: "POST",
      body: JSON.stringify({ name, host }),
    }),
  cloneServer: (
    id: string,
    opts: {
      name: string;
      host?: string;
      request?: DeployRequest;
      copyData: CopyData;
      backupKey?: string;
      stopSource: boolean;
      start: boolean;
    },
    onEvent: (e: DeployEvent) => void,
  ) => stream(`/api/servers/${encodeURIComponent(id)}/clone`, { method: "POST", body: JSON.stringify(opts) }, onEvent),
  stacks: () => json<Stack[]>("/api/stacks"),
  /** A stack, with its Compose file and .env for admins. */
  stack: (name: string) => json<{ stack: Stack; yaml?: string; env?: string }>(`/api/stacks/${encodeURIComponent(name)}`),
  previewStack: (name: string, yaml: string, env: string, existing: boolean, host = "local") =>
    json<StackPlan>("/api/stacks/preview", {
      method: "POST",
      body: JSON.stringify({ name, yaml, env, existing, host }),
    }),
  createStack: (name: string, yaml: string, env: string, onEvent: (e: DeployEvent) => void, host = "local") =>
    stream("/api/stacks", { method: "POST", body: JSON.stringify({ name, yaml, env, host }) }, onEvent),
  updateStack: (name: string, yaml: string, env: string, onEvent: (e: DeployEvent) => void) =>
    stream(`/api/stacks/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify({ yaml, env }) }, onEvent),
  stackAction: (name: string, action: "start" | "stop" | "restart" | "update", onEvent: (e: DeployEvent) => void) =>
    stream(`/api/stacks/${encodeURIComponent(name)}/${action}`, { method: "POST" }, onEvent),
  deleteStack: (name: string) => json<void>(`/api/stacks/${encodeURIComponent(name)}`, { method: "DELETE" }),
  /** Installs a server on a Docker host (the local one if none). */
  install: (req: DeployRequest, onEvent: (e: DeployEvent) => void, host?: string) =>
    stream("/api/servers", { method: "POST", body: JSON.stringify({ ...req, host }) }, onEvent),
  /** Host ports the settings would use that are already in use; `server` is the one being edited, `host` where a new one goes. */
  checkPorts: (request: DeployRequest, server?: string, host?: string) =>
    json<{ conflicts: PortConflict[]; error?: string }>("/api/servers/check-ports", {
      method: "POST",
      body: JSON.stringify({ request, server, host }),
    }),
  setTags: (id: string, tags: string[]) =>
    json<{ tags: string[] }>(`/api/servers/${encodeURIComponent(id)}/tags`, { method: "PUT", body: JSON.stringify({ tags }) }),
  setFavourite: (id: string, favourite: boolean) =>
    json<{ favourites: string[] }>(`/api/servers/${encodeURIComponent(id)}/favourite`, {
      method: "PUT",
      body: JSON.stringify({ favourite }),
    }),
  /** Runs one action on several servers, streaming each one's outcome (target: the server). */
  bulk: (servers: string[], action: BulkAction, onEvent: (e: DeployEvent) => void) =>
    stream("/api/servers/bulk", { method: "POST", body: JSON.stringify({ servers, action }) }, onEvent),
  /** Docker hosts; admins also get how each is reached, and hlm's SSH key. */
  hosts: () => json<{ hosts: DockerHost[]; sshKey?: SSHKeyInfo }>("/api/hosts"),
  testHost: (h: DockerHostInput) =>
    json<DockerVersion>("/api/hosts/test", { method: "POST", body: JSON.stringify(h) }),
  createHost: (h: DockerHostInput) => json<DockerHost>("/api/hosts", { method: "POST", body: JSON.stringify(h) }),
  updateHost: (h: DockerHostInput) =>
    json<DockerHost>(`/api/hosts/${encodeURIComponent(h.id)}`, { method: "PUT", body: JSON.stringify(h) }),
  deleteHost: (id: string) => json<void>(`/api/hosts/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** A single-use token for a new host's agent (confirmed admins). */
  agentEnrolment: (h: DockerHostInput) =>
    json<AgentEnrolment>("/api/hosts/agent-enrolment", { method: "POST", body: JSON.stringify(h) }),
  checkHost: (id: string) => json<DockerVersion>(`/api/hosts/${encodeURIComponent(id)}/check`, { method: "POST" }),
  /** Signs in to an SSH host: is Docker there, and how would hlm set it up? */
  inspectHost: (h: DockerHostInput) =>
    json<HostInspection>("/api/hosts/inspect", { method: "POST", body: JSON.stringify(h) }),
  /** Installs Docker CE on an SSH host (or starts it, or lets the user use it); streams the output. */
  setupDocker: (h: DockerHostInput, onEvent: (e: DeployEvent) => void) =>
    stream("/api/hosts/setup-docker", { method: "POST", body: JSON.stringify(h) }, onEvent),
  /** Makes certificates and sets Docker's API up with TLS on a host over SSH, then saves the host; streams the output. */
  setupTLS: (h: DockerHostInput & { port: number }, onEvent: (e: DeployEvent) => void) =>
    stream("/api/hosts/setup-tls", { method: "POST", body: JSON.stringify(h) }, onEvent),
  /** What disk cleanup can delete on a Docker host; nothing is deleted. */
  cleanupCandidates: (host?: string) => json<CleanupPlan>(`/api/host/cleanup${hostQuery(host)}`),
  cleanup: (req: CleanupRequest, host?: string) =>
    json<CleanupResult>(`/api/host/cleanup${hostQuery(host)}`, { method: "POST", body: JSON.stringify(req) }),
  cleanupSettings: () => json<CleanupSettings>("/api/system/cleanup"),
  saveCleanupSettings: (s: Pick<CleanupSettings, "keepOldImages" | "weekly">) =>
    json<CleanupSettings>("/api/system/cleanup", { method: "PUT", body: JSON.stringify(s) }),
  statusPageSettings: () => json<StatusPageSettingsView>("/api/system/status-page"),
  /** viewPassword / detailsPassword: a new password, "" to remove it, or left out to keep it. */
  saveStatusPage: (settings: StatusPageSettings & { viewPassword?: string; detailsPassword?: string; sites: StatusSite[] }) =>
    json<StatusPageSettingsView>("/api/system/status-page", { method: "PUT", body: JSON.stringify(settings) }),
  statusPageSuggest: (server: string) =>
    json<AddressSuggestion[]>(`/api/system/status-page/suggest?server=${encodeURIComponent(server)}`),
  /** Makes hlm's SSH key, if there isn't one. */
  hostsSSHKey: () => json<SSHKeyInfo>("/api/hosts/ssh-key", { method: "POST" }),
  reconfigure: (id: string, req: DeployRequest, onEvent: (e: DeployEvent) => void) =>
    stream(`/api/servers/${encodeURIComponent(id)}/config`, { method: "PUT", body: JSON.stringify(req) }, onEvent),
  setAutostart: (id: string, enabled: boolean) =>
    json<Server>(`/api/servers/${encodeURIComponent(id)}/autostart`, {
      method: "PUT",
      body: JSON.stringify({ enabled }),
    }),

  authStatus: () => json<AuthStatus>("/api/auth/status"),
  /** Signs in with a local account, starting a session. */
  /** Signs in, or says a second factor (or setting one up) comes next. */
  login: (username: string, password: string) =>
    json<AuthUser & { twoFactor?: boolean; twoFactorSetup?: boolean }>("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),
  loginCode: (code: string) => json<AuthUser>("/api/auth/login/2fa", { method: "POST", body: JSON.stringify({ code }) }),
  loginSetup: () => json<TwoFactorSetup>("/api/auth/login/2fa/setup", { method: "POST" }),
  loginEnable: (code: string) =>
    json<{ recoveryCodes: string[] }>("/api/auth/login/2fa/enable", { method: "POST", body: JSON.stringify({ code }) }),
  beginTwoFactor: (username: string) =>
    json<TwoFactorSetup>(`/api/auth/accounts/${encodeURIComponent(username)}/2fa/setup`, { method: "POST" }),
  enableTwoFactor: (username: string, code: string) =>
    json<{ recoveryCodes: string[] }>(`/api/auth/accounts/${encodeURIComponent(username)}/2fa/enable`, {
      method: "POST",
      body: JSON.stringify({ code }),
    }),
  newRecoveryCodes: (username: string) =>
    json<{ recoveryCodes: string[] }>(`/api/auth/accounts/${encodeURIComponent(username)}/2fa/recovery`, { method: "POST" }),
  /** Turns off two-factor: your own, or (an admin) resets someone's. */
  disableTwoFactor: (username: string) =>
    json<void>(`/api/auth/accounts/${encodeURIComponent(username)}/2fa`, { method: "DELETE" }),
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
  addSSHKey: (username: string, publicKey: string, name: string, allowFrom: string[]) =>
    json<SSHKey>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys`, {
      method: "POST",
      body: JSON.stringify({ publicKey, name, allowFrom }),
    }),
  setSSHKeyAllowFrom: (username: string, id: string, allowFrom: string[]) =>
    json<SSHKey>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ allowFrom }),
    }),
  deleteSSHKey: (username: string, id: string) =>
    json<void>(`/api/auth/accounts/${encodeURIComponent(username)}/ssh-keys/${encodeURIComponent(id)}`, { method: "DELETE" }),
  deleteAccount: (username: string) => json<void>(`/api/auth/accounts/${encodeURIComponent(username)}`, { method: "DELETE" }),
  apiKeys: () => json<APIKeyInfo[]>("/api/auth/keys"),
  /** Makes an API key; the response's key is the only time its secret is shown. */
  createAPIKey: (name: string, days: number, role: KeyRole, allowFrom: string[]) =>
    json<APIKeyInfo & { key: string }>("/api/auth/keys", { method: "POST", body: JSON.stringify({ name, days, role, allowFrom }) }),
  setAPIKeyAllowFrom: (id: string, allowFrom: string[]) =>
    json<APIKeyInfo>(`/api/auth/keys/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify({ allowFrom }) }),
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
    json<{
      configured: boolean;
      settings: ProxySettings;
      key: SSHKeyInfo | null;
      upstream: UpstreamState;
      statusSites?: ProxyStatusSites | null;
    }>("/api/proxy"),
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
    json<{ config: string; output: string; servers: ProxiedServer[]; firewall?: FirewallSync; status?: ProxyStatusSites }>("/api/proxy/apply", {
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

  /** A host's CPU, memory, network and disks: this host's, or another's (with hlm's agent). */
  host: (id?: string) =>
    json<HostSnapshot>(id && id !== "local" ? `/api/host?host=${encodeURIComponent(id)}` : "/api/host"),
  /** A Docker host's disk usage per server ("local", or another host's ID). */
  storage: (host = "local") => json<StorageReport>(`/api/host/storage?host=${encodeURIComponent(host)}`),
  refreshStorage: (host = "local") =>
    json<StorageReport>(`/api/host/storage/refresh?host=${encodeURIComponent(host)}`, { method: "POST" }),
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

  console: (id: string) => json<ConsoleStatus>(`/api/servers/${encodeURIComponent(id)}/console`),
  saveConsole: (id: string, input: ConsoleInput) =>
    json<ConsoleStatus>(`/api/servers/${encodeURIComponent(id)}/console`, { method: "PUT", body: JSON.stringify(input) }),
  /** Checks console settings before they're saved; resolves to a description of what worked. */
  testConsole: (id: string, input: ConsoleInput) =>
    json<{ message: string }>(`/api/servers/${encodeURIComponent(id)}/console/test`, {
      method: "POST",
      body: JSON.stringify(input),
    }),
  consoleCommand: (id: string, command: string) =>
    json<ConsoleResult>(`/api/servers/${encodeURIComponent(id)}/console/command`, {
      method: "POST",
      body: JSON.stringify({ command }),
    }),

  tasks: (id: string) => json<ServerTasks>(`/api/servers/${encodeURIComponent(id)}/tasks`),
  /** Adds a task, or changes the one with t.id. */
  saveTask: (id: string, t: ScheduledTask) =>
    t.id
      ? json<ScheduledTask>(`/api/servers/${encodeURIComponent(id)}/tasks/${encodeURIComponent(t.id)}`, {
          method: "PUT",
          body: JSON.stringify(t),
        })
      : json<ScheduledTask>(`/api/servers/${encodeURIComponent(id)}/tasks`, { method: "POST", body: JSON.stringify(t) }),
  deleteTask: (id: string, task: string) =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/tasks/${encodeURIComponent(task)}`, { method: "DELETE" }),
  runTask: (id: string, task: string) =>
    json<void>(`/api/servers/${encodeURIComponent(id)}/tasks/${encodeURIComponent(task)}/run`, { method: "POST" }),
  configFiles: (id: string) => json<ConfigFiles>(`/api/servers/${encodeURIComponent(id)}/config-files`),
  setConfigFiles: (id: string, pinned: ConfigFile[]) =>
    json<ConfigFiles>(`/api/servers/${encodeURIComponent(id)}/config-files`, { method: "PUT", body: JSON.stringify({ pinned }) }),
  audit: (f: AuditFilter) => json<AuditPage>(`/api/audit?${auditQuery(f)}`),
  /** Downloads the matching entries as a file. */
  exportAudit: async (f: AuditFilter, format: "csv" | "jsonl") => {
    const res = await request(`/api/audit/export?${auditQuery({ ...f, limit: undefined })}&format=${format}`);
    const name = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "")?.[1] ?? `hlm-audit.${format}`;
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
  },
  serverActivity: (id: string, before?: string) =>
    json<AuditPage>(`/api/servers/${encodeURIComponent(id)}/activity${before ? `?before=${encodeURIComponent(before)}` : ""}`),
  myActivity: () => json<AuditPage>("/api/auth/activity"),
  auditSettings: () => json<AuditSettings>("/api/system/audit"),
  setAuditSettings: (retentionMonths: number, mirror: boolean) =>
    json<AuditSettings>("/api/system/audit", { method: "PUT", body: JSON.stringify({ retentionMonths, mirror }) }),
  hostHistory: (range: HistoryRange, host?: string) =>
    json<MetricHistory>(
      `/api/host/history?range=${range}${host && host !== "local" ? `&host=${encodeURIComponent(host)}` : ""}`,
    ),
  serverHistory: (id: string, range: HistoryRange) =>
    json<MetricHistory>(`/api/servers/${encodeURIComponent(id)}/history?range=${range}`),
  metricsSettings: () => json<MetricsSettings>("/api/system/metrics"),
  setMetricsSettings: (s: MetricsSettings) => json<MetricsSettings>("/api/system/metrics", { method: "PUT", body: JSON.stringify(s) }),
  registries: () => json<RegistryLogin[]>("/api/registries"),
  addRegistry: (f: RegistryForm) => json<RegistryLogin>("/api/registries", { method: "POST", body: JSON.stringify(f) }),
  updateRegistry: (host: string, f: RegistryForm) =>
    json<RegistryLogin>(`/api/registries/${encodeURIComponent(host)}`, { method: "PUT", body: JSON.stringify(f) }),
  deleteRegistry: (host: string) => json<void>(`/api/registries/${encodeURIComponent(host)}`, { method: "DELETE" }),
  testRegistry: (f: RegistryForm) =>
    json<{ status: string; digest?: string }>("/api/registries/test", { method: "POST", body: JSON.stringify(f) }),
  gameStatuses: () => json<Record<string, GameSummary>>("/api/query"),
  gameStatus: (id: string) => json<GameStatus>(`/api/servers/${encodeURIComponent(id)}/query`),
  setQuerySettings: (id: string, set: QuerySettings) =>
    json<GameStatus>(`/api/servers/${encodeURIComponent(id)}/query/settings`, { method: "PUT", body: JSON.stringify(set) }),
  /** Queries with these settings without saving them; automatic settings say what was found. */
  testQuery: (id: string, set: QuerySettings) =>
    json<{ settings: QuerySettings; result: QueryResult }>(`/api/servers/${encodeURIComponent(id)}/query/test`, {
      method: "POST",
      body: JSON.stringify(set),
    }),
  serverHealth: (id: string) => json<ServerHealth>(`/api/servers/${encodeURIComponent(id)}/health`),
  setServerHealth: (id: string, set: HealthSettings) =>
    json<ServerHealth>(`/api/servers/${encodeURIComponent(id)}/health`, { method: "PUT", body: JSON.stringify(set) }),
  testServerHealth: (id: string, check?: HealthCheck) =>
    json<{ healthy: boolean; exitCode: number; output: string }>(`/api/servers/${encodeURIComponent(id)}/health/test`, {
      method: "POST",
      body: JSON.stringify({ check }),
    }),
  updatePolicy: (id: string) => json<ServerUpdatePolicy>(`/api/servers/${encodeURIComponent(id)}/update-policy`),
  setUpdatePolicy: (id: string, p: UpdatePolicy) =>
    json<ServerUpdatePolicy>(`/api/servers/${encodeURIComponent(id)}/update-policy`, { method: "PUT", body: JSON.stringify(p) }),
  autoUpdates: () => json<AutoUpdates>("/api/updates/auto"),
  systemUpdatePolicy: () => json<SystemUpdatePolicy>("/api/system/update-policy"),
  setSystemUpdatePolicy: (g: { default: UpdatePolicy; checkHours: number; self: SelfUpdatePolicy }) =>
    json<SystemUpdatePolicy>("/api/system/update-policy", { method: "PUT", body: JSON.stringify(g) }),
  upcomingTasks: () => json<UpcomingTask[]>("/api/schedule/upcoming"),
  timezone: () => json<TimezoneInfo>("/api/system/timezone"),
  setTimezone: (timezone: string) =>
    json<TimezoneInfo>("/api/system/timezone", { method: "PUT", body: JSON.stringify({ timezone }) }),

  /** Path of a stream of log lines, for followEvents. */
  logsUrl: (id: string, tail = 500) => withTail(`/api/servers/${encodeURIComponent(id)}/logs`, tail),
  /** Path of a stream of cloudflared's log lines. */
  tunnelLogsUrl: (tail = 500) => withTail("/api/tunnel/logs", tail),
  connectionLogsUrl: (id: string, tail = 500) => withTail(`/api/connections/${encodeURIComponent(id)}/logs`, tail),

  httpsStatus: () => json<HTTPSStatus>("/api/system/https"),
  saveHTTPS: (s: { enabled: boolean; domain: string; email: string; staging: boolean }) =>
    json<HTTPSStatus>("/api/system/https", { method: "PUT", body: JSON.stringify(s) }),
  renewHTTPS: () => json<HTTPSStatus>("/api/system/https/renew", { method: "POST" }),

  notifications: () => json<NotifySettings>("/api/notifications"),
  saveNotificationSettings: (baseUrl: string, diskPercent: number) =>
    json<NotifySettings>("/api/notifications", { method: "PUT", body: JSON.stringify({ baseUrl, diskPercent }) }),
  notificationHistory: () => json<NotifyRecord[]>("/api/notifications/history"),
  addNotificationTarget: (t: NotifyTargetInput) =>
    json<NotifyTarget>("/api/notifications/targets", { method: "POST", body: JSON.stringify(t) }),
  updateNotificationTarget: (id: string, t: NotifyTargetInput) =>
    json<NotifyTarget>(`/api/notifications/targets/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(t) }),
  deleteNotificationTarget: (id: string) =>
    json<void>(`/api/notifications/targets/${encodeURIComponent(id)}`, { method: "DELETE" }),
  /** Sends a test message to a target as entered; blank secrets come from the saved target with its id. */
  testNotificationTarget: (t: NotifyTargetInput) =>
    json<void>("/api/notifications/test", { method: "POST", body: JSON.stringify(t) }),

  /** Confirms it's the signed-in person, for changes that could give lasting access. */
  confirm: (password: string, code?: string) =>
    json<{ confirmedUntil: string }>("/api/auth/confirm", { method: "POST", body: JSON.stringify({ password, code }) }),
};

function auditQuery(f: AuditFilter): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(f)) if (v !== undefined && v !== "") q.set(k, String(v));
  return q.toString();
}

/** Adds a backup's key to a download path, for download. */
function withKey(path: string, key: string): string {
  return `${path}?${new URLSearchParams({ key })}`;
}

function withTail(path: string, tail: number): string {
  return `${path}?${new URLSearchParams({ tail: String(tail) })}`;
}
