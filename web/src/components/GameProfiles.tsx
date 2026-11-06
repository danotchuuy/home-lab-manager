import { useEffect, useState } from "react";
import { api } from "../api";
import type { GameProfile, GameProfiles as State } from "../types";
import { Spinner } from "./common";

/** A starting point for a game hlm doesn't know yet. */
const NEW_PROFILE = `id: mygame
name: My Game
# Words in the image's name or tag that identify it: all the words of any one set.
match: [[mygame]]
query:
  a2s_ports: [27015]   # Steam query ports to try
console:
  rcon_port: 27015
  say: say             # for scheduled tasks' warnings
  save: save
  commands: [status, "say "]
settings_files:
  - names: [server.cfg]
`;

/** How a profile recognises images: "minecraft + bedrock or mc". */
function matchText(p: GameProfile) {
  return p.match.map((set) => set.join(" + ")).join(" or ");
}

/** System → Game profiles: what hlm knows about each game, and your own. */
export function GameProfiles() {
  const [state, setState] = useState<State>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  // The profile shown, and the YAML being edited (undefined: not editing).
  const [shown, setShown] = useState<{ id: string; yaml: string }>();
  const [draft, setDraft] = useState<string>();
  const [saving, setSaving] = useState(false);

  const load = () => api.gameProfiles().then(setState, (e) => setError((e as Error).message));
  useEffect(() => {
    load();
  }, []);

  async function run(f: () => Promise<unknown>) {
    setError(undefined);
    setNotice(undefined);
    try {
      await f();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  const view = (id: string) =>
    run(async () => {
      if (shown?.id === id) return setShown(undefined);
      setShown({ id, yaml: await api.gameProfileYAML(id) });
    });

  const editCopy = (id: string) =>
    run(async () => {
      setDraft(await api.gameProfileYAML(id));
      setShown(undefined);
    });

  const remove = (p: GameProfile) =>
    run(async () => {
      const then = p.replaces ? " The built-in profile is used again." : "";
      if (!confirm(`Delete your profile for ${p.name} (${p.file})?${then}`)) return;
      setState(await api.deleteGameProfile(p.id));
      setShown(undefined);
    });

  const reload = () =>
    run(async () => {
      setState(await api.reloadGameProfiles());
      setNotice("Read the folder again.");
    });

  async function save() {
    if (draft === undefined) return;
    setSaving(true);
    await run(async () => {
      const p = await api.importGameProfile(draft);
      setDraft(undefined);
      setNotice(`Saved ${p.name} as ${p.file}.${p.replaces ? " It's used instead of the built-in profile." : ""}`);
      await load();
    });
    setSaving(false);
  }

  async function pickFile(file?: File) {
    if (!file) return;
    if (file.size > 64 << 10) return setError("A profile can be at most 64 KiB.");
    setDraft(await file.text());
  }

  return (
    <div className="game-profiles">
      <p className="muted small no-margin">
        A game profile is what hlm knows about a game: how to recognise it from the server&apos;s image, its status
        query ports, its console&apos;s RCON port and commands, how to read its player list, the commands scheduled
        tasks use to warn players and save, and its settings files. Add your own for a game hlm doesn&apos;t know, or to
        change a built-in one: yours is used instead of a built-in profile with the same id. Profiles are only data;
        they can&apos;t run anything.
      </p>
      {error && <div className="alert error small">{error}</div>}
      {notice && <div className="alert info small">{notice}</div>}
      {state && state.errors.length > 0 && (
        <div className="alert error small">
          {state.errors.length === 1 ? "This file isn't used" : "These files aren't used"}; the rest are:
          <ul className="no-margin">
            {state.errors.map((e) => (
              <li key={e.file}>
                <code>{e.file}</code>: {e.error}
              </li>
            ))}
          </ul>
        </div>
      )}
      {!state ? (
        <Spinner />
      ) : (
        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Game</th>
                <th>Image words</th>
                <th>Profile</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {state.profiles.map((p) => (
                <tr key={p.id}>
                  <td>
                    {p.name} <code className="muted">{p.id}</code>
                  </td>
                  <td>
                    <code>{matchText(p)}</code>
                  </td>
                  <td>
                    {p.source === "custom" ? (
                      <>
                        Yours{" "}
                        <span className="muted">
                          ({p.file}
                          {p.replaces ? ", replaces the built-in one" : ""})
                        </span>
                      </>
                    ) : (
                      <span className="muted">Built-in</span>
                    )}
                  </td>
                  <td className="actions">
                    <button className="btn small" onClick={() => view(p.id)}>
                      {shown?.id === p.id ? "Hide" : "View"}
                    </button>
                    <button className="btn small" onClick={() => editCopy(p.id)}>
                      {p.source === "custom" ? "Edit" : "Edit a copy"}
                    </button>
                    {p.source === "custom" && (
                      <button className="btn small danger" onClick={() => remove(p)}>
                        Delete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {shown && (
        <div className="field">
          <span className="field-label">
            <code>{shown.id}</code>
          </span>
          <pre className="op-output game-profile-yaml">{shown.yaml}</pre>
        </div>
      )}
      {draft === undefined ? (
        <div className="btn-row">
          <button className="btn small" onClick={() => setDraft(NEW_PROFILE)}>
            Add a game
          </button>
          <label className="btn small">
            Import a file…
            <input
              type="file"
              accept=".yaml,.yml,text/yaml"
              hidden
              onChange={(e) => {
                pickFile(e.target.files?.[0]);
                e.target.value = "";
              }}
            />
          </label>
          <button className="btn small" onClick={reload} title={state?.dir}>
            Read the folder again
          </button>
        </div>
      ) : (
        <div className="field">
          <span className="field-label">Profile (YAML)</span>
          <textarea
            className="code-input"
            rows={Math.min(30, Math.max(12, draft.split("\n").length + 1))}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            spellCheck={false}
          />
          <p className="muted small no-margin">
            Saving checks it first. Keeping the id of a built-in profile replaces it; a new id adds a game. The fields
            are described in the README&apos;s &quot;Game profiles&quot; section.
          </p>
          <div className="btn-row">
            <button className="btn primary small" onClick={save} disabled={saving || !draft.trim()}>
              {saving ? "Saving…" : "Save profile"}
            </button>
            <button className="btn small" onClick={() => setDraft(undefined)} disabled={saving}>
              Cancel
            </button>
          </div>
        </div>
      )}
      {state && (
        <p className="muted small">
          Your profiles are kept in <code>{state.dir}</code>. You can also put <code>.yaml</code> files there and read
          the folder again; they&apos;re in hlm&apos;s configuration backups.
        </p>
      )}
    </div>
  );
}
