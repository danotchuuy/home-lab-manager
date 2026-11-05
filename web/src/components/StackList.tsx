import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api } from "../api";
import type { DeployEvent, Stack } from "../types";
import { usePoll } from "../usePoll";
import { DeployProgress } from "./DeployProgress";
import { HostBadge, useHosts } from "./HostPicker";
import { StateBadge } from "./common";

const OPEN_KEY = "hlm.openStacks";

function openStacks(): string[] {
  try {
    return JSON.parse(localStorage.getItem(OPEN_KEY) ?? "[]") as string[];
  } catch {
    return [];
  }
}

/** The Servers page's Compose stacks: one card each, opening to its services. */
export function StackList({ group, groups }: { group: string; groups: Record<string, string[] | undefined> }) {
  const admin = useAdmin();
  const hosts = useHosts();
  const [stacks, setStacks] = useState<Stack[]>([]);
  const [open, setOpen] = useState<string[]>(openStacks);
  const [events, setEvents] = useState<Record<string, DeployEvent[]>>({});
  const [busy, setBusy] = useState<string>();

  const load = useCallback(() => {
    api.stacks().then(setStacks, () => {});
  }, []);
  usePoll(load, busy ? 3000 : 10000, [load, busy]);

  const toggle = (name: string) => {
    const next = open.includes(name) ? open.filter((n) => n !== name) : [...open, name];
    setOpen(next);
    try {
      localStorage.setItem(OPEN_KEY, JSON.stringify(next));
    } catch {
      // Not remembered.
    }
  };

  async function act(name: string, action: "start" | "stop" | "restart" | "update") {
    setBusy(name);
    setEvents((e) => ({ ...e, [name]: [] }));
    if (!open.includes(name)) toggle(name);
    try {
      await api.stackAction(name, action, (ev) => setEvents((e) => ({ ...e, [name]: [...(e[name] ?? []), ev] })));
    } catch (err) {
      setEvents((e) => ({ ...e, [name]: [...(e[name] ?? []), { stage: "error", message: (err as Error).message }] }));
    } finally {
      setBusy(undefined);
      load();
    }
  }

  // A group filter shows stacks with a service in that group.
  const shown = stacks.filter((st) => !group || st.services.some((m) => groups[m.server]?.includes(group)));
  if (shown.length === 0) return null;
  return (
    <div className="stack-list">
      {shown.map((st) => {
        const up = st.services.filter((m) => m.state === "running").length;
        const isOpen = open.includes(st.name);
        const working = busy === st.name || st.busy;
        return (
          <div key={st.name} className="card stack-card">
            <div className="stack-head">
              <button className="link-button stack-toggle" aria-expanded={isOpen} onClick={() => toggle(st.name)}>
                {isOpen ? "▾" : "▸"} <span className="stack-name">{st.name}</span>
              </button>
              <span className="badge">stack</span>
              <HostBadge host={st.host} hosts={hosts} />
              <span className={`muted small ${up === st.services.length ? "" : "stack-partial"}`}>
                {up} of {st.services.length} running
              </span>
              <div className="btn-row">
                <button
                  className="btn small"
                  disabled={!!working}
                  onClick={() => act(st.name, up > 0 ? "restart" : "start")}
                >
                  {up > 0 ? "Restart" : "Start"}
                </button>
                {up > 0 && (
                  <button className="btn small danger" disabled={!!working} onClick={() => act(st.name, "stop")}>
                    Stop
                  </button>
                )}
                <button
                  className="btn small"
                  disabled={!!working}
                  onClick={() => act(st.name, "update")}
                  title="Pull each service's image and recreate those that changed"
                >
                  Update
                </button>
                {admin && (
                  <Link className="btn small ghost" to={`/stacks/${encodeURIComponent(st.name)}`}>
                    Edit
                  </Link>
                )}
              </div>
            </div>
            {st.error && <div className="alert error small">The stack&apos;s file can&apos;t be read: {st.error}</div>}
            {isOpen && (
              <ul className="stack-services">
                {st.services.map((m) => (
                  <li key={m.service}>
                    {m.state === "missing" ? (
                      <span>{m.service}</span>
                    ) : (
                      <Link to={`/servers/${encodeURIComponent(m.server)}`}>{m.service}</Link>
                    )}
                    <StateBadge state={m.state} health={m.health} />
                    <code className="muted small">{m.image}</code>
                    <span className="muted small">{m.status}</span>
                  </li>
                ))}
              </ul>
            )}
            {(events[st.name]?.length ?? 0) > 0 && <DeployProgress events={events[st.name]} />}
          </div>
        );
      })}
    </div>
  );
}
