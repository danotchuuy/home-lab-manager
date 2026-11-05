import { useEffect, useState } from "react";
import { api } from "../api";
import type {
  AddressSuggestion,
  Server,
  ServerGroup,
  StatusPageEntry,
  StatusPageSettings as Settings,
  StatusSite,
} from "../types";
import { bareName } from "../util";
import { Spinner } from "./common";
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
  const [groups, setGroups] = useState<ServerGroup[]>([]);

  useEffect(() => {
    api.statusPageSettings().then(
      (r) => {
        setSet(r.settings);
        setSaved(r.settings);
        setHas({ view: r.hasViewPassword, details: r.hasDetailsPassword });
        setSites(r.sites);
      },
      (e) => setError((e as Error).message),
    );
    api.servers().then(setServers, () => {});
    api.groups().then(setGroups, () => {});
  }, []);

  if (!set) return error ? <div className="alert error small">{error}</div> : <Spinner />;

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
      setSites(r.sites);
      setViewPassword(undefined);
      setDetailsPassword(undefined);
      setSaves((n) => n + 1);
      setDone(true);
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
      {sites.map((site, i) => (
        <SiteFields
          key={(site.id ?? "new") + i + saves}
          site={site}
          groups={groups}
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
  onChange,
  onRemove,
}: {
  site: StatusSite;
  groups: ServerGroup[];
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
          <div className="status-site-groups">
            {groups.map((g) => (
              <label key={g.id} className="radio small">
                <input
                  type="checkbox"
                  checked={site.groups.includes(g.id)}
                  onChange={(e) =>
                    onChange({
                      groups: e.target.checked ? [...site.groups, g.id] : site.groups.filter((x) => x !== g.id),
                    })
                  }
                />
                {g.name}
              </label>
            ))}
          </div>
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
      <div className="btn-row">
        <button type="button" className="btn small ghost danger" onClick={onRemove}>
          Remove this site
        </button>
      </div>
    </fieldset>
  );
}
