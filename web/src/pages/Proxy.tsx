import { useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError } from "../api";
import { Ago, Spinner } from "../components/common";
import { ProxyFirewall } from "../components/ProxyFirewall";
import { WireGuardTunnel } from "../components/WireGuardTunnel";
import type { ProxyStatusSites } from "../types";
import type {
  Adoption,
  FirewallSync,
  ProxiedServer,
  ProxyInfo,
  ProxySettings,
  Server,
  SSHKeyInfo,
  UpstreamState,
  WireGuardResult,
  WireGuardStatus,
} from "../types";


const EMPTY: ProxySettings = {
  host: "",
  port: 22,
  user: "root",
  upstream: "",
  upstreamSource: "manual",
  logMode: "noip",
  servers: [],
};

/** An error from a remote operation, with whatever the server printed. */
interface OpError {
  message: string;
  output?: string;
  /** nginx has its own stream block, which hlm can adopt. */
  streamBlock?: boolean;
}

function opError(e: unknown): OpError {
  return {
    message: (e as Error).message,
    output: e instanceof ApiError ? ((e.body?.output as string) || undefined) : undefined,
    streamBlock: e instanceof ApiError && e.body?.streamBlock === true,
  };
}

function Copy({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      className="btn small"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } catch {
          // Clipboard unavailable (e.g. plain HTTP); the text can be selected instead.
        }
      }}
    >
      {done ? "Copied" : "Copy"}
    </button>
  );
}

/** Describes what applying or removing the relays did to the firewall. */
function firewallNote(fw?: FirewallSync): string {
  if (!fw) return "";
  if (fw.error) return ` The firewall couldn't be updated: ${fw.error}`;
  return fw.changed ? " The firewall was updated." : "";
}

function Output({ text }: { text?: string }) {
  if (!text) return null;
  return <pre className="op-output">{text}</pre>;
}

export function ProxyPage() {
  const [loaded, setLoaded] = useState(false);
  const [configured, setConfigured] = useState(false);
  const [key, setKey] = useState<SSHKeyInfo | null>(null);
  const [form, setForm] = useState<ProxySettings>(EMPTY);
  const [servers, setServers] = useState<Server[]>([]);
  const [info, setInfo] = useState<ProxyInfo>();
  const configFile = info?.configFile || "/etc/nginx/hlm-stream.conf";
  const [hostKey, setHostKey] = useState<{ key: string; fingerprint: string; type: string }>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<OpError>();
  const [result, setResult] = useState<{ text: string; output?: string }>();
  const [preview, setPreview] = useState<{ config: string; servers: ProxiedServer[]; path: string }>();
  const [importing, setImporting] = useState(false);
  const [privateKey, setPrivateKey] = useState("");
  const [unifiReady, setUnifiReady] = useState(false);
  const [upstream, setUpstream] = useState<UpstreamState>({});
  // The address UniFi reports for the selected WAN, or why it couldn't.
  // Reviewing the change to nginx's own stream block, and where it was
  // started from: the connection check, or a failed apply (which is retried).
  const [adoption, setAdoption] = useState<{ plan: Adoption; from: "info" | "apply" }>();
  // Bumped after applying or removing, so the firewall status reloads.
  const [fwVersion, setFwVersion] = useState(0);
  const [wanLookup, setWanLookup] = useState<{ wan: string; address?: string; error?: string }>();
  const [tunnel, setTunnel] = useState<WireGuardStatus>();

  useEffect(() => {
    (async () => {
      const p = await api.proxy();
      // Generate a key straight away if there isn't one, so the public key
      // can be copied to the server.
      setKey(p.key ?? (await api.generateKey()));
      setConfigured(p.configured);
      setUpstream(p.upstream ?? {});
      if (p.configured) setForm({ ...EMPTY, ...p.settings, servers: p.settings.servers ?? [] });
      setServers(await api.servers());
      setUnifiReady((await api.unifiConfig().catch(() => null))?.configured ?? false);
      setTunnel(await api.wireGuard().catch(() => undefined));
      setLoaded(true);
    })().catch((e) => setError(opError(e)));
  }, []);

  const wan = form.unifiWan ?? "wan";
  const fromUnifi = form.upstreamSource === "unifi";
  const fromTunnel = form.upstreamSource === "wireguard";
  useEffect(() => {
    if (!fromUnifi || !unifiReady) return;
    let live = true;
    setWanLookup(undefined);
    api
      .unifiWAN(wan)
      .then((r) => live && setWanLookup({ wan, address: r.address }))
      .catch((e) => live && setWanLookup({ wan, error: (e as Error).message }));
    return () => {
      live = false;
    };
  }, [fromUnifi, unifiReady, wan]);

  const refreshUpstream = async () => setUpstream((await api.proxy()).upstream ?? {});

  const checkWAN = () =>
    run("wan", async () => {
      await api.saveProxy(form);
      const st = await api.checkProxyWAN();
      setUpstream(st);
      if (st.current) setWanLookup({ wan, address: st.current });
      if (!st.error) setResult({ text: st.applied ? `The proxy sends traffic to ${st.applied}.` : `UniFi reports ${st.current}.` });
    });

  const set = <K extends keyof ProxySettings>(k: K, v: ProxySettings[K]) => setForm((f) => ({ ...f, [k]: v }));

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setResult(undefined);
    try {
      await fn();
    } catch (e) {
      const hk = e instanceof ApiError ? (e.body?.hostKey as string | undefined) : undefined;
      if (hk && e instanceof ApiError)
        setHostKey({ key: hk, fingerprint: e.body?.hostKeyFingerprint as string, type: (e.body?.hostKeyType as string) || "ed25519" });
      else setError(opError(e));
    } finally {
      setBusy(undefined);
    }
  }

  const test = (st = form) =>
    run("test", async () => {
      setInfo(await api.testProxy(st));
    });

  function trustHostKey() {
    const st = { ...form, hostKey: hostKey!.key };
    setForm(st);
    setHostKey(undefined);
    test(st);
  }

  const save = (e?: FormEvent) => {
    e?.preventDefault();
    return run("save", async () => {
      const st = await api.saveProxy(form);
      setForm({ ...EMPTY, ...st });
      setConfigured(true);
      setResult({ text: "Saved." });
    });
  };

  const install = () =>
    run("install", async () => {
      // Installing works on the saved server, so save what's in the form first.
      await api.saveProxy(form);
      setConfigured(true);
      const r = await api.installNginx();
      setResult({ text: "nginx is installed.", output: r.output });
      setInfo(await api.testProxy(form));
    });

  const showPreview = () =>
    run("preview", async () => {
      setPreview(await api.previewProxy());
    });

  const apply = () =>
    run("apply", async () => {
      await api.saveProxy(form);
      const r = await api.applyProxy();
      setPreview({ config: r.config, servers: r.servers, path: configFile });
      setResult({
        text:
          "Applied: nginx on the proxy server now relays the selected servers." +
          statusNote(r.status) +
          firewallNote(r.firewall),
        output: [r.output, r.status?.output].filter(Boolean).join("\n"),
      });
      setFwVersion((v) => v + 1);
      await refreshUpstream();
      setInfo(await api.testProxy(form));
    });

  const reviewAdoption = (from: "info" | "apply") =>
    run("adopt-plan", async () => {
      const plan = await api.planAdoption();
      if (!plan.found) throw new Error("nginx no longer has a stream block of its own; apply again.");
      setAdoption({ plan, from });
    });

  const adopt = () =>
    run("adopt", async () => {
      const from = adoption?.from;
      const r = await api.adoptStreamBlock();
      setAdoption(undefined);
      setResult({ text: `Added hlm's include to the stream block in ${r.adoption.file}.`, output: r.output });
      if (from === "apply") {
        const a = await api.applyProxy();
        setPreview({ config: a.config, servers: a.servers, path: configFile });
        setResult({
          text: `Added hlm's include to the stream block in ${r.adoption.file} and applied: nginx now relays the selected servers.${firewallNote(a.firewall)}`,
          output: a.output,
        });
        setFwVersion((v) => v + 1);
        await refreshUpstream();
      }
      setInfo(await api.testProxy(form));
    });

  const adoptPanel = adoption && (
    <AdoptPanel plan={adoption.plan} busy={busy === "adopt"} onAdopt={adopt} onCancel={() => setAdoption(undefined)} />
  );

  // The tunnel's setup may have made it the home address and re-applied nginx.
  async function tunnelChanged(st: WireGuardStatus, r?: WireGuardResult) {
    setTunnel(st);
    const p = await api.proxy();
    if (p.configured) setForm((f) => ({ ...f, upstreamSource: p.settings.upstreamSource, upstream: p.settings.upstream, upstreamPorts: p.settings.upstreamPorts }));
    setUpstream(p.upstream ?? {});
    if (r?.apply) setPreview({ config: r.apply.config, servers: r.apply.servers, path: configFile });
    if (r?.firewall) setFwVersion((v) => v + 1);
  }

  const remove = () => {
    if (!confirm("Remove all relays from the proxy server? Players connecting through it will be cut off.")) return;
    run("remove", async () => {
      const r = await api.removeProxy();
      setResult({ text: "Removed the relays from the proxy server." + firewallNote(r.firewall), output: r.output });
      setFwVersion((v) => v + 1);
      await refreshUpstream();
    });
  };

  const replaceKey = () => {
    if (!confirm("Make a new SSH key? The old one stops working; you'll need to add the new public key to the server.")) return;
    run("key", async () => setKey(await api.generateKey(true)));
  };

  const importKey = () =>
    run("key", async () => {
      setKey(await api.importKey(privateKey));
      setPrivateKey("");
      setImporting(false);
    });

  function toggleServer(name: string, on: boolean) {
    set("servers", on ? [...form.servers, name].sort() : form.servers.filter((s) => s !== name));
  }

  if (!loaded && !error) return <div className="page"><Spinner /></div>;

  const authLine = key ? `mkdir -p ~/.ssh && echo ${JSON.stringify(key.public)} >> ~/.ssh/authorized_keys` : "";

  return (
    <div className="page proxy-page">
      <h1>Proxy</h1>
      <p className="muted">
        Run game traffic through nginx on another machine, such as a VPS. Players connect to that machine, which relays
        TCP and UDP to your home network, so they never see your home address, and the proxy keeps no record of players&apos;
        IP addresses.
      </p>

      {error && (
        <div className="alert error">
          {error.message}
          <Output text={error.output} />
          {error.streamBlock && !adoption && (
            <div className="btn-row">
              <button className="btn small primary" onClick={() => reviewAdoption("apply")} disabled={!!busy}>
                {busy === "adopt-plan" ? "Reading nginx's configuration…" : "Review the change"}
              </button>
            </div>
          )}
        </div>
      )}
      {adoption?.from === "apply" && adoptPanel}
      {result && (
        <div className="alert success">
          {result.text}
          <Output text={result.output} />
        </div>
      )}

      <section className="card">
        <h2>1. SSH key</h2>
        {key ? (
          <>
            <p className="muted small no-margin">
              hlm signs in with this key (created <Ago iso={key.created} />; the private half is stored encrypted on this
              machine). Add the public key to the proxy server&apos;s <code>~/.ssh/authorized_keys</code> for the user hlm
              signs in as:
            </p>
            <div className="key-box">
              <code>{key.public}</code>
              <Copy text={key.public} />
            </div>
            <p className="muted small no-margin">Or run this on the proxy server:</p>
            <div className="key-box">
              <code>{authLine}</code>
              <Copy text={authLine} />
            </div>
            <p className="muted small">Fingerprint {key.fingerprint}</p>
          </>
        ) : (
          <Spinner />
        )}
        <div className="btn-row">
          <button className="btn small ghost" onClick={replaceKey} disabled={!!busy}>
            Make a new key
          </button>
          <button className="btn small ghost" onClick={() => setImporting((i) => !i)}>
            {importing ? "Cancel import" : "Use my own key"}
          </button>
        </div>
        {importing && (
          <div className="import-key">
            <textarea
              value={privateKey}
              onChange={(e) => setPrivateKey(e.target.value)}
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              rows={6}
              spellCheck={false}
            />
            <button className="btn small primary" onClick={importKey} disabled={!privateKey.trim() || !!busy}>
              Import private key
            </button>
          </div>
        )}
      </section>

      <form className="card config-form" onSubmit={save}>
        <h2>2. Proxy server</h2>
        <div className="field-grid">
          <label className="field">
            <span className="field-label">Host</span>
            <input value={form.host} onChange={(e) => set("host", e.target.value)} placeholder="vps.example.com" required spellCheck={false} />
          </label>
          <label className="field">
            <span className="field-label">SSH port</span>
            <input type="number" value={form.port} onChange={(e) => set("port", Number(e.target.value))} min={1} max={65535} />
          </label>
          <label className="field">
            <span className="field-label">User</span>
            <input value={form.user} onChange={(e) => set("user", e.target.value)} spellCheck={false} />
            <span className="field-help">root, or a user that can run sudo without a password.</span>
          </label>
        </div>

        {hostKey && (
          <div className="alert warn cert-prompt">
            <strong>First connection to {form.host}.</strong> Check that this is the server&apos;s {hostKey.type.toUpperCase()} host
            key fingerprint (on the server: <code>ssh-keygen -lf /etc/ssh/ssh_host_{hostKey.type}_key.pub</code>):
            <code className="fingerprint">{hostKey.fingerprint}</code>
            <div className="btn-row">
              <button type="button" className="btn small primary" onClick={trustHostKey}>
                It matches, trust it
              </button>
              <button type="button" className="btn small ghost" onClick={() => setHostKey(undefined)}>
                Cancel
              </button>
            </div>
          </div>
        )}
        {form.hostKey && !hostKey && (
          <p className="muted small no-margin">
            Trusting the server&apos;s host key.{" "}
            <button type="button" className="link-button" onClick={() => set("hostKey", "")}>
              forget it
            </button>
          </p>
        )}

        {info && (
          <ServerInfo
            info={info}
            onInstall={install}
            onAdopt={() => reviewAdoption("info")}
            busy={busy}
          />
        )}
        {adoption?.from === "info" && adoptPanel}

        <h2>3. What to relay</h2>
        <div className="field-grid">
          <div className="field">
            <span className="field-label">Home address</span>
            <label className="radio">
              <input type="radio" checked={!fromUnifi && !fromTunnel} onChange={() => set("upstreamSource", "manual")} /> Enter it
            </label>
            <label className={`radio${tunnel?.configured ? "" : " disabled"}`}>
              <input
                type="radio"
                checked={fromTunnel}
                disabled={!tunnel?.configured && !fromTunnel}
                onChange={() => set("upstreamSource", "wireguard")}
              />{" "}
              Through the WireGuard tunnel{tunnel?.homeAddress && <> (<code>{tunnel.homeAddress}</code>)</>}
            </label>
            <label className={`radio${unifiReady ? "" : " disabled"}`}>
              <input
                type="radio"
                checked={fromUnifi}
                disabled={!unifiReady && !fromUnifi}
                onChange={() => setForm((f) => ({ ...f, upstreamSource: "unifi", unifiWan: f.unifiWan ?? "wan" }))}
              />{" "}
              Use the internet address from UniFi and keep it up to date
            </label>
            {!unifiReady && (
              <span className="field-help">
                Set up UniFi under <Link to="/system">System</Link> to look up your address from the gateway.
              </span>
            )}
            {fromTunnel ? (
              <span className="field-help">
                {tunnel?.configured ? (
                  <>
                    nginx sends traffic through the tunnel to <code>{tunnel.homeAddress}</code>, this machine&apos;s end, on the
                    servers&apos; own ports. Nothing needs forwarding at home.
                  </>
                ) : (
                  <span className="form-error">The tunnel isn&apos;t set up; set it up under 4. WireGuard tunnel below.</span>
                )}
              </span>
            ) : fromUnifi ? (
              <>
                <select value={wan} onChange={(e) => set("unifiWan", e.target.value as "wan" | "wan2")} aria-label="WAN">
                  <option value="wan">WAN</option>
                  <option value="wan2">WAN2</option>
                </select>
                {wanLookup?.wan === wan && wanLookup.address && (
                  <span className="field-help">
                    UniFi reports <code>{wanLookup.address}</code>.
                  </span>
                )}
                {wanLookup?.wan === wan && wanLookup.error && <span className="form-error small">{wanLookup.error}</span>}
                <span className="field-help">
                  hlm checks the address every few minutes and, when your ISP changes it, updates the proxy server
                  (once you&apos;ve applied the configuration). Forward the game ports there, e.g. with the UniFi integration.
                </span>
              </>
            ) : (
              <>
                <input value={form.upstream} onChange={(e) => set("upstream", e.target.value)} placeholder="home.example.net" spellCheck={false} />
                <span className="field-help">
                  Where the proxy sends traffic: your home&apos;s public IP or dynamic-DNS name (with the game ports forwarded
                  there, e.g. by the UniFi integration), or a VPN address. hlm can also set up a WireGuard tunnel for you (4. below).
                </span>
                <label className="radio">
                  <input
                    type="radio"
                    checked={(form.upstreamPorts ?? "internet") === "internet"}
                    onChange={() => set("upstreamPorts", "internet")}
                  />{" "}
                  Send to the internet ports (your router forwards them)
                </label>
                <label className="radio">
                  <input type="radio" checked={form.upstreamPorts === "local"} onChange={() => set("upstreamPorts", "local")} /> Send
                  to the servers&apos; own ports (a VPN address that reaches this machine)
                </label>
              </>
            )}
          </div>
          <div className="field">
            <span className="field-label">Logging</span>
            <label className="radio">
              <input type="radio" checked={form.logMode === "noip"} onChange={() => set("logMode", "noip")} /> Log
              connections without IP addresses
            </label>
            <label className="radio">
              <input type="radio" checked={form.logMode === "off"} onChange={() => set("logMode", "off")} /> Don&apos;t log
              connections
            </label>
            <span className="field-help">Either way, nginx&apos;s error log only keeps critical errors for the relays, since its messages name the client.</span>
          </div>
        </div>
        <div className="field">
          <span className="field-label">Servers</span>
          {servers.length === 0 && <span className="muted small">No servers yet.</span>}
          <div className="server-checks">
            {servers.map((s) => (
              <label key={s.id} className="radio">
                <input type="checkbox" checked={form.servers.includes(s.name)} onChange={(e) => toggleServer(s.name, e.target.checked)} />
                {s.name}
                <span className="muted small">{s.app}</span>
              </label>
            ))}
          </div>
        </div>

        <fieldset className="config-section">
          <legend>Status page sites</legend>
          <label className="radio">
            <input type="checkbox" checked={!!form.statusPage} onChange={(e) => set("statusPage", e.target.checked)} />
            Serve the <Link to="/system#status-page">status page&apos;s sites</Link> here too
          </label>
          <span className="field-help">
            nginx on the proxy server answers each site&apos;s domain and passes only the
            status page to hlm, through the WireGuard tunnel: hlm&apos;s own port isn&apos;t opened to the internet. So the
            WireGuard tunnel has to be the home address.
            {form.statusPage && form.upstreamSource !== "wireguard" && (
              <strong className="form-error"> Choose the WireGuard tunnel as the home address above.</strong>
            )}
          </span>
          {form.statusPage && (
            <div className="field">
              <label className="radio">
                <input
                  type="checkbox"
                  checked={!!form.statusOnAddress}
                  onChange={(e) => {
                    set("statusOnAddress", e.target.checked);
                    if (e.target.checked && !form.statusPort) set("statusPort", 80);
                  }}
                />
                Also on the proxy server&apos;s own address ({form.host || "its host"}), where players find the game
                servers
              </label>
              {form.statusOnAddress && (
                <label className="field">
                  <span className="field-label">Port</span>
                  <input
                    type="number"
                    min={1}
                    max={65535}
                    value={form.statusPort ?? 80}
                    onChange={(e) => set("statusPort", Number(e.target.value) || 0)}
                    style={{ maxWidth: "8rem" }}
                  />
                  <span className="field-help">
                    Plain HTTP:{" "}
                    <code>
                      http://{form.host || "proxy"}
                      {(form.statusPort ?? 80) === 80 ? "" : `:${form.statusPort}`}/status
                    </code>
                    . On 80 it shares the port with the sites&apos; domains; any other port can&apos;t be one a game
                    server is relayed on. hlm opens it in the proxy&apos;s Linode firewall, if it manages one.
                  </span>
                </label>
              )}
            </div>
          )}
          {form.statusPage && (
            <div className="field status-tls">
              <span className="field-label">HTTPS</span>
              <label className="radio">
                <input type="radio" name="statusTls" checked={!form.statusTls} onChange={() => set("statusTls", "")} />
                None: HTTP only (you put the domains&apos; DNS at the proxy server)
              </label>
              <label className="radio">
                <input
                  type="radio"
                  name="statusTls"
                  checked={form.statusTls === "letsencrypt"}
                  onChange={() => set("statusTls", "letsencrypt")}
                />
                Let&apos;s Encrypt: certbot on the proxy server gets and renews free certificates (you point the
                domains&apos; DNS at the proxy server)
              </label>
              <label className="radio">
                <input
                  type="radio"
                  name="statusTls"
                  checked={form.statusTls === "cloudflare"}
                  onChange={() => set("statusTls", "cloudflare")}
                />
                Cloudflare: hlm points each domain at the proxy server through Cloudflare, and installs a Cloudflare
                Origin CA certificate there
              </label>
              {form.statusTls === "cloudflare" && (
                <span className="field-help">
                  Uses the <Link to="/tunnel">Cloudflare account</Link> (the tunnel itself isn&apos;t needed): the
                  domains have to be on it, and its token needs <strong>Zone → SSL and Certificates: Edit</strong> as
                  well as DNS: Edit. Set the domain&apos;s SSL/TLS mode to <strong>Full (strict)</strong> in the
                  Cloudflare dashboard. The proxy&apos;s firewall then lets only Cloudflare reach ports 80 and 443.
                </span>
              )}
              {form.statusTls === "letsencrypt" && (
                <label className="field">
                  <span className="field-label">Email for Let&apos;s Encrypt (optional)</span>
                  <input
                    type="email"
                    value={form.statusEmail ?? ""}
                    onChange={(e) => set("statusEmail", e.target.value)}
                    placeholder="you@example.com"
                  />
                  <span className="field-help">Where Let&apos;s Encrypt sends notices about the certificates.</span>
                </label>
              )}
            </div>
          )}
        </fieldset>

        <div className="form-footer">
          <button type="button" className="btn small" onClick={() => test()} disabled={!!busy || !form.host}>
            {busy === "test" ? "Connecting…" : "Test connection"}
          </button>
          <div className="spacer" />
          <button type="submit" className="btn small primary" disabled={!!busy || !form.host}>
            {busy === "save" ? "Saving…" : "Save"}
          </button>
        </div>
      </form>

      <WireGuardTunnel
        beforeSetup={async () => {
          // Setting up works on the saved server, so save what's in the form first.
          setForm({ ...EMPTY, ...(await api.saveProxy(form)) });
          setConfigured(true);
        }}
        onChange={tunnelChanged}
        disabled={!!busy}
        hasServer={!!form.host}
      />

      {configured && (
        <section className="card">
          <h2>5. nginx</h2>
          <p className="muted small">
            hlm writes the relays to <code>{configFile}</code> on the proxy server and only changes that file
            (adding one include to nginx.conf the first time). nginx checks the new configuration before reloading; if it&apos;s
            rejected, the previous one is put back. Remember to allow the game ports in the proxy server&apos;s firewall.
          </p>
          <div className="btn-row">
            <button className="btn small" onClick={showPreview} disabled={!!busy}>
              {busy === "preview" ? "Loading…" : "Preview configuration"}
            </button>
            <button className="btn small primary" onClick={apply} disabled={!!busy}>
              {busy === "apply" ? "Applying…" : "Save & apply"}
            </button>
            <button className="btn small danger" onClick={remove} disabled={!!busy}>
              Remove relays
            </button>
          </div>
          {form.upstreamSource === "unifi" && (
            <UpstreamStatus state={upstream} onCheck={checkWAN} busy={busy === "wan"} disabled={!!busy} />
          )}
          {preview && (
            <>
              <ul className="relay-list">
                {preview.servers.map((s) => (
                  <li key={s.server}>
                    <strong>{s.server}</strong>{" "}
                    {s.address && <span className="muted small">→ {s.address} </span>}
                    {s.error ? (
                      <span className="form-error">{s.error}</span>
                    ) : s.ports.length ? (
                      <span className="muted small">
                        {s.ports.map((p) => `${p.port}/${p.proto}${p.local !== p.port ? ` (server port ${p.local})` : ""}`).join(", ")}
                      </span>
                    ) : (
                      <span className="muted small">no published ports</span>
                    )}
                    {s.skipped?.length ? (
                      <span className="form-error small"> · skipped (used by another server): {s.skipped.map((p) => `${p.port}/${p.proto}`).join(", ")}</span>
                    ) : null}
                  </li>
                ))}
              </ul>
              <pre className="op-output config-preview">{preview.config}</pre>
            </>
          )}
        </section>
      )}
      {configured && <ProxyFirewall version={fwVersion} />}
    </div>
  );
}

function UpstreamStatus({
  state,
  onCheck,
  busy,
  disabled,
}: {
  state: UpstreamState;
  onCheck: () => void;
  busy: boolean;
  disabled: boolean;
}) {
  const behind = state.applied && state.current && state.applied !== state.current;
  return (
    <div className="server-info upstream-status">
      <ul>
        <li className={state.applied ? (behind ? "bad" : "ok") : "bad"}>
          {state.applied ? (
            <>
              The proxy sends traffic to <code>{state.applied}</code>
              {state.appliedAt && <span className="muted"> (applied <Ago iso={state.appliedAt} />)</span>}
            </>
          ) : (
            "Not applied yet: the address is kept up to date once you apply"
          )}
        </li>
        {state.current && (
          <li className={behind ? "bad" : "ok"}>
            UniFi reports <code>{state.current}</code>
            {state.checkedAt && <span className="muted"> (checked <Ago iso={state.checkedAt} />)</span>}
          </li>
        )}
      </ul>
      {state.error && <div className="form-error small">{state.error}</div>}
      <button className="btn small" onClick={onCheck} disabled={disabled}>
        {busy ? "Checking…" : "Check now"}
      </button>
    </div>
  );
}

/** Shows the change to nginx's own stream block before making it. */
function AdoptPanel({ plan, busy, onAdopt, onCancel }: { plan: Adoption; busy: boolean; onAdopt: () => void; onCancel: () => void }) {
  return (
    <div className="adopt-panel">
      <p className="small no-margin">
        nginx already has a <code>stream {"{ }"}</code> block in <code>{plan.file}</code>, and only one is allowed, so hlm
        adds its include as the first line inside it. Nothing else in the file changes. hlm keeps a backup as{" "}
        <code>{plan.file}.hlm-bak</code>, checks the result with <code>nginx -t</code>, and puts the file back if the check
        fails. Relays already in the block keep working; ports they use can&apos;t also be relayed by hlm.
      </p>
      {plan.included ? (
        <p className="small">The block already includes hlm&apos;s file.</p>
      ) : (
        <div className="adopt-diff">
          <div>
            <span className="field-label">Now</span>
            <pre className="op-output">{plan.block}</pre>
          </div>
          <div>
            <span className="field-label">After</span>
            <pre className="op-output">{plan.proposed}</pre>
          </div>
        </div>
      )}
      <div className="btn-row">
        <button type="button" className="btn small primary" onClick={onAdopt} disabled={busy}>
          {busy ? "Changing…" : "Add the include"}
        </button>
        <button type="button" className="btn small ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function ServerInfo({
  info,
  onInstall,
  onAdopt,
  busy,
}: {
  info: ProxyInfo;
  onInstall: () => void;
  onAdopt: () => void;
  busy?: string;
}) {
  const canSudo = info.root || info.sudo;
  return (
    <div className="server-info">
      <div>
        <strong>Connected</strong> {info.os && <span className="muted">· {info.os}</span>}
      </div>
      <ul>
        <li className={canSudo ? "ok" : "bad"}>
          {info.root ? "Signed in as root" : info.sudo ? "Can use sudo without a password" : "Can't use sudo without a password: hlm can't manage nginx"}
        </li>
        <li className={info.nginx ? "ok" : "bad"}>{info.nginx ? `nginx ${info.nginx}` : "nginx isn't installed"}</li>
        {info.nginx && <li className={info.stream ? "ok" : "bad"}>{info.stream ? "Stream (TCP/UDP) module available" : "Stream module missing"}</li>}
        {info.nginx && <li className={info.running ? "ok" : "bad"}>{info.running ? "Running" : "Not running"}</li>}
        {info.nginx && (
          <li className={info.included || !info.streamBlock ? "ok" : "bad"}>
            {info.included ? (
              "nginx includes hlm's relays"
            ) : info.streamBlock ? (
              <>
                nginx has its own stream block{info.streamFile && <> in <code>{info.streamFile}</code></>}; hlm needs to add
                its include inside it{" "}
                <button type="button" className="btn small" onClick={onAdopt} disabled={!!busy || !canSudo}>
                  {busy === "adopt-plan" ? "Reading…" : "Review the change"}
                </button>
              </>
            ) : (
              "hlm will add its include to nginx.conf"
            )}
          </li>
        )}
      </ul>
      {(!info.nginx || !info.stream) && canSudo && (
        <button type="button" className="btn small primary" onClick={onInstall} disabled={!!busy || !info.packageManager}>
          {busy === "install" ? "Installing… (this can take a few minutes)" : info.packageManager ? `Install nginx (${info.packageManager})` : "Install nginx yourself (no apt or dnf)"}
        </button>
      )}
    </div>
  );
}

/** What applying did for the status page's sites. */
function statusNote(r?: ProxyStatusSites): string {
  if (!r) return "";
  if (r.error) return ` The status page's sites weren't served: ${r.error}`;
  const at = r.address ? ` The status page is at ${r.address}.` : "";
  if (r.domains.length === 0) return at || " There are no status page sites to serve yet.";
  const http = r.domains.filter((d) => !r.https.includes(d));
  let note = `${at} Status page sites: ${r.https.length ? `${r.https.join(", ")} over HTTPS` : ""}${r.https.length && http.length ? "; " : ""}${http.length ? `${http.join(", ")} over HTTP` : ""}.`;
  const failed = Object.entries(r.failed ?? {});
  if (failed.length && r.tls === "cloudflare")
    note += ` Not set up on Cloudflare: ${failed.map(([d, why]) => `${d} (${why})`).join(", ")}.`;
  else if (failed.length)
    note += ` No certificate yet for ${failed.map(([d, why]) => `${d} (${why})`).join(", ")}; is its DNS pointing at the proxy server?`;
  if (r.notes?.length) note += " " + r.notes.join(" ");
  return note;
}
