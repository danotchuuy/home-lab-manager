import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { ForwardStatus, Server } from "../types";
import { ServerForwarding } from "./ServerForwarding";

/** A server's UniFi rules in a few words. */
function summary(st?: ForwardStatus | Error): { text: string; ok: boolean } {
  if (!st) return { text: "checking…", ok: true };
  if (st instanceof Error) return { text: st.message, ok: false };
  if (st.error) return { text: st.error, ok: false };
  const n = (s: string) => st.rules.filter((r) => r.state === s).length;
  const parts = [
    n("forwarded") && `${n("forwarded")} forwarded`,
    n("outdated") && `${n("outdated")} to update`,
    n("missing") && `${n("missing")} not yet`,
    st.stale.length && `${st.stale.length} to remove`,
  ].filter(Boolean) as string[];
  if (parts.length === 0) return { text: st.note || "nothing to forward", ok: true };
  return { text: parts.join(" · "), ok: n("outdated") + n("missing") + st.stale.length === 0 };
}

/**
 * Every server's UniFi port forwarding, under System → UniFi: what's in
 * place, the same ports panel as each server's Port forwarding tab, and
 * updating every server's rules at once.
 */
export function UnifiServers() {
  const [servers, setServers] = useState<Server[]>();
  const [status, setStatus] = useState<Record<string, ForwardStatus | Error>>({});
  const [open, setOpen] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<{ ok: boolean; text: string }>();

  const check = useCallback(async (name: string) => {
    try {
      const st = await api.portForward(name);
      setStatus((m) => ({ ...m, [name]: st }));
    } catch (e) {
      setStatus((m) => ({ ...m, [name]: e as Error }));
    }
  }, []);

  useEffect(() => {
    api.servers().then((list) => {
      const own = list.filter((s) => !s.stack);
      setServers(own);
      own.forEach((s) => check(s.name));
    }, () => setServers([]));
  }, [check]);

  // Brings every server's rules up to date, one at a time.
  async function updateAll() {
    if (!servers) return;
    setBusy(true);
    setNote(undefined);
    const failed: string[] = [];
    for (const s of servers) {
      const st = status[s.name];
      if (!st || st instanceof Error || (st.rules.length === 0 && st.stale.length === 0)) continue;
      try {
        const next = await api.syncPortForward(s.name);
        setStatus((m) => ({ ...m, [s.name]: next }));
        if (next.error) failed.push(`${s.name} (${next.error})`);
      } catch (e) {
        failed.push(`${s.name} (${(e as Error).message})`);
      }
    }
    setNote(failed.length ? { ok: false, text: `Not updated: ${failed.join("; ")}.` } : { ok: true, text: "Every server's rules are up to date." });
    setBusy(false);
  }

  if (!servers || servers.length === 0) return null;
  const pending = Object.values(status).some((st) => !(st instanceof Error) && !summary(st).ok);
  return (
    <div className="unifi-servers">
      <div className="settings-actions">
        <h3>Servers</h3>
        <button type="button" className="btn small" onClick={updateAll} disabled={busy || !pending}>
          {busy ? "Updating…" : "Update all rules"}
        </button>
      </div>
      <p className="muted small no-margin">
        Each server&apos;s rules on the gateway. <strong>Ports</strong> opens the same settings as its Port forwarding tab.
      </p>
      <table className="data-table">
        <tbody>
          {servers.map((s) => {
            const sum = summary(status[s.name]);
            return (
              <tr key={s.id}>
                <td>
                  <Link to={`/servers/${encodeURIComponent(s.name)}?tab=ports`}>{s.name}</Link>{" "}
                  <span className="muted small">{s.app}</span>
                </td>
                <td className={sum.ok ? "muted small" : "warn-text small"}>{sum.text}</td>
                <td className="actions">
                  <button
                    type="button"
                    className={`btn small ghost${open === s.name ? " active" : ""}`}
                    onClick={() => setOpen(open === s.name ? undefined : s.name)}
                  >
                    Ports
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {note && <div className={`alert ${note.ok ? "success" : "error"} small`}>{note.text}</div>}
      {open && (
        <section className="embedded-forwarding">
          <div className="settings-actions">
            <h2>{open}&apos;s ports</h2>
            <button type="button" className="btn small ghost" onClick={() => setOpen(undefined)}>
              Close
            </button>
          </div>
          <ServerForwarding key={open} server={open} canManage hostnames={false} onSaved={() => check(open)} />
        </section>
      )}
    </div>
  );
}
