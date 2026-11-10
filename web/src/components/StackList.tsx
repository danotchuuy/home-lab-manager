import { useCallback, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useAdmin } from "../access";
import { api } from "../api";
import type { DeployEvent, Server, Stack } from "../types";
import { usePoll } from "../usePoll";
import { DeployProgress } from "./DeployProgress";
import { HostBadge, useHosts } from "./HostPicker";

// Stacks are open unless closed; the closed ones are remembered.
const CLOSED_KEY = "hlm.closedStacks";

function closedStacks(): string[] {
  try {
    return JSON.parse(localStorage.getItem(CLOSED_KEY) ?? "[]") as string[];
  } catch {
    return [];
  }
}

/**
 * The Servers page's Compose stacks: each a group with the stack's own
 * actions, holding its servers' cards as the rest of the list shows them.
 * servers are the stacks' servers the list shows (searched, filtered and
 * sorted); while filtering, only stacks with one of them are shown.
 */
export function StackList({
  servers,
  filtering,
  renderServer,
}: {
  servers: Server[];
  filtering: boolean;
  renderServer: (s: Server) => ReactNode;
}) {
  const admin = useAdmin();
  const hosts = useHosts();
  const [stacks, setStacks] = useState<Stack[]>([]);
  const [closed, setClosed] = useState<string[]>(closedStacks);
  const [events, setEvents] = useState<Record<string, DeployEvent[]>>({});
  const [busy, setBusy] = useState<string>();

  const load = useCallback(() => {
    api.stacks().then(setStacks, () => {});
  }, []);
  usePoll(load, busy ? 3000 : 10000, [load, busy]);

  const setOpen = (name: string, open: boolean) => {
    const next = open ? closed.filter((n) => n !== name) : [...closed.filter((n) => n !== name), name];
    setClosed(next);
    try {
      localStorage.setItem(CLOSED_KEY, JSON.stringify(next));
    } catch {
      // Not remembered.
    }
  };

  async function act(name: string, action: "start" | "stop" | "restart" | "update") {
    setBusy(name);
    setEvents((e) => ({ ...e, [name]: [] }));
    setOpen(name, true);
    try {
      await api.stackAction(name, action, (ev) => setEvents((e) => ({ ...e, [name]: [...(e[name] ?? []), ev] })));
    } catch (err) {
      setEvents((e) => ({ ...e, [name]: [...(e[name] ?? []), { stage: "error", message: (err as Error).message }] }));
    } finally {
      setBusy(undefined);
      load();
    }
  }

  const byStack = new Map<string, Server[]>();
  for (const srv of servers) if (srv.stack) byStack.set(srv.stack, [...(byStack.get(srv.stack) ?? []), srv]);
  const shown = stacks.filter((st) => !filtering || byStack.has(st.name));
  if (shown.length === 0) return null;
  return (
    <div className="stack-list">
      {shown.map((st) => {
        const up = st.services.filter((m) => m.state === "running").length;
        // While filtering, the matches show whether or not it's closed.
        const isOpen = filtering || !closed.includes(st.name);
        const working = busy === st.name || st.busy;
        const members = byStack.get(st.name) ?? [];
        const missing = st.services.filter((m) => m.state === "missing");
        return (
          <section key={st.name} className="stack-group">
            <div className="card stack-card">
              <div className="stack-head">
                <button className="link-button stack-toggle" aria-expanded={isOpen} onClick={() => setOpen(st.name, !isOpen)}>
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
                    {up > 0 ? "Restart all" : "Start all"}
                  </button>
                  {up > 0 && (
                    <button className="btn small danger" disabled={!!working} onClick={() => act(st.name, "stop")}>
                      Stop all
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
              {isOpen && missing.length > 0 && (
                <p className="muted small no-margin stack-missing">
                  Not created yet: {missing.map((m) => m.service).join(", ")}. Start the stack to create{" "}
                  {missing.length === 1 ? "it" : "them"}.
                </p>
              )}
              {(events[st.name]?.length ?? 0) > 0 && <DeployProgress events={events[st.name]} />}
            </div>
            {isOpen && members.length > 0 && <div className="server-list stack-servers">{members.map(renderServer)}</div>}
          </section>
        );
      })}
    </div>
  );
}
