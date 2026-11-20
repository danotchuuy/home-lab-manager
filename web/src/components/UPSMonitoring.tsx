import { useEffect, useState } from "react";
import { api } from "../api";
import type { DockerHost, NUTUPS, UPSSettings } from "../types";
import { Spinner } from "./common";

const same = (a?: UPSSettings, b?: UPSSettings) => JSON.stringify(a) === JSON.stringify(b);

/** Settings without empty UPS names, as they're saved. */
function normal(s: UPSSettings): UPSSettings {
  const hosts: Record<string, string> = {};
  for (const [h, name] of Object.entries(s.hosts)) if (name.trim()) hosts[h] = name.trim();
  // Only hosts with a UPS can be shut down when it runs low.
  const shutdown = [...new Set(s.shutdown ?? [])].filter((h) => hosts[h]).sort();
  return {
    ...s,
    address: s.address.trim(),
    username: s.username?.trim() ?? "",
    password: s.password ?? "",
    hosts,
    shutdown,
  };
}

/**
 * System → UPS: the Network UPS Tools server, the UPS each host is on, and
 * what hlm does when a battery runs low.
 */
export function UPSMonitoring() {
  const [saved, setSaved] = useState<UPSSettings>();
  const [form, setForm] = useState<UPSSettings>();
  const [hosts, setHosts] = useState<DockerHost[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [askFrom, setAskFrom] = useState("local");
  const [found, setFound] = useState<NUTUPS[]>();
  const [finding, setFinding] = useState(false);
  const [findError, setFindError] = useState<string>();

  useEffect(() => {
    api.upsSettings().then(
      (s) => {
        const n = normal(s);
        setSaved(n);
        setForm(n);
      },
      (e) => setError((e as Error).message),
    );
    api.hosts().then(
      (l) => setHosts(l.hosts),
      () => setHosts([]),
    );
  }, []);

  if (!form) return error ? <div className="alert error small">{error}</div> : <Spinner />;
  const set = (patch: Partial<UPSSettings>) => {
    setForm({ ...form, ...patch });
    setNotice(undefined);
  };
  const changed = !same(normal(form), saved);
  const allHosts = hosts.length > 0 ? hosts : [{ id: "local", name: "This host", local: true } as DockerHost];
  const hostLabel = (h: DockerHost) => (h.local ? "This host" : `${h.name} (${h.id})`);
  const shutdown = form.shutdown ?? [];
  const setShutdown = (id: string, on: boolean) =>
    set({ shutdown: on ? [...shutdown.filter((h) => h !== id), id] : shutdown.filter((h) => h !== id) });
  const anyAgent = allHosts.some((h) => h.kind === "agent");

  async function save() {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const s = normal(await api.setUPSSettings(normal(form!)));
      setSaved(s);
      setForm(s);
      setNotice("Saved.");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function find() {
    setFinding(true);
    setFindError(undefined);
    setFound(undefined);
    try {
      setFound(await api.listUPS(normal(form!), askFrom));
    } catch (e) {
      setFindError((e as Error).message);
    } finally {
      setFinding(false);
    }
  }

  return (
    <div className="ups-settings">
      <p className="muted small no-margin">
        hlm reads each host&apos;s UPS from a <a href="https://networkupstools.org/">Network UPS Tools</a> server
        (upsd). A host with hlm&apos;s agent asks the NUT server itself, so it only has to be reachable from that host;
        hlm asks for this host and hosts reached over SSH or TLS. Choose the <strong>Power</strong> events under{" "}
        <a href="#notifications">Notifications</a> to hear about outages.
      </p>

      <div className="field-grid">
        <label className="field">
          <span className="field-label">NUT server</span>
          <input
            value={form.address}
            onChange={(e) => set({ address: e.target.value })}
            placeholder="e.g. nas.lan or 192.168.1.10:3493"
          />
        </label>
        <label className="field">
          <span className="field-label">Username (optional)</span>
          <input value={form.username ?? ""} onChange={(e) => set({ username: e.target.value })} autoComplete="off" />
        </label>
        <label className="field">
          <span className="field-label">Password</span>
          <input
            type="password"
            value={form.password ?? ""}
            onChange={(e) => set({ password: e.target.value })}
            placeholder={saved?.hasPassword && form.username ? "Saved (leave empty to keep)" : ""}
            disabled={!form.username}
            autoComplete="new-password"
          />
        </label>
      </div>
      <span className="field-help">
        Reading a UPS usually needs no account. NUT sends the password unencrypted, so use a read-only account
        from <code>upsd.users</code>.
      </span>

      <h4>UPS per host</h4>
      <div className="btn-row">
        <button className="btn small" onClick={find} disabled={finding || !form.address.trim()}>
          {finding ? "Looking…" : "List the server's UPSes"}
        </button>
        {allHosts.length > 1 && (
          <label className="muted small">
            as seen from{" "}
            <select value={askFrom} onChange={(e) => setAskFrom(e.target.value)}>
              {allHosts.map((h) => (
                <option key={h.id} value={h.id}>
                  {hostLabel(h)}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>
      {findError && <div className="alert error small">{findError}</div>}
      {found && (
        <p className="muted small">
          {found.length === 0
            ? "The NUT server has no UPSes."
            : `The NUT server has ${found.map((u) => (u.description ? `${u.name} (${u.description})` : u.name)).join(", ")}.`}
        </p>
      )}
      <datalist id="nut-upses">
        {found?.map((u) => (
          <option key={u.name} value={u.name}>
            {u.description}
          </option>
        ))}
      </datalist>
      <table className="data-table">
        <thead>
          <tr>
            <th>Host</th>
            <th>UPS on the NUT server</th>
            {anyAgent && <th>Shut down when low</th>}
          </tr>
        </thead>
        <tbody>
          {allHosts.map((h) => (
            <tr key={h.id}>
              <td>{hostLabel(h)}</td>
              <td>
                <input
                  aria-label={`UPS for ${hostLabel(h)}`}
                  list="nut-upses"
                  value={form.hosts[h.id] ?? ""}
                  onChange={(e) => set({ hosts: { ...form.hosts, [h.id]: e.target.value } })}
                  placeholder="Not watched"
                />
              </td>
              {anyAgent && (
                <td>
                  {h.kind === "agent" ? (
                    <input
                      type="checkbox"
                      aria-label={`Shut ${hostLabel(h)} down when its battery runs low`}
                      checked={shutdown.includes(h.id)}
                      disabled={!form.stopOnLow || !form.hosts[h.id]?.trim()}
                      onChange={(e) => setShutdown(h.id, e.target.checked)}
                    />
                  ) : (
                    <span className="muted small" title="Only hosts with hlm's agent can be shut down">
                      —
                    </span>
                  )}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>

      <h4>When the battery runs low</h4>
      <label className="radio small">
        <input
          type="checkbox"
          checked={form.stopOnLow}
          onChange={(e) => set({ stopOnLow: e.target.checked, shutdown: e.target.checked ? shutdown : [] })}
        />
        Stop the host&apos;s running servers and stacks
      </label>
      {form.stopOnLow && (
        <div className="field-grid">
          <label className="field">
            <span className="field-label">On battery and charge below (%)</span>
            <input
              type="number"
              min={0}
              max={95}
              value={form.stopBelowCharge}
              onChange={(e) => set({ stopBelowCharge: Number(e.target.value) })}
            />
          </label>
          <label className="field">
            <span className="field-label">or runtime below (minutes)</span>
            <input
              type="number"
              min={0}
              max={120}
              value={form.stopBelowRuntime}
              onChange={(e) => set({ stopBelowRuntime: Number(e.target.value) })}
            />
          </label>
        </div>
      )}
      {form.stopOnLow && shutdown.length > 0 && (
        <label className="radio small">
          <input
            type="checkbox"
            checked={form.shutdownIdle}
            onChange={(e) => set({ shutdownIdle: e.target.checked })}
          />
          Shut those hosts down sooner if nothing is running: once on battery for
          <span className="inline-input">
            <input
              type="number"
              min={0}
              max={120}
              value={form.shutdownIdleAfter}
              onChange={(e) => set({ shutdownIdleAfter: Number(e.target.value) })}
              disabled={!form.shutdownIdle}
              aria-label="Minutes on battery"
            />
          </span>
          minutes with none of hlm&apos;s servers or stacks running there, leaving the battery to hosts that have some
        </label>
      )}
      {form.stopOnLow && shutdown.length > 0 && (
        <div className="alert warn small">
          Hosts ticked under <strong>Shut down when low</strong> power off once their servers have stopped: their agent
          runs the host&apos;s own <code>poweroff</code>. If mains comes back before the battery runs out, the UPS never
          cuts power, so a machine set to power on when AC returns stays off. Use Wake-on-LAN, or NUT&apos;s upsmon
          with killpower, if it has to come back by itself.
        </div>
      )}
      {form.stopOnLow && (
        <span className="field-help">
          Servers also stop when the UPS itself says its battery is low. 0 leaves a limit out. Only hlm&apos;s servers
          and stacks are stopped, each with its usual stop timeout, so leave enough runtime for them to save.
        </span>
      )}
      <label className="radio small">
        <input
          type="checkbox"
          checked={form.restartOnPower}
          onChange={(e) => set({ restartOnPower: e.target.checked })}
          disabled={!form.stopOnLow}
        />
        Start them again once the power has been back for
        <span className="inline-input">
          <input
            type="number"
            min={0}
            max={1440}
            value={form.restartAfter}
            onChange={(e) => set({ restartAfter: Number(e.target.value) })}
            disabled={!form.stopOnLow || !form.restartOnPower}
            aria-label="Minutes"
          />
        </span>
        minutes
      </label>

      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button className="btn small primary" onClick={save} disabled={busy || !changed}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
