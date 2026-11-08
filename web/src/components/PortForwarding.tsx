import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { ForwardRule, ForwardStatus } from "../types";
import { Spinner } from "./common";

const PROTO: Record<string, string> = { tcp: "TCP", udp: "UDP", tcp_udp: "TCP + UDP" };

function stateBadge(r: ForwardRule) {
  if (r.conflicts?.length && r.state === "missing") return <span className="badge state-dead">conflict</span>;
  switch (r.state) {
    case "forwarded":
      return <span className="badge state-running">forwarded</span>;
    case "outdated":
      return <span className="badge state-restarting">out of date</span>;
    case "missing":
      return <span className="badge">not forwarded</span>;
    default:
      return <span className="badge">unknown</span>;
  }
}

/** Shows and manages a server's port forwarding rules on the UniFi gateway. */
export function PortForwarding({ server, onChanged }: { server: string; onChanged?: () => void }) {
  const [status, setStatus] = useState<ForwardStatus>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string>();

  const load = useCallback(async () => {
    try {
      setStatus(await api.portForward(server));
      setError(undefined);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [server]);

  useEffect(() => {
    load();
  }, [load]);

  async function sync() {
    setBusy(true);
    setNotice(undefined);
    try {
      const st = await api.syncPortForward(server);
      setStatus(st);
      setError(undefined);
      if (!st.error) setNotice("Port forwarding is up to date.");
      onChanged?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function stop() {
    if (!confirm(`Remove ${server}'s port forwarding rules? Players outside your network won't be able to connect.`)) return;
    setBusy(true);
    setNotice(undefined);
    try {
      const r = await api.removePortForward(server);
      setNotice(`Removed ${r.removed} rule${r.removed === 1 ? "" : "s"}.`);
      await load();
      onChanged?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (error && !status) return <div className="card"><div className="alert error">{error}</div></div>;
  if (!status) return <div className="card"><Spinner /></div>;

  if (!status.configured) {
    return (
      <div className="card">
        <h2>Port forwarding</h2>
        <p className="muted">
          Connect your UniFi gateway to open this server&apos;s ports to the internet from here.
        </p>
        <Link className="btn primary small" to="/system#unifi">
          Set up UniFi
        </Link>
      </div>
    );
  }

  const ours = status.rules.filter((r) => r.existing);
  const needsWork = status.rules.some((r) => r.state !== "forwarded") || status.stale.length > 0;

  return (
    <div className="card port-forwarding">
      <div className="settings-actions">
        <div>
          <h2>Port forwarding</h2>
          <p className="muted small no-margin">
            Rules on your UniFi gateway that send internet traffic to this server
            {status.target && (
              <>
                {" "}
                at <code>{status.target}</code>
              </>
            )}
            . Forwarded ports are reachable by anyone on the internet.
          </p>
        </div>
        <div className="btn-row">
          {status.rules.length > 0 && (
            <button className="btn small primary" onClick={sync} disabled={busy || !needsWork}>
              {busy ? "Working…" : ours.length ? "Update rules" : "Forward ports"}
            </button>
          )}
          {(ours.length > 0 || status.stale.length > 0) && (
            <button className="btn small danger" onClick={stop} disabled={busy}>
              Stop forwarding
            </button>
          )}
        </div>
      </div>

      {status.note && <div className="alert warn small">{status.note}</div>}
      {status.error && <div className="alert error">{status.error}</div>}
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert success">{notice}</div>}

      {status.rules.length > 0 && (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Internet port</th>
                <th>Forwards to</th>
                <th>Protocol</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {status.rules.map((r) => (
                <tr key={r.key}>
                  <td>
                    <code>{r.dstPort}</code>
                  </td>
                  <td>
                    <code>
                      {r.fwd}:{r.fwdPort}
                    </code>
                    {r.state === "outdated" && r.existing && (
                      <div className="muted small">
                        currently {r.existing.enabled ? "" : "disabled, "}→ {r.existing.fwd}:{r.existing.fwd_port}
                      </div>
                    )}
                  </td>
                  <td>{PROTO[r.proto] ?? r.proto}</td>
                  <td>
                    {stateBadge(r)}
                    {r.conflicts?.map((c) => (
                      <div key={c._id} className="small form-error">
                        Port already used by &quot;{c.name}&quot; → {c.fwd}:{c.fwd_port}
                      </div>
                    ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {status.stale.length > 0 && (
        <p className="muted small">
          {status.stale.length} older rule{status.stale.length === 1 ? "" : "s"} for ports this server no longer uses (
          {status.stale.map((s) => s.dst_port).join(", ")}) will be removed by &quot;Update rules&quot;.
        </p>
      )}
    </div>
  );
}
