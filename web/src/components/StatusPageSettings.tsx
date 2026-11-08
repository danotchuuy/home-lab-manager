import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { MultiSelect } from "./MultiSelect";
import { api } from "../api";
import type {
  AddressSuggestion,
  Server,
  ServerGroup,
  StatusPageEntry,
  StatusPageSettings as Settings,
  StatusSite,
  IdleStatus,
  ProxySettings,
  ProxyStatusSites,
  TunnelRoute,
  TunnelRoutes,
} from "../types";
import { bareName } from "../util";
import { Spinner } from "./common";
import { RouteBadge } from "./Hostnames";
import { StatusOnProxyFields, statusNote } from "./StatusOnProxy";
import { CopyButton } from "./ConnectInfo";

const SOURCE: Record<AddressSuggestion["source"], string> = { proxy: "proxy server", unifi: "UniFi port forward" };

/** System → Status page: a public page saying which servers are up, for people without an account. */
export function StatusPageSettings() {
  const [set, setSet] = useState<Settings>();
  const [saved, setSaved] = useState<Settings>();
  const [servers, setServers] = useState<Server[]>([]);
  const [suggestions, setSuggestions] = useState<Record<string, AddressSuggestion[]>>({});
  const [adding, setAdding] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [done, setDone] = useState(false);
  // Whether each password is set, and what a save changes: a new one, "" to remove it, or undefined to keep it.
  const [has, setHas] = useState({ view: false, details: false });
  const [viewPassword, setViewPassword] = useState<string>();
  const [detailsPassword, setDetailsPassword] = useState<string>();
  // Bumped on each save, to reset the password fields.
  const [saves, setSaves] = useState(0);
  const [sites, setSites] = useState<StatusSite[]>([]);
  // The tunnel's routes, to publish the sites' domains; undefined while
  // Cloudflare Tunnel isn't set up.
  const [tunnel, setTunnel] = useState<TunnelRoutes>();
  const [savedHosts, setSavedHosts] = useState<string[]>([]);
  const [savedSites, setSavedSites] = useState<StatusSite[]>([]);
  // Set when a site's proxy switch turns serving the page on the proxy on.
  const [proxyTurnOn, setProxyTurnOn] = useState(false);
  // The nginx proxy server, which can serve the sites too.
  const [proxy, setProxy] = useState<{
    configured: boolean;
    on: boolean;
    host: string;
    result?: ProxyStatusSites | null;
  }>();
  // The proxy's settings, to serve the page there from here too.
  const [proxyForm, setProxyForm] = useState<ProxySettings>();
  const [proxyBusy, setProxyBusy] = useState(false);
  const [proxyNote, setProxyNote] = useState<{ ok: boolean; text: string }>();
  const [groups, setGroups] = useState<ServerGroup[]>([]);

  useEffect(() => {
    api.statusPageSettings().then(
      (r) => {
        setSet(r.settings);
        setSaved(r.settings);
        setHas({ view: r.hasViewPassword, details: r.hasDetailsPassword });
        setSites(r.sites);
        setSavedSites(r.sites);
        setSavedHosts(r.sites.map((x) => x.hostname));
      },
      (e) => setError((e as Error).message),
    );
    api.servers().then(setServers, () => {});
    api.groups().then(setGroups, () => {});
    api.tunnelRoutes().then(setTunnel, () => setTunnel(undefined));
    api.proxy().then(
      (p) => {
        setProxy({
          configured: p.configured,
          on: !!p.settings.statusPage,
          host: p.settings.host,
          result: p.statusSites,
        });
        setProxyForm(p.settings);
      },
      () => setProxy(undefined),
    );
  }, []);

  if (!set) return error ? <div className="alert error small">{error}</div> : <Spinner />;

  const setProxyField = <K extends keyof ProxySettings>(k: K, v: ProxySettings[K]) =>
    setProxyForm((f) => (f ? { ...f, [k]: v } : f));

  // Saves the proxy's settings and applies them (its relays as well, as the
  // Proxy page's Save & apply does).
  async function applyOnProxy() {
    if (!proxyForm) return;
    setProxyBusy(true);
    setProxyNote(undefined);
    try {
      const saved = await api.saveProxy(proxyForm);
      setProxyForm(saved);
      const r = await api.applyProxy();
      setProxy((p) => (p ? { ...p, on: !!saved.statusPage, result: r.status ?? null } : p));
      const note = statusNote(r.status).trim();
      setProxyNote({ ok: !r.status?.error, text: note || "Applied: the status page isn't served on the proxy server." });
    } catch (e) {
      setProxyNote({ ok: false, text: (e as Error).message });
    } finally {
      setProxyBusy(false);
    }
  }

  const change = (c: Partial<Settings>) => {
    setSet({ ...set, ...c });
    setDone(false);
  };
  const changeEntry = (i: number, c: Partial<StatusPageEntry>) =>
    change({ servers: set.servers.map((e, j) => (j === i ? { ...e, ...c } : e)) });
  const move = (i: number, by: number) => {
    const list = [...set.servers];
    const [e] = list.splice(i, 1);
    list.splice(i + by, 0, e);
    change({ servers: list });
  };
  const byName = Object.fromEntries(servers.map((s) => [s.name, s]));
  const available = servers.filter((s) => !s.stack && !set.servers.some((e) => e.server === s.name));

  async function add() {
    if (!adding || !set) return;
    const srv = byName[adding];
    let address = "";
    try {
      const sug = await api.statusPageSuggest(adding);
      setSuggestions((prev) => ({ ...prev, [adding]: sug }));
      address = sug[0]?.address ?? "";
    } catch {
      // Suggestions are a convenience.
    }
    change({
      servers: [
        ...set.servers,
        {
          server: adding,
          displayName: srv?.app ?? bareName(adding),
          showPlayers: true,
          showNames: false,
          showVersion: false,
          address,
        },
      ],
    });
    setAdding("");
  }

  // Turning a site's proxy switch on also turns on serving the page on the
  // proxy, if it's off; the other sites, shown off until then, stay off.
  function setSiteProxy(i: number, on: boolean) {
    const turnOn = on && !proxy?.on && !proxyTurnOn;
    if (turnOn) setProxyTurnOn(true);
    setSites(sites.map((x, j) => (j === i ? { ...x, noProxy: !on } : turnOn ? { ...x, noProxy: true } : x)));
    setDone(false);
  }
  // Which sites' domains the proxy serves, to tell whether saving changes it.
  const proxyKey = (list: StatusSite[]) =>
    JSON.stringify(list.filter((x) => !x.noProxy).map((x) => x.hostname.trim().toLowerCase()).sort());

  async function save() {
    if (!set) return;
    setBusy(true);
    setError(undefined);
    try {
      const r = await api.saveStatusPage({
        ...set,
        allowFrom: set.allowFrom?.filter((a) => a.trim()),
        viewPassword,
        detailsPassword,
        sites,
      });
      setSet(r.settings);
      setSaved(r.settings);
      setHas({ view: r.hasViewPassword, details: r.hasDetailsPassword });
      const proxyChanged = proxyKey(r.sites) !== proxyKey(savedSites) || set.enabled !== saved?.enabled;
      setSites(r.sites);
      setSavedSites(r.sites);
      setSavedHosts(r.sites.map((x) => x.hostname));
      setViewPassword(undefined);
      setDetailsPassword(undefined);
      setSaves((n) => n + 1);
      setDone(true);
      // The proxy follows: its sites' domains as just saved.
      if (proxy?.configured && proxyForm && (proxyTurnOn || (proxy.on && proxyChanged))) {
        setProxyNote(undefined);
        try {
          if (proxyTurnOn && !proxyForm.statusPage) setProxyForm(await api.saveProxy({ ...proxyForm, statusPage: true }));
          setProxyTurnOn(false);
          const a = await api.applyProxyStatus();
          setProxy((p) => (p ? { ...p, on: true, result: a.status ?? null } : p));
          if (a.status?.error) {
            setProxyNote({ ok: false, text: `The proxy server: ${a.status.error}` });
            setError(`Saved, but the proxy server didn't take the sites: ${a.status.error}`);
          }
        } catch (e) {
          setProxyNote({ ok: false, text: `Saved, but the proxy server wasn't updated: ${(e as Error).message}` });
          setError(`Saved, but the proxy server wasn't updated: ${(e as Error).message}`);
        }
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const origin = window.location.origin;
  // Badges are shown for hlm's own address (a site's domain has the same paths, for its servers).
  const pageURL = `${origin}/status`;
  return (
    <div className="status-settings config-form">
      <p className="muted small no-margin">
        A page anyone can open, without an account, saying whether your servers are up, who&apos;s on and how to
        connect. It shows only what you choose here: no server settings, addresses inside your network or anything else
        of hlm.
      </p>
      <label className="radio">
        <input type="checkbox" checked={set.enabled} onChange={(e) => change({ enabled: e.target.checked })} />
        Show the status page at{" "}
        <a href="/status" target="_blank" rel="noreferrer">
          /status
        </a>
      </label>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Title</span>
          <input
            value={set.title ?? ""}
            onChange={(e) => change({ title: e.target.value })}
            placeholder="Game servers"
          />
        </label>
      </div>
      <label className="field">
        <span className="field-label">Message (optional)</span>
        <textarea
          rows={2}
          value={set.message ?? ""}
          onChange={(e) => change({ message: e.target.value })}
          placeholder="Maintenance on Saturday at 10:00"
        />
      </label>
      <label className="field">
        <span className="field-label">Only from (optional)</span>
        <textarea
          rows={2}
          className="code-input"
          value={(set.allowFrom ?? []).join("\n")}
          onChange={(e) => change({ allowFrom: e.target.value.split("\n") })}
          placeholder={"10.8.0.0/24"}
          spellCheck={false}
        />
        <span className="field-help">
          Addresses or ranges, one per line, such as your VPN&apos;s. Empty lets anyone see it. Behind a tunnel or
          proxy, that&apos;s the tunnel&apos;s address.
        </span>
      </label>

      <div className="field-grid">
        <PasswordField
          key={`view-${saves}`}
          label={
            sites.length
              ? "Password to see the page on hlm's own address (optional)"
              : "Password to see the page (optional)"
          }
          help="Without it, the page shows nothing. Leave it off for a page anyone can see."
          isSet={has.view}
          value={viewPassword}
          onChange={(v) => {
            setViewPassword(v);
            setDone(false);
          }}
        />
        <PasswordField
          key={`details-${saves}`}
          label={
            sites.length
              ? "Password for names and addresses on hlm's own address (optional)"
              : "Password for names and addresses (optional)"
          }
          help="Players' names, the addresses to connect to and your notes need it; the rest of the page doesn't. It also opens the page."
          isSet={has.details}
          value={detailsPassword}
          onChange={(v) => {
            setDetailsPassword(v);
            setDone(false);
          }}
        />
      </div>
      {(has.view || has.details) && (
        <p className="muted small no-margin">
          A password is asked for once per browser, for 30 days, or until it&apos;s changed. With a password to see the
          page, badges only work in a browser that has given it.
        </p>
      )}

      <h3>Sites</h3>
      <p className="muted small no-margin">
        The page under its own domain, for a group of friends or the public: only the servers in the server groups you
        choose, behind that site&apos;s own passwords (or none). A site&apos;s domain reaches only the status page: the
        rest of hlm answers &ldquo;not found&rdquo; there. Point a Cloudflare Tunnel route (or a proxy) for it at hlm.
        On any other address, such as hlm&apos;s own, the page shows every server below, with the passwords above.
      </p>
      {proxy?.result?.address && (
        <p className="small no-margin">
          On the proxy server&apos;s own address, beside the game servers:{" "}
          <a href={proxy.result.address} target="_blank" rel="noreferrer">
            {proxy.result.address} ↗
          </a>{" "}
          (every server below, with the passwords above).
          {proxy.result.addressError && <span className="form-error"> No HTTPS yet: {proxy.result.addressError}</span>}
        </p>
      )}
      {sites.map((site, i) => (
        <SiteFields
          key={(site.id ?? "new") + i + saves}
          site={site}
          groups={groups}
          cloudflare={
            <>
              <SiteCloudflare
                hostname={site.hostname}
                saved={savedHosts.includes(site.hostname)}
                tunnel={tunnel}
                onChange={setTunnel}
              />
              <SiteProxy site={site} proxy={proxy} turningOn={proxyTurnOn} onChange={(on) => setSiteProxy(i, on)} />
            </>
          }
          onChange={(c) => {
            setSites(sites.map((x, j) => (j === i ? { ...x, ...c } : x)));
            setDone(false);
          }}
          onRemove={() => {
            setSites(sites.filter((_, j) => j !== i));
            setDone(false);
          }}
        />
      ))}
      <div className="btn-row">
        <button
          type="button"
          className="btn small"
          onClick={() => {
            setSites([...sites, { hostname: "", groups: [] }]);
            setDone(false);
          }}
        >
          Add a site
        </button>
      </div>

      <h3>On the proxy server</h3>
      {!proxy?.configured || !proxyForm ? (
        <p className="muted small no-margin">
          With a <Link to="/proxy">proxy server</Link> (nginx on a VPS, say) and its WireGuard tunnel set up, the page
          and its sites can be served there too, beside the game servers, without opening hlm&apos;s port.
        </p>
      ) : (
        <fieldset className="config-section">
          <legend>Proxy server ({proxyForm.host})</legend>
          <StatusOnProxyFields form={proxyForm} set={setProxyField} where="status" />
          <div className="btn-row">
            <button type="button" className="btn small" onClick={applyOnProxy} disabled={proxyBusy}>
              {proxyBusy ? "Applying…" : "Save & apply on the proxy"}
            </button>
            <span className="muted small">
              Saves these with the <Link to="/proxy">proxy&apos;s settings</Link> and applies its configuration (its
              game relays too, as they are).
            </span>
          </div>
          {proxyNote && <div className={`alert ${proxyNote.ok ? "success" : "error"} small`}>{proxyNote.text}</div>}
        </fieldset>
      )}

      <h3>Servers</h3>
      {set.servers.length === 0 && <p className="muted small">No servers on the page yet.</p>}
      {set.servers.map((e, i) => {
        const srv = byName[e.server];
        const sug = suggestions[e.server] ?? [];
        return (
          <fieldset key={e.server} className="config-section status-entry">
            <legend>
              {bareName(e.server)}
              {!srv && <span className="muted small"> (no longer exists)</span>}
            </legend>
            <div className="field-grid">
              <label className="field">
                <span className="field-label">Shown as</span>
                <input
                  value={e.displayName ?? ""}
                  onChange={(ev) => changeEntry(i, { displayName: ev.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">Address players use</span>
                <input
                  value={e.address ?? ""}
                  onChange={(ev) => changeEntry(i, { address: ev.target.value })}
                  placeholder="play.example.com:2456"
                  spellCheck={false}
                />
                <span className="field-help">
                  {sug.length > 0 ? (
                    <>
                      Suggested:{" "}
                      {sug.map((s) => (
                        <button
                          key={s.address}
                          type="button"
                          className="link-button"
                          onClick={() => changeEntry(i, { address: s.address })}
                        >
                          {s.address} ({SOURCE[s.source]})
                        </button>
                      ))}
                    </>
                  ) : (
                    <button
                      type="button"
                      className="link-button"
                      onClick={async () => {
                        const list = await api.statusPageSuggest(e.server).catch(() => []);
                        setSuggestions((prev) => ({ ...prev, [e.server]: list }));
                        if (list.length === 0)
                          setError(`hlm has no public address for ${bareName(e.server)}: type it in.`);
                      }}
                    >
                      Suggest from the proxy or port forwards
                    </button>
                  )}
                </span>
              </label>
              <label className="field">
                <span className="field-label">Note (optional)</span>
                <input
                  value={e.hint ?? ""}
                  onChange={(ev) => changeEntry(i, { hint: ev.target.value })}
                  placeholder="Ask in Discord for the password"
                />
                <span className="field-help">Shown to everyone: never the password itself.</span>
              </label>
            </div>
            <div className="status-entry-options">
              <label className="radio small">
                <input
                  type="checkbox"
                  checked={e.showPlayers}
                  onChange={(ev) =>
                    changeEntry(i, { showPlayers: ev.target.checked, showNames: ev.target.checked && e.showNames })
                  }
                />
                Players online
              </label>
              <label className="radio small">
                <input
                  type="checkbox"
                  checked={e.showNames}
                  disabled={!e.showPlayers}
                  onChange={(ev) => changeEntry(i, { showNames: ev.target.checked })}
                />
                Their names
              </label>
              <label className="radio small">
                <input
                  type="checkbox"
                  checked={e.showVersion}
                  onChange={(ev) => changeEntry(i, { showVersion: ev.target.checked })}
                />
                Version and map
              </label>
              <WakeNote server={e.server} />
              <span className="spacer" />
              <button
                type="button"
                className="icon-button"
                aria-label={`Move ${bareName(e.server)} up`}
                disabled={i === 0}
                onClick={() => move(i, -1)}
              >
                ↑
              </button>
              <button
                type="button"
                className="icon-button"
                aria-label={`Move ${bareName(e.server)} down`}
                disabled={i === set.servers.length - 1}
                onClick={() => move(i, 1)}
              >
                ↓
              </button>
              <button
                type="button"
                className="btn small ghost danger"
                onClick={() => change({ servers: set.servers.filter((_, j) => j !== i) })}
              >
                Remove
              </button>
            </div>
            {saved?.enabled && e.slug && saved.servers.some((x) => x.slug === e.slug) && (
              <div className="status-badge-row small">
                <img src={`/api/status-page/${e.slug}.svg`} alt={`${e.displayName} status badge`} height={20} />
                <code>{`${pageURL.replace("/status", "")}/api/status-page/${e.slug}.svg`}</code>
                <CopyButton
                  text={`![${e.displayName}](${pageURL.replace("/status", "")}/api/status-page/${e.slug}.svg)`}
                  label="Copy the badge as Markdown"
                >
                  Copy Markdown
                </CopyButton>
              </div>
            )}
          </fieldset>
        );
      })}
      {available.length > 0 && (
        <div className="btn-row">
          <select aria-label="Server to add" value={adding} onChange={(e) => setAdding(e.target.value)}>
            <option value="">Add a server…</option>
            {available.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name} ({s.app})
              </option>
            ))}
          </select>
          <button type="button" className="btn small" onClick={add} disabled={!adding}>
            Add
          </button>
        </div>
      )}

      {error && <div className="alert error small">{error}</div>}
      {done && (
        <div className="alert success small">
          Saved.{" "}
          {set.enabled && (
            <a href="/status" target="_blank" rel="noreferrer">
              Open the status page ↗
            </a>
          )}
        </div>
      )}
      <div className="btn-row">
        <button className="btn primary" onClick={save} disabled={busy}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}

/**
 * A password that's kept unless it's changed or removed: value is undefined
 * to keep it, "" to remove it, or the new password.
 */
function PasswordField({
  label,
  help,
  isSet,
  value,
  onChange,
}: {
  label: string;
  help: string;
  isSet: boolean;
  value: string | undefined;
  onChange: (v: string | undefined) => void;
}) {
  const [changing, setChanging] = useState(false);
  const removing = isSet && value === "";
  const typing = !isSet || changing;
  return (
    <div className="field">
      <span className="field-label">{label}</span>
      {removing ? (
        <div className="small">
          Removed when you save.{" "}
          <button type="button" className="link-button" onClick={() => onChange(undefined)}>
            Keep it
          </button>
        </div>
      ) : typing ? (
        <>
          <input
            type="password"
            autoComplete="new-password"
            aria-label={label}
            value={value ?? ""}
            placeholder={isSet ? "New password" : "None"}
            autoFocus={changing}
            onChange={(e) => onChange(e.target.value || undefined)}
          />
          {isSet && (
            <button
              type="button"
              className="link-button small"
              onClick={() => {
                setChanging(false);
                onChange(undefined);
              }}
            >
              Keep the current one
            </button>
          )}
        </>
      ) : (
        <div className="small">
          Set.{" "}
          <button type="button" className="link-button" onClick={() => setChanging(true)}>
            Change
          </button>
          {" · "}
          <button type="button" className="link-button" onClick={() => onChange("")}>
            Remove
          </button>
        </div>
      )}
      <span className="field-help">{help}</span>
    </div>
  );
}

/** One status site: its domain, which groups' servers it shows, and its own passwords. */
function SiteFields({
  site,
  groups,
  cloudflare,
  onChange,
  onRemove,
}: {
  site: StatusSite;
  groups: ServerGroup[];
  cloudflare: ReactNode;
  onChange: (c: Partial<StatusSite>) => void;
  onRemove: () => void;
}) {
  const label = site.hostname || "New site";
  return (
    <fieldset className="config-section status-site">
      <legend>{label}</legend>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Domain</span>
          <input
            value={site.hostname}
            onChange={(e) => onChange({ hostname: e.target.value })}
            placeholder="friends.example.com"
            spellCheck={false}
            required
          />
        </label>
        <label className="field">
          <span className="field-label">Title (optional)</span>
          <input
            value={site.title ?? ""}
            onChange={(e) => onChange({ title: e.target.value })}
            placeholder="The page's"
          />
        </label>
      </div>
      <div className="field">
        <span className="field-label">Shows the servers in</span>
        {groups.length === 0 ? (
          <span className="field-help">
            There are no server groups yet (System → Server groups), so it shows every server on the page.
          </span>
        ) : (
          <MultiSelect
            label={`Server groups ${site.hostname || "the site"} shows`}
            options={groups.map((g) => ({ value: g.id, label: g.name }))}
            value={site.groups}
            onChange={(ids) => onChange({ groups: ids })}
            placeholder="Add a group…"
            empty="Every server on the page"
          />
        )}
        <span className="field-help">
          {site.groups.length === 0
            ? "No group chosen: every server on the page."
            : "Servers on the page that are in any of these groups."}
        </span>
      </div>
      <label className="field">
        <span className="field-label">Message (optional)</span>
        <input
          value={site.message ?? ""}
          onChange={(e) => onChange({ message: e.target.value })}
          placeholder="The page's"
        />
      </label>
      <div className="field-grid">
        <PasswordField
          label={`Password to see ${label}`}
          help="Optional. Only this site's passwords work here."
          isSet={!!site.hasViewPassword}
          value={site.viewPassword}
          onChange={(v) => onChange({ viewPassword: v })}
        />
        <PasswordField
          label={`Password for names and addresses on ${label}`}
          help="Optional. It also opens the site."
          isSet={!!site.hasDetailsPassword}
          value={site.detailsPassword}
          onChange={(v) => onChange({ detailsPassword: v })}
        />
      </div>
      {cloudflare}
      <div className="btn-row">
        <button type="button" className="btn small ghost danger" onClick={onRemove}>
          Remove this site
        </button>
      </div>
    </fieldset>
  );
}

/** A site's domain on Cloudflare Tunnel: whether it's routed to hlm, and a button to route it. */
function SiteCloudflare({
  hostname,
  saved,
  tunnel,
  onChange,
}: {
  hostname: string;
  saved: boolean;
  tunnel?: TunnelRoutes;
  onChange: (t: TunnelRoutes) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const host = hostname.trim().toLowerCase().replace(/\.+$/, "");
  if (!host) return null;
  if (!tunnel || !tunnel.tunnel) {
    return (
      <p className="muted small no-margin">
        To publish {host} through Cloudflare, set up <Link to="/tunnel">Cloudflare Tunnel</Link> first; or point any
        proxy for it at hlm.
      </p>
    );
  }
  const route = tunnel.routes.find((r) => r.hostname === host);

  async function publish() {
    if (!tunnel) return;
    setBusy(true);
    setError(undefined);
    try {
      // The routes as they are, plus this domain to hlm itself.
      const routes: TunnelRoute[] = tunnel.routes.map((r) => ({
        hostname: r.hostname,
        zoneId: r.zoneId,
        server: r.server,
        address: r.address,
        port: r.port,
        scheme: r.scheme,
        noTLSVerify: r.noTLSVerify,
        hlm: r.hlm,
      }));
      routes.push({ hostname: host, hlm: true, port: 0, scheme: "http" });
      await api.setTunnelRoutes(routes);
      onChange(await api.tunnelRoutes());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="small status-site-cloudflare">
      {route && route.hlm ? (
        <>
          <RouteBadge route={route} />{" "}
          {route.state === "published" ? (
            <a href={`https://${host}/status`} target="_blank" rel="noreferrer">
              https://{host}/status ↗
            </a>
          ) : (
            <Link to="/tunnel">Tunnel page</Link>
          )}
          {route.error && <div className="form-error">{route.error}</div>}
        </>
      ) : route ? (
        <div className="alert warn small">
          {host} already goes to {route.server || route.address} on the <Link to="/tunnel">Tunnel page</Link>. Change it
          to &ldquo;Home Lab Manager&rdquo; there for this site.
        </div>
      ) : (
        <>
          <button type="button" className="btn small" onClick={publish} disabled={busy || !saved}>
            {busy ? "Publishing…" : "Publish on Cloudflare"}
          </button>{" "}
          <span className="muted">
            {saved
              ? "Adds a Cloudflare Tunnel route (and its DNS record) for this domain, to hlm."
              : "Save the site first."}
          </span>
        </>
      )}
      {error && <div className="alert error small">{error}</div>}
    </div>
  );
}

/**
 * A site's domain on the nginx proxy server: whether it's served there (a
 * switch, applied when the settings are saved), and how it's going.
 */
function SiteProxy({
  site,
  proxy,
  turningOn,
  onChange,
}: {
  site: StatusSite;
  proxy?: { configured: boolean; on: boolean; host: string; result?: ProxyStatusSites | null };
  /** Serving the page on the proxy is turned on when the settings are saved. */
  turningOn: boolean;
  onChange: (on: boolean) => void;
}) {
  const host = site.hostname.trim().toLowerCase().replace(/\.+$/, "");
  if (!host || !proxy?.configured) return null;
  const on = (proxy.on || turningOn) && !site.noProxy;
  const r = proxy.result;
  let state: ReactNode = null;
  if (on && r?.error) {
    state = <span className="form-error">{r.error}</span>;
  } else if (on && (!r || !r.domains.includes(host))) {
    state = <span className="muted">served once the settings are saved; point {host}&apos;s DNS at the proxy server</span>;
  } else if (on && r && r.https.includes(host)) {
    const note = r.tls === "cloudflare" ? r.notes?.find((n) => n.startsWith(host + "'")) : undefined;
    state = (
      <>
        served over HTTPS{r.tls === "cloudflare" ? " through Cloudflare" : ""}:{" "}
        <a href={`https://${host}/status`} target="_blank" rel="noreferrer">
          https://{host}/status ↗
        </a>
        {note && <span className="warn-text"> ({note})</span>}
      </>
    );
  } else if (on && r) {
    state = (
      <>
        served over HTTP
        {r.failed?.[host] && (
          <span className="form-error">
            {" "}
            ({r.tls === "cloudflare" ? "not set up on Cloudflare" : "no certificate yet"}: {r.failed[host]})
          </span>
        )}
      </>
    );
  }
  return (
    <div className="small status-site-proxy">
      <label className="radio">
        <input type="checkbox" checked={on} onChange={(e) => onChange(e.target.checked)} />
        Serve it on the proxy server ({proxy.host})
      </label>
      {state && <span> · {state}</span>}
    </div>
  );
}

/** Whether visitors can start a server from the page while it's asleep: set on its Schedule tab. */
function WakeNote({ server }: { server: string }) {
  const [st, setSt] = useState<IdleStatus>();
  useEffect(() => {
    api.serverIdle(server).then(setSt, () => {});
  }, [server]);
  if (!st) return null;
  const tab = `/servers/${encodeURIComponent(server)}?tab=schedule`;
  return (
    <span className="small muted">
      {st.minutes > 0 && st.wakeFromStatusPage ? (
        <>
          Visitors can start it while it&apos;s asleep (<Link to={tab}>its idle settings</Link>)
        </>
      ) : (
        <>
          <Link to={tab}>Let visitors start it</Link> while it&apos;s asleep
        </>
      )}
    </span>
  );
}
