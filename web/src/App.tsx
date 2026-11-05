import { useEffect, useState, type FormEvent } from "react";
import { Link, NavLink, Route, Routes, useLocation } from "react-router-dom";
import { AdminContext } from "./access";
import { api, getToken, setToken, UNAUTHORIZED_EVENT } from "./api";
import { ConfirmPrompt } from "./components/ConfirmPrompt";
import { usePoll } from "./usePoll";
import { CatalogPage } from "./pages/Catalog";
import { ConnectionDetailPage } from "./pages/ConnectionDetail";
import { HostPage } from "./pages/Host";
import { ProxyPage } from "./pages/Proxy";
import { TunnelPage } from "./pages/Tunnel";
import { InstallPage } from "./pages/Install";
import { ImportServerPage } from "./pages/ImportServer";
import { CloneServerPage } from "./pages/CloneServer";
import { ServerDetailPage } from "./pages/ServerDetail";
import { ServersPage } from "./pages/Servers";
import { SystemPage } from "./pages/System";
import type { AuthStatus, Status, SystemInfo } from "./types";

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
      <header className="topbar">
        <div className="brand">🏠 Home Lab Manager</div>
        <nav>
          <NavLink to="/" end>
            Servers
          </NavLink>
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

      <main>
        <Routes>
          <Route path="/" element={<ServersPage />} />
          <Route path="/catalog" element={<CatalogPage />} />
          <Route path="/catalog/:id" element={<InstallPage />} />
          <Route path="/servers/:name" element={<ServerDetailPage />} />
          <Route path="/servers/:name/clone" element={<CloneServerPage />} />
          <Route path="/import" element={<ImportServerPage />} />
          <Route path="/connections/:name" element={<ConnectionDetailPage />} />
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

  async function signIn(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.login(username.trim(), password);
      onDone();
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
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
