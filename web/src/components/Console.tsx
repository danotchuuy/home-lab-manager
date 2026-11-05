import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "../api";
import type { ConsoleInput, ConsoleMode, ConsoleStatus } from "../types";
import type { AppendLines } from "./LogViewer";

const HISTORY_SIZE = 100;
const historyKey = (server: string) => `hlm.console.${server}`;

function loadHistory(server: string): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(historyKey(server)) ?? "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  } catch {
    return [];
  }
}

function saveHistory(server: string, h: string[]) {
  try {
    localStorage.setItem(historyKey(server), JSON.stringify(h.slice(-HISTORY_SIZE)));
  } catch {
    // Storage may be unavailable; history just won't persist.
  }
}

const MODE_LABEL: Record<ConsoleMode, string> = {
  "": "Not set up",
  stdin: "Typed into the server's console (stdin)",
  rcon: "RCON",
  rest: "Palworld's REST API",
  exec: "A command run in the container",
};

/**
 * The command line under a server's console output. Commands and the replies
 * that come back (over RCON or from a command run in the container) are
 * added to the output; commands typed into the server's own console show
 * up in its output by themselves.
 */
export function ConsoleInputBar({
  server,
  running,
  canManage,
  append,
}: {
  server: string;
  running: boolean;
  canManage: boolean;
  append: AppendLines;
}) {
  const [status, setStatus] = useState<ConsoleStatus>();
  const [loadError, setLoadError] = useState<string>();
  const [command, setCommand] = useState("");
  const [busy, setBusy] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const history = useRef<string[]>(loadHistory(server));
  // Position while browsing history with the arrow keys; history.length is "not browsing".
  const pos = useRef(history.current.length);
  const draft = useRef("");
  const inputRef = useRef<HTMLInputElement>(null);

  const load = useCallback(async () => {
    try {
      setStatus(await api.console(server));
      setLoadError(undefined);
    } catch (e) {
      setLoadError((e as Error).message);
    }
  }, [server]);

  useEffect(() => {
    load();
  }, [load, running]);

  async function send(e?: React.FormEvent) {
    e?.preventDefault();
    const cmd = command.trim();
    if (!cmd || busy) return;
    setBusy(true);
    append(`> ${cmd}`, "command");
    const h = history.current.filter((x) => x !== cmd);
    h.push(cmd);
    history.current = h.slice(-HISTORY_SIZE);
    pos.current = history.current.length;
    saveHistory(server, history.current);
    setCommand("");
    try {
      const res = await api.consoleCommand(server, cmd);
      if (res.output) append(res.output, "reply");
      else if (res.mode !== "stdin") append("(no reply)", "reply");
      if (res.exitCode) append(`exit code ${res.exitCode}`, "error");
    } catch (err) {
      append((err as Error).message, "error");
      if (err instanceof ApiError && err.status === 400) load();
    } finally {
      setBusy(false);
      inputRef.current?.focus();
    }
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLInputElement>) {
    const h = history.current;
    if (e.key === "ArrowUp" && pos.current > 0) {
      if (pos.current === h.length) draft.current = command;
      pos.current--;
      setCommand(h[pos.current]);
      e.preventDefault();
    } else if (e.key === "ArrowDown" && pos.current < h.length) {
      pos.current++;
      setCommand(pos.current === h.length ? draft.current : h[pos.current]);
      e.preventDefault();
    }
  }

  function quick(q: string) {
    setCommand(q);
    pos.current = history.current.length;
    inputRef.current?.focus();
  }

  if (loadError) return <div className="alert error small">{loadError}</div>;
  if (!status) return null;

  const setUp = status.settings.mode !== "";

  return (
    <div className="console-input">
      {setUp && status.ready && status.quickCommands.length > 0 && (
        <div className="quick-commands">
          {status.quickCommands.map((q) => (
            <button key={q} type="button" className="chip" onClick={() => quick(q)} title="Put this in the command line">
              {q.trim()}
              {q.endsWith(" ") ? " …" : ""}
            </button>
          ))}
        </div>
      )}
      <form className="console-form" onSubmit={send}>
        <span className="console-prompt">&gt;</span>
        <input
          ref={inputRef}
          value={command}
          onChange={(e) => {
            setCommand(e.target.value);
            pos.current = history.current.length;
          }}
          onKeyDown={onKeyDown}
          placeholder={
            status.ready
              ? `Type a command (${MODE_LABEL[status.settings.mode]}); ↑ and ↓ go through earlier ones`
              : (status.reason ?? "")
          }
          disabled={!status.ready}
          maxLength={1024}
          spellCheck={false}
          autoComplete="off"
          aria-label="Console command"
        />
        <button type="submit" className="btn small" disabled={!status.ready || busy || !command.trim()}>
          {busy ? "Sending…" : "Send"}
        </button>
        {canManage && (
          <button type="button" className="btn small ghost" onClick={() => setSettingsOpen(!settingsOpen)}>
            {setUp ? "Console settings" : "Set up commands"}
          </button>
        )}
      </form>
      {!setUp && !canManage && (
        <p className="muted small">Sending commands isn&apos;t set up for this server; a manager of it can set it up.</p>
      )}
      {settingsOpen && canManage && (
        <ConsoleSettingsForm
          server={server}
          status={status}
          onSaved={(st) => {
            setStatus(st);
            setSettingsOpen(false);
          }}
          onCancel={() => setSettingsOpen(false)}
        />
      )}
    </div>
  );
}

function ConsoleSettingsForm({
  server,
  status,
  onSaved,
  onCancel,
}: {
  server: string;
  status: ConsoleStatus;
  onSaved: (st: ConsoleStatus) => void;
  onCancel: () => void;
}) {
  // Start from the saved settings, or what was detected if nothing is saved.
  const start = status.settings.mode ? status.settings : (status.detected ?? status.settings);
  const [form, setForm] = useState<ConsoleInput>({ ...start, password: "" });
  const [quickText, setQuickText] = useState((status.settings.quickCommands ?? []).join("\n"));
  const [clearPassword, setClearPassword] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const set = <K extends keyof ConsoleInput>(k: K, v: ConsoleInput[K]) => setForm((f) => ({ ...f, [k]: v }));

  const value = (): ConsoleInput => ({
    ...form,
    port: form.port || undefined,
    quickCommands: quickText
      .split("\n")
      .map((q) => q.trimEnd())
      .filter((q) => q.trim()),
    clearPassword,
  });

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

  const test = () => run("test", async () => setNotice((await api.testConsole(server, value())).message));
  const save = (e: React.FormEvent) => {
    e.preventDefault();
    run("save", async () => onSaved(await api.saveConsole(server, value())));
  };

  const detected = status.detected;
  const usingDetected =
    !!detected &&
    detected.mode === form.mode &&
    (detected.exec ?? "") === (form.exec ?? "") &&
    (detected.passwordVar ?? "") === (form.passwordVar ?? "");

  return (
    <form className="config-form console-settings" onSubmit={save}>
      {detected && !usingDetected && (
        <div className="alert info small">
          {status.detectedBecause}{" "}
          <button type="button" className="link-button" onClick={() => setForm({ ...detected, password: "" })}>
            Use that
          </button>
        </div>
      )}
      <div className="field">
        <span className="field-label">How this server takes commands</span>
        {(Object.keys(MODE_LABEL) as ConsoleMode[]).map((m) => (
          <label key={m} className="radio small">
            <input type="radio" checked={form.mode === m} onChange={() => set("mode", m)} />
            {MODE_LABEL[m]}
          </label>
        ))}
      </div>

      {form.mode === "stdin" && (
        <div className="field-help">
          Commands are typed into the server program, as if at its own console, and its answers appear in the output above.
          This suits servers that read commands from their console, such as Terraria, Factorio or Minecraft run directly.
          {!status.stdinOpen && (
            <div className="alert warn small">
              This container&apos;s Docker options turn its input off (<code>--interactive=false</code>), so it can&apos;t take
              commands this way.
            </div>
          )}
        </div>
      )}

      {form.mode === "rest" && (
        <div className="field-help">
          Palworld&apos;s REST API replaces its RCON: turn it on with <code>RESTAPIEnabled=True</code> (most images:{" "}
          <code>REST_API_ENABLED=true</code>). hlm signs in as <code>admin</code> with the server&apos;s admin password and
          takes the usual commands: <code>ShowPlayers</code>, <code>Metrics</code>, <code>Info</code>, <code>Save</code>,{" "}
          <code>Broadcast &lt;message&gt;</code>, <code>KickPlayer &lt;userId&gt; [message]</code>,{" "}
          <code>BanPlayer &lt;userId&gt; [message]</code>, <code>UnBanPlayer &lt;userId&gt;</code>,{" "}
          <code>Shutdown &lt;seconds&gt; [message]</code> and <code>DoExit</code>. The world is saved before hlm stops,
          restarts or updates the server. The API isn&apos;t meant for the internet: don&apos;t forward its port.
        </div>
      )}

      {(form.mode === "rcon" || form.mode === "rest") && (
        <div className="field-grid">
          <label className="field">
            <span className="field-label">Password from variable</span>
            <select value={form.passwordVar ?? ""} onChange={(e) => set("passwordVar", e.target.value)}>
              <option value="">(none: enter it below)</option>
              {status.envVars.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
            <span className="field-help">
              The container setting that holds the {form.mode === "rest" ? "admin" : "RCON or admin"} password.
            </span>
          </label>
          <label className="field">
            <span className="field-label">Or this password</span>
            <input
              type="password"
              value={form.password ?? ""}
              onChange={(e) => {
                set("password", e.target.value);
                setClearPassword(false);
              }}
              placeholder={status.hasPassword && !clearPassword ? "Saved; leave empty to keep it" : ""}
              autoComplete="new-password"
            />
            <span className="field-help">
              For games that keep it in a settings file. Stored encrypted and never shown again.
              {status.hasPassword && (
                <>
                  {" "}
                  <label className="radio small">
                    <input type="checkbox" checked={clearPassword} onChange={(e) => setClearPassword(e.target.checked)} />
                    Forget the saved password
                  </label>
                </>
              )}
            </span>
          </label>
          <label className="field">
            <span className="field-label">{form.mode === "rest" ? "REST API port" : "RCON port"}</span>
            <input
              type="number"
              min={1}
              max={65535}
              value={form.port ?? ""}
              onChange={(e) => set("port", e.target.value ? Number(e.target.value) : undefined)}
              placeholder="Found automatically"
            />
            <span className="field-help">
              {form.mode === "rest"
                ? "The port inside the container: 8212 unless RESTAPIPort (or REST_API_PORT) changes it."
                : "The port inside the container, e.g. 25575 for Minecraft or Palworld, 27020 for ARK, 28016 for Rust."}
            </span>
          </label>
          <label className="field">
            <span className="field-label">Address (optional)</span>
            <input value={form.host ?? ""} onChange={(e) => set("host", e.target.value)} placeholder="Found automatically" />
            <span className="field-help">
              Only if hlm can&apos;t reach the server by itself: a host name or IP where the port above can be reached.
            </span>
          </label>
        </div>
      )}

      {form.mode === "exec" && (
        <label className="field">
          <span className="field-label">Command to run</span>
          <input value={form.exec ?? ""} onChange={(e) => set("exec", e.target.value)} placeholder="rcon-cli {command}" spellCheck={false} />
          <span className="field-help">
            Run in the container for each command, with {"{command}"} replaced by what you type (or added at the end). It
            isn&apos;t run by a shell. Operators of this server can see it, so don&apos;t put passwords in it.
          </span>
        </label>
      )}

      {form.mode !== "" && (
        <label className="field">
          <span className="field-label">Your quick commands (optional, one per line)</span>
          <textarea rows={3} value={quickText} onChange={(e) => setQuickText(e.target.value)} placeholder={"whitelist add \nsay Restarting soon"} />
          <span className="field-help">
            Shown as shortcuts above the command line
            {status.game ? `, after the usual ones for ${status.game}` : ""}. End one with a space to type the rest yourself.
          </span>
        </label>
      )}

      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert success small">{notice}</div>}
      <div className="btn-row">
        <button type="submit" className="btn small primary" disabled={!!busy}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        {form.mode !== "" && (
          <button type="button" className="btn small" onClick={test} disabled={!!busy || !status.running}>
            {busy === "test" ? "Testing…" : "Test"}
          </button>
        )}
        <button type="button" className="btn small ghost" onClick={onCancel} disabled={!!busy}>
          Cancel
        </button>
      </div>
    </form>
  );
}
