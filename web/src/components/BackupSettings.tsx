import { useEffect, useState } from "react";
import { api } from "../api";
import type { BackupSettings as Settings, BackupSettingsInput } from "../types";
import { Spinner } from "./common";

const empty: BackupSettingsInput = {
  endpoint: "",
  region: "",
  bucket: "",
  accessKey: "",
  secretKey: "",
  virtualHost: false,
  prefix: "",
  caCert: "",
};

function toInput(s: Settings): BackupSettingsInput {
  return {
    endpoint: s.endpoint ?? "",
    region: s.region ?? "",
    bucket: s.bucket ?? "",
    accessKey: s.accessKey ?? "",
    secretKey: "",
    virtualHost: s.virtualHost,
    prefix: s.prefix ?? "",
    caCert: s.caCert ?? "",
  };
}

/**
 * Reads a certificate file as PEM. Binary (DER) files, often named .cer or
 * .der, are wrapped as PEM; the server checks either way.
 */
async function readCertFile(file: File): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer());
  const text = new TextDecoder().decode(bytes);
  if (text.includes("-----BEGIN")) return text.trim() + "\n";
  let bin = "";
  bytes.forEach((b) => (bin += String.fromCharCode(b)));
  const b64 = btoa(bin).replace(/.{64}/g, "$&\n");
  return `-----BEGIN CERTIFICATE-----\n${b64.trim()}\n-----END CERTIFICATE-----\n`;
}

/**
 * Connects hlm to an S3-compatible bucket (SeaweedFS, MinIO, AWS S3, …) that
 * servers' backups go to. The secret key never comes back from the server.
 */
export function BackupSettings({ onChange }: { onChange?: () => void }) {
  const [status, setStatus] = useState<Settings>();
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState<BackupSettingsInput>(empty);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  useEffect(() => {
    api.backupSettings().then(
      (s) => {
        setStatus(s);
        setForm(toInput(s));
      },
      (e) => setError((e as Error).message),
    );
  }, []);

  const set = <K extends keyof BackupSettingsInput>(k: K, v: BackupSettingsInput[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNotice(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const test = () =>
    run("test", async () => {
      const { backups } = await api.testBackupSettings(form);
      setNotice(
        `Connected: hlm can list, write and delete in the bucket.${
          backups ? ` It already holds ${backups} backup${backups === 1 ? "" : "s"}.` : ""
        }`,
      );
    });

  const save = () =>
    run("save", async () => {
      const s = await api.saveBackupSettings(form);
      setStatus(s);
      setForm(toInput(s));
      setEditing(false);
      setNotice("Saved. Choose what to back up on each server's Backups tab.");
      onChange?.();
    });

  const remove = () => {
    if (!confirm("Disconnect backup storage? Scheduled backups stop; the backups already in the bucket are kept."))
      return;
    run("remove", async () => {
      await api.deleteBackupSettings();
      setStatus(await api.backupSettings());
      setForm(empty);
      onChange?.();
    });
  };

  if (!status) return error ? <div className="alert error">{error}</div> : <Spinner />;

  const messages = (
    <>
      {status.error && <div className="alert error">{status.error}</div>}
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}
    </>
  );

  if (status.configured && !editing) {
    return (
      <div>
        <dl className="facts">
          <dt>Endpoint</dt>
          <dd>
            <code>{status.endpoint}</code>
            {status.virtualHost && <span className="muted"> · virtual-hosted buckets</span>}
          </dd>
          <dt>Bucket</dt>
          <dd>
            <code>
              {status.bucket}/{status.prefix}/
            </code>
            <span className="muted"> · region {status.region || "us-east-1"}</span>
          </dd>
          <dt>Access key</dt>
          <dd>
            <code>{status.accessKey}</code> <span className="muted">(secret stored encrypted)</span>
          </dd>
          {status.caCerts?.length ? (
            <>
              <dt>Trusted CA</dt>
              <dd>
                {status.caCerts.map((c) => (
                  <div key={c.subject + c.notAfter}>
                    <code>{c.subject}</code>
                    <span className={new Date(c.notAfter) < new Date() ? "form-error" : "muted"}>
                      {" "}
                      · {new Date(c.notAfter) < new Date() ? "expired" : "expires"}{" "}
                      {new Date(c.notAfter).toLocaleDateString()}
                    </span>
                  </div>
                ))}
              </dd>
            </>
          ) : null}
        </dl>
        {messages}
        <div className="btn-row">
          <button className="btn small" onClick={() => setEditing(true)} disabled={!!busy}>
            Change
          </button>
          <button className="btn small danger" onClick={remove} disabled={!!busy}>
            {busy === "remove" ? "Removing…" : "Disconnect"}
          </button>
        </div>
      </div>
    );
  }

  const ready =
    form.endpoint.trim() && form.bucket.trim() && form.accessKey.trim() && (form.secretKey || status.hasSecret);

  return (
    <div className="config-form">
      <p className="muted small no-margin">
        Back up folders from game servers to S3-compatible storage, such as SeaweedFS, MinIO, Garage or AWS S3. Each
        backup is a <code>.tar.gz</code> of the folders you choose, stored as{" "}
        <code>
          {form.prefix.trim().replace(/^\/+|\/+$/g, "") || "hlm"}/&lt;server&gt;/&lt;server&gt;-&lt;time&gt;.tar.gz
        </code>
        .
      </p>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Endpoint URL</span>
          <input
            value={form.endpoint}
            onChange={(e) => set("endpoint", e.target.value)}
            placeholder="http://seaweedfs.lan:8333"
            autoComplete="off"
          />
          <span className="field-help">
            The S3 API&apos;s address. SeaweedFS serves it on port 8333 (weed s3 / weed server -s3).
          </span>
        </label>
        <label className="field">
          <span className="field-label">Bucket</span>
          <input
            value={form.bucket}
            onChange={(e) => set("bucket", e.target.value)}
            placeholder="game-backups"
            autoComplete="off"
          />
          <span className="field-help">It must exist already.</span>
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
            placeholder={status.hasSecret ? "Unchanged" : ""}
            autoComplete="off"
          />
          <span className="field-help">Stored encrypted on this machine and never shown again.</span>
        </label>
        <label className="field">
          <span className="field-label">Folder in the bucket</span>
          <input
            value={form.prefix}
            onChange={(e) => set("prefix", e.target.value)}
            placeholder="hlm"
            autoComplete="off"
          />
        </label>
        <label className="field">
          <span className="field-label">Region</span>
          <input
            value={form.region}
            onChange={(e) => set("region", e.target.value)}
            placeholder="us-east-1"
            autoComplete="off"
          />
          <span className="field-help">SeaweedFS and MinIO accept any region; leave it empty.</span>
        </label>
      </div>
      <div className="field ca-cert">
        <span className="field-label">Private CA certificate (optional)</span>
        <textarea
          value={form.caCert}
          onChange={(e) => set("caCert", e.target.value)}
          rows={form.caCert ? 6 : 3}
          spellCheck={false}
          placeholder={"-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----"}
        />
        <div className="btn-row">
          <label className="btn small ghost">
            Upload certificate…
            <input
              type="file"
              accept=".crt,.pem,.cer,.der,application/x-x509-ca-cert,application/pkix-cert"
              hidden
              onChange={async (e) => {
                const file = e.target.files?.[0];
                e.target.value = "";
                if (file) set("caCert", await readCertFile(file));
              }}
            />
          </label>
          {form.caCert && (
            <button className="btn small ghost" onClick={() => set("caCert", "")}>
              Clear
            </button>
          )}
        </div>
        <span className="field-help">
          For an <code>https://</code> endpoint whose certificate your own CA signed: paste the CA&apos;s certificate
          (PEM) or upload its file (.crt, .pem or .cer). It&apos;s trusted for backups only, alongside the usual public
          CAs. Never paste a private key here.
        </span>
      </div>
      <label className="radio small">
        <input type="checkbox" checked={form.virtualHost} onChange={(e) => set("virtualHost", e.target.checked)} />
        Virtual-hosted buckets (<code>bucket.host</code> instead of <code>host/bucket</code>); leave off for SeaweedFS
      </label>
      {messages}
      <div className="btn-row">
        <button className="btn small" onClick={test} disabled={!!busy || !ready}>
          {busy === "test" ? "Testing…" : "Test connection"}
        </button>
        <button className="btn small primary" onClick={save} disabled={!!busy || !ready}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        {editing && (
          <button
            className="btn small ghost"
            onClick={() => {
              setEditing(false);
              setForm(toInput(status));
            }}
          >
            Cancel
          </button>
        )}
      </div>
    </div>
  );
}
