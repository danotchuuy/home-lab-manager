import { useEffect, useState, type FormEvent } from "react";
import { api, setConfirmer } from "../api";
import { getPasskey, passkeyError, passkeysUnavailable } from "../passkeys";

interface Ask {
  method: string;
  /** The account uses two-factor, so a code is needed too. */
  twoFactor: boolean;
  /** The account has passkeys, which confirm on their own. */
  passkey: boolean;
  resolve: (ok: boolean) => void;
}

/**
 * Asks the person to confirm it's them when a change could give lasting
 * access, like a new API key, then lets the change go ahead. A password
 * session confirms with the password; single sign-on by signing in again,
 * which leaves the page, so the change has to be made again after.
 */
export function ConfirmPrompt() {
  const [ask, setAsk] = useState<Ask>();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    setConfirmer(
      (method, twoFactor, passkey) => new Promise<boolean>((resolve) => setAsk({ method, twoFactor, passkey, resolve })),
    );
    return () => setConfirmer(undefined);
  }, []);

  if (!ask) return null;
  const done = (ok: boolean) => {
    ask.resolve(ok);
    setAsk(undefined);
    setPassword("");
    setCode("");
    setError(undefined);
  };

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.confirm(password, ask?.twoFactor ? code : undefined);
      done(true);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function withPasskey() {
    setBusy(true);
    setError(undefined);
    try {
      await api.passkeyConfirm(await getPasskey(await api.beginPasskeyConfirm()));
      done(true);
    } catch (err) {
      setError(passkeyError(err));
    } finally {
      setBusy(false);
    }
  }

  const back = window.location.pathname + window.location.search + window.location.hash;
  const passkeyProblem = passkeysUnavailable();
  // With passkeys but no authenticator app, the password alone isn't enough.
  const passwordForm = !ask.passkey || ask.twoFactor;
  return (
    <div className="modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="confirm-title">
      <div className="card modal">
        <h2 id="confirm-title">Confirm it&apos;s you</h2>
        <p className="muted small no-margin">
          This changes who can get in, so it needs you to confirm it&apos;s you. It lasts 10 minutes.
        </p>
        {ask.method === "oidc" ? (
          <>
            <p className="small no-margin">You&apos;ll sign in with single sign-on again, then come back here. Make the change again after.</p>
            <div className="btn-row">
              <a className="btn primary" href={`/auth/login?confirm=1&return=${encodeURIComponent(back)}`}>
                Sign in again
              </a>
              <button type="button" className="btn ghost" onClick={() => done(false)}>
                Cancel
              </button>
            </div>
          </>
        ) : (
          <>
            {ask.passkey && (
              <div className="modal-form">
                {passkeyProblem && <p className="muted small no-margin">{passkeyProblem}</p>}
                {!passwordForm && error && <div className="alert error">{error}</div>}
                <div className="btn-row">
                  <button type="button" className="btn primary" onClick={withPasskey} disabled={busy || !!passkeyProblem}>
                    {busy ? "Waiting for the passkey…" : "Use your passkey"}
                  </button>
                  {!passwordForm && (
                    <button type="button" className="btn ghost" onClick={() => done(false)}>
                      Cancel
                    </button>
                  )}
                </div>
              </div>
            )}
            {ask.passkey && passwordForm && <div className="sign-in-or">or</div>}
            {passwordForm && (
              <form onSubmit={submit} className="modal-form">
                <label className="field">
                  <span className="field-label">Your password</span>
                  <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus required />
                </label>
                {ask.twoFactor && (
                  <label className="field">
                    <span className="field-label">Code from your authenticator app (or a recovery code)</span>
                    <input value={code} onChange={(e) => setCode(e.target.value)} inputMode="numeric" autoComplete="one-time-code" required />
                  </label>
            )}
            {error && <div className="alert error">{error}</div>}
            <div className="btn-row">
              <button className="btn primary" type="submit" disabled={busy || !password || (ask.twoFactor && !code.trim())}>
                {busy ? "Checking…" : "Confirm"}
              </button>
              <button type="button" className="btn ghost" onClick={() => done(false)}>
                Cancel
              </button>
            </div>
          </form>
            )}
          </>
        )}
      </div>
    </div>
  );
}
