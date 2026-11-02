import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { AuthStatus, ServerGroup } from "../types";
import { Spinner } from "./common";
import { SSHKeys } from "./SSHKeys";

const MIN_PASSWORD = 10;

/** What someone who isn't an admin sees of their own account: their groups and their password. */
export function MyAccount() {
  const [status, setStatus] = useState<AuthStatus>();
  const [groups, setGroups] = useState<ServerGroup[]>([]);
  const [current, setCurrent] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string }>();

  useEffect(() => {
    api.authStatus().then(setStatus, (e) => setResult({ ok: false, text: (e as Error).message }));
    api.groups().then(setGroups, () => {});
  }, []);

  async function change(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setResult(undefined);
    try {
      await api.changePassword(status!.user!.username!, password, current);
      setCurrent("");
      setPassword("");
      setResult({ ok: true, text: "Changed your password. Your other sessions have ended." });
    } catch (err) {
      setResult({ ok: false, text: (err as Error).message });
    } finally {
      setBusy(false);
    }
  }

  if (!status) return result ? <div className="alert error">{result.text}</div> : <Spinner />;
  const user = status.user;
  return (
    <>
      <p className="muted">
        {user ? `Signed in as ${user.name || user.username || user.email}.` : "Signed in with an API key."} Your server
        groups: {groups.length > 0 ? groups.map((g) => g.name).join(", ") : "none yet; ask an admin."}
      </p>
      {user?.method === "local" && (
        <form className="inline-form" onSubmit={change}>
          <label className="field">
            <span className="field-label">Current password</span>
            <input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" required />
          </label>
          <label className="field">
            <span className="field-label">New password</span>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={MIN_PASSWORD} autoComplete="new-password" required />
          </label>
          <button className="btn small primary" type="submit" disabled={busy || password.length < MIN_PASSWORD || !current}>
            {busy ? "Saving…" : "Change password"}
          </button>
        </form>
      )}
      {result && <div className={`alert ${result.ok ? "success" : "error"}`}>{result.text}</div>}
      {user?.method === "local" && user.username && (
        <>
          <h3>SSH keys</h3>
          <SSHKeys username={user.username} />
        </>
      )}
    </>
  );
}
