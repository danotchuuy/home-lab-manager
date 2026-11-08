import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError } from "../api";
import type { UnifiConfig, UnifiInput } from "../types";
import { Spinner } from "./common";
import { UnifiServers } from "./UnifiServers";

/** Formats a hex SHA-256 fingerprint as AB:CD:… for comparing by eye. */
function pretty(fp: string): string {
  return (fp.toUpperCase().match(/.{1,2}/g) ?? []).join(":");
}

function inputFrom(c?: UnifiConfig): UnifiInput {
  return {
    url: c?.url ?? "",
    site: c?.site ?? "default",
    authType: c?.authType ?? "password",
    username: c?.username ?? "",
    password: "",
    apiKey: "",
    fingerprint: c?.fingerprint ?? "",
    interface: c?.interface ?? "wan",
    forwardTo: c?.forwardTo ?? "",
  };
}

/** Connects hlm to a UniFi gateway for port forwarding. Credentials never come back from the server. */
export function UnifiSettings() {
  const [config, setConfig] = useState<UnifiConfig>();
  const [form, setForm] = useState<UnifiInput>(inputFrom());
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState<"test" | "save" | "delete">();
  const [result, setResult] = useState<{ ok: boolean; text: string }>();
  const [untrusted, setUntrusted] = useState<string>();

  useEffect(() => {
    api.unifiConfig().then((c) => {
      setConfig(c);
      setForm(inputFrom(c));
    });
  }, []);

  const set = <K extends keyof UnifiInput>(key: K, value: UnifiInput[K]) => setForm((f) => ({ ...f, [key]: value }));

  async function run(kind: "test" | "save", override?: Partial<UnifiInput>) {
    const cfg = { ...form, ...override };
    setBusy(kind);
    setResult(undefined);
    setUntrusted(undefined);
    try {
      if (kind === "test") {
        const r = await api.testUnifi(cfg);
        setResult({
          ok: true,
          text: `Connected to ${r.unifiOS ? "a UniFi OS console" : "a UniFi Network controller"}; it has ${r.portForwards} port forwarding rule${r.portForwards === 1 ? "" : "s"}.`,
        });
      } else {
        const c = await api.saveUnifi(cfg);
        setConfig(c);
        setForm(inputFrom(c));
        setEditing(false);
        setResult({ ok: true, text: "Saved. Servers' Port forwarding tabs can now open ports on your gateway." });
      }
    } catch (e) {
      const fp = e instanceof ApiError ? (e.body?.certFingerprint as string | undefined) : undefined;
      if (fp) setUntrusted(fp);
      else setResult({ ok: false, text: (e as Error).message });
    } finally {
      setBusy(undefined);
    }
  }

  function trust() {
    const fp = untrusted!;
    set("fingerprint", fp);
    run("test", { fingerprint: fp });
  }

  async function remove() {
    if (!confirm("Remove the UniFi settings and stored credentials? Rules already on the gateway stay.")) return;
    setBusy("delete");
    try {
      await api.deleteUnifi();
      const c = await api.unifiConfig();
      setConfig(c);
      setForm(inputFrom(c));
      setResult(undefined);
    } finally {
      setBusy(undefined);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    run("save");
  }

  if (!config) return <Spinner />;

  if (config.configured && !editing) {
    return (
      <div className="unifi">
        {config.error && <div className="alert error">{config.error}</div>}
        <dl className="facts">
          <dt>Controller</dt>
          <dd>
            <code>{config.url}</code> · site <code>{config.site}</code>
          </dd>
          <dt>Signed in with</dt>
          <dd>{config.authType === "apikey" ? "API key (stored encrypted)" : `Local account ${config.username} (password stored encrypted)`}</dd>
          <dt>Rules apply to</dt>
          <dd>{config.interface === "both" ? "both WANs" : config.interface?.toUpperCase()}</dd>
          <dt>Forward to</dt>
          <dd>
            {config.forwardTo || config.detectedHostIp || <span className="form-error">unknown; set an address</span>}
            {!config.forwardTo && config.detectedHostIp && <span className="muted"> (this host, detected)</span>}
          </dd>
          {config.fingerprint && (
            <>
              <dt>Trusted certificate</dt>
              <dd>
                <code className="fingerprint">{pretty(config.fingerprint)}</code>
              </dd>
            </>
          )}
        </dl>
        {result && <div className={`alert ${result.ok ? "success" : "error"}`}>{result.text}</div>}
        <div className="btn-row">
          <button className="btn small" onClick={() => run("test")} disabled={!!busy}>
            {busy === "test" ? "Testing…" : "Test connection"}
          </button>
          <button className="btn small" onClick={() => setEditing(true)}>
            Change
          </button>
          <button className="btn small danger" onClick={remove} disabled={!!busy}>
            Remove
          </button>
        </div>
        <UnifiServers />
      </div>
    );
  }

  const apiKey = form.authType === "apikey";
  return (
    <form className="unifi config-form" onSubmit={submit} autoComplete="off">
      <p className="muted small no-margin">
        Lets servers open their ports to the internet through your UniFi gateway&apos;s port forwarding. Use a local admin
        account made for hlm (UniFi OS: Admins &amp; Users → add a local-access-only admin), or an API key.
      </p>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Controller address</span>
          <input value={form.url} onChange={(e) => set("url", e.target.value)} placeholder="https://192.168.1.1" required spellCheck={false} />
          <span className="field-help">Your UniFi console, or https://host:8443 for a self-hosted controller.</span>
        </label>
        <label className="field">
          <span className="field-label">Site</span>
          <input value={form.site} onChange={(e) => set("site", e.target.value)} spellCheck={false} />
          <span className="field-help">The site ID from the controller&apos;s URL; usually &quot;default&quot;.</span>
        </label>
      </div>

      <div className="segmented" role="radiogroup" aria-label="Sign in with">
        <button type="button" role="radio" aria-checked={!apiKey} className={!apiKey ? "on" : ""} onClick={() => set("authType", "password")}>
          Local account
          <span>Username and password. Works on all controllers.</span>
        </button>
        <button type="button" role="radio" aria-checked={apiKey} className={apiKey ? "on" : ""} onClick={() => set("authType", "apikey")}>
          API key
          <span>UniFi OS only (Control Plane → Integrations). Test it: not every version allows it here.</span>
        </button>
      </div>

      <div className="field-grid">
        {apiKey ? (
          <label className="field">
            <span className="field-label">API key</span>
            <input
              type="password"
              value={form.apiKey}
              onChange={(e) => set("apiKey", e.target.value)}
              placeholder={config.hasApiKey ? "Saved; leave empty to keep it" : ""}
              autoComplete="new-password"
            />
          </label>
        ) : (
          <>
            <label className="field">
              <span className="field-label">Username</span>
              <input value={form.username} onChange={(e) => set("username", e.target.value)} autoComplete="off" spellCheck={false} />
            </label>
            <label className="field">
              <span className="field-label">Password</span>
              <input
                type="password"
                value={form.password}
                onChange={(e) => set("password", e.target.value)}
                placeholder={config.hasPassword ? "Saved; leave empty to keep it" : ""}
                autoComplete="new-password"
              />
              <span className="field-help">Accounts with two-factor sign-in can&apos;t be used.</span>
            </label>
          </>
        )}
      </div>

      <div className="field-grid">
        <label className="field">
          <span className="field-label">Open ports on</span>
          <select value={form.interface} onChange={(e) => set("interface", e.target.value as UnifiInput["interface"])}>
            <option value="wan">WAN (primary internet)</option>
            <option value="wan2">WAN2</option>
            <option value="both">Both WANs</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">Forward to</span>
          <input
            value={form.forwardTo}
            onChange={(e) => set("forwardTo", e.target.value)}
            placeholder={config.detectedHostIp ? `${config.detectedHostIp} (this host, detected)` : "This host's LAN address"}
            spellCheck={false}
          />
          <span className="field-help">
            Where traffic for servers that publish ports goes. Servers on a LAN network use their own address.
          </span>
        </label>
      </div>

      {untrusted && (
        <div className="alert warn cert-prompt">
          <strong>The controller&apos;s certificate isn&apos;t trusted.</strong> UniFi consoles use a self-signed certificate,
          so hlm can trust this exact one instead of turning checks off. Compare its SHA-256 fingerprint with the one your
          browser shows for {form.url} (padlock → certificate):
          <code className="fingerprint">{pretty(untrusted)}</code>
          <div className="btn-row">
            <button type="button" className="btn small primary" onClick={trust}>
              They match, trust it
            </button>
            <button type="button" className="btn small ghost" onClick={() => setUntrusted(undefined)}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {form.fingerprint && !untrusted && (
        <p className="muted small no-margin">
          Trusting certificate <code className="fingerprint">{pretty(form.fingerprint)}</code>{" "}
          <button type="button" className="link-button" onClick={() => set("fingerprint", "")}>
            forget
          </button>
        </p>
      )}
      {result && <div className={`alert ${result.ok ? "success" : "error"}`}>{result.text}</div>}

      <div className="form-footer">
        {config.configured && (
          <button type="button" className="btn ghost small" onClick={() => { setEditing(false); setForm(inputFrom(config)); setResult(undefined); setUntrusted(undefined); }}>
            Cancel
          </button>
        )}
        <div className="spacer" />
        <button type="button" className="btn small" onClick={() => run("test")} disabled={!!busy || !form.url}>
          {busy === "test" ? "Testing…" : "Test connection"}
        </button>
        <button type="submit" className="btn small primary" disabled={!!busy || !form.url}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
      </div>
      <p className="muted small no-margin">
        Credentials are encrypted on this machine (AES-256-GCM) and never sent back to the browser. Saving checks that they work first.
      </p>
    </form>
  );
}
