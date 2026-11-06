import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { PlayerAction, PlayerEntry, PlayerListName, PlayersOverview } from "../types";
import { Ago, Spinner } from "./common";

const LISTS: { list: PlayerListName; title: string; add: PlayerAction; remove: PlayerAction; empty: string }[] = [
  { list: "ban", title: "Bans", add: "ban", remove: "unban", empty: "Nobody is banned." },
  { list: "allow", title: "Allow list", add: "allow", remove: "disallow", empty: "Nobody is on the allow list." },
  { list: "admin", title: "Admins", add: "admin", remove: "unadmin", empty: "There are no admins." },
];

const ACTION_LABEL: Record<PlayerAction, string> = {
  kick: "Kick",
  ban: "Ban",
  unban: "Unban",
  allow: "Allow",
  disallow: "Take off the allow list",
  admin: "Make admin",
  unadmin: "Remove admin",
};

const PAST: Record<PlayerAction, string> = {
  kick: "kicked",
  ban: "banned",
  unban: "unbanned",
  allow: "added to the allow list",
  disallow: "taken off the allow list",
  admin: "made admin",
  unadmin: "no longer admin",
};

/** Who's shown: their name, or ID if that's all there is. */
const who = (p: PlayerEntry) => p.name || p.id || "?";

/** A server's Players tab: who's on, kicks and bans, and its lists. */
export function ServerPlayers({
  server,
  running,
  canManage,
}: {
  server: string;
  running: boolean;
  canManage: boolean;
}) {
  const [ov, setOv] = useState<PlayersOverview>();
  // loadError is reading the tab failing; error an action failing.
  const [loadError, setLoadError] = useState<string>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);

  const load = useCallback(
    () =>
      api.serverPlayers(server).then(
        (o) => {
          setOv(o);
          setLoadError(undefined);
        },
        (e) => setLoadError((e as Error).message),
      ),
    [server],
  );
  useEffect(() => {
    load();
  }, [load, running]);

  if (!ov)
    return <div className="card">{loadError ? <div className="alert error small">{loadError}</div> : <Spinner />}</div>;
  if (ov.unsupported) {
    return (
      <div className="card">
        <h2>Players</h2>
        <p className="muted small">
          {ov.unsupported}. An admin can add them to its profile under System → Game profiles (the{" "}
          <code>console.actions</code> and <code>player_lists</code> fields).
        </p>
      </div>
    );
  }

  /** Whether an action can be done now, and by this person. */
  const can = (a: PlayerAction, list?: PlayerListName) => {
    if (a !== "kick" && !canManage) return false;
    if (ov.running) return ov.actions.includes(a);
    return !!list && !!ov.lists?.[list]?.editable;
  };
  const needsId = (a: PlayerAction) => ov.needsId?.includes(a) ?? false;

  async function act(a: PlayerAction, p: PlayerEntry, askReason = false) {
    let reason = "";
    if (askReason) {
      const r = prompt(`${ACTION_LABEL[a]} ${who(p)}? A reason (optional):`, "");
      if (r === null) return;
      reason = r;
    } else if (!confirm(`${ACTION_LABEL[a]}: ${who(p)}?`)) return;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const res = await api.playerAction(server, a, { name: p.name, id: p.id, reason });
      setNotice(`${who(p)} ${PAST[a]}.${res.output ? ` The server said: ${res.output}` : ""}`);
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
    load();
  }

  const listsShown = LISTS.filter((l) => ov.lists?.[l.list] || ov.actions.includes(l.add));
  const addable = listsShown.filter((l) => can(l.add, l.list));

  return (
    <div className="card players">
      <div className="settings-actions">
        <div>
          <h2>Players</h2>
          <p className="muted small">
            {ov.running
              ? "Commands go through the server's console, as if typed there."
              : ov.lists && Object.values(ov.lists).some((l) => l?.editable)
                ? "The server is stopped: its list files can be changed, and it reads them when it starts."
                : "The server is stopped: start it to kick, ban or change its lists."}
          </p>
        </div>
        <button className="btn small" onClick={() => load()} disabled={busy}>
          Refresh
        </button>
      </div>
      {loadError && <div className="alert error small">{loadError}</div>}
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert info small">{notice}</div>}

      {ov.running && (
        <section>
          <h3>Online now</h3>
          {ov.onlineError && ov.onlineFrom !== "console" && (
            <p className="muted small">
              The console couldn&apos;t list them ({ov.onlineError})
              {ov.onlineFrom === "query" ? "; these are from the last status check, without IDs." : "."}
            </p>
          )}
          {ov.online.length === 0 ? (
            <p className="muted small">Nobody is on.</p>
          ) : (
            <table className="data-table">
              <tbody>
                {ov.online.map((p, i) => (
                  <tr key={(p.id ?? "") + p.name + i}>
                    <td>
                      {p.name} {p.id && <code className="muted small">{p.id}</code>}
                    </td>
                    <td className="actions">
                      {(["kick", "ban", "admin"] as PlayerAction[])
                        .filter((a) => can(a))
                        .map((a) => (
                          <button
                            key={a}
                            className={`btn small${a === "ban" ? " danger" : ""}`}
                            disabled={busy || (needsId(a) && !p.id)}
                            title={needsId(a) && !p.id ? "This game needs the player's ID, which hlm doesn't know" : ""}
                            onClick={() => act(a, p, a === "kick" || a === "ban")}
                          >
                            {ACTION_LABEL[a]}
                          </button>
                        ))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      )}

      {listsShown.map((l) => {
        const list = ov.lists?.[l.list];
        return (
          <section key={l.list}>
            <h3>
              {l.title} {list?.path && <code className="muted small">{list.path}</code>}
            </h3>
            {!list ? (
              <p className="muted small">hlm can add to this list, but doesn&apos;t know where the game keeps it.</p>
            ) : list.error ? (
              <p className="muted small">{list.error}</p>
            ) : list.players.length === 0 ? (
              <p className="muted small">{l.empty}</p>
            ) : (
              <table className="data-table">
                <tbody>
                  {list.players.map((p, i) => (
                    <tr key={(p.id ?? "") + (p.name ?? "") + i}>
                      <td>
                        {p.name} {p.id && <code className="muted small">{p.id}</code>}
                        {p.reason && <span className="muted small"> · {p.reason}</span>}
                      </td>
                      <td className="actions">
                        {can(l.remove, l.list) && (
                          <button className="btn small" disabled={busy} onClick={() => act(l.remove, p)}>
                            {ACTION_LABEL[l.remove]}
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        );
      })}

      {addable.length > 0 && <AddPlayer lists={addable} needsId={needsId} busy={busy} onAdd={act} />}

      {ov.log.length > 0 && (
        <details className="small run-history">
          <summary>Done through hlm</summary>
          <ul>
            {ov.log.map((r, i) => (
              <li key={i}>
                <Ago iso={r.time} /> · {r.by || "someone"}: {ACTION_LABEL[r.action].toLowerCase()} {r.player}
                {r.reason && <span className="muted"> ({r.reason})</span>}
                {r.error && <span className="run-failed"> · failed: {r.error}</span>}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

/** Adds someone who isn't online to a list. */
function AddPlayer({
  lists,
  needsId,
  busy,
  onAdd,
}: {
  lists: typeof LISTS;
  needsId: (a: PlayerAction) => boolean;
  busy: boolean;
  onAdd: (a: PlayerAction, p: PlayerEntry) => void;
}) {
  const [list, setList] = useState(lists[0].list);
  const [value, setValue] = useState("");
  const l = lists.find((x) => x.list === list) ?? lists[0];
  // Lists hlm edits as files (stopped lines files), and commands taking
  // IDs, want an ID; the rest a name.
  const byId = needsId(l.add);

  function submit(e: FormEvent) {
    e.preventDefault();
    const v = value.trim();
    if (!v) return;
    onAdd(l.add, byId ? { id: v } : { name: v });
    setValue("");
  }

  return (
    <form className="config-form add-player" onSubmit={submit}>
      <h3>Add a player</h3>
      <div className="field-grid">
        <label className="field">
          <span className="field-label">To</span>
          <select value={list} onChange={(e) => setList(e.target.value as PlayerListName)}>
            {lists.map((x) => (
              <option key={x.list} value={x.list}>
                {x.title}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">{byId ? "Player ID" : "Player name or ID"}</span>
          <input value={value} onChange={(e) => setValue(e.target.value)} maxLength={64} spellCheck={false} />
        </label>
      </div>
      <div className="btn-row">
        <button className="btn small primary" disabled={busy || !value.trim()}>
          {ACTION_LABEL[l.add]}
        </button>
      </div>
    </form>
  );
}
