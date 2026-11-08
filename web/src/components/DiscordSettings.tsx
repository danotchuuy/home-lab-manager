import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { MultiSelect } from "./MultiSelect";
import { api } from "../api";
import type {
  DiscordAction,
  DiscordGuild,
  DiscordGuildState,
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

const NEW_GUILD: DiscordGuild = { id: "", groups: [], details: false, rules: [] };

/** A Discord server's roles, or why they couldn't be read. */
type Roles = { roles?: DiscordRole[]; error?: string };

/**
 * Connects a Discord bot to one or more Discord servers, each with the game
 * servers it sees and what each of its roles may do. The bot token never
 * comes back from the server.
 */
export function DiscordSettings() {
  const [view, setView] = useState<DiscordView>();
  const [form, setForm] = useState<Settings>();
  const [token, setToken] = useState("");
  const [editingToken, setEditingToken] = useState(false);
  // Roles by Discord server ID.
  const [roles, setRoles] = useState<Record<string, Roles>>({});
  const [groups, setGroups] = useState<ServerGroup[]>([]);
  const [test, setTest] = useState<DiscordTest>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

  const loadRoles = useCallback((v: DiscordView) => {
    if (!v.hasToken) return;
    for (const g of v.settings.guilds) {
      api.discordRoles(g.id).then(
        (r) => setRoles((cur) => ({ ...cur, [g.id]: { roles: r } })),
        (e) => setRoles((cur) => ({ ...cur, [g.id]: { error: (e as Error).message } })),
      );
    }
  }, []);
  const show = useCallback(
    (v: DiscordView) => {
      setView(v);
      setForm(v.settings);
      loadRoles(v);
    },
    [loadRoles],
  );

  useEffect(() => {
    api.discord().then(show, (e) => setError((e as Error).message));
    api.groups().then(setGroups, () => {});
  }, [show]);

  // The connection's state changes on its own: follow it while it's on.
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
  const setGuild = (i: number, patch: Partial<DiscordGuild>) =>
    setForm({ ...form, guilds: form.guilds.map((g, j) => (j === i ? { ...g, ...patch } : g)) });
  const guildState = (id: string): DiscordGuildState | undefined =>
    test?.result.guilds?.find((g) => g.id === id) ?? view.state.guilds?.find((g) => g.id === id);

  async function save() {
    if (!form) return;
    setBusy("save");
    setError(undefined);
    setNotice(undefined);
    try {
      const guilds = form.guilds.map((g) => ({
        ...g,
        id: g.id.trim(),
        rules: g.rules.map((r) => ({
          ...r,
          roleName: roles[g.id]?.roles?.find((x) => x.id === r.roleId)?.name ?? r.roleName,
        })),
      }));
      const v = await api.saveDiscord({ ...form, guilds }, needsToken && token.trim() ? token.trim() : undefined);
      show(v);
      setToken("");
      setEditingToken(false);
      setTest(undefined);
      setNotice("Saved.");
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
      if (!t.error) loadRoles(view!);
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(undefined);
  }

  async function disconnect() {
    if (!confirm("Turn the bot off, take its commands out of every Discord server and forget its token?")) return;
    setBusy("disconnect");
    setError(undefined);
    try {
      show(await api.disconnectDiscord());
      setTest(undefined);
      setRoles({});
      setNotice("The bot is off and its token forgotten.");
    } catch (e) {
      setError((e as Error).message);
      api.discord().then(show, () => {});
    }
    setBusy(undefined);
  }

  const st = view.state;
  const invite = test?.result.inviteUrl || view.inviteUrl;

  return (
    <div className="discord-settings">
      <p className="muted small">
        People in your Discord servers can use <code>/status</code>, <code>/players</code>, <code>/start</code>,{" "}
        <code>/stop</code>, <code>/restart</code> and <code>/say</code>. hlm connects out to Discord, so nothing has to
        reach hlm from the internet. Discord sees only servers on the <Link to="/system#status-page">status page</Link>,
        as the page shows them, and each Discord server only the ones you choose.
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
            In Discord, turn on <strong>Developer Mode</strong> (Settings → Advanced), then right-click each server the
            bot should answer in and choose <strong>Copy Server ID</strong>.
          </li>
          <li>
            Save, then open the invite link that appears here to add the bot to each server. It asks for no permissions.
          </li>
          <li>For each Discord server, choose the game servers it sees and what its roles may do.</li>
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
      </div>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={form.enabled}
          onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
        />{" "}
        Turn the bot on
      </label>

      <h3>Discord servers</h3>
      {form.guilds.length === 0 && <p className="muted small">None yet: add the Discord servers the bot answers in.</p>}
      {form.guilds.map((g, i) => (
        <GuildEditor
          key={i}
          guild={g}
          state={guildState(g.id)}
          roles={roles[g.id]}
          saved={view.hasToken && view.settings.guilds.some((x) => x.id === g.id)}
          groups={groups}
          onChange={(patch) => setGuild(i, patch)}
          onRemove={() => setForm({ ...form, guilds: form.guilds.filter((_, j) => j !== i) })}
        />
      ))}
      <button className="btn small" onClick={() => setForm({ ...form, guilds: [...form.guilds, { ...NEW_GUILD }] })}>
        Add a Discord server
      </button>

      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert info small">{notice}</div>}
      {test?.error && <div className="alert error small">{test.error}</div>}
      {test && !test.error && (
        <div className="alert info small">
          {test.result.bot} is connected. Each Discord server&apos;s line above says whether its commands are there.
        </div>
      )}
      {invite && (
        <p className="small">
          Invite link:{" "}
          <a href={invite} target="_blank" rel="noreferrer noopener">
            add the bot to a Discord server
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

/** One Discord server: which game servers it sees, and its rules. */
function GuildEditor({
  guild,
  state,
  roles,
  saved,
  groups,
  onChange,
  onRemove,
}: {
  guild: DiscordGuild;
  state?: DiscordGuildState;
  roles?: Roles;
  saved: boolean;
  groups: ServerGroup[];
  onChange: (patch: Partial<DiscordGuild>) => void;
  onRemove: () => void;
}) {
  const setRule = (i: number, patch: Partial<DiscordRule>) =>
    onChange({ rules: guild.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const roleName = (id: string) => {
    const name = roles?.roles?.find((r) => r.id === id)?.name ?? guild.rules.find((r) => r.roleId === id)?.roleName;
    return !name || name === "@everyone" || id === guild.id ? (id === guild.id ? "Everyone" : id) : name;
  };
  // A rule can narrow to the groups this Discord server sees.
  const ruleGroups = guild.groups.length ? groups.filter((g) => guild.groups.includes(g.id)) : groups;
  const unusedRoles = (roles?.roles ?? []).filter((r) => !guild.rules.some((x) => x.roleId === r.id));

  return (
    <div className="discord-guild">
      <div className="discord-rule-head">
        <strong>{guild.name || state?.name || (guild.id ? `Discord server ${guild.id}` : "New Discord server")}</strong>
        <button className="btn small" onClick={onRemove}>
          Remove
        </button>
      </div>
      {state && (
        <p className={`small ${state.error ? "discord-guild-error" : "muted"}`}>
          {state.error ?? (state.commands ? `Commands added${state.name ? ` in ${state.name}` : ""}.` : "")}
        </p>
      )}
      <div className="field-grid">
        <label className="field">
          <span className="field-label">Discord server ID</span>
          <input
            value={guild.id}
            onChange={(e) => onChange({ id: e.target.value })}
            inputMode="numeric"
            placeholder="123456789012345678"
          />
        </label>
        <label className="field">
          <span className="field-label">Name here (optional)</span>
          <input value={guild.name ?? ""} onChange={(e) => onChange({ name: e.target.value })} maxLength={100} />
        </label>
      </div>

      <div className="field">
        <span className="field-label">Sees</span>
        <MultiSelect
          label="Server groups this Discord server sees"
          options={groups.map((g) => ({ value: g.id, label: g.name }))}
          value={guild.groups}
          onChange={(ids) => onChange({ groups: ids })}
          placeholder="Limit to server groups…"
          empty="Every server on the status page"
        />
      </div>
      <label className="checkbox small">
        <input type="checkbox" checked={guild.details} onChange={(e) => onChange({ details: e.target.checked })} /> Show
        addresses and who&apos;s playing (what the status page shows behind its details password)
      </label>

      <h4>Who may do what</h4>
      {guild.rules.length === 0 && <p className="muted small">No rules: everyone can use /status, and nothing else.</p>}
      {guild.rules.map((r, i) => (
        <div key={r.roleId} className="discord-rule">
          <div className="discord-rule-head">
            <strong>{roleName(r.roleId)}</strong>
            <button className="btn small" onClick={() => onChange({ rules: guild.rules.filter((_, j) => j !== i) })}>
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
          <div className="field">
            <span className="field-label">On</span>
            <MultiSelect
              label={`Server groups ${roleName(r.roleId)} may act on`}
              options={ruleGroups.map((g) => ({ value: g.id, label: g.name }))}
              value={r.groups}
              onChange={(ids) => setRule(i, { groups: ids })}
              placeholder="Limit to server groups…"
              empty="Every server it sees"
            />
          </div>
        </div>
      ))}
      {roles?.roles ? (
        unusedRoles.length > 0 && (
          <label className="field add-role">
            <span className="field-label">Add a role</span>
            <select
              value=""
              onChange={(e) => {
                const role = roles.roles?.find((x) => x.id === e.target.value);
                if (role)
                  onChange({
                    rules: [...guild.rules, { roleId: role.id, roleName: role.name, groups: [], actions: ["players"] }],
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
          {saved
            ? (roles?.error ?? "Reading its roles…")
            : "Save the bot's token and this Discord server, and add the bot to it, to choose roles."}
        </p>
      )}
    </div>
  );
}
