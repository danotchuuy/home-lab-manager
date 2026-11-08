import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { SYSTEM_SECTIONS, SYSTEM_TABS, sectionPath } from "../systemSections";
import { api } from "../api";
import { getTheme, setTheme, type Theme } from "../theme";
import type { Job, Server } from "../types";
import { bareName } from "../util";

// --- theme ---

const THEMES: { value: Theme; glyph: string; label: string }[] = [
  { value: "system", glyph: "◐", label: "System" },
  { value: "light", glyph: "☀", label: "Light" },
  { value: "dark", glyph: "☾", label: "Dark" },
];

/** Header button: System → Light → Dark. */
export function ThemeToggle() {
  const [theme, set] = useState<Theme>(getTheme);
  const i = THEMES.findIndex((t) => t.value === theme);
  const next = THEMES[(i + 1) % THEMES.length];
  return (
    <button
      type="button"
      className="icon-button theme-toggle"
      aria-label={`Theme: ${THEMES[i].label}. Switch to ${next.label}`}
      title={`Theme: ${THEMES[i].label} (click for ${next.label})`}
      onClick={() => {
        setTheme(next.value);
        set(next.value);
      }}
    >
      <span aria-hidden="true">{THEMES[i].glyph}</span>
    </button>
  );
}

// --- toasts ---

const TOAST_EVENT = "hlm-toast";

interface Toast {
  id: number;
  message: string;
  kind: "info" | "success" | "error";
  link?: string;
}

/** Shows a short message in the corner. */
export function toast(message: string, kind: Toast["kind"] = "info", link?: string) {
  window.dispatchEvent(new CustomEvent(TOAST_EVENT, { detail: { message, kind, link } }));
}

/** Where toasts appear; read out by screen readers. */
export function Toaster() {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const navigate = useNavigate();
  useEffect(() => {
    let n = 0;
    const on = (e: Event) => {
      const t = { id: ++n, ...(e as CustomEvent).detail } as Toast;
      setToasts((prev) => [...prev.slice(-3), t]);
      setTimeout(() => setToasts((prev) => prev.filter((x) => x.id !== t.id)), t.kind === "error" ? 10000 : 6000);
    };
    window.addEventListener(TOAST_EVENT, on);
    return () => window.removeEventListener(TOAST_EVENT, on);
  }, []);
  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={`toast ${t.kind}`}>
          {t.link ? (
            <button type="button" className="link-button" onClick={() => navigate(t.link!)}>
              {t.message}
            </button>
          ) : (
            t.message
          )}
          <button
            type="button"
            className="icon-button"
            aria-label="Dismiss"
            onClick={() => setToasts((prev) => prev.filter((x) => x.id !== t.id))}
          >
            ×
          </button>
        </div>
      ))}
    </div>
  );
}

const JOB_DONE: Record<Job["kind"], [string, string]> = {
  install: ["installed", "Installing"],
  update: ["updated", "Updating"],
  reconfigure: ["changed", "Changing"],
};

/** Says when an install or update finishes, unless it's the server being looked at. */
export function JobToasts({ enabled }: { enabled: boolean }) {
  const location = useLocation();
  const here = useRef(location.pathname);
  here.current = location.pathname;
  useEffect(() => {
    if (!enabled) return;
    const running = new Set<string>();
    let first = true;
    const poll = async () => {
      if (document.hidden) return;
      let jobs: Job[];
      try {
        jobs = await api.jobs();
      } catch {
        return;
      }
      for (const j of jobs) {
        if (!j.finished) {
          running.add(j.id);
          continue;
        }
        if (!running.delete(j.id) || first) continue;
        const page = `/servers/${encodeURIComponent(j.server)}`;
        if (here.current === page) continue;
        const [done, doing] = JOB_DONE[j.kind] ?? ["done", "Working on"];
        if (j.error) toast(`${doing} ${bareName(j.server)} failed: ${j.error}`, "error", page);
        else toast(`${bareName(j.server)} was ${done}`, "success", page);
      }
      first = false;
    };
    poll();
    const t = setInterval(poll, 5000);
    return () => clearInterval(t);
  }, [enabled]);
  return null;
}

// --- keyboard ---

const SHORTCUTS: [string, string][] = [
  ["/", "Search the server list"],
  ["Ctrl K  or  ⌘ K", "Command palette: go to a server or page, or start, stop or restart a server"],
  ["g s", "Servers"],
  ["g c", "Catalog"],
  ["g h", "Host"],
  ["g y", "System"],
  ["?", "These shortcuts"],
  ["Esc", "Close"],
];

function typing(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  return !!el && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName));
}

/** Keyboard shortcuts, their help, and the command palette. */
export function Shortcuts({ admin }: { admin: boolean }) {
  const navigate = useNavigate();
  const [help, setHelp] = useState(false);
  const [palette, setPalette] = useState(false);
  useEffect(() => {
    let g = 0; // when "g" was pressed
    const on = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
        return;
      }
      if (e.key === "Escape") {
        setHelp(false);
        setPalette(false);
        return;
      }
      if (typing(e.target) || e.ctrlKey || e.metaKey || e.altKey) return;
      if (e.key === "?") {
        setHelp((h) => !h);
        return;
      }
      if (e.key === "/") {
        e.preventDefault();
        const search = document.querySelector<HTMLInputElement>("[data-search]");
        if (search) search.focus();
        else navigate("/?focus=search");
        return;
      }
      if (e.key === "g") {
        g = Date.now();
        return;
      }
      if (Date.now() - g < 1500) {
        const to = { s: "/", c: "/catalog", h: "/host", y: "/system" }[e.key];
        g = 0;
        if (to && (admin || to === "/")) navigate(to);
      }
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [navigate, admin]);
  return (
    <>
      {help && (
        <div className="modal-backdrop" onClick={() => setHelp(false)}>
          <div
            className="card modal shortcuts"
            role="dialog"
            aria-label="Keyboard shortcuts"
            onClick={(e) => e.stopPropagation()}
          >
            <h2>Keyboard shortcuts</h2>
            <table className="data-table">
              <tbody>
                {SHORTCUTS.map(([k, what]) => (
                  <tr key={k}>
                    <td>
                      <kbd>{k}</kbd>
                    </td>
                    <td>{what}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="btn-row">
              <button className="btn" onClick={() => setHelp(false)} autoFocus>
                Close
              </button>
            </div>
          </div>
        </div>
      )}
      {palette && <CommandPalette admin={admin} onClose={() => setPalette(false)} />}
    </>
  );
}

interface Command {
  label: string;
  hint?: string;
  /** More words it's found by. */
  keywords?: string;
  run: () => void | Promise<void>;
}

/** Ctrl/⌘-K: type to find a server or page, or an action on a server. */
function CommandPalette({ admin, onClose }: { admin: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [servers, setServers] = useState<Server[]>([]);
  const [active, setActive] = useState(0);
  useEffect(() => {
    api.servers().then(setServers, () => {});
  }, []);

  const commands = useMemo(() => {
    const out: Command[] = [];
    const go = (label: string, to: string) => out.push({ label, hint: "page", run: () => navigate(to) });
    go("Servers", "/");
    if (admin) {
      go("Catalog", "/catalog");
      go("Host", "/host");
      go("Proxy", "/proxy");
      go("Tunnel", "/tunnel");
      go("System", "/system");
      go("Import a server", "/import");
      // Each of System's tabs and settings, found by its words too.
      for (const t of SYSTEM_TABS) {
        if (t.id !== "overview") out.push({ label: `System: ${t.label}`, hint: "page", run: () => navigate(`/system?tab=${t.id}`) });
      }
      for (const sec of SYSTEM_SECTIONS) {
        if (sec.id === "release-notes") continue;
        out.push({ label: `System: ${sec.title}`, hint: "setting", keywords: sec.keywords, run: () => navigate(sectionPath(sec)) });
      }
    }
    for (const s of servers) {
      const page = `/servers/${encodeURIComponent(s.name)}`;
      out.push({ label: s.name, hint: s.app, run: () => navigate(page) });
      const operator = s.access === "admin" || s.access === "manager" || s.access === "operator";
      if (!operator) continue;
      const act = (verb: "start" | "stop" | "restart", label: string) =>
        out.push({
          label: `${label} ${s.name}`,
          hint: "action",
          run: async () => {
            try {
              await api.action(s.id, verb);
              toast(
                `${label === "Start" ? "Started" : label === "Stop" ? "Stopped" : "Restarted"} ${bareName(s.name)}`,
                "success",
                page,
              );
            } catch (e) {
              toast(`${label} ${bareName(s.name)} failed: ${(e as Error).message}`, "error", page);
            }
          },
        });
      if (s.state === "running") {
        act("restart", "Restart");
        act("stop", "Stop");
      } else act("start", "Start");
    }
    return out;
  }, [servers, admin, navigate]);

  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const shown = commands
    .filter((c) => words.every((w) => `${c.label} ${c.hint ?? ""} ${c.keywords ?? ""}`.toLowerCase().includes(w)))
    // Without a query, actions and settings would crowd out the places to go.
    .filter((c) => words.length > 0 || (c.hint !== "action" && c.hint !== "setting"))
    .slice(0, 12);

  function run(c?: Command) {
    if (!c) return;
    onClose();
    c.run();
  }

  return (
    <div className="modal-backdrop palette-backdrop" onClick={onClose}>
      <div
        className="card modal palette"
        role="dialog"
        aria-label="Command palette"
        onClick={(e) => e.stopPropagation()}
      >
        <input
          autoFocus
          value={query}
          placeholder="Go to a server or page, or type “restart valheim”…"
          aria-label="Command"
          aria-controls="palette-results"
          aria-activedescendant={shown[active] ? `palette-${active}` : undefined}
          onChange={(e) => {
            setQuery(e.target.value);
            setActive(0);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setActive((a) => Math.min(a + 1, shown.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setActive((a) => Math.max(a - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              run(shown[active]);
            }
          }}
        />
        <ul id="palette-results" role="listbox" className="palette-results">
          {shown.map((c, i) => (
            <li
              key={c.label + (c.hint ?? "")}
              id={`palette-${i}`}
              role="option"
              aria-selected={i === active}
              className={i === active ? "active" : undefined}
              onMouseEnter={() => setActive(i)}
              onClick={() => run(c)}
            >
              <span>{c.label}</span>
              {c.hint && <span className="muted small">{c.hint}</span>}
            </li>
          ))}
          {shown.length === 0 && <li className="muted">Nothing matches.</li>}
        </ul>
      </div>
    </div>
  );
}
