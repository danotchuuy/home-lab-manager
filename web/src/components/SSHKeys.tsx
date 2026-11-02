import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { SSHKey } from "../types";
import { timeAgo } from "../util";
import { splitAddresses } from "./APIKeys";
import { Spinner } from "./common";

/**
 * The public keys that can sign in as an account through the API. Adding one,
 * or changing where it can be used from, takes confirming it's you.
 */
export function SSHKeys({ username, onChange }: { username: string; onChange?: () => void }) {
  const [keys, setKeys] = useState<SSHKey[]>();
  const [publicKey, setPublicKey] = useState("");
  const [name, setName] = useState("");
  const [allowFrom, setAllowFrom] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const load = useCallback(() => api.sshKeys(username).then(setKeys, (e) => setError((e as Error).message)), [username]);
  useEffect(() => {
    load();
  }, [load]);

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    setError(undefined);
    try {
      await action();
      await load();
      onChange?.();
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }

  async function add(e: FormEvent) {
    e.preventDefault();
    if (await run(() => api.addSSHKey(username, publicKey.trim(), name.trim(), splitAddresses(allowFrom)))) {
      setPublicKey("");
      setName("");
      setAllowFrom("");
    }
  }

  function editAllowFrom(k: SSHKey) {
    const next = prompt(`Where can "${k.name}" be used from? Addresses or ranges, separated by commas. Leave empty for anywhere.`, (k.allowFrom ?? []).join(", "));
    if (next !== null) run(() => api.setSSHKeyAllowFrom(username, k.id, splitAddresses(next)));
  }

  function remove(k: SSHKey) {
    if (!confirm(`Remove the SSH key "${k.name}"? What it signed in stops working straight away.`)) return;
    run(() => api.deleteSSHKey(username, k.id));
  }

  if (!keys) return error ? <div className="alert error">{error}</div> : <Spinner />;
  return (
    <div className="ssh-keys">
      <p className="muted small">
        These keys can sign in as {username} through the API, with the same access. A script signs a challenge with{" "}
        <code>ssh-keygen -Y sign</code> and gets a token that lasts an hour.
      </p>
      {keys.length > 0 && (
        <ul className="ssh-key-list">
          {keys.map((k) => (
            <li key={k.id}>
              <span>
                <strong>{k.name}</strong> <code>{k.fingerprint}</code>
                <span className="muted small">
                  {" "}
                  {k.type} · added {timeAgo(k.added)}
                  {k.lastUsed ? ` · last used ${timeAgo(k.lastUsed)}` : " · not used yet"} · from{" "}
                  <button type="button" className="link-button" onClick={() => editAllowFrom(k)} disabled={busy} title="Change where the key can be used from">
                    {k.allowFrom?.length ? k.allowFrom.join(", ") : "anywhere"}
                  </button>
                </span>
              </span>
              <button type="button" className="btn small ghost danger" onClick={() => remove(k)} disabled={busy}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <form onSubmit={add}>
        <label className="field">
          <span className="field-label">Public key</span>
          <textarea value={publicKey} onChange={(e) => setPublicKey(e.target.value)} rows={3} placeholder="ssh-ed25519 AAAA… you@laptop" spellCheck={false} required />
        </label>
        <div className="inline-form">
          <label className="field">
            <span className="field-label">Name (optional)</span>
            <input value={name} onChange={(e) => setName(e.target.value)} maxLength={100} placeholder="The key's comment" />
          </label>
          <label className="field">
            <span className="field-label">Allowed from (optional)</span>
            <input value={allowFrom} onChange={(e) => setAllowFrom(e.target.value)} placeholder="e.g. 10.0.3.115" />
          </label>
          <button className="btn small primary" type="submit" disabled={busy || !publicKey.trim()}>
            Add key
          </button>
        </div>
      </form>
      {error && <div className="alert error">{error}</div>}
    </div>
  );
}
