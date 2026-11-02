import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { Spinner } from "../components/common";
import { applyNote, HostnamesEditor } from "../components/Hostnames";
import { LogViewer } from "../components/LogViewer";
import type { CloudflareTest, TunnelSetupInput, TunnelStatus } from "../types";
import { timeAgo } from "../util";

const HEALTH: Record<string, string> = {
  healthy: "Cloudflare sees the tunnel as healthy",
  degraded: "Cloudflare sees the tunnel as degraded: some of its connections are down",
  down: "Cloudflare sees the tunnel as down: cloudflared isn't connected",
  inactive: "Cloudflare hasn't seen cloudflared connect yet",
  deleted: "The tunnel was deleted in Cloudflare; set it up again to make a new one",
};

/** Publishes servers' web interfaces on your own domain through a Cloudflare Tunnel. */
export function TunnelPage() {
  const [status, setStatus] = useState<TunnelStatus>();
  const [error, setError] = useState<string>();

  const reload = () =>
    api.tunnel().then(
      (st) => {
        setStatus(st);
        setError(undefined);
      },
      (e) => setError((e as Error).message),
    );

  useEffect(() => {
    reload();
  }, []);

  if (!status) return <div className="page">{error ? <div className="alert error">{error}</div> : <Spinner />}</div>;

  return (
    <div className="page tunnel-page">
      <h1>Cloudflare Tunnel</h1>
      <p className="muted">
        Give servers with a web interface (admin panels, maps, file managers) an address on your own domain, like{" "}
        <code>panel.example.com</code>. hlm runs Cloudflare&apos;s connector, <code>cloudflared</code>, on the Docker host. It
        connects out to Cloudflare, so nothing is forwarded at home and your home address stays private. Cloudflare serves
        each hostname over HTTPS with its own certificate.
      </p>
      <p className="muted small">
        Cloudflare Tunnel carries web traffic (HTTP and HTTPS) to visitors&apos; browsers. Game traffic, which is usually UDP,
        still needs <Link to="/proxy">the proxy</Link> or port forwarding.
      </p>
      {status.error && <div className="alert error">Couldn&apos;t reach Cloudflare: {status.error}</div>}

      <section className="card">
        <h2>1. Cloudflare account</h2>
        <CloudflareAccount status={status} onChange={setStatus} />
      </section>

      <section className="card">
        <h2>2. Tunnel</h2>
        {status.configured ? (
          <TunnelCard status={status} onChange={setStatus} />
        ) : (
          <p className="muted small no-margin">Connect your Cloudflare account first.</p>
        )}
      </section>

      <section className="card">
        <h2>3. Hostnames</h2>
        {status.configured ? (
          <>
            <p className="muted small">
              Each hostname sends visitors to one of a server&apos;s TCP ports. hlm adds a proxied <code>CNAME</code> record
              for it pointing at the tunnel, and never changes records it didn&apos;t make. Cloudflare&apos;s free certificate
              covers one level of subdomain (<code>panel.example.com</code>, not <code>a.panel.example.com</code>).
              Anyone can open a published hostname, so make sure the web interface has a password, or protect it with
              Cloudflare Access.
            </p>
            <p className="muted small">
              Choose <strong>Other server</strong> to publish any web server cloudflared can reach, by IP address or host
              name and port, such as a NAS at <code>192.168.1.20:5000</code>. With host networking (the default),{" "}
              <code>localhost</code> is the Docker host and LAN addresses are reached through it. On a LAN network,
              cloudflared reaches the LAN directly, but not the Docker host&apos;s own address. Leave the subdomain empty to
              use the domain itself (<code>example.com</code>).
            </p>
            {status.zones.length === 0 && !status.error && (
              <div className="alert warn small">The token can&apos;t see any domains; give it Zone → Zone: Read and Zone → DNS: Edit.</div>
            )}
            {/* Reloaded when the tunnel comes or goes, which changes what's published. */}
            <HostnamesEditor key={status.tunnel?.id ?? "none"} zones={status.zones} />
          </>
        ) : (
          <p className="muted small no-margin">Connect your Cloudflare account first.</p>
        )}
      </section>

      {status.tunnel && (
        <section className="card">
          <h2>cloudflared log</h2>
          <LogViewer url={api.tunnelLogsUrl()} running={status.tunnel.connector.running} />
        </section>
      )}
    </div>
  );
}

/** Saves the API token and the account the tunnel goes in. The token never comes back from the server. */
function CloudflareAccount({ status, onChange }: { status: TunnelStatus; onChange: (st: TunnelStatus) => void }) {
  const [editing, setEditing] = useState(false);
  const [token, setToken] = useState("");
  const [test, setTest] = useState<CloudflareTest>();
  const [accountId, setAccountId] = useState("");
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

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

  const runTest = () =>
    run("test", async () => {
      const t = await api.testCloudflare(token);
      setTest(t);
      setAccountId((id) => id || t.accounts[0]?.id || "");
      setNotice(
        `Connected. The token can use ${t.accounts.length} account${t.accounts.length === 1 ? "" : "s"} and ${t.zones.length} domain${t.zones.length === 1 ? "" : "s"}${t.zones.length ? ` (${t.zones.map((z) => z.name).join(", ")})` : ""}.`,
      );
    });

  const save = () =>
    run("save", async () => {
      const st = await api.saveCloudflare(token, accountId);
      onChange(st);
      setToken("");
      setTest(undefined);
      setEditing(false);
    });

  const disconnect = () => {
    if (!confirm("Forget the Cloudflare API token?")) return;
    run("remove", async () => {
      await api.deleteCloudflare();
      onChange(await api.tunnel());
    });
  };

  const messages = (
    <>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}
    </>
  );

  if (!status.configured || editing) {
    return (
      <div className="config-form">
        <label className="field">
          <span className="field-label">API token</span>
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            placeholder={status.configured ? "New token" : ""}
            autoComplete="off"
          />
          <span className="field-help">
            Create one in the Cloudflare dashboard (My Profile → API Tokens → Create Token → Custom token) with{" "}
            <strong>Account → Cloudflare Tunnel: Edit</strong>, <strong>Zone → DNS: Edit</strong> and{" "}
            <strong>Zone → Zone: Read</strong>, for the domains you want to use. It&apos;s stored encrypted on this machine
            and never shown again.
          </span>
        </label>
        {test && test.accounts.length > 1 && (
          <label className="field">
            <span className="field-label">Account</span>
            <select value={accountId} onChange={(e) => setAccountId(e.target.value)}>
              {test.accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
            <span className="field-help">The tunnel is created in this account.</span>
          </label>
        )}
        {messages}
        <div className="btn-row">
          <button className="btn small" onClick={runTest} disabled={!!busy || !token.trim()}>
            {busy === "test" ? "Testing…" : "Test connection"}
          </button>
          <button className="btn small primary" onClick={save} disabled={!!busy || !token.trim()}>
            {busy === "save" ? "Saving…" : "Save"}
          </button>
          {editing && (
            <button className="btn small ghost" onClick={() => setEditing(false)}>
              Cancel
            </button>
          )}
        </div>
      </div>
    );
  }

  return (
    <div>
      <dl className="facts">
        <dt>Account</dt>
        <dd>
          <strong>{status.accountName}</strong> <span className="muted">· API token stored encrypted</span>
        </dd>
        <dt>Domains</dt>
        <dd>
          {status.zones.length ? (
            status.zones.map((z) => z.name + (z.status !== "active" ? ` (${z.status})` : "")).join(", ")
          ) : (
            <span className="muted">none the token can see</span>
          )}
        </dd>
      </dl>
      {messages}
      <div className="btn-row">
        <button className="btn small" onClick={() => setEditing(true)} disabled={!!busy}>
          Change token
        </button>
        <button
          className="btn small danger"
          onClick={disconnect}
          disabled={!!busy || !!status.tunnel}
          title={status.tunnel ? "Remove the tunnel first" : undefined}
        >
          {busy === "remove" ? "Removing…" : "Disconnect"}
        </button>
      </div>
    </div>
  );
}

/** Sets up the tunnel and controls its cloudflared container. */
function TunnelCard({ status, onChange }: { status: TunnelStatus; onChange: (st: TunnelStatus) => void }) {
  const t = status.tunnel;
  const [input, setInput] = useState<TunnelSetupInput>({
    name: t?.name ?? "hlm",
    lanNetwork: t?.lanNetwork ?? "",
    lanAddress: t?.lanAddress ?? "",
  });
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [note, setNote] = useState<string>();
  // The last setup's console; it stays up until the next one.
  const [log, setLog] = useState<{ step: boolean; text: string }[]>([]);
  const consoleRef = useRef<HTMLPreElement>(null);
  useEffect(() => {
    const el = consoleRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [log]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setError(undefined);
    setNote(undefined);
    try {
      await fn();
    } catch (e) {
      setError((e as Error).message);
      // A failed setup may have changed part of it.
      api.tunnel().then(onChange, () => {});
    } finally {
      setBusy(undefined);
    }
  }

  const setup = () =>
    run("setup", async () => {
      setLog([]);
      let done: TunnelStatus | undefined;
      let applied: string | undefined;
      let failure: string | undefined;
      let connected = false;
      await api.setupTunnel(input, (e) => {
        if (e.stage === "step" || e.stage === "output") {
          const line = { step: e.stage === "step", text: e.message ?? "" };
          setLog((l) => [...l.slice(-1999), line]);
          if (line.text.includes("Registered tunnel connection")) connected = true;
        } else if (e.stage === "done") {
          done = e.status;
          if (e.apply) applied = applyNote(e.apply, true);
        } else if (e.stage === "error") failure = e.message;
      });
      if (failure) throw new Error(failure);
      if (!done) throw new Error("The setup stopped without finishing; check the tunnel below and set it up again.");
      onChange(done);
      setEditing(false);
      if (connected) setNote(`The tunnel is up.${applied ? ` ${applied}` : ""}`);
      else
        setError(
          `cloudflared is running but hasn't connected to Cloudflare yet; see its log below.${applied ? ` ${applied}` : ""}`,
        );
    });

  const control = (action: "start" | "stop" | "restart") =>
    run(action, async () => {
      onChange(await api.controlTunnel(action));
    });

  const remove = () => {
    if (
      !confirm(
        "Remove the tunnel? cloudflared is stopped, the DNS records hlm made are removed, and the tunnel is deleted in Cloudflare. Your hostnames are kept for the next tunnel.",
      )
    )
      return;
    run("remove", async () => {
      onChange(await api.removeTunnel());
      setNote("Removed the tunnel.");
    });
  };

  const form = (
    <div className="field-grid">
      <label className="field">
        <span className="field-label">Tunnel name</span>
        <input
          value={input.name}
          onChange={(e) => setInput({ ...input, name: e.target.value })}
          placeholder="hlm"
          disabled={!!t}
          spellCheck={false}
        />
        <span className="field-help">
          {t ? "The tunnel keeps its name." : "How it's listed in the Cloudflare dashboard (Zero Trust → Networks → Tunnels)."}
        </span>
      </label>
      <label className="field">
        <span className="field-label">Reach servers with their own LAN address</span>
        <select value={input.lanNetwork} onChange={(e) => setInput({ ...input, lanNetwork: e.target.value })}>
          <option value="">No: only this host&apos;s ports (host networking)</option>
          {status.lanNetworks.map((n) => (
            <option key={n.name} value={n.name}>
              Through {n.name} ({n.driver} {n.subnet}
              {n.parent ? ` on ${n.parent}` : ""})
            </option>
          ))}
        </select>
        <span className="field-help">
          {status.lanNetworks.length
            ? "For servers on a LAN network with their own address, which the host itself can't reach: cloudflared joins that network (and Docker's bridge, for published ports)."
            : "No macvlan or ipvlan networks on the Docker host. Servers with published ports don't need one."}
        </span>
      </label>
      {input.lanNetwork && (
        <label className="field">
          <span className="field-label">cloudflared&apos;s LAN address (optional)</span>
          <input
            value={input.lanAddress}
            onChange={(e) => setInput({ ...input, lanAddress: e.target.value })}
            placeholder="Docker chooses one"
            spellCheck={false}
          />
          <span className="field-help">A free address on that network, outside your router&apos;s DHCP range.</span>
        </label>
      )}
    </div>
  );

  const consoleView = log.length > 0 && (
    <pre className="op-output wg-console" ref={consoleRef} aria-live="polite">
      {log.map((l, i) => (l.step ? <strong key={i}>{`▸ ${l.text}\n`}</strong> : <span key={i}>{`${l.text}\n`}</span>))}
      {busy === "setup" && <span className="muted">…</span>}
    </pre>
  );
  const messages = (
    <>
      {error && <div className="alert error">{error}</div>}
      {note && <div className="alert success">{note}</div>}
    </>
  );
  const setupLabel = busy === "setup" ? "Setting up…" : undefined;

  if (!t) {
    return (
      <>
        <p className="muted small">
          hlm creates the tunnel in your Cloudflare account and runs <code>cloudflared</code> in a{" "}
          <code>hlm-cloudflared</code> container on the Docker host ({status.image}), restarting unless stopped.
        </p>
        {form}
        {messages}
        {consoleView}
        <div className="btn-row">
          <button className="btn small primary" onClick={setup} disabled={!!busy}>
            {setupLabel ?? "Set up tunnel"}
          </button>
        </div>
      </>
    );
  }

  const c = t.connector;
  const healthy = t.health === "healthy";
  return (
    <>
      <div className="server-info">
        <ul>
          <li className={c.running ? "ok" : "bad"}>
            {c.running
              ? `The hlm-cloudflared container is running${c.startedAt ? ` (started ${timeAgo(c.startedAt)})` : ""}`
              : c.error || `The hlm-cloudflared container is ${c.state}${c.exitCode ? ` (exit code ${c.exitCode})` : ""}`}
          </li>
          {t.health !== undefined && (
            <li className={healthy ? "ok" : "bad"}>
              {HEALTH[t.health] ?? `Cloudflare reports the tunnel as ${t.health}`}
              {t.connections.length > 0 &&
                `: ${t.connections.length} connection${t.connections.length === 1 ? "" : "s"} (${[...new Set(t.connections.map((x) => x.colo_name))].join(", ")})`}
            </li>
          )}
          {t.lanNetwork && (
            <li className={c.lanAddress ? "ok" : "bad"}>
              Reaches servers&apos; own addresses on <code>{t.lanNetwork}</code> ({t.lanSubnet})
              {c.lanAddress ? (
                <>
                  {" "}
                  from <code>{c.lanAddress}</code>
                </>
              ) : (
                "; cloudflared has no address there yet"
              )}
            </li>
          )}
        </ul>
        <p className="muted small no-margin">
          Tunnel <strong>{t.name}</strong> · <code>{t.id}</code> · created {timeAgo(t.created)}
        </p>
      </div>
      {editing && form}
      {messages}
      {consoleView}
      <div className="btn-row">
        {c.running || c.state === "restarting" ? (
          <>
            <button className="btn small" onClick={() => control("restart")} disabled={!!busy}>
              {busy === "restart" ? "Restarting…" : "Restart"}
            </button>
            <button className="btn small" onClick={() => control("stop")} disabled={!!busy}>
              {busy === "stop" ? "Stopping…" : "Stop"}
            </button>
          </>
        ) : (
          c.state !== "missing" && (
            <button className="btn small primary" onClick={() => control("start")} disabled={!!busy}>
              {busy === "start" ? "Starting…" : "Start"}
            </button>
          )
        )}
        {editing ? (
          <button className="btn small primary" onClick={setup} disabled={!!busy}>
            {setupLabel ?? "Save & set up again"}
          </button>
        ) : (
          <>
            <button className="btn small" onClick={() => setEditing(true)} disabled={!!busy}>
              Change
            </button>
            <button className="btn small" onClick={setup} disabled={!!busy} title="Recreate the container with the latest image">
              {setupLabel ?? "Set up again"}
            </button>
          </>
        )}
        <button className="btn small danger" onClick={remove} disabled={!!busy}>
          {busy === "remove" ? "Removing…" : "Remove tunnel"}
        </button>
      </div>
    </>
  );
}
