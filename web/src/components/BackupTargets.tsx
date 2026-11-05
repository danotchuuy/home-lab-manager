import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { BackupTarget } from "../types";

interface Form {
  id?: string;
  name: string;
  endpoint: string;
  bucket: string;
  accessKey: string;
  secretKey: string;
  prefix: string;
  region: string;
  virtualHost: boolean;
  caCert: string;
  encrypt: boolean;
}

const blank: Form = {
  name: "",
  endpoint: "",
  bucket: "",
  accessKey: "",
  secretKey: "",
  prefix: "",
  region: "",
  virtualHost: false,
  caCert: "",
  encrypt: true,
};

const toForm = (t: BackupTarget): Form => ({
  id: t.id,
  name: t.name,
  endpoint: t.endpoint ?? "",
  bucket: t.bucket ?? "",
  accessKey: t.accessKey ?? "",
  secretKey: "",
  prefix: t.prefix ?? "",
  region: t.region ?? "",
  virtualHost: t.virtualHost,
  caCert: t.caCert ?? "",
  encrypt: t.encrypt,
});

/** Shows a new encryption key once, with what to do with it. */
function KeyNotice({ name, k, onClose }: { name: string; k: string; onClose: () => void }) {
  return (
    <div className="alert warn key-notice">
      <p className="no-margin">
        <strong>{name}&apos;s encryption key</strong>. Keep a copy somewhere safe, away from this machine (a password
        manager, say): without it, backups there can&apos;t be read if this hlm and its configuration are lost.
      </p>
      <code className="key">{k}</code>
      <p className="small no-margin">
        Decrypt a downloaded backup without hlm: <code>hlm decrypt-backup KEY backup.tar.gz plain.tar.gz</code>. hlm
        keeps the key in its encrypted settings, so its configuration backups include it.
      </p>
      <div className="btn-row">
        <button className="btn small" onClick={() => navigator.clipboard?.writeText(k)}>
          Copy
        </button>
        <button className="btn small ghost" onClick={onClose}>
          I&apos;ve saved it
        </button>
      </div>
    </div>
  );
}

/**
 * System → Backup storage, below the primary: the primary's encryption, and
 * other storage that plans can also copy backups to.
 */
export function BackupTargets({ version, onChange }: { version: number; onChange?: () => void }) {
  const [targets, setTargets] = useState<BackupTarget[]>();
  const [form, setForm] = useState<Form>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [shownKey, setShownKey] = useState<{ name: string; key: string }>();

  const load = useCallback(() => {
    api.backupTargets().then(setTargets, (e) => setError((e as Error).message));
  }, []);
  useEffect(load, [load, version]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  if (!targets || targets.length === 0) return error ? <div className="alert error">{error}</div> : null;
  const [primary, ...others] = targets;

  const save = (f: Form) =>
    run("save", async () => {
      const t = await api.saveBackupTarget(f);
      if (t.newKey) setShownKey({ name: t.name, key: t.newKey });
      setForm(undefined);
      load();
      onChange?.();
    });

  const setPrimaryEncrypt = (on: boolean) => {
    if (
      !on &&
      !confirm(
        "Stop encrypting new backups? Backups already encrypted stay encrypted, and are still read with the key.",
      )
    )
      return;
    save({ ...toForm(primary), encrypt: on });
  };

  const remove = (t: BackupTarget) => {
    if (!confirm(`Remove ${t.name}? Plans stop copying there; the backups already in it are kept.`)) return;
    run("remove:" + t.id, async () => {
      await api.deleteBackupTarget(t.id);
      load();
      onChange?.();
    });
  };

  const showKey = (t: BackupTarget) =>
    run("key:" + t.id, async () => setShownKey({ name: t.name, key: (await api.backupTargetKey(t.id)).key }));

  const set = <K extends keyof Form>(k: K, v: Form[K]) => setForm((f) => f && { ...f, [k]: v });

  return (
    <div className="backup-targets">
      <div className="settings-actions">
        <div>
          <h3>Encryption</h3>
          <p className="muted small no-margin">
            {primary.encrypt
              ? "New backups here are encrypted (AES-256-GCM) before they're stored."
              : "Backups here are stored as plain .tar.gz files. Encrypting them protects them from anyone who can read the bucket."}
          </p>
        </div>
        <div className="btn-row">
          <button className="btn small" disabled={!!busy} onClick={() => setPrimaryEncrypt(!primary.encrypt)}>
            {primary.encrypt ? "Stop encrypting" : "Encrypt backups"}
          </button>
          {primary.hasKey && (
            <button className="btn small ghost" disabled={!!busy} onClick={() => showKey(primary)}>
              Show key
            </button>
          )}
        </div>
      </div>

      <h3>Other backup storage</h3>
      <p className="muted small">
        Somewhere else to keep a second copy, such as an off-site bucket (Backblaze B2, Cloudflare R2, AWS S3, another
        SeaweedFS). Each server&apos;s Backups tab chooses whether to copy there, and how many copies to keep.
      </p>
      {others.length > 0 && (
        <ul className="target-list">
          {others.map((t) => (
            <li key={t.id}>
              <strong>{t.name}</strong>{" "}
              <code className="small">
                {t.endpoint} · {t.bucket}/{t.prefix}/
              </code>
              {t.encrypt && <span className="badge"> encrypted</span>}
              <span className="btn-row">
                <button className="btn ghost small" onClick={() => setForm(toForm(t))} disabled={!!busy}>
                  Change
                </button>
                {t.hasKey && (
                  <button className="btn ghost small" onClick={() => showKey(t)} disabled={!!busy}>
                    Show key
                  </button>
                )}
                <button className="btn ghost small danger" onClick={() => remove(t)} disabled={!!busy}>
                  Remove
                </button>
              </span>
            </li>
          ))}
        </ul>
      )}
      {shownKey && <KeyNotice name={shownKey.name} k={shownKey.key} onClose={() => setShownKey(undefined)} />}
      {error && <div className="alert error">{error}</div>}
      {!form ? (
        <button className="btn small" onClick={() => setForm(blank)}>
          + Add backup storage
        </button>
      ) : (
        <div className="config-form target-form">
          <div className="field-grid">
            <label className="field">
              <span className="field-label">Name</span>
              <input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="Off-site" />
            </label>
            <label className="field">
              <span className="field-label">Endpoint URL</span>
              <input
                value={form.endpoint}
                onChange={(e) => set("endpoint", e.target.value)}
                placeholder="https://s3.eu-central-003.backblazeb2.com"
                autoComplete="off"
              />
            </label>
            <label className="field">
              <span className="field-label">Bucket</span>
              <input value={form.bucket} onChange={(e) => set("bucket", e.target.value)} autoComplete="off" />
            </label>
            <label className="field">
              <span className="field-label">Folder in the bucket</span>
              <input value={form.prefix} onChange={(e) => set("prefix", e.target.value)} placeholder="hlm" />
            </label>
            <label className="field">
              <span className="field-label">Access key</span>
              <input value={form.accessKey} onChange={(e) => set("accessKey", e.target.value)} autoComplete="off" />
            </label>
            <label className="field">
              <span className="field-label">Secret key</span>
              <input
                type="password"
                value={form.secretKey}
                onChange={(e) => set("secretKey", e.target.value)}
                placeholder={form.id ? "Unchanged" : ""}
                autoComplete="off"
              />
            </label>
            <label className="field">
              <span className="field-label">Region</span>
              <input value={form.region} onChange={(e) => set("region", e.target.value)} placeholder="us-east-1" />
            </label>
          </div>
          <label className="radio small">
            <input type="checkbox" checked={form.virtualHost} onChange={(e) => set("virtualHost", e.target.checked)} />
            Virtual-hosted buckets (<code>bucket.host</code>), as some cloud storage wants
          </label>
          <label className="radio small">
            <input type="checkbox" checked={form.encrypt} onChange={(e) => set("encrypt", e.target.checked)} />
            Encrypt backups before they&apos;re stored here (recommended for storage you don&apos;t run yourself)
          </label>
          <div className="btn-row">
            <button
              className="btn small primary"
              onClick={() => save(form)}
              disabled={!!busy || !form.name.trim() || !form.endpoint.trim() || !form.bucket.trim()}
            >
              {busy === "save" ? "Checking and saving…" : "Save"}
            </button>
            <button className="btn small ghost" onClick={() => setForm(undefined)}>
              Cancel
            </button>
          </div>
          <p className="muted small">Saving checks hlm can list, write and delete there first.</p>
        </div>
      )}
    </div>
  );
}
