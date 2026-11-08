import { useCallback, useEffect, useMemo, useState } from "react";
import { MultiSelect } from "./MultiSelect";
import { api } from "../api";
import type {
  NotifyDelivery,
  NotifyKind,
  NotifyRecord,
  NotifySettings,
  NotifyTarget,
  NotifyTargetInput,
  Server,
  ServerGroup,
  Severity,
  TargetKind,
} from "../types";

import { Ago, Spinner } from "./common";

const KIND_LABEL: Record<TargetKind, string> = {
  discord: "Discord",
  ntfy: "ntfy",
  gotify: "Gotify",
  webhook: "Webhook",
  email: "Email",
};

const SEVERITY_LABEL: Record<Severity, string> = { info: "Info", warning: "Warning", error: "Error" };

/** The events a new target gets: every warning and error. */
const defaultEvents = (kinds: NotifyKind[]) => kinds.filter((k) => k.severity !== "info").map((k) => k.name);

function blankTarget(kinds: NotifyKind[]): NotifyTargetInput {
  return { name: "", kind: "discord", enabled: true, events: defaultEvents(kinds), url: "", topic: "", security: "starttls" };
}

/** A saved target as the form starts out: secrets blank, meaning "keep". */
function formFrom(t: NotifyTarget): NotifyTargetInput {
  return {
    id: t.id,
    name: t.name,
    kind: t.kind,
    enabled: t.enabled,
    events: [...(t.events ?? [])],
    servers: t.servers ?? [],
    groups: t.groups ?? [],
    quietStart: t.quietStart ?? "",
    quietEnd: t.quietEnd ?? "",
    url: t.url ?? "",
    topic: t.topic ?? "",
    token: "",
    headers: (t.headers ?? []).map((h) => ({ name: h.name, value: "" })),
    smtpHost: t.smtpHost ?? "",
    smtpPort: t.smtpPort,
    security: t.security ?? "starttls",
    username: t.username ?? "",
    password: "",
    from: t.from ?? "",
    to: t.to ?? [],
  };
}

function DeliveryLine({ d }: { d?: NotifyDelivery }) {
  if (!d) return <span className="muted">nothing sent yet</span>;
  if (d.ok) return <span className="notify-ok">✓ sent <Ago iso={d.at} /></span>;
  return (
    <span className="form-error" title={d.error}>
      ✗ failed <Ago iso={d.at} />: {d.error}
    </span>
  );
}

/**
 * System → Notifications: where hlm sends alerts about crashes, failed
 * backups, updates and the like, and which ones.
 */
export function NotificationSettings() {
  const [settings, setSettings] = useState<NotifySettings>();
  const [history, setHistory] = useState<NotifyRecord[]>();
  const [servers, setServers] = useState<Server[]>([]);
  const [groups, setGroups] = useState<ServerGroup[]>([]);
  const [editing, setEditing] = useState<NotifyTargetInput>();
  const [baseUrl, setBaseUrl] = useState("");
  const [diskPercent, setDiskPercent] = useState(90);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const load = useCallback(async () => {
    const [s, h] = await Promise.all([api.notifications(), api.notificationHistory()]);
    setSettings(s);
    setHistory(h);
    setBaseUrl(s.baseUrl);
    setDiskPercent(s.diskPercent);
  }, []);

  useEffect(() => {
    load().catch((e) => setError((e as Error).message));
    api.servers().then(setServers).catch(() => {});
    api.groups().then(setGroups).catch(() => {});
  }, [load]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const saveGeneral = () =>
    run("general", async () => {
      const s = await api.saveNotificationSettings(baseUrl, diskPercent);
      setSettings(s);
      setNotice("Saved.");
    });

  const save = (t: NotifyTargetInput) =>
    run("save", async () => {
      if (t.id) await api.updateNotificationTarget(t.id, t);
      else await api.addNotificationTarget(t);
      setEditing(undefined);
      setNotice(`Saved ${t.name || KIND_LABEL[t.kind]}.`);
      await load();
    });

  const test = (t: NotifyTargetInput) =>
    run("test", async () => {
      await api.testNotificationTarget(t);
      setNotice(`Sent a test message to ${t.name || KIND_LABEL[t.kind]}. Check that it arrived.`);
    });

  const remove = (t: NotifyTarget) => {
    if (!confirm(`Remove ${t.name}? It won't get any more notifications.`)) return;
    run("remove", async () => {
      await api.deleteNotificationTarget(t.id);
      await load();
    });
  };

  if (!settings) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const messages = (
    <>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}
    </>
  );

  if (editing) {
    return (
      <>
        <TargetForm
          initial={editing}
          saved={settings.targets.find((t) => t.id === editing.id)}
          kinds={settings.kinds}
          servers={servers}
          groups={groups}
          busy={busy}
          onSave={save}
          onTest={test}
          onCancel={() => {
            setEditing(undefined);
            setError(undefined);
            setNotice(undefined);
          }}
        />
        {messages}
      </>
    );
  }

  const generalChanged = baseUrl !== settings.baseUrl || diskPercent !== settings.diskPercent;

  return (
    <div className="notifications">
      <p className="muted small no-margin">
        hlm can tell you when a server crashes, a backup fails, an update is available, the disk is nearly full and more.
        Repeats of the same alert are held back for a while, so a server that keeps failing doesn&apos;t flood you.
      </p>

      {settings.targets.length > 0 ? (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Target</th>
                <th>Events</th>
                <th>Last sent</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {settings.targets.map((t) => (
                <tr key={t.id} className={t.enabled ? undefined : "muted"}>
                  <td>
                    <strong>{t.name}</strong> <span className="badge">{KIND_LABEL[t.kind]}</span>
                    {!t.enabled && <span className="badge">off</span>}
                    <div className="muted small">{targetSummary(t)}</div>
                  </td>
                  <td className="small">
                    {t.events.length} of {settings.kinds.length}
                    {t.servers?.length || t.groups?.length ? <div className="muted">some servers only</div> : null}
                    {t.quietStart && (
                      <div className="muted">
                        quiet {t.quietStart}–{t.quietEnd}
                      </div>
                    )}
                  </td>
                  <td className="small">
                    <DeliveryLine d={t.last} />
                  </td>
                  <td className="actions">
                    <button className="btn small" onClick={() => test(formFrom(t))} disabled={!!busy}>
                      Test
                    </button>
                    <button className="btn small" onClick={() => setEditing(formFrom(t))} disabled={!!busy}>
                      Edit
                    </button>
                    <button className="btn small ghost danger" onClick={() => remove(t)} disabled={!!busy}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="muted">No notification targets yet.</p>
      )}

      {messages}
      {settings.dropped ? (
        <div className="alert warn small">
          {settings.dropped} events were dropped because too many were waiting to be sent.
        </div>
      ) : null}
      <div className="btn-row">
        <button className="btn small primary" onClick={() => setEditing(blankTarget(settings.kinds))} disabled={!!busy}>
          Add a target
        </button>
      </div>

      <div className="field-grid notify-general">
        <label className="field">
          <span className="field-label">hlm&apos;s address, for links</span>
          <input
            type="url"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            placeholder={settings.defaultBaseUrl || "http://192.168.1.10:8080"}
          />
          <span className="field-help">
            {settings.defaultBaseUrl
              ? `Leave empty to use the HTTPS address, ${settings.defaultBaseUrl}.`
              : "How you open hlm in a browser. Without it, messages don't link to the page they're about."}
          </span>
        </label>
        <label className="field">
          <span className="field-label">Warn when a disk is fuller than</span>
          <span className="inline-input">
            <input
              type="number"
              min={50}
              max={99}
              value={diskPercent}
              onChange={(e) => setDiskPercent(Number(e.target.value))}
            />
            %
          </span>
          <span className="field-help">For the Docker host&apos;s filesystems, checked every 5 minutes.</span>
        </label>
      </div>
      {generalChanged && (
        <div className="btn-row">
          <button className="btn small primary" onClick={saveGeneral} disabled={!!busy}>
            {busy === "general" ? "Saving…" : "Save"}
          </button>
        </div>
      )}

      {history && <History records={history} onRefresh={() => load().catch((e) => setError((e as Error).message))} />}
    </div>
  );
}

function targetSummary(t: NotifyTarget): string {
  switch (t.kind) {
    case "discord":
    case "webhook":
      return t.urlHint ?? "";
    case "ntfy":
      return `${t.url?.replace(/\/$/, "")}/${t.topic}`;
    case "gotify":
      return t.url ?? "";
    case "email":
      return `to ${(t.to ?? []).join(", ")}`;
  }
}

interface FormProps {
  initial: NotifyTargetInput;
  saved?: NotifyTarget;
  kinds: NotifyKind[];
  servers: Server[];
  groups: ServerGroup[];
  busy?: string;
  onSave: (t: NotifyTargetInput) => void;
  onTest: (t: NotifyTargetInput) => void;
  onCancel: () => void;
}

/** Adds or edits a target. Secrets are never shown; left blank, they keep their saved values. */
function TargetForm({ initial, saved, kinds, servers, groups, busy, onSave, onTest, onCancel }: FormProps) {
  const [t, setT] = useState<NotifyTargetInput>(initial);
  const [toText, setToText] = useState((initial.to ?? []).join(", "));
  const [someServers, setSomeServers] = useState(!!(initial.servers?.length || initial.groups?.length));
  const set = <K extends keyof NotifyTargetInput>(k: K, v: NotifyTargetInput[K]) => setT((old) => ({ ...old, [k]: v }));
  // A saved target's secrets only carry over while its kind stays the same.
  const keeps = saved && saved.kind === t.kind;

  const areas = useMemo(() => {
    const out: { area: string; kinds: NotifyKind[] }[] = [];
    for (const k of kinds) {
      const a = out.find((x) => x.area === k.area);
      if (a) a.kinds.push(k);
      else out.push({ area: k.area, kinds: [k] });
    }
    return out;
  }, [kinds]);

  const toggle = (list: string[] | undefined, v: string, on: boolean) =>
    on ? [...(list ?? []), v] : (list ?? []).filter((x) => x !== v);

  const value = (): NotifyTargetInput => ({
    ...t,
    to: toText
      .split(/[,;\s]+/)
      .map((s) => s.trim())
      .filter(Boolean),
    servers: someServers ? t.servers : [],
    groups: someServers ? t.groups : [],
    smtpPort: t.smtpPort || undefined,
  });

  const secretHint = (has?: boolean) => (keeps && has ? "Saved; leave empty to keep it" : "");

  return (
    <form
      className="config-form notify-form"
      onSubmit={(e) => {
        e.preventDefault();
        onSave(value());
      }}
    >
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Kind</span>
          <select value={t.kind} onChange={(e) => set("kind", e.target.value as TargetKind)}>
            {(Object.keys(KIND_LABEL) as TargetKind[]).map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">Name</span>
          <input value={t.name} onChange={(e) => set("name", e.target.value)} placeholder={KIND_LABEL[t.kind]} maxLength={64} />
        </label>
        <label className="field checkbox-field">
          <span className="field-label">
            <input type="checkbox" checked={t.enabled} onChange={(e) => set("enabled", e.target.checked)} />
            Send notifications here
          </span>
        </label>
      </div>

      <fieldset className="config-section">
        <legend>{KIND_LABEL[t.kind]}</legend>
        <div className="field-grid">
          {(t.kind === "discord" || t.kind === "webhook") && (
            <label className="field">
              <span className="field-label">Webhook URL</span>
              <input
                type="url"
                value={t.url ?? ""}
                onChange={(e) => set("url", e.target.value)}
                placeholder={keeps && saved?.urlHint ? `${saved.urlHint} (saved; leave empty to keep it)` : "https://"}
                autoComplete="off"
                required={!keeps}
              />
              <span className="field-help">
                {t.kind === "discord"
                  ? "In Discord: channel settings → Integrations → Webhooks → New Webhook → Copy Webhook URL. It's stored encrypted and never shown again."
                  : "hlm POSTs a JSON body (kind, severity, title, message, server, link, time, fields) to it. Stored encrypted."}
              </span>
            </label>
          )}
          {(t.kind === "ntfy" || t.kind === "gotify") && (
            <label className="field">
              <span className="field-label">Server</span>
              <input
                type="url"
                value={t.url ?? ""}
                onChange={(e) => set("url", e.target.value)}
                placeholder={t.kind === "ntfy" ? "https://ntfy.sh" : "https://gotify.example.com"}
                required
              />
            </label>
          )}
          {t.kind === "ntfy" && (
            <label className="field">
              <span className="field-label">Topic</span>
              <input value={t.topic ?? ""} onChange={(e) => set("topic", e.target.value)} placeholder="homelab-alerts" required />
              <span className="field-help">On the public ntfy.sh, anyone who guesses the topic can read it; pick a long, random one.</span>
            </label>
          )}
          {(t.kind === "ntfy" || t.kind === "gotify") && (
            <label className="field">
              <span className="field-label">{t.kind === "ntfy" ? "Access token (optional)" : "Application token"}</span>
              <input
                type="password"
                value={t.token ?? ""}
                onChange={(e) => set("token", e.target.value)}
                placeholder={secretHint(saved?.hasToken)}
                autoComplete="off"
                required={t.kind === "gotify" && !(keeps && saved?.hasToken)}
              />
              <span className="field-help">Stored encrypted and never shown again.</span>
            </label>
          )}
          {t.kind === "email" && (
            <>
              <label className="field">
                <span className="field-label">Mail server</span>
                <input value={t.smtpHost ?? ""} onChange={(e) => set("smtpHost", e.target.value)} placeholder="smtp.example.com" required />
              </label>
              <label className="field">
                <span className="field-label">Security</span>
                <select value={t.security} onChange={(e) => set("security", e.target.value as NotifyTargetInput["security"])}>
                  <option value="starttls">STARTTLS (usually port 587)</option>
                  <option value="tls">TLS (usually port 465)</option>
                  <option value="none">None (only on a trusted network)</option>
                </select>
              </label>
              <label className="field">
                <span className="field-label">Port</span>
                <input
                  type="number"
                  min={1}
                  max={65535}
                  value={t.smtpPort ?? ""}
                  onChange={(e) => set("smtpPort", e.target.value ? Number(e.target.value) : undefined)}
                  placeholder={{ starttls: "587", tls: "465", none: "25" }[t.security ?? "starttls"]}
                />
              </label>
              <label className="field">
                <span className="field-label">Username (optional)</span>
                <input value={t.username ?? ""} onChange={(e) => set("username", e.target.value)} autoComplete="off" />
              </label>
              <label className="field">
                <span className="field-label">Password</span>
                <input
                  type="password"
                  value={t.password ?? ""}
                  onChange={(e) => set("password", e.target.value)}
                  placeholder={secretHint(saved?.hasPassword)}
                  autoComplete="new-password"
                  disabled={!t.username}
                />
              </label>
              <label className="field">
                <span className="field-label">From</span>
                <input value={t.from ?? ""} onChange={(e) => set("from", e.target.value)} placeholder="Home Lab Manager <hlm@example.com>" required />
              </label>
              <label className="field">
                <span className="field-label">To</span>
                <input value={toText} onChange={(e) => setToText(e.target.value)} placeholder="me@example.com, you@example.com" required />
              </label>
            </>
          )}
        </div>
        {t.kind === "webhook" && (
          <HeadersEditor
            headers={t.headers ?? []}
            saved={keeps ? saved?.headers?.map((h) => h.name) : undefined}
            onChange={(h) => set("headers", h)}
          />
        )}
      </fieldset>

      <fieldset className="config-section">
        <legend>Events</legend>
        <div className="btn-row no-margin">
          <button type="button" className="link-button" onClick={() => set("events", kinds.map((k) => k.name))}>
            All
          </button>
          <button type="button" className="link-button" onClick={() => set("events", defaultEvents(kinds))}>
            Warnings and errors
          </button>
          <button type="button" className="link-button" onClick={() => set("events", [])}>
            None
          </button>
        </div>
        <div className="notify-events">
          {areas.map(({ area, kinds }) => (
            <div key={area}>
              <div className="field-label">{area}</div>
              {kinds.map((k) => (
                <label key={k.name} className="radio small" title={k.name}>
                  <input
                    type="checkbox"
                    checked={t.events.includes(k.name)}
                    onChange={(e) => set("events", toggle(t.events, k.name, e.target.checked))}
                  />
                  <span className={`sev-dot sev-${k.severity}`} title={SEVERITY_LABEL[k.severity]} />
                  {k.description}
                </label>
              ))}
            </div>
          ))}
        </div>
      </fieldset>

      <fieldset className="config-section">
        <legend>Servers</legend>
        <label className="radio small">
          <input type="radio" checked={!someServers} onChange={() => setSomeServers(false)} />
          All servers
        </label>
        <label className="radio small">
          <input type="radio" checked={someServers} onChange={() => setSomeServers(true)} />
          Only these servers and groups (alerts that aren&apos;t about a server, like a full disk, are always sent)
        </label>
        {someServers && (
          <div className="notify-servers field-grid">
            {groups.length > 0 && (
              <label className="field">
                <span className="field-label">Server groups</span>
                <MultiSelect
                  label="Server groups"
                  options={groups.map((g) => ({ value: g.id, label: g.name }))}
                  value={t.groups ?? []}
                  onChange={(ids) => set("groups", ids)}
                  placeholder="Add a group…"
                  empty="None"
                />
              </label>
            )}
            <label className="field">
              <span className="field-label">Servers</span>
              <MultiSelect
                label="Servers"
                options={servers.map((s) => ({ value: s.name, label: s.name, hint: s.app }))}
                value={t.servers ?? []}
                onChange={(names) => set("servers", names)}
                placeholder="Add a server…"
                empty="None"
              />
            </label>
          </div>
        )}
      </fieldset>

      <fieldset className="config-section">
        <legend>Quiet hours</legend>
        <div className="inline-input">
          <input type="time" value={t.quietStart ?? ""} onChange={(e) => set("quietStart", e.target.value)} />
          to
          <input type="time" value={t.quietEnd ?? ""} onChange={(e) => set("quietEnd", e.target.value)} />
          {(t.quietStart || t.quietEnd) && (
            <button
              type="button"
              className="link-button"
              onClick={() => setT((old) => ({ ...old, quietStart: "", quietEnd: "" }))}
            >
              Clear
            </button>
          )}
        </div>
        <span className="field-help">
          During quiet hours, only errors are sent; the rest wait until they end. Times are in hlm&apos;s time zone.
        </span>
      </fieldset>

      <div className="btn-row">
        <button type="submit" className="btn small primary" disabled={!!busy}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button type="button" className="btn small" onClick={() => onTest(value())} disabled={!!busy}>
          {busy === "test" ? "Sending…" : "Send a test"}
        </button>
        <button type="button" className="btn small ghost" onClick={onCancel} disabled={!!busy}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function HeadersEditor({
  headers,
  saved,
  onChange,
}: {
  headers: { name: string; value: string }[];
  saved?: string[];
  onChange: (h: { name: string; value: string }[]) => void;
}) {
  const update = (i: number, h: { name: string; value: string }) => onChange(headers.map((x, j) => (j === i ? h : x)));
  return (
    <div className="field">
      <span className="field-label">Extra headers (optional)</span>
      {headers.map((h, i) => (
        <div key={i} className="inline-input">
          <input value={h.name} onChange={(e) => update(i, { ...h, value: h.value, name: e.target.value })} placeholder="Authorization" />
          <input
            type="password"
            value={h.value}
            onChange={(e) => update(i, { ...h, value: e.target.value })}
            placeholder={saved?.some((n) => n.toLowerCase() === h.name.toLowerCase()) ? "Saved; leave empty to keep it" : "Value"}
            autoComplete="off"
          />
          <button type="button" className="btn small ghost" onClick={() => onChange(headers.filter((_, j) => j !== i))}>
            Remove
          </button>
        </div>
      ))}
      <div>
        <button type="button" className="link-button" onClick={() => onChange([...headers, { name: "", value: "" }])}>
          Add a header
        </button>
      </div>
      <span className="field-help">For webhooks that need a token. Values are stored encrypted and never shown again.</span>
    </div>
  );
}

function deliveriesText(r: NotifyRecord): string {
  if (r.suppressed) return "a repeat; not sent again yet";
  if (!r.deliveries.length) return "no target wanted it";
  return r.deliveries
    .map((d) => (d.held ? `${d.name}: held for quiet hours` : d.ok ? `${d.name}: sent` : `${d.name}: failed (${d.error})`))
    .join(" · ");
}

function History({ records, onRefresh }: { records: NotifyRecord[]; onRefresh: () => void }) {
  const [all, setAll] = useState(false);
  const shown = all ? records : records.slice(0, 10);
  return (
    <div className="notify-history">
      <h3>
        Recent alerts{" "}
        <button type="button" className="link-button" onClick={onRefresh}>
          Refresh
        </button>
      </h3>
      {records.length === 0 ? (
        <p className="muted small">Nothing has happened yet.</p>
      ) : (
        <ul className="notify-history-list">
          {shown.map((r, i) => (
            <li key={i}>
              <span className={`sev-dot sev-${r.severity}`} title={SEVERITY_LABEL[r.severity]} />
              <span className="muted small" title={new Date(r.time).toLocaleString()}>
                <Ago iso={r.time} />
              </span>{" "}
              <strong>{r.title}</strong>
              <div className="muted small">{deliveriesText(r)}</div>
            </li>
          ))}
        </ul>
      )}
      {records.length > 10 && (
        <button type="button" className="link-button" onClick={() => setAll(!all)}>
          {all ? "Show fewer" : `Show all ${records.length}`}
        </button>
      )}
    </div>
  );
}
