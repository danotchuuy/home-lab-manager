import { createPasskey, getPasskey, passkeyError, passkeysUnavailable } from "./passkeys";
import { useEffect, useState, type FormEvent } from "react";
import { Link, NavLink, Route, Routes, useLocation } from "react-router-dom";
import { AdminContext } from "./access";
import { api, getToken, setToken, UNAUTHORIZED_EVENT } from "./api";
import { ConfirmPrompt } from "./components/ConfirmPrompt";
import { JobToasts, Shortcuts, ThemeToggle, Toaster } from "./components/Shell";
import { RecoveryCodes, TwoFactorSetupForm } from "./components/TwoFactor";
import { usePoll } from "./usePoll";
import { CatalogPage } from "./pages/Catalog";
import { ConnectionDetailPage } from "./pages/ConnectionDetail";
import { HostPage } from "./pages/Host";
import { ProxyPage } from "./pages/Proxy";
import { TunnelPage } from "./pages/Tunnel";
import { BackupsPage } from "./pages/Backups";
import { InstallPage } from "./pages/Install";
import { ImportServerPage } from "./pages/ImportServer";
import { AdoptPage } from "./pages/Adopt";
import { CloneServerPage } from "./pages/CloneServer";
import { MoveServerPage } from "./pages/MoveServer";
import { StackAdoptPage } from "./pages/StackAdopt";
import { StackEditorPage } from "./pages/StackEditor";
import { ServerDetailPage } from "./pages/ServerDetail";
import { ServersPage } from "./pages/Servers";
import { SystemPage } from "./pages/System";
import type { AuthStatus, Status, SystemInfo, TwoFactorSetup } from "./types";

const DISMISSED_KEY = "hlm.dismissedUpdate";
/** DISMISSED_KEY before the short name changed from gsh to hlm. */
const LEGACY_DISMISSED_KEY = "gsh.dismissedUpdate";

function dismissedVersion(): string {
  try {
    return localStorage.getItem(DISMISSED_KEY) ?? localStorage.getItem(LEGACY_DISMISSED_KEY) ?? "";
  } catch {
    return "";
  }
}

export function App() {
  const [status, setStatus] = useState<Status>();
  const [needToken, setNeedToken] = useState(false);
  const [auth, setAuth] = useState<AuthStatus>();
  const [system, setSystem] = useState<SystemInfo>();
  const [dismissed, setDismissed] = useState(dismissedVersion);
  const location = useLocation();
  const admin = auth?.role === "admin";

  useEffect(() => {
    api.authStatus().then(setAuth, () => {});
    const onUnauthorized = () => {
      // Accounts or single sign-on may have been set up since the page loaded.
      api.authStatus().then(setAuth, () => {});
      setNeedToken(true);
    };
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, []);
  usePoll(
    () =>
      Promise.all([
        api.status().then(setStatus, () => setStatus(undefined)),
        api.system().then(setSystem, () => {}),
      ]),
    30000,
  );

  return (
    <AdminContext.Provider value={admin}>
    <div className="layout">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="topbar">
        <div className="brand">🏠 Home Lab Manager</div>
        <nav>
          <NavLink to="/" end>
            Servers
          </NavLink>
          <NavLink to="/backups">Backups</NavLink>
          {admin && (
            <>
              <NavLink to="/catalog">Catalog</NavLink>
              <NavLink to="/host">Host</NavLink>
              <NavLink to="/proxy">Proxy</NavLink>
              <NavLink to="/tunnel">Tunnel</NavLink>
            </>
          )}
        </nav>
        <div className="spacer" />
        {!needToken && <DockerStatus status={status} />}
        <ThemeToggle />
        {auth?.user && <UserMenu user={auth.user.name || auth.user.username || auth.user.email || auth.user.sub} />}
        {system && (
          <NavLink to="/system" className="version-link" title="Version and updates">
            {system.version}
            {admin && system.update.available && <span className="update-dot" aria-label="update available" />}
          </NavLink>
        )}
      </header>

      {admin &&
        system?.update.available &&
        system.update.latest &&
        dismissed !== system.update.latest.version &&
        location.pathname !== "/system" && (
          <div className="update-banner">
            <span>
              Home Lab Manager <strong>{system.update.latest.version}</strong> is available (you have{" "}
              {system.version}).
            </span>
            <Link to="/system" className="btn small primary">
              View update
            </Link>
            <button
              className="btn small ghost"
              onClick={() => {
                const v = system.update.latest!.version;
                try {
                  localStorage.setItem(DISMISSED_KEY, v);
                } catch {
                  // Not persisted; the banner comes back on the next visit.
                }
                setDismissed(v);
              }}
            >
              Dismiss
            </button>
          </div>
        )}

      {needToken && <SignInPrompt auth={auth} onDone={() => window.location.reload()} />}
      <ConfirmPrompt />
      <Toaster />
      <JobToasts enabled={!needToken} />
      <Shortcuts admin={admin} />

      <main id="main">
        <Routes>
          <Route path="/" element={<ServersPage />} />
          <Route path="/catalog" element={<CatalogPage />} />
          <Route path="/catalog/:id" element={<InstallPage />} />
          <Route path="/servers/:name" element={<ServerDetailPage />} />
          <Route path="/servers/:name/clone" element={<CloneServerPage />} />
          <Route path="/servers/:name/move" element={<MoveServerPage />} />
          <Route path="/import" element={<ImportServerPage />} />
          <Route path="/adopt" element={<AdoptPage />} />
          <Route path="/stacks/new" element={<StackEditorPage />} />
          <Route path="/stacks/adopt" element={<StackAdoptPage />} />
          <Route path="/stacks/:name" element={<StackEditorPage />} />
          <Route path="/connections/:name" element={<ConnectionDetailPage />} />
          <Route path="/backups" element={<BackupsPage />} />
          <Route path="/host" element={<HostPage />} />
          <Route path="/system" element={<SystemPage />} />
          <Route path="/proxy" element={<ProxyPage />} />
          <Route path="/tunnel" element={<TunnelPage />} />
          <Route path="*" element={<div className="page">Not found.</div>} />
        </Routes>
      </main>
    </div>
    </AdminContext.Provider>
  );
}

function DockerStatus({ status }: { status?: Status }) {
  if (!status) return <span className="docker-status off">Backend unreachable</span>;
  if (!status.docker.ok) {
    return (
      <span className="docker-status off" title={[status.docker.host, status.docker.error].filter(Boolean).join("\n")}>
        Docker unreachable
      </span>
    );
  }
  return (
    <span
      className="docker-status on"
      title={[
        status.docker.host && `Host: ${status.docker.host}`,
        status.appdataRoot && `Appdata: ${status.appdataRoot}`,
        `Catalog: ${status.catalog.apps} apps`,
      ]
        .filter(Boolean)
        .join("\n")}
    >
      Docker {status.docker.version}
    </span>
  );
}

/** Where to sign in with the provider, coming back to this page. */
function loginUrl(): string {
  const here = window.location.pathname + window.location.search + window.location.hash;
  return `/auth/login?return=${encodeURIComponent(here)}`;
}

/**
 * Asks to sign in when the API refuses a request: with a local account, through
 * single sign-on, or with an API key or HLM_TOKEN.
 */
function SignInPrompt({ auth, onDone }: { auth?: AuthStatus; onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [token, setTokenValue] = useState(getToken());
  const local = !!auth?.local;
  const oidc = auth?.oidc ? auth.oidcName || "single sign-on" : undefined;
  // Before the status arrives, or with nothing else to offer, ask for a token straight away.
  const tokenOnly = !local && !oidc;
  const [showToken, setShowToken] = useState(false);
  // After the password: a code, or setting two-factor up (then its recovery codes).
  const [step, setStep] = useState<"password" | "code" | "setup">("password");
  // The account's second factors, after the password.
  const [factors, setFactors] = useState<{ code: boolean; passkey: boolean }>({ code: true, passkey: false });
  const passkeyProblem = passkeysUnavailable();
  const [code, setCode] = useState("");
  const [setup, setSetup] = useState<TwoFactorSetup>();
  const [recovery, setRecovery] = useState<string[]>();

  async function attempt(fn: () => Promise<void>) {
    setBusy(true);
    setError(undefined);
    try {
      await fn();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const signIn = (e: FormEvent) => {
    e.preventDefault();
    attempt(async () => {
      const r = await api.login(username.trim(), password);
      if (r.twoFactor) {
        setFactors({ code: r.code !== false, passkey: !!r.passkey });
        setStep("code");
      } else if (r.twoFactorSetup) {
        setSetup(await api.loginSetup());
        setStep("setup");
      } else onDone();
    });
  };

  const sendCode = (e: FormEvent) => {
    e.preventDefault();
    attempt(async () => {
      await api.loginCode(code);
      onDone();
    });
  };

  // Passkeys: alone, as the second factor, or added when two-factor is required.
  const passkeyAlone = () =>
    attempt(async () => {
      try {
        await api.passkeyLogin(await getPasskey(await api.beginPasskeyLogin()));
      } catch (e) {
        throw new Error(passkeyError(e));
      }
      onDone();
    });
  const passkeySecond = () =>
    attempt(async () => {
      try {
        await api.passkeySecond(await getPasskey(await api.beginPasskeySecond()));
      } catch (e) {
        throw new Error(passkeyError(e));
      }
      onDone();
    });
  const passkeySetup = () =>
    attempt(async () => {
      try {
        await api.passkeySetup({ name: "Passkey", ...(await createPasskey(await api.beginPasskeySetup())) });
      } catch (e) {
        throw new Error(passkeyError(e));
      }
      onDone();
    });

  const enable = (c: string) =>
    attempt(async () => {
      setRecovery((await api.loginEnable(c)).recoveryCodes);
    });

  if (step === "code" || step === "setup") {
    return (
      <div className="modal-backdrop">
        <div className="card modal">
          <h2>{step === "code" ? "Two-factor sign-in" : "Set up two-factor sign-in"}</h2>
          {step === "code" && factors.passkey && (
            <div className="sign-in-form">
              <button type="button" className="btn primary sign-in-button" onClick={passkeySecond} disabled={busy}>
                {busy ? "Waiting for the passkey…" : "Use your passkey"}
              </button>
              {passkeyProblem && <p className="muted small no-margin">{passkeyProblem}</p>}
              {!factors.code && error && <div className="alert error">{error}</div>}
            </div>
          )}
          {step === "code" && factors.passkey && factors.code && <div className="sign-in-or">or</div>}
          {step === "code" && !factors.code ? null : step === "code" ? (
            <form className="sign-in-form" onSubmit={sendCode}>
              <label className="field">
                <span className="field-label">Code from your authenticator app</span>
                <input
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoFocus
                  required
                />
                <span className="field-help">Lost your phone? Enter one of your recovery codes instead.</span>
              </label>
              {error && <div className="alert error">{error}</div>}
              <button
                className={`btn sign-in-button${factors.passkey ? "" : " primary"}`}
                type="submit"
                disabled={busy || !code.trim()}
              >
                {busy ? "Checking…" : "Sign in"}
              </button>
            </form>
          ) : recovery ? (
            <RecoveryCodes codes={recovery} onDone={onDone} doneLabel="I've saved them; continue" />
          ) : (
            <>
              <p className="muted small">
                Your account has to use two-factor sign-in (an admin requires it), so set it up now to finish signing in:
                a passkey, or an authenticator app.
              </p>
              {!passkeyProblem && (
                <>
                  <button type="button" className="btn primary" onClick={passkeySetup} disabled={busy}>
                    Add a passkey
                  </button>
                  <div className="sign-in-or">or</div>
                </>
              )}
              {setup && <TwoFactorSetupForm setup={setup} onCode={enable} busy={busy} error={error} />}
            </>
          )}
          <button
            type="button"
            className="link-button"
            onClick={() => {
              setStep("password");
              setCode("");
              setError(undefined);
            }}
          >
            Start over
          </button>
        </div>
      </div>
    );
  }

  function useToken(e: FormEvent) {
    e.preventDefault();
    setToken(token.trim());
    onDone();
  }

  return (
    <div className="modal-backdrop">
      <div className="card modal">
        <h2>Sign in</h2>
        {local && (
          <form className="sign-in-form" onSubmit={signIn}>
            <label className="field">
              <span className="field-label">Username</span>
              <input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" autoFocus required />
            </label>
            <label className="field">
              <span className="field-label">Password</span>
              <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
            </label>
            {error && <div className="alert error">{error}</div>}
            <button className="btn primary sign-in-button" type="submit" disabled={busy}>
              {busy ? "Signing in…" : "Sign in"}
            </button>
          </form>
        )}
        {local && !passkeyProblem && (
          <button type="button" className="btn sign-in-button" onClick={passkeyAlone} disabled={busy}>
            Sign in with a passkey
          </button>
        )}
        {local && oidc && <div className="sign-in-or">or</div>}
        {oidc && (
          <a className={`btn sign-in-button${local ? "" : " primary"}`} href={loginUrl()}>
            Sign in with {oidc}
          </a>
        )}
        {tokenOnly || showToken ? (
          <form className="sign-in-form" onSubmit={useToken}>
            <p className="muted small no-margin">
              {tokenOnly
                ? "Enter an API key, or the token set in HLM_TOKEN, to continue."
                : "An API key, or the token set in HLM_TOKEN:"}
            </p>
            <input type="password" value={token} onChange={(e) => setTokenValue(e.target.value)} autoFocus={tokenOnly} aria-label="API key or token" />
            <div className="form-footer">
              <div className="spacer" />
              <button className={`btn${tokenOnly ? " primary" : ""}`} type="submit">
                Use it
              </button>
            </div>
          </form>
        ) : (
          <button type="button" className="link-button" onClick={() => setShowToken(true)}>
            Use an API key or HLM_TOKEN instead
          </button>
        )}
      </div>
    </div>
  );
}

/** The signed-in user, with a way to sign out. */
function UserMenu({ user }: { user: string }) {
  async function signOut() {
    try {
      await api.logout();
    } finally {
      window.location.reload();
    }
  }
  return (
    <span className="user-menu">
      <span title="Signed in">{user}</span>
      <button type="button" className="link-button" onClick={signOut}>
        Sign out
      </button>
    </span>
  );
}
