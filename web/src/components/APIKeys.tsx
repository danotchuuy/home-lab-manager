import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { APIKeyInfo, KeyRole } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

/** API keys for scripts and other programs, sent as a bearer token. A key's secret is shown once. */
export function APIKeys() {
  const [keys, setKeys] = useState<APIKeyInfo[]>();
  const [name, setName] = useState("");
  const [days, setDays] = useState("0");
  const [role, setRole] = useState<KeyRole>("member");
  const [allowFrom, setAllowFrom] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [made, setMade] = useState<{ name: string; key: string }>();
  const [copied, setCopied] = useState(false);

  const load = () => api.apiKeys().then(setKeys, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  async function create(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setCopied(false);
    try {
      const k = await api.createAPIKey(name.trim(), Number(days), role, splitAddresses(allowFrom));
      setMade({ name: k.name, key: k.key });
      setName("");
      setAllowFrom("");
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function revoke(k: APIKeyInfo) {
    if (!confirm(`Revoke the API key "${k.name}"? Anything using it stops working.`)) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.deleteAPIKey(k.id);
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function editAllowFrom(k: APIKeyInfo) {
    const next = prompt(`Where can "${k.name}" be used from? Addresses or ranges, separated by commas. Leave empty for anywhere.`, k.allowFrom.join(", "));
    if (next === null) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.setAPIKeyAllowFrom(k.id, splitAddresses(next));
      await load();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(made!.key);
      setCopied(true);
    } catch {
      // No clipboard (e.g. plain HTTP); the key can still be selected.
    }
  }

  if (!keys) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const origin = window.location.origin;
  return (
    <div className="api-keys">
      <p className="muted small no-margin">
        For scripts and other programs. Send a key as <code>Authorization: Bearer hlm_…</code>; it works even when
        sign-in is required. An admin key can do everything except manage keys, accounts, groups and sign-in, which
        take you at a browser; a member key only what server groups give it. A deployer key can also create servers in
        a group it manages, but they can&apos;t reach the host. Limit a key to the addresses that use it where you can.
      </p>
      {made && (
        <div className="alert success new-key">
          <span>
            <strong>{made.name}:</strong> copy the key now. It isn&apos;t shown again.
          </span>
          <code>{made.key}</code>
          <div className="btn-row">
            <button type="button" className="btn small" onClick={copy}>
              {copied ? "Copied" : "Copy"}
            </button>
            <button type="button" className="btn small ghost" onClick={() => setMade(undefined)}>
              Done
            </button>
          </div>
          <span className="small">
            Try it: <code>curl -H &quot;Authorization: Bearer {made.key.slice(0, 16)}…&quot; {origin}/api/servers</code>
          </span>
        </div>
      )}
      {keys.length > 0 && (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Key</th>
                <th>Role</th>
                <th>Allowed from</th>
                <th>Created</th>
                <th>Last used</th>
                <th>Expires</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id}>
                  <td>{k.name}</td>
                  <td>
                    <code>{k.prefix}</code>
                  </td>
                  <td>{k.role === "admin" ? "Admin" : k.role === "deployer" ? "Deployer" : "Member"}</td>
                  <td>
                    <button type="button" className="link-button" onClick={() => editAllowFrom(k)} disabled={busy} title="Change where the key can be used from">
                      {k.allowFrom.length > 0 ? k.allowFrom.join(", ") : "anywhere"}
                    </button>
                  </td>
                  <td title={k.created}>{timeAgo(k.created)}</td>
                  <td title={k.lastUsed}>{k.lastUsed ? timeAgo(k.lastUsed) : "never"}</td>
                  <td title={k.expires}>
                    {k.expired ? (
                      <span className="form-error">expired</span>
                    ) : k.expires ? (
                      new Date(k.expires).toLocaleDateString()
                    ) : (
                      "never"
                    )}
                  </td>
                  <td className="actions">
                    <button type="button" className="btn small ghost danger" onClick={() => revoke(k)} disabled={busy}>
                      Revoke
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {error && <div className="alert error">{error}</div>}
      <form className="inline-form" onSubmit={create}>
        <label className="field">
          <span className="field-label">Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Home Assistant" maxLength={100} required />
        </label>
        <label className="field">
          <span className="field-label">Expires</span>
          <select value={days} onChange={(e) => setDays(e.target.value)}>
            <option value="0">Never</option>
            <option value="30">In 30 days</option>
            <option value="90">In 90 days</option>
            <option value="365">In a year</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">Role</span>
          <select value={role} onChange={(e) => setRole(e.target.value as KeyRole)}>
            <option value="member">Member</option>
            <option value="deployer">Deployer</option>
            <option value="admin">Admin</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">Allowed from (optional)</span>
          <input value={allowFrom} onChange={(e) => setAllowFrom(e.target.value)} placeholder="e.g. 10.0.3.115, 192.168.11.0/24" />
        </label>
        <button className="btn small" type="submit" disabled={busy || !name.trim()}>
          {busy ? "Creating…" : "Create key"}
        </button>
      </form>
    </div>
  );
}

/** Splits a list of addresses typed with commas or spaces. */
export function splitAddresses(text: string): string[] {
  return text.split(/[\s,]+/).filter(Boolean);
}
