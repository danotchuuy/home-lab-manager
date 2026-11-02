import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { ClientSecret, OIDCInput, OIDCSettings } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

const CALLBACK_PATH = "/auth/callback";

function inputFrom(c?: OIDCSettings): OIDCInput {
  return {
    name: c?.name ?? "Authelia",
    issuer: c?.issuer ?? "",
    clientId: c?.clientId ?? "home-lab-manager",
    clientSecret: "",
    publicUrl: c?.publicUrl ?? window.location.origin,
    scopes: c?.scopes ?? "openid profile email groups",
    allowedUsers: c?.allowedUsers ?? [],
    allowedGroups: c?.allowedGroups ?? [],
    adminUsers: c?.adminUsers ?? [],
    adminGroups: c?.adminGroups ?? [],
  };
}

/** "a, b c" → ["a", "b", "c"] */
function splitList(s: string): string[] {
  return s.split(/[\s,]+/).filter(Boolean);
}

/** Where to sign in with the provider, coming back to this card. */
const checkUrl = `/auth/login?return=${encodeURIComponent("/system#sso")}`;

/**
 * The client to register in Authelia's configuration for these settings, with
 * the digest of the secret when hlm generated it.
 */
function autheliaSnippet(form: OIDCInput, digest?: string): string {
  const redirect = (form.publicUrl.replace(/\/+$/, "") || "https://hlm.example.com") + CALLBACK_PATH;
  const scopes = splitList(form.scopes).map((s) => `'${s}'`).join(", ");
  return `identity_providers:
  oidc:
    clients:
      - client_id: '${form.clientId || "home-lab-manager"}'
        client_name: 'Home Lab Manager'
${
    digest
      ? `        client_secret: '${digest}'`
      : `        # The digest of the client secret: authelia crypto hash generate pbkdf2 --variant sha512 --password '<the secret>'
        client_secret: '$pbkdf2-sha512$310000$...'`
  }
        public: false
        authorization_policy: 'two_factor'
        require_pkce: true
        pkce_challenge_method: 'S256'
        redirect_uris:
          - '${redirect}'
        scopes: [${scopes}]
        response_types: ['code']
        grant_types: ['authorization_code']
        token_endpoint_auth_method: 'client_secret_basic'`;
}

/** Single sign-on through an OpenID Connect provider such as Authelia. */
export function SSOSettings() {
  const [config, setConfig] = useState<OIDCSettings>();
  const [form, setForm] = useState<OIDCInput>(inputFrom());
  const [users, setUsers] = useState("");
  const [groups, setGroups] = useState("");
  const [adminUsers, setAdminUsers] = useState("");
  const [adminGroups, setAdminGroups] = useState("");
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState<"test" | "save" | "delete">();
  const [result, setResult] = useState<{ ok: boolean; text: string }>();
  // A secret hlm generated for this form; it's saved with the settings.
  const [generated, setGenerated] = useState<ClientSecret>();
  const [copied, setCopied] = useState(false);

  function load(c: OIDCSettings) {
    setConfig(c);
    setGenerated(undefined);
    const f = inputFrom(c);
    setForm(f);
    setUsers(f.allowedUsers.join(", "));
    setGroups(f.allowedGroups.join(", "));
    setAdminUsers(f.adminUsers.join(", "));
    setAdminGroups(f.adminGroups.join(", "));
  }

  useEffect(() => {
    api.oidcSettings().then(load, (e) => setResult({ ok: false, text: (e as Error).message }));
  }, []);

  const set = <K extends keyof OIDCInput>(key: K, value: OIDCInput[K]) => setForm((f) => ({ ...f, [key]: value }));

  async function generateSecret() {
    setCopied(false);
    try {
      const cs = await api.generateClientSecret();
      setGenerated(cs);
      set("clientSecret", cs.secret);
    } catch (e) {
      setResult({ ok: false, text: `Couldn't generate a client secret: ${(e as Error).message}` });
    }
  }

  // A new provider gets a secret straight away; a saved one keeps its own unless asked.
  const showForm = !!config && (!config.configured || editing);
  useEffect(() => {
    if (showForm && !config?.hasSecret && !generated) generateSecret();
  }, [showForm, config?.hasSecret]);

  // The generated secret is still the one in the form.
  const usingGenerated = !!generated && form.clientSecret === generated.secret;

  async function copySecret() {
    try {
      await navigator.clipboard.writeText(form.clientSecret);
      setCopied(true);
    } catch {
      // No clipboard (e.g. plain HTTP); the secret can still be selected.
    }
  }
  const input = (): OIDCInput => ({
    ...form,
    allowedUsers: splitList(users),
    allowedGroups: splitList(groups),
    adminUsers: splitList(adminUsers),
    adminGroups: splitList(adminGroups),
  });

  async function test() {
    setBusy("test");
    setResult(undefined);
    try {
      const r = await api.testOIDC(input());
      setResult({
        ok: true,
        text: `Found the provider at ${r.issuer}${r.pkce ? "" : " (it doesn't list PKCE support; sign-in may still work)"}. The client ID and secret are checked when you sign in.`,
      });
    } catch (e) {
      setResult({ ok: false, text: (e as Error).message });
    } finally {
      setBusy(undefined);
    }
  }

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy("save");
    setResult(undefined);
    try {
      load(await api.saveOIDC(input()));
      setEditing(false);
      setResult({ ok: true, text: "Saved. Sign in with it to check the settings work." });
    } catch (err) {
      setResult({ ok: false, text: (err as Error).message });
    } finally {
      setBusy(undefined);
    }
  }

  async function remove() {
    if (!confirm("Remove single sign-on? Everyone is signed out, including people signed in with a local account.")) return;
    setBusy("delete");
    try {
      await api.deleteOIDC();
      load(await api.oidcSettings());
      setResult(undefined);
    } catch (e) {
      setResult({ ok: false, text: (e as Error).message });
    } finally {
      setBusy(undefined);
    }
  }

  if (!config) return result ? <div className="alert error">{result.text}</div> : <Spinner />;

  const alerts = (
    <>
      {config.disabled && (
        <div className="alert warn">HLM_OIDC=off is set, so these settings are ignored: nobody can sign in with the provider.</div>
      )}
      {config.error && <div className="alert error">{config.error}</div>}
      {result && <div className={`alert ${result.ok ? "success" : "error"}`}>{result.text}</div>}
    </>
  );

  if (config.configured && !editing) {
    const verified = !!config.verified;
    return (
      <div className="sso-settings">
        <dl className="facts">
          <dt>Provider</dt>
          <dd>
            {config.name} · <code>{config.issuer}</code>
          </dd>
          <dt>Client</dt>
          <dd>
            <code>{config.clientId}</code> (secret stored encrypted)
          </dd>
          <dt>Redirect URI</dt>
          <dd>
            <code>{config.redirectUrl}</code>
          </dd>
          <dt>Who may sign in</dt>
          <dd>
            {config.allowedUsers.length === 0 && config.allowedGroups.length === 0
              ? "Anyone the provider signs in"
              : [
                  config.allowedUsers.length > 0 && `users ${config.allowedUsers.join(", ")}`,
                  config.allowedGroups.length > 0 && `groups ${config.allowedGroups.join(", ")}`,
                ]
                  .filter(Boolean)
                  .join("; or ")}
          </dd>
          <dt>Checked</dt>
          <dd>
            {verified ? (
              <>
                {config.verifiedBy} signed in {timeAgo(config.verified)}
              </>
            ) : (
              <span className="form-error">Nobody has signed in with these settings yet</span>
            )}
          </dd>
        </dl>
        {alerts}
        <div className="btn-row">
          <a className={`btn small${verified ? "" : " primary"}`} href={checkUrl}>
            {verified ? `Sign in with ${config.name} again` : `Sign in with ${config.name} to check`}
          </a>
          <button className="btn small" onClick={() => setEditing(true)}>
            Change
          </button>
          <button className="btn small danger" onClick={remove} disabled={!!busy}>
            Remove
          </button>
        </div>
      </div>
    );
  }

  const redirect = form.publicUrl.replace(/\/+$/, "") + CALLBACK_PATH;
  return (
    <form className="sso-settings config-form" onSubmit={save} autoComplete="off">
      <p className="muted small no-margin">
        Sign in through an OpenID Connect provider such as Authelia, Authentik, Keycloak or Pocket ID. Register Home Lab
        Manager there as a confidential client with the redirect URI below. Keep a local account (above) for when the
        provider is down.
      </p>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Provider name</span>
          <input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="Authelia" />
          <span className="field-help">Shown on the sign-in button.</span>
        </label>
        <label className="field">
          <span className="field-label">Issuer URL</span>
          <input value={form.issuer} onChange={(e) => set("issuer", e.target.value)} placeholder="https://auth.example.com" required spellCheck={false} />
          <span className="field-help">For Authelia, its own address. hlm reads /.well-known/openid-configuration there.</span>
        </label>
        <label className="field">
          <span className="field-label">Client ID</span>
          <input value={form.clientId} onChange={(e) => set("clientId", e.target.value)} required spellCheck={false} />
        </label>
        <label className="field">
          <span className="field-label">Client secret</span>
          <input
            type={usingGenerated ? "text" : "password"}
            value={form.clientSecret}
            onChange={(e) => set("clientSecret", e.target.value)}
            placeholder={config.hasSecret ? "Saved; leave empty to keep it" : ""}
            autoComplete="new-password"
            spellCheck={false}
            className={usingGenerated ? "secret-value" : undefined}
          />
          <span className="field-help">
            {usingGenerated ? (
              <>
                Generated by hlm. Authelia&apos;s block below has its digest; other providers take the secret itself.{" "}
                <button type="button" className="link-button" onClick={copySecret}>
                  {copied ? "copied" : "copy"}
                </button>
              </>
            ) : (
              <>The secret itself, not the digest in Authelia&apos;s configuration.</>
            )}{" "}
            <button type="button" className="link-button" onClick={generateSecret}>
              {config.hasSecret || form.clientSecret ? "generate a new one" : "generate"}
            </button>
          </span>
        </label>
        <label className="field">
          <span className="field-label">Home Lab Manager&apos;s address</span>
          <input value={form.publicUrl} onChange={(e) => set("publicUrl", e.target.value)} required spellCheck={false} />
          <span className="field-help">
            Where you open it, e.g. its Cloudflare Tunnel hostname. Redirect URI: <code>{redirect}</code>
          </span>
        </label>
        <label className="field">
          <span className="field-label">Scopes</span>
          <input value={form.scopes} onChange={(e) => set("scopes", e.target.value)} spellCheck={false} />
          <span className="field-help">&quot;groups&quot; is needed to allow groups below.</span>
        </label>
        <label className="field">
          <span className="field-label">Allowed users</span>
          <input value={users} onChange={(e) => setUsers(e.target.value)} placeholder="Anyone" spellCheck={false} />
          <span className="field-help">Usernames or emails, separated by commas.</span>
        </label>
        <label className="field">
          <span className="field-label">Allowed groups</span>
          <input value={groups} onChange={(e) => setGroups(e.target.value)} placeholder="Any" spellCheck={false} />
          <span className="field-help">With neither, anyone the provider signs in gets in.</span>
        </label>
        <label className="field">
          <span className="field-label">Admin users</span>
          <input value={adminUsers} onChange={(e) => setAdminUsers(e.target.value)} placeholder="Everyone" spellCheck={false} />
          <span className="field-help">Usernames or emails, separated by commas.</span>
        </label>
        <label className="field">
          <span className="field-label">Admin groups</span>
          <input value={adminGroups} onChange={(e) => setAdminGroups(e.target.value)} placeholder="Everyone" spellCheck={false} />
          <span className="field-help">With neither, everyone let in is an admin. Others are members, using server groups.</span>
        </label>
      </div>
      <details className="small" open={usingGenerated}>
        <summary>The client in Authelia&apos;s configuration</summary>
        <pre className="op-output">{autheliaSnippet(input(), usingGenerated ? generated.autheliaDigest : undefined)}</pre>
        {usingGenerated && (
          <p className="muted no-margin">
            Add it under <code>identity_providers.oidc.clients</code> and restart Authelia, then save here. The secret only
            reaches Authelia as this digest.
          </p>
        )}
      </details>
      {alerts}
      <div className="form-footer">
        {config.configured && (
          <button
            type="button"
            className="btn ghost small"
            onClick={() => {
              setEditing(false);
              load(config);
              setResult(undefined);
            }}
          >
            Cancel
          </button>
        )}
        <div className="spacer" />
        <button type="button" className="btn small" onClick={test} disabled={!!busy}>
          {busy === "test" ? "Testing…" : "Test"}
        </button>
        <button type="submit" className="btn small primary" disabled={!!busy}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}
