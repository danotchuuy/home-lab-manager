import { useEffect, useState, type FormEvent } from "react";
import { api, setConfirmer } from "../api";

/** Where the single sign-on popup says it's done (internal/auth ConfirmChannel). */
const CONFIRM_CHANNEL = "hlm-confirm";
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
 * session confirms with the password; single sign-on by signing in again in
 * a popup, so the page and what was typed into it stay as they were and the
 * change goes ahead after. Only if popups are blocked does it leave the page.
 */
export function ConfirmPrompt() {
  const [ask, setAsk] = useState<Ask>();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  /** The single sign-on popup is open. */
  const [waiting, setWaiting] = useState(false);

  useEffect(() => {
    setConfirmer(
      (method, twoFactor, passkey) => new Promise<boolean>((resolve) => setAsk({ method, twoFactor, passkey, resolve })),
    );
    return () => setConfirmer(undefined);
  }, []);

  // While the popup is open: it says when it's done, on a channel of this
  // site's (window.opener may not survive the provider's pages). Coming back
  // to this window checks too, in case the message was missed.
  useEffect(() => {
    if (!ask || !waiting) return;
    let over = false;
    const check = async () => {
      try {
        const st = await api.authStatus();
        if (!over && st.confirmedUntil && new Date(st.confirmedUntil) > new Date()) {
          over = true;
          finish(ask, true);
        }
      } catch {
        // Try again on the next message or focus.
      }
    };
    const ch = typeof BroadcastChannel === "undefined" ? undefined : new BroadcastChannel(CONFIRM_CHANNEL);
    if (ch) ch.onmessage = check;
    window.addEventListener("focus", check);
    return () => {
      over = true;
      ch?.close();
      window.removeEventListener("focus", check);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ask, waiting]);

  function finish(a: Ask, ok: boolean) {
    a.resolve(ok);
    setAsk(undefined);
    setPassword("");
    setCode("");
    setError(undefined);
    setWaiting(false);
  }

  if (!ask) return null;
  const done = (ok: boolean) => finish(ask, ok);

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
  const ssoURL = `/auth/login?confirm=1&return=${encodeURIComponent(back)}`;
  function openPopup() {
    const w = 520;
    const h = 720;
    const left = window.screenX + Math.max(0, (window.outerWidth - w) / 2);
    const top = window.screenY + Math.max(0, (window.outerHeight - h) / 3);
    const win = window.open(`${ssoURL}&popup=1`, "hlm-confirm", `popup,width=${w},height=${h},left=${left},top=${top}`);
    if (!win) {
      // Popups blocked: sign in on this page instead, which loses it.
      setError("Your browser blocked the sign-in window. Allow popups for this site and try again, or sign in on this page (what you've entered here will be lost).");
      return;
    }
    win.focus();
    setError(undefined);
    setWaiting(true);
  }
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
            <p className="small no-margin">
              {waiting
                ? "Sign in again in the window that opened. This page stays as it is, and the change goes ahead once you have."
                : "You'll sign in with single sign-on again in a new window. This page stays as it is, and the change goes ahead after."}
            </p>
            {error && <div className="alert error small">{error}</div>}
            <div className="btn-row">
              <button type="button" className="btn primary" onClick={openPopup}>
                {waiting ? "Open the window again" : "Sign in again"}
              </button>
              {error && (
                <a className="btn" href={ssoURL}>
                  Sign in on this page
                </a>
              )}
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
