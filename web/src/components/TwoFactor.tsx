import { useState, type FormEvent } from "react";
import { api } from "../api";
import type { TwoFactorSetup } from "../types";

/** The QR code and secret of a new authenticator entry, and a field for its first code. */
export function TwoFactorSetupForm({
  setup,
  onCode,
  busy,
  error,
}: {
  setup: TwoFactorSetup;
  onCode: (code: string) => void;
  busy?: boolean;
  error?: string;
}) {
  const [code, setCode] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onCode(code);
  };
  return (
    <form className="two-factor-setup" onSubmit={submit}>
      <p className="small no-margin">
        Scan this with an authenticator app (Aegis, 2FAS, Google Authenticator, 1Password, Bitwarden, …), or enter the
        key by hand. hlm made the code itself; the key isn&apos;t sent anywhere.
      </p>
      {/* An SVG hlm drew from the otpauth:// URL. */}
      <div
        className="qr"
        dangerouslySetInnerHTML={{ __html: setup.qr }}
        role="img"
        aria-label="QR code for the authenticator app"
      />
      <p className="small">
        Key: <code className="secret">{setup.secret.replace(/(.{4})/g, "$1 ").trim()}</code>
      </p>
      <label className="field">
        <span className="field-label">The 6-digit code it shows now</span>
        <input
          value={code}
          onChange={(e) => setCode(e.target.value)}
          inputMode="numeric"
          autoComplete="one-time-code"
          pattern="[0-9 ]{6,7}"
          autoFocus
          required
        />
      </label>
      {error && <div className="alert error">{error}</div>}
      <button className="btn primary" type="submit" disabled={busy || code.replace(/\s/g, "").length !== 6}>
        {busy ? "Checking…" : "Turn on two-factor"}
      </button>
    </form>
  );
}

/** Recovery codes, shown once. */
export function RecoveryCodes({
  codes,
  onDone,
  doneLabel = "I've saved them",
}: {
  codes: string[];
  onDone: () => void;
  doneLabel?: string;
}) {
  const text = codes.join("\n") + "\n";
  const save = () => {
    const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "hlm-recovery-codes.txt";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return (
    <div className="recovery-codes">
      <p className="small no-margin">
        <strong>Recovery codes.</strong> Each signs in once instead of a code from the app, if you lose your phone. Keep
        them somewhere safe; they aren&apos;t shown again.
      </p>
      <ol className="codes">
        {codes.map((c) => (
          <li key={c}>
            <code>{c}</code>
          </li>
        ))}
      </ol>
      <div className="btn-row">
        <button type="button" className="btn small" onClick={() => navigator.clipboard?.writeText(text)}>
          Copy
        </button>
        <button type="button" className="btn small" onClick={save}>
          Download
        </button>
        <button type="button" className="btn small primary" onClick={onDone}>
          {doneLabel}
        </button>
      </div>
    </div>
  );
}

/** My account → Two-factor: set it up, new recovery codes, or turn it off. */
export function TwoFactorCard({
  username,
  status,
  onChange,
}: {
  username: string;
  status: { enabled: boolean; recoveryLeft: number; required: boolean; passkeys?: number };
  onChange: () => void;
}) {
  const [setup, setSetup] = useState<TwoFactorSetup>();
  const [codes, setCodes] = useState<string[]>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const start = () => run(async () => setSetup(await api.beginTwoFactor(username)));
  const enable = (code: string) =>
    run(async () => {
      const r = await api.enableTwoFactor(username, code);
      setSetup(undefined);
      setCodes(r.recoveryCodes);
    });
  const regenerate = () => {
    if (!confirm("Make new recovery codes? The old ones stop working.")) return;
    run(async () => setCodes((await api.newRecoveryCodes(username)).recoveryCodes));
  };
  const disable = () => {
    if (
      !confirm(
        (status.passkeys ?? 0) > 0
          ? "Turn off the authenticator app? Your passkeys stay your second factor."
          : "Turn off two-factor? Signing in then takes only your password.",
      )
    )
      return;
    run(async () => {
      await api.disableTwoFactor(username);
      onChange();
    });
  };

  return (
    <div className="two-factor-card">
      <h3>Two-factor sign-in: authenticator app</h3>
      {codes ? (
        <RecoveryCodes
          codes={codes}
          onDone={() => {
            setCodes(undefined);
            onChange();
          }}
        />
      ) : setup ? (
        <>
          <TwoFactorSetupForm setup={setup} onCode={enable} busy={busy} error={error} />
          <button type="button" className="btn small ghost" onClick={() => setSetup(undefined)}>
            Cancel
          </button>
        </>
      ) : status.enabled ? (
        <>
          <p className="small no-margin">
            <strong>On.</strong> Signing in takes your password and a code from your authenticator app.{" "}
            {status.recoveryLeft} recovery code{status.recoveryLeft === 1 ? "" : "s"} left.
            {status.required && " It's required for your account."}
          </p>
          {error && <div className="alert error">{error}</div>}
          <div className="btn-row">
            <button className="btn small" onClick={regenerate} disabled={busy}>
              New recovery codes
            </button>
            <button className="btn small" onClick={start} disabled={busy} title="Move it to a new app or phone">
              Set up again
            </button>
            {(!status.required || (status.passkeys ?? 0) > 0) && (
              <button className="btn small danger" onClick={disable} disabled={busy}>
                Turn off
              </button>
            )}
          </div>
        </>
      ) : (
        <>
          <p className="small no-margin">
            Off. With it on, signing in takes a code from an authenticator app on your phone as well as your password,
            so a leaked password alone isn&apos;t enough.
          </p>
          {error && <div className="alert error">{error}</div>}
          <button className="btn small primary" onClick={start} disabled={busy}>
            Set up two-factor
          </button>
        </>
      )}
    </div>
  );
}
