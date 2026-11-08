import { useEffect, useState, type FormEvent } from "react";
import { api, UNAUTHORIZED_EVENT } from "../api";
import type { Account, AuthConfig, AuthStatus, RequireTwoFactor, Role } from "../types";
import { PasskeysCard } from "./Passkeys";
import { TwoFactorCard } from "./TwoFactor";

import { Ago, Spinner } from "./common";
import { SSHKeys } from "./SSHKeys";
import { MySignIns } from "./ActivityLog";

const MIN_PASSWORD = 10;

/**
 * Whether sign-in is required, how long it lasts, and the local accounts.
 * Requiring sign-in takes being signed in, so there's a way in known to work.
 */
export function SignInSettings() {
  const [config, setConfig] = useState<AuthConfig>();
  const [status, setStatus] = useState<AuthStatus>();
  const [accounts, setAccounts] = useState<Account[]>();
  const [hours, setHours] = useState("");
  const [busy, setBusy] = useState<string>();
  const [result, setResult] = useState<{ ok: boolean; text: string }>();

  async function load() {
    const [c, st, list] = await Promise.all([api.authConfig(), api.authStatus(), api.accounts()]);
    setConfig(c);
    setStatus(st);
    setAccounts(list);
    setHours(String(c.sessionHours));
  }

  useEffect(() => {
    load().catch((e) => setResult({ ok: false, text: (e as Error).message }));
  }, []);

  async function run(kind: string, f: () => Promise<string | void>) {
    setBusy(kind);
    setResult(undefined);
    try {
      const text = await f();
      await load();
      if (text) setResult({ ok: true, text });
    } catch (e) {
      setResult({ ok: false, text: (e as Error).message });
    } finally {
      setBusy(undefined);
    }
  }

  function setRequired(required: boolean) {
    if (!required && !confirm("Stop requiring sign-in? Without HLM_TOKEN, anyone who can reach Home Lab Manager can use it.")) return;
    run("require", async () => {
      await api.saveAuthConfig({ ...config!, required });
      return required ? "Sign-in is required now." : "Sign-in is no longer required.";
    });
  }

  function saveHours(e: FormEvent) {
    e.preventDefault();
    run("hours", async () => {
      await api.saveAuthConfig({ ...config!, sessionHours: Number(hours) });
      return "Saved. New sign-ins last that long.";
    });
  }

  if (!config || !accounts || !status) return result ? <div className="alert error">{result.text}</div> : <Spinner />;

  const me = status.user;
  return (
    <div className="sign-in-settings">
      <p className="muted small no-margin">
        {config.required ? (
          <>
            <strong>Sign-in is required.</strong> The API refuses requests without a sign-in, an API key or HLM_TOKEN.
          </>
        ) : (
          <>
            <strong>Sign-in isn&apos;t required.</strong> Anyone who can reach Home Lab Manager can use it
            {status.token ? ", unless HLM_TOKEN stops them" : ""}. Add an account (or set up single sign-on below), sign in
            with it, then require sign-in.
          </>
        )}
      </p>
      <div className="btn-row">
        {config.required ? (
          <button className="btn small" onClick={() => setRequired(false)} disabled={!!busy}>
            {busy === "require" ? "Saving…" : "Stop requiring sign-in"}
          </button>
        ) : (
          <button
            className={`btn small${me ? " primary" : ""}`}
            onClick={() => setRequired(true)}
            disabled={!!busy || !me}
            title={me ? undefined : "Sign in first, so requiring sign-in can't lock everyone out"}
          >
            {busy === "require" ? "Saving…" : "Require sign-in"}
          </button>
        )}
        {me ? (
          <span className="muted small">
            Signed in as {me.name || me.username} ({me.method === "local" ? "local account" : status.oidcName || "single sign-on"})
          </span>
        ) : (
          (status.local || status.oidc) && (
            <button type="button" className="btn small ghost" onClick={() => window.dispatchEvent(new Event(UNAUTHORIZED_EVENT))}>
              Sign in
            </button>
          )
        )}
      </div>
      <form className="inline-form" onSubmit={saveHours}>
        <label className="field">
          <span className="field-label">Sign-ins last (hours)</span>
          <input type="number" min={1} max={2160} value={hours} onChange={(e) => setHours(e.target.value)} />
        </label>
        <button className="btn small" type="submit" disabled={!!busy || Number(hours) === config.sessionHours}>
          {busy === "hours" ? "Saving…" : "Save"}
        </button>
      </form>
      <div className="field require-2fa">
        <span className="field-label">Require two-factor sign-in for local accounts</span>
        <select
          aria-label="Require two-factor sign-in for local accounts"
          value={config.requireTwoFactor ?? ""}
          disabled={
            !!busy ||
            (!config.requireTwoFactor &&
              me?.method === "local" &&
              !status.twoFactor?.enabled &&
              !status.twoFactor?.passkeys)
          }
          onChange={(e) => {
            const requireTwoFactor = e.target.value as RequireTwoFactor;
            run("2fa", async () => {
              await api.saveAuthConfig({ ...config, requireTwoFactor });
              return requireTwoFactor
                ? `Two-factor is required for ${requireTwoFactor === "all" ? "every local account" : "admins"}. Accounts without it set it up at their next sign-in.`
                : "Two-factor isn't required any more; accounts that have it keep it.";
            });
          }}
        >
          <option value="">Not required (each account chooses)</option>
          <option value="admins">Required for admins</option>
          <option value="all">Required for every local account</option>
        </select>
        <span className="field-help">
          {!config.requireTwoFactor && me?.method === "local" && !status.twoFactor?.enabled && !status.twoFactor?.passkeys
            ? "Set up two-factor for your own account first (below: a passkey or the authenticator app), so requiring it can't lock you out."
            : "A passkey or the authenticator app counts. Accounts that have to use it and don't yet set it up right after their password at the next sign-in. Single sign-on and API keys aren't affected: the provider handles its own."}
        </span>
      </div>
      {result && <div className={`alert ${result.ok ? "success" : "error"}`}>{result.text}</div>}

      {me?.method === "local" && me.username && status.twoFactor && (
        <>
          <PasskeysCard username={me.username} required={status.twoFactor.required} hasApp={status.twoFactor.enabled} />
          <TwoFactorCard username={me.username} status={status.twoFactor} onChange={() => load().catch(() => {})} />
        </>
      )}
      {/* As on a member's own System page. */}
      {me && <MySignIns />}

      <h3>Local accounts</h3>
      <p className="muted small no-margin">
        A username and password kept by Home Lab Manager, for when single sign-on is down or there isn&apos;t any. If
        nobody can sign in, <code>docker exec home-lab-manager hlm reset-password admin</code> on the host prints a new
        password for <code>admin</code> (making the account if needed). Add <code>--admin</code> to make an existing
        account an admin, and <code>--reset-2fa</code> to turn off its two-factor sign-in (the app and its passkeys) if its phone is lost. Admins can do everything; members get only what server groups give them.
      </p>
      {accounts.length > 0 && (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Username</th>
                <th>Role</th>
                <th>Password changed</th>
                <th>Two-factor</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {accounts.map((a) => (
                <AccountRow
                  key={a.username}
                  account={a}
                  me={me?.method === "local" && me.username?.toLowerCase() === a.username.toLowerCase()}
                  busy={!!busy}
                  run={run}
                  onChange={() => api.accounts().then(setAccounts, () => {})}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
      <AddAccount busy={!!busy} run={run} />
    </div>
  );
}

type Run = (kind: string, f: () => Promise<string | void>) => Promise<void>;

function AccountRow({ account, me, busy, run, onChange }: { account: Account; me: boolean; busy: boolean; run: Run; onChange: () => void }) {
  const [changing, setChanging] = useState(false);
  const [showKeys, setShowKeys] = useState(false);
  const [password, setPassword] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const mismatch = confirmPw !== "" && password !== confirmPw;

  function change(e: FormEvent) {
    e.preventDefault();
    run("password", async () => {
      await api.changePassword(account.username, password);
      setChanging(false);
      setPassword("");
      setConfirmPw("");
      return `Changed ${account.username}'s password.${me ? "" : " Its other sessions have ended."}`;
    });
  }

  function setRole(role: Role) {
    run("role", async () => {
      await api.setAccountRole(account.username, role);
      return `${account.username} is now ${role === "admin" ? "an admin" : "a member"}.`;
    });
  }

  function resetTwoFactor() {
    if (!confirm(`Reset ${account.username}'s two-factor sign-in, for a lost phone or passkey? It turns off the authenticator app and removes their passkeys. Until they set it up again, their password alone signs in (or, if it's required, they set it up at their next sign-in).`)) return;
    run("2fa-reset", async () => {
      await api.disableTwoFactor(account.username);
      return `Reset ${account.username}'s two-factor sign-in.`;
    });
  }

  function remove() {
    if (!confirm(`Remove the account ${account.username}? Its sessions end.`)) return;
    run("delete", async () => {
      await api.deleteAccount(account.username);
      return `Removed ${account.username}.`;
    });
  }

  return (
    <>
      <tr>
        <td>
          {account.username} {me && <span className="muted small">(you)</span>}
        </td>
        <td>
          <select
            aria-label={`Role of ${account.username}`}
            value={account.role}
            disabled={busy}
            onChange={(e) => setRole(e.target.value as Role)}
          >
            <option value="admin">Admin</option>
            <option value="member">Member</option>
          </select>
        </td>
        <td title={account.passwordChanged}><Ago iso={account.passwordChanged} /></td>
        <td>
          {account.twoFactor || (account.passkeys ?? 0) > 0 ? (
            <span title={account.twoFactor ? `${account.recoveryLeft ?? 0} recovery codes left` : undefined}>
              {[account.twoFactor && "App", (account.passkeys ?? 0) > 0 && `${account.passkeys} passkey${account.passkeys === 1 ? "" : "s"}`]
                .filter(Boolean)
                .join(", ")}{" "}
              {!me && (
                <button type="button" className="link-button" onClick={resetTwoFactor} disabled={busy}>
                  Reset
                </button>
              )}
            </span>
          ) : (
            <span className="muted">Off</span>
          )}
        </td>
        <td className="actions">
          <button type="button" className="btn small ghost" onClick={() => setChanging((v) => !v)} disabled={busy}>
            Change password
          </button>
          <button type="button" className="btn small ghost" onClick={() => setShowKeys((v) => !v)} aria-expanded={showKeys}>
            SSH keys ({account.sshKeys})
          </button>
          <button type="button" className="btn small ghost danger" onClick={remove} disabled={busy}>
            Remove
          </button>
        </td>
      </tr>
      {showKeys && (
        <tr>
          <td colSpan={5}>
            <SSHKeys username={account.username} onChange={onChange} />
          </td>
        </tr>
      )}
      {changing && (
        <tr>
          <td colSpan={5}>
            <form className="inline-form" onSubmit={change}>
              <label className="field">
                <span className="field-label">New password</span>
                <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={MIN_PASSWORD} autoComplete="new-password" required autoFocus />
              </label>
              <label className="field">
                <span className="field-label">Again</span>
                <input type="password" value={confirmPw} onChange={(e) => setConfirmPw(e.target.value)} autoComplete="new-password" required />
              </label>
              <button className="btn small primary" type="submit" disabled={busy || mismatch || password.length < MIN_PASSWORD}>
                Save
              </button>
              {mismatch && <span className="form-error">The passwords don&apos;t match</span>}
            </form>
          </td>
        </tr>
      )}
    </>
  );
}

function AddAccount({ busy, run }: { busy: boolean; run: Run }) {
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<Role>("member");
  const [password, setPassword] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const mismatch = confirmPw !== "" && password !== confirmPw;

  function add(e: FormEvent) {
    e.preventDefault();
    run("add", async () => {
      await api.createAccount(username.trim(), password, role);
      const name = username.trim();
      setUsername("");
      setPassword("");
      setConfirmPw("");
      return `Added ${name}. Sign in with it before requiring sign-in.`;
    });
  }

  return (
    <form className="inline-form" onSubmit={add} autoComplete="off">
      <label className="field">
        <span className="field-label">Username</span>
        <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" spellCheck={false} required />
      </label>
      <label className="field">
        <span className="field-label">Role</span>
        <select value={role} onChange={(e) => setRole(e.target.value as Role)} title="The first account is always an admin">
          <option value="member">Member</option>
          <option value="admin">Admin</option>
        </select>
      </label>
      <label className="field">
        <span className="field-label">Password</span>
        <input
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          minLength={MIN_PASSWORD}
          placeholder={`At least ${MIN_PASSWORD} characters`}
          autoComplete="new-password"
          required
        />
      </label>
      <label className="field">
        <span className="field-label">Again</span>
        <input type="password" value={confirmPw} onChange={(e) => setConfirmPw(e.target.value)} autoComplete="new-password" required />
      </label>
      <button className="btn small" type="submit" disabled={busy || mismatch || password.length < MIN_PASSWORD || !username.trim()}>
        {busy ? "Saving…" : "Add account"}
      </button>
      {mismatch && <span className="form-error">The passwords don&apos;t match</span>}
    </form>
  );
}
