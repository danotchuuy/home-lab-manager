import { useEffect, useState } from "react";
import { api } from "../api";
import type { DockerHost } from "../types";

/** The Docker hosts, loaded once per page. */
export function useHosts(): DockerHost[] | undefined {
  const [hosts, setHosts] = useState<DockerHost[]>();
  useEffect(() => {
    api.hosts().then(
      (r) => setHosts(r.hosts),
      () => setHosts([]),
    );
  }, []);
  return hosts;
}

/** Picks the Docker host a new server goes on. Shows nothing while there's only this host. */
export function HostPicker({
  value,
  onChange,
  help,
}: {
  value: string;
  onChange: (host: string) => void;
  help?: string;
}) {
  const hosts = useHosts();
  if (!hosts || hosts.length < 2) return null;
  return (
    <label className="field host-picker">
      <span className="field-label">Docker host</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {hosts.map((h) => (
          <option key={h.id} value={h.id} disabled={h.status ? !h.status.ok : false}>
            {h.name}
            {h.local ? "" : ` (${h.id})`}
            {h.status && !h.status.ok ? " — can't be reached" : ""}
          </option>
        ))}
      </select>
      <span className="field-help">
        {help ?? "The machine the server runs on. Its data folders and ports are on that machine."}
      </span>
    </label>
  );
}

/** Marks a server that's on another Docker host. */
export function HostBadge({ host, hosts }: { host?: string; hosts?: DockerHost[] }) {
  if (!host || host === "local") return null;
  const h = hosts?.find((x) => x.id === host);
  const down = h?.status && !h.status.ok;
  return (
    <span
      className={`badge host-badge${down ? " down" : ""}`}
      title={down ? `${h?.name} can't be reached` : "Docker host"}
    >
      {h?.name ?? host}
    </span>
  );
}
