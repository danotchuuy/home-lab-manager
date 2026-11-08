import { useEffect, useState } from "react";
import { api } from "../api";
import type { DeployEvent, LaunchOption } from "../types";
import { DeployProgress } from "./DeployProgress";

/**
 * A server's game's launch options, such as Nightingale's status endpoint,
 * from its game profile: what each does, whether it's on, and (for its
 * managers) a switch. Turning one on or off recreates the server with it.
 */
export function LaunchOptionsCard({
  server,
  running,
  canManage,
  onChanged,
}: {
  server: string;
  running: boolean;
  canManage: boolean;
  onChanged: () => void;
}) {
  const [options, setOptions] = useState<LaunchOption[]>();
  const [busy, setBusy] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>([]);

  const load = () => api.launchOptions(server).then(setOptions, () => setOptions([]));
  useEffect(() => {
    load();
  }, [server]);

  async function toggle(o: LaunchOption) {
    const on = !o.on;
    const what = `${on ? "Turn on" : "Turn off"} ${o.name}?`;
    if (!confirm(running ? `${what} ${server} restarts, disconnecting any players.` : what)) return;
    setBusy(o.id);
    setEvents([]);
    try {
      await api.setLaunchOption(server, o.id, on, (e) => setEvents((prev) => [...prev, e]));
    } catch (e) {
      setEvents((prev) => [...prev, { stage: "error", message: (e as Error).message }]);
    } finally {
      setBusy(undefined);
      await load();
      onChanged();
    }
  }

  if (!options || options.length === 0) return null;
  return (
    <div className="card launch-options">
      <h2>Game options</h2>
      {options.map((o) => (
        <div key={o.id} className="launch-option">
          <label className="switch">
            <input type="checkbox" checked={o.on} disabled={!canManage || !!busy} onChange={() => toggle(o)} />
            <span className="switch-track" aria-hidden="true" />
            <strong>{o.name}</strong>
            {busy === o.id && <span className="muted small">{o.on ? "Turning off…" : "Turning on…"}</span>}
          </label>
          {o.description && <p className="muted small no-margin">{o.description}</p>}
          <p className="muted small no-margin">
            Adds <code>{o.args.replaceAll("{port}", String(o.port ?? ""))}</code> to {o.where}
            {o.port ? `, and port ${o.port}/${o.proto || "tcp"} to its ports` : ""}.
          </p>
        </div>
      ))}
      {events.length > 0 && <DeployProgress events={events} />}
    </div>
  );
}
