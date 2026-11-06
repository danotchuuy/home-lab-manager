import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type {
  DiscordAction,
  DiscordRole,
  DiscordRule,
  DiscordSettings as Settings,
  DiscordTest,
  DiscordView,
  ServerGroup,
} from "../types";
import { Ago, Spinner } from "./common";

const ACTIONS: { action: DiscordAction; label: string }[] = [
  { action: "players", label: "See who's playing" },
  { action: "start", label: "Start" },
  { action: "stop", label: "Stop" },
  { action: "restart", label: "Restart" },
  { action: "say", label: "Say something in the game" },
];

const STATUS: Record<DiscordView["state"]["status"], string> = {
  off: "Off",
  connecting: "Connecting…",
  connected: "Connected",
  error: "Stopped",
};

/**
 * Connects a Discord bot, and says what each Discord role may do. The bot
 * token never comes back from the server.
 */
export function DiscordSettings() {
  const [view, setView] = useState<DiscordView>();
  const [form, setForm] = useState<Settings>();
  const [token, setToken] = useState("");
  const [editingToken, setEditingToken] = useState(false);
  const [roles, setRoles] = useState<DiscordRole[]>();
  const [rolesError, setRolesError] = useState<string>();
  const [groups, setGroups] = useState<ServerGroup[]>([]);
  const [test, setTest] = useState<DiscordTest>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const show = useCallback((v: DiscordView) => {
    setView(v);
    setForm(v.settings);
  }, []);
  const loadRoles = useCallback(() => {
    api.discordRoles().then(
      (r) => {
        setRoles(r);
        setRolesError(undefined);
      },
      (e) => setRolesError((e as Error).message),
    );
  }, []);

  useEffect(() => {
    api.discord().then(
      (v) => {
        show(v);
        if (v.hasToken && v.settings.guildId) loadRoles();
      },
      (e) => setError((e as Error).message),
    );
    api.groups().then(setGroups, () => {});
  }, [show, loadRoles]);

  // The connection's state changes on its own: follow it while it's open.
  useEffect(() => {
    if (!view?.settings.enabled) return;
    const t = setInterval(() => {
      api.discord().then(
        (v) => setView((cur) => (cur ? { ...cur, state: v.state, inviteUrl: v.inviteUrl } : v)),
        () => {},
      );
    }, 5000);
    return () => clearInterval(t);
  }, [view?.settings.enabled]);

  if (!view || !form) return error ? <div className="alert error small">{error}</div> : <Spinner />;

  const needsToken = !view.hasToken || editingToken;
  const set = (patch: Partial<Settings>) => setForm({ ...form, ...patch });
  const setRule = (i: number, patch: Partial<DiscordRule>) =>
    set({ rules: form.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const roleName = (id: string) =>
    roles?.find((r) => r.id === id)?.name ?? form.rules.find((r) => r.roleId === id)?.roleName ?? id;

  async function save() {
    if (!form) return;
    setBusy("save");
    setError(undefined);
    setNotice(undefined);
    try {
      const rules = form.rules.map((r) => ({ ...r, roleName: roleName(r.roleId) }));
      const v = await api.saveDiscord({ ...form, rules }, needsToken && token.trim() ? token.trim() : undefined);
      show(v);
      setToken("");
      setEditingToken(false);
      setNotice("Saved.");
      if (v.hasToken && v.settings.guildId) loadRoles();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(undefined);
  }

  async function runTest() {
    setBusy("test");
    setError(undefined);
    setNotice(undefined);
    try {
      const t = await api.testDiscord();
      setTest(t);
      if (!t.error) loadRoles();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(undefined);
  }

  async function disconnect() {
    if (!confirm("Turn the bot off, take its commands out of the Discord server and forget its token?")) return;
    setBusy("disconnect");
    setError(undefined);
    try {
      show(await api.disconnectDiscord());
      setTest(undefined);
      setRoles(undefined);
      setNotice("The bot is off and its token forgotten.");
    } catch (e) {
      setError((e as Error).message);
      api.discord().then(show, () => {});
    }
    setBusy(undefined);
  }

  const st = view.state;
  const invite = test?.result.inviteUrl || view.inviteUrl;
  const unusedRoles = (roles ?? []).filter((r) => !form.rules.some((x) => x.roleId === r.id));

  return (
    <div className="discord-settings">
      <p className="muted small">
        People in your Discord server can use <code>/status</code>, <code>/players</code>, <code>/start</code>,{" "}
        <code>/stop</code>, <code>/restart</code> and <code>/say</code>. hlm connects out to Discord, so nothing has to
        reach hlm from the internet. Discord sees only the servers on the{" "}
        <Link to="/system#status-page">status page</Link>, as the page shows them.
      </p>

      {view.hasToken && (
        <p className={`small discord-state discord-${st.status}`}>
          <strong>{STATUS[st.status]}</strong>
          {st.status === "connected" && st.bot && (
            <>
              {" "}
              as {st.bot}
              {st.since && (
                <>
                  {" "}
                  since <Ago iso={st.since} />
                </>
              )}
              {st.commands ? " · commands added" : ""}
            </>
          )}
          {st.error && <span className="muted"> · {st.error}</span>}
        </p>
      )}

      <details className="small" open={!view.hasToken}>
        <summary>Setting up the bot</summary>
        <ol>
          <li>
            In the{" "}
            <a href="https://discord.com/developers/applications" target="_blank" rel="noreferrer noopener">
              Discord developer portal
            </a>
            , choose <strong>New Application</strong> and name it (say, “Game servers”).
          </li>
          <li>
            Under <strong>Bot</strong>, choose <strong>Reset Token</strong> and paste the token below. It needs no
            privileged intents.
          </li>
          <li>
            In Discord, turn on <strong>Developer Mode</strong> (Settings → Advanced), then right-click your server and
            choose <strong>Copy Server ID</strong>.
          </li>
          <li>
            Save, then open the invite link that appears here to add the bot to your server. It asks for no permissions.
          </li>
          <li>Choose which roles may do what. Everyone in the server can use /status.</li>
        </ol>
      </details>

      <div className="field-grid">
        <label className="field">
          <span className="field-label">Bot token</span>
          {needsToken ? (
            <input
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoComplete="off"
              placeholder={view.hasToken ? "A new token" : "From the developer portal"}
            />
          ) : (
            <span>
              Saved{" "}
              <button className="btn small" onClick={() => setEditingToken(true)}>
                Change
              </button>
            </span>
          )}
        </label>
        <label className="field">
          <span className="field-label">Discord server ID</span>
          <input
            value={form.guildId}
            onChange={(e) => set({ guildId: e.target.value })}
            inputMode="numeric"
            placeholder="123456789012345678"
          />
        </label>
      </div>
      <label className="checkbox">
        <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Turn the
        bot on
      </label>
      <label className="checkbox">
        <input type="checkbox" checked={form.details} onChange={(e) => set({ details: e.target.checked })} /> Show
        addresses and who&apos;s playing (what the status page shows behind its details password)
      </label>

      <h3>Who may do what</h3>
      {form.rules.length === 0 && <p className="muted small">No rules: everyone can use /status, and nothing else.</p>}
      {form.rules.map((r, i) => (
        <div key={r.roleId} className="discord-rule">
          <div className="discord-rule-head">
            <strong>{roleName(r.roleId) === "@everyone" ? "Everyone" : roleName(r.roleId)}</strong>
            <button className="btn small" onClick={() => set({ rules: form.rules.filter((_, j) => j !== i) })}>
              Remove
            </button>
          </div>
          <div className="discord-checks">
            {ACTIONS.map((a) => (
              <label key={a.action} className="checkbox">
                <input
                  type="checkbox"
                  checked={r.actions.includes(a.action)}
                  onChange={(e) =>
                    setRule(i, {
                      actions: e.target.checked ? [...r.actions, a.action] : r.actions.filter((x) => x !== a.action),
                    })
                  }
                />{" "}
                {a.label}
              </label>
            ))}
          </div>
          <div className="discord-checks small">
            <span className="muted">On:</span>
            <label className="checkbox">
              <input type="checkbox" checked={r.groups.length === 0} onChange={() => setRule(i, { groups: [] })} />{" "}
              every server on the status page
            </label>
            {groups.map((g) => (
              <label key={g.id} className="checkbox">
                <input
                  type="checkbox"
                  checked={r.groups.includes(g.id)}
                  onChange={(e) =>
                    setRule(i, {
                      groups: e.target.checked ? [...r.groups, g.id] : r.groups.filter((x) => x !== g.id),
                    })
                  }
                />{" "}
                {g.name}
              </label>
            ))}
          </div>
        </div>
      ))}
      {roles ? (
        unusedRoles.length > 0 && (
          <label className="field add-role">
            <span className="field-label">Add a role</span>
            <select
              value=""
              onChange={(e) => {
                const role = roles.find((x) => x.id === e.target.value);
                if (role)
                  set({
                    rules: [...form.rules, { roleId: role.id, roleName: role.name, groups: [], actions: ["players"] }],
                  });
              }}
            >
              <option value="">Choose a role…</option>
              {unusedRoles.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name === "@everyone" ? "Everyone (@everyone)" : r.name}
                </option>
              ))}
            </select>
          </label>
        )
      ) : (
        <p className="muted small">
          {rolesError ?? "Save the token and server ID, and add the bot to your server, to choose roles."}
        </p>
      )}

      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert info small">{notice}</div>}
      {test && (
        <div className={`alert small ${test.error ? "error" : "info"}`}>
          {test.error ??
            `${test.result.bot} is in ${test.result.guild}, and its commands are there. Type / in Discord to see them.`}
        </div>
      )}
      {invite && (
        <p className="small">
          Invite link:{" "}
          <a href={invite} target="_blank" rel="noreferrer noopener">
            add the bot to your Discord server
          </a>
        </p>
      )}
      <div className="btn-row">
        <button className="btn primary small" onClick={save} disabled={!!busy}>
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        {view.hasToken && (
          <>
            <button className="btn small" onClick={runTest} disabled={!!busy}>
              {busy === "test" ? "Testing…" : "Test"}
            </button>
            <button className="btn small danger" onClick={disconnect} disabled={!!busy}>
              Disconnect
            </button>
          </>
        )}
      </div>
    </div>
  );
}
