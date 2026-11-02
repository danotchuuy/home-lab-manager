import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { HTTPSStatus } from "../types";
import { timeAgo } from "../util";
import { Spinner } from "./common";

const STATE_TEXT: Record<string, string> = {
  off: "Off: hlm is served over plain HTTP.",
  waiting: "Waiting for the Cloudflare connection.",
  issuing: "Getting a certificate from Let's Encrypt…",
  active: "On.",
  error: "Couldn't get a certificate.",
};

/**
 * HTTPS with a Let's Encrypt certificate, proved with a DNS record made
 * through the Cloudflare connection. Optional: while it's off, hlm stays on
 * plain HTTP and its cookies aren't marked Secure, since browsers wouldn't
 * send Secure cookies back over HTTP.
 */
export function HTTPSSettings() {
  const [st, setSt] = useState<HTTPSStatus>();
  const [form, setForm] = useState({ enabled: false, domain: "", email: "", staging: false });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const load = () =>
    api.httpsStatus().then((s) => {
      setSt(s);
      return s;
    });

  useEffect(() => {
    load().then(
      (s) => setForm({ enabled: s.enabled, domain: s.domain ?? "", email: s.email ?? "", staging: !!s.staging }),
      (e) => setError((e as Error).message),
    );
  }, []);

  // Follow a certificate being issued.
  useEffect(() => {
    if (st?.state !== "issuing") return;
    const t = setInterval(() => load().catch(() => {}), 3000);
    return () => clearInterval(t);
  }, [st?.state]);

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      setSt(await api.saveHTTPS({ ...form, domain: form.domain.trim(), email: form.email.trim() }));
      setTimeout(() => load().catch(() => {}), 1000);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function renew() {
    setError(undefined);
    try {
      await api.renewHTTPS();
      setTimeout(() => load().catch(() => {}), 1000);
    } catch (err) {
      setError((err as Error).message);
    }
  }

  if (!st) return error ? <div className="alert error">{error}</div> : <Spinner />;
  const state = st.active ? "active" : st.state;
  const changed = form.enabled !== st.enabled || form.domain.trim() !== (st.domain ?? "") || form.email.trim() !== (st.email ?? "") || form.staging !== !!st.staging;
  return (
    <div className="https-settings">
      <p className="muted small no-margin">
        Serves hlm over HTTPS, on the same port, with a free certificate from Let&apos;s Encrypt. It&apos;s proved with a
        DNS record made through the Cloudflare connection, so hlm needn&apos;t be reachable from the internet. Once
        there&apos;s a certificate, plain HTTP is sent to HTTPS and sign-in cookies only travel over it. While it&apos;s
        off, cookies work over plain HTTP, as they do now.
      </p>
      {st.off && <div className="alert warn small">HLM_TLS=off is set on the host, so HTTPS is off whatever is saved here.</div>}
      {!st.cloudflare && <div className="alert info small">Connect Cloudflare first, on the Tunnel page: the certificate is proved with a DNS record there.</div>}
      <div className={`alert small ${state === "active" ? "success" : state === "error" ? "error" : "info"}`}>
        <strong>{STATE_TEXT[state] ?? state}</strong>
        {st.error && <> {st.error}</>}
        {st.certFor && (
          <>
            {" "}
            Certificate for <code>{st.certFor}</code> from {st.issuer}, issued {st.issued ? timeAgo(st.issued) : "?"}, valid until{" "}
            {st.notAfter ? new Date(st.notAfter).toLocaleDateString() : "?"}; it renews itself.
          </>
        )}
        {st.active && st.domain && (
          <>
            {" "}
            Use <code>https://{st.domain}{window.location.port ? `:${window.location.port}` : ""}</code>.
          </>
        )}
      </div>
      <form onSubmit={save}>
        <label className="radio">
          <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} /> Serve hlm over HTTPS
        </label>
        <div className="inline-form">
          <label className="field">
            <span className="field-label">Name hlm is reached at</span>
            <input value={form.domain} onChange={(e) => setForm({ ...form, domain: e.target.value })} placeholder="hlm.home.example.com" />
          </label>
          <label className="field">
            <span className="field-label">Email for expiry notices (optional)</span>
            <input type="email" value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="you@example.com" />
          </label>
        </div>
        <label className="radio small">
          <input type="checkbox" checked={form.staging} onChange={(e) => setForm({ ...form, staging: e.target.checked })} /> Use Let&apos;s
          Encrypt&apos;s staging server (for trying it out: browsers won&apos;t trust its certificates)
        </label>
        <p className="muted small">
          The name has to be in a Cloudflare zone, and point at this server on your network (a local DNS entry, or a
          record with its private address). Things that reach hlm by plain HTTP, like a tunnel route to
          <code> http://…:8080</code>, will be sent to HTTPS: change them to <code>https://</code> with the name. If
          you use single sign-on, change the redirect URL registered with your provider to <code>https://</code> too.
        </p>
        {error && <div className="alert error">{error}</div>}
        <div className="btn-row">
          <button className="btn small primary" type="submit" disabled={busy || !changed}>
            {busy ? "Saving…" : "Save"}
          </button>
          {st.enabled && (
            <button type="button" className="btn small" onClick={renew} disabled={busy || st.state === "issuing"}>
              Check certificate now
            </button>
          )}
        </div>
      </form>
    </div>
  );
}
