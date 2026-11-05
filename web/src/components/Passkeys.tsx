import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import { createPasskey, passkeyError, passkeysUnavailable } from "../passkeys";
import type { PasskeyInfo } from "../types";
import { Ago, Spinner } from "./common";

/** A name for a new passkey from the browser and system, e.g. "Firefox on Linux". */
function suggestName(): string {
  const ua = navigator.userAgent;
  const browser = /Edg\//.test(ua)
    ? "Edge"
    : /Firefox\//.test(ua)
      ? "Firefox"
      : /Chrome\//.test(ua)
        ? "Chrome"
        : /Safari\//.test(ua)
          ? "Safari"
          : "Browser";
  const os = /iPhone|iPad/.test(ua)
    ? "iOS"
    : /Android/.test(ua)
      ? "Android"
      : /Mac OS X/.test(ua)
        ? "macOS"
        : /Windows/.test(ua)
          ? "Windows"
          : /Linux/.test(ua)
            ? "Linux"
            : "";
  return os ? `${browser} on ${os}` : browser;
}

/** My account → Passkeys: sign in without a password, or as the second factor. */
export function PasskeysCard({ username, required, hasApp }: { username: string; required: boolean; hasApp: boolean }) {
  const [list, setList] = useState<PasskeyInfo[]>();
  const [name, setName] = useState(suggestName);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [renaming, setRenaming] = useState<{ id: string; name: string }>();
  const problem = passkeysUnavailable();
  const host = window.location.hostname;

  const load = () => api.passkeys(username).then(setList, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, [username]); // eslint-disable-line react-hooks/exhaustive-deps

  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError(undefined);
    try {
      await fn();
    } catch (e) {
      setError(passkeyError(e));
    } finally {
      setBusy(false);
    }
  }

  const add = (e: FormEvent) => {
    e.preventDefault();
    run(async () => {
      const created = await createPasskey(await api.beginAddPasskey(username));
      await api.addPasskey(username, { name: name.trim() || "Passkey", ...created });
      setName(suggestName());
      await load();
    });
  };

  const rename = (e: FormEvent) => {
    e.preventDefault();
    if (!renaming) return;
    run(async () => {
      await api.renamePasskey(username, renaming.id, renaming.name);
      setRenaming(undefined);
      await load();
    });
  };

  const remove = (p: PasskeyInfo) => {
    if (!confirm(`Remove the passkey "${p.name}"? It can't sign in to hlm any more.`)) return;
    run(async () => {
      await api.removePasskey(username, p.id);
      await load();
    });
  };

  // With two-factor required and no app, the last passkey stays.
  const keepLast = required && !hasApp && (list?.length ?? 0) <= 1;

  return (
    <div className="two-factor-card passkeys-card">
      <h3>Passkeys</h3>
      <p className="small no-margin">
        A passkey signs you in without a password, with your fingerprint, face, PIN or a security key, and can't be
        phished: it only works on the site it was made for. It also counts as your second factor after a password, and
        for confirming it&apos;s you.
      </p>
      {!list ? (
        <Spinner />
      ) : (
        list.length > 0 && (
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Works on</th>
                  <th>Added</th>
                  <th>Last used</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {list.map((p) => (
                  <tr key={p.id}>
                    <td>
                      {renaming?.id === p.id ? (
                        <form className="inline-form" onSubmit={rename}>
                          <input
                            value={renaming.name}
                            onChange={(e) => setRenaming({ id: p.id, name: e.target.value })}
                            maxLength={64}
                            aria-label="Passkey name"
                            autoFocus
                          />
                          <button className="btn small primary" type="submit" disabled={busy || !renaming.name.trim()}>
                            Save
                          </button>
                          <button type="button" className="btn small ghost" onClick={() => setRenaming(undefined)}>
                            Cancel
                          </button>
                        </form>
                      ) : (
                        <>
                          {p.name}
                          {p.synced && (
                            <span className="badge" title="Synced: it can be on several of your devices">
                              synced
                            </span>
                          )}
                        </>
                      )}
                    </td>
                    <td>
                      <code className="small">{p.rpId}</code>
                    </td>
                    <td className="small">
                      <Ago iso={p.created} />
                    </td>
                    <td className="small">
                      <Ago iso={p.lastUsed} never="not yet" />
                    </td>
                    <td className="row-actions">
                      <button
                        className="btn small ghost"
                        onClick={() => setRenaming({ id: p.id, name: p.name })}
                        disabled={busy}
                      >
                        Rename
                      </button>
                      <button
                        className="btn small ghost danger"
                        onClick={() => remove(p)}
                        disabled={busy || keepLast}
                        title={
                          keepLast
                            ? "Two-factor is required: add another passkey or the authenticator app first"
                            : undefined
                        }
                      >
                        Remove
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )
      )}
      {problem ? (
        <p className="muted small no-margin">{problem}</p>
      ) : (
        <form className="inline-form" onSubmit={add}>
          <label className="field">
            <span className="field-label">New passkey&apos;s name</span>
            <input value={name} onChange={(e) => setName(e.target.value)} maxLength={64} />
          </label>
          <button className="btn small primary" type="submit" disabled={busy}>
            {busy ? "Waiting for the passkey…" : "Add a passkey"}
          </button>
        </form>
      )}
      {list && list.some((p) => p.rpId !== host) && (
        <p className="muted small no-margin">
          A passkey works on the address it was added on: open hlm at that address to use the others.
        </p>
      )}
      {error && <div className="alert error small">{error}</div>}
    </div>
  );
}
