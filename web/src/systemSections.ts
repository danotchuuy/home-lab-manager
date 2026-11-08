/**
 * The System page's tabs and sections: which tab each section is on, its
 * title and words to find it by. The page, its "Find a setting" box, links
 * like /system#linode and the command palette all go by this list.
 */

export const SYSTEM_TABS = [
  { id: "overview", label: "Overview" },
  { id: "access", label: "Access" },
  { id: "backups", label: "Backups" },
  { id: "games", label: "Games" },
  { id: "sharing", label: "Sharing" },
  { id: "network", label: "Network" },
  { id: "monitoring", label: "Monitoring" },
] as const;

export type SystemTab = (typeof SYSTEM_TABS)[number]["id"];

export interface SystemSection {
  id: string;
  tab: SystemTab;
  title: string;
  keywords: string;
}

export const SYSTEM_SECTIONS: SystemSection[] = [
  { id: "version", tab: "overview", title: "Home Lab Manager", keywords: "version update upgrade release latest check" },
  { id: "release-notes", tab: "overview", title: "What's new", keywords: "release notes changelog" },
  { id: "updates", tab: "overview", title: "Automatic updates", keywords: "auto update window servers images schedule" },
  { id: "time", tab: "overview", title: "Time zone and schedule", keywords: "timezone clock tz" },
  { id: "sign-in", tab: "access", title: "Sign-in", keywords: "accounts users password two-factor 2fa passkey roles login ssh keys" },
  { id: "sso", tab: "access", title: "Single sign-on (Authelia, OpenID Connect)", keywords: "sso oidc authelia authentik keycloak" },
  { id: "https", tab: "access", title: "HTTPS", keywords: "tls certificate lets encrypt domain ssl" },
  { id: "api-keys", tab: "access", title: "API keys", keywords: "token automation scripts" },
  { id: "server-groups", tab: "access", title: "Server groups", keywords: "permissions roles access groups" },
  { id: "backup", tab: "backups", title: "Backup storage", keywords: "s3 seaweedfs minio bucket targets" },
  { id: "config-backup", tab: "backups", title: "Backups of hlm's configuration", keywords: "config restore export settings" },
  { id: "game-profiles", tab: "games", title: "Game profiles", keywords: "query console rcon launch options profiles" },
  { id: "registries", tab: "games", title: "Registries", keywords: "docker hub ghcr login images pull credentials" },
  { id: "status-page", tab: "sharing", title: "Status page", keywords: "public sites badges domains proxy cloudflare" },
  { id: "discord", tab: "sharing", title: "Discord", keywords: "bot commands slash" },
  { id: "unifi", tab: "network", title: "UniFi port forwarding", keywords: "router gateway ports forward ubiquiti" },
  { id: "linode", tab: "network", title: "Linode firewall", keywords: "firewall vps proxy cloud" },
  { id: "notifications", tab: "monitoring", title: "Notifications", keywords: "alerts email webhook ntfy discord gotify pushover" },
  { id: "activity", tab: "monitoring", title: "Activity", keywords: "audit log history who" },
  { id: "prometheus", tab: "monitoring", title: "Prometheus", keywords: "metrics grafana scrape" },
];

/** A section by its ID (the part after # in /system#linode). */
export function sectionInfo(id: string): SystemSection | undefined {
  return SYSTEM_SECTIONS.find((s) => s.id === id);
}

/** Where a section is: its tab, and its ID for scrolling to it. */
export function sectionPath(s: SystemSection): string {
  return `/system${s.tab === "overview" ? "" : `?tab=${s.tab}`}#${s.id}`;
}

/** A section that has to be in the list (one the System page shows). */
export function systemSection(id: string): SystemSection {
  const s = sectionInfo(id);
  if (!s) throw new Error(`no System section "${id}"`);
  return s;
}
