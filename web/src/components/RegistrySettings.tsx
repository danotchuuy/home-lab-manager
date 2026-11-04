import { useEffect, useState } from "react";
import { api } from "../api";
import type { RegistryForm, RegistryLogin } from "../types";
import { Spinner } from "./common";

const EMPTY: RegistryForm = { host: "", username: "", password: "", deployers: false, image: "" };

/** System → Registries: logins for private image registries. */
export function RegistrySettings() {
  const [list, setList] = useState<RegistryLogin[]>();
  const [error, setError] = useState<string>();
  // The host being edited; "" for a new login.
  const [editing, setEditing] = useState<string>();

  const load = () => api.registries().then(setList, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  async function remove(host: string) {
    if (!confirm(`Remove the login for ${host}? Pulls and update checks for its private images will fail.`)) return;
    try {
      await api.deleteRegistry(host);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <div className="registries">
      <p className="muted small no-margin">
        Logins for private images: a private Docker Hub repository, GHCR, or your own registry. Installs, updates, update
        checks and image imports use the login for the image&apos;s registry. A Docker Hub login also raises Docker
        Hub&apos;s pull limit. Passwords and tokens are kept encrypted and only ever sent to Docker, for that registry;
        they&apos;re never shown again.
      </p>
      {error && <div className="alert error small">{error}</div>}
      {!list ? (
        <Spinner />
      ) : list.length === 0 ? (
        <p className="muted small">No registry logins: only public images can be pulled.</p>
      ) : (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Registry</th>
                <th>Username</th>
                <th>Deployers</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={r.host}>
                  <td>
                    <code>{r.host}</code>
                  </td>
                  <td>{r.username}</td>
                  <td>{r.deployers ? "Allowed" : <span className="muted">No</span>}</td>
                  <td className="actions">
                    <button className="btn small" onClick={() => setEditing(r.host)}>
                      Change
                    </button>
                    <button className="btn small danger" onClick={() => remove(r.host)}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {editing === undefined ? (
        <div className="btn-row">
          <button className="btn small" onClick={() => setEditing("")}>
            Add a registry login
          </button>
        </div>
      ) : (
        <RegistryEditor
          key={editing}
          existing={list?.find((r) => r.host === editing)}
          onDone={(saved) => {
            setEditing(undefined);
            if (saved) load();
          }}
        />
      )}
    </div>
  );
}

function RegistryEditor({ existing, onDone }: { existing?: RegistryLogin; onDone: (saved: boolean) => void }) {
  const [form, setForm] = useState<RegistryForm>(existing ? { ...EMPTY, ...existing } : EMPTY);
  const [busy, setBusy] = useState<"save" | "test">();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const set = <K extends keyof RegistryForm>(k: K, v: RegistryForm[K]) => setForm((f) => ({ ...f, [k]: v }));

  async function save() {
    setBusy("save");
    setError(undefined);
    try {
      const { image: _image, ...body } = form;
      if (existing) await api.updateRegistry(existing.host, body);
      else await api.addRegistry(body);
      onDone(true);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function test() {
    setBusy("test");
    setError(undefined);
    setNotice(undefined);
    try {
      const r = await api.testRegistry(form);
      setNotice(r.digest ? `${r.status}. Found ${form.image} (${r.digest.slice(0, 19)}…).` : `${r.status}.`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const host = form.host.trim() || "registry.example.com";
  return (
    <div className="config-section registry-editor">
      <h3>{existing ? `Change ${existing.host}` : "Add a registry login"}</h3>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Registry</span>
          <input value={form.host} onChange={(e) => set("host", e.target.value)} placeholder="docker.io, ghcr.io or registry.lan:5000" autoFocus={!existing} />
          <span className="field-help">The host in the image&apos;s name. Images without one, like itzg/minecraft-server, are on docker.io.</span>
        </label>
        <label className="field">
          <span className="field-label">Username</span>
          <input value={form.username} onChange={(e) => set("username", e.target.value)} autoComplete="off" />
        </label>
        <label className="field">
          <span className="field-label">Password or access token</span>
          <input
            type="password"
            value={form.password}
            onChange={(e) => set("password", e.target.value)}
            autoComplete="new-password"
            placeholder={existing ? "Leave blank to keep the saved one" : ""}
          />
          <span className="field-help">
            Prefer a read-only token: on Docker Hub an access token with &ldquo;Read-only&rdquo;, on GitHub a classic token with
            read:packages.
          </span>
        </label>
        <label className="field">
          <span className="field-label">Image to try (optional)</span>
          <input value={form.image} onChange={(e) => set("image", e.target.value)} placeholder={`${host}/team/app:latest`} />
          <span className="field-help">Test looks it up with this login, to check it can see the image, not just sign in.</span>
        </label>
      </div>
      <label className="radio small">
        <input type="checkbox" checked={form.deployers} onChange={(e) => set("deployers", e.target.checked)} />
        Allow deployer keys and members who deploy servers to install images with this login
      </label>
      <p className="muted small">
        Anyone who can update a server already on this registry updates it with this login, whether or not that&apos;s
        ticked.
      </p>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button className="btn small primary" onClick={save} disabled={!!busy || !form.host.trim() || !form.username.trim() || (!existing && !form.password)}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button className="btn small" onClick={test} disabled={!!busy || !form.host.trim() || (!existing && !form.password)}>
          {busy === "test" ? "Testing…" : "Test"}
        </button>
        <button className="btn small ghost" onClick={() => onDone(false)} disabled={!!busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}
