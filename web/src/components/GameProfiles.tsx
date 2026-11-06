import { useEffect, useState } from "react";
import { api } from "../api";
import type { GameProfile, GameProfileLink, GameProfilePreview, GameProfiles as State } from "../types";
import { Ago, Spinner } from "./common";
import { hostOf, ImportFromLink, ProfileIndexes, ProfilePreview, saveBlob } from "./ProfileSharing";

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
  // Importing from a link: asking for it, then the fetched profile.
  const [asking, setAsking] = useState(false);
  const [preview, setPreview] = useState<{
    preview: GameProfilePreview;
    link: GameProfileLink;
    update?: boolean;
    changedLocally?: boolean;
  }>();
  const [checking, setChecking] = useState(false);

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

  const showPreview = (p: GameProfilePreview, link: GameProfileLink, update?: GameProfile) => {
    setAsking(false);
    setDraft(undefined);
    setPreview({ preview: p, link, update: !!update, changedLocally: update?.origin?.changedLocally });
  };

  // An update to a profile imported before: fetched again from where it came.
  const reviewUpdate = (p: GameProfile) =>
    run(async () => {
      const o = p.origin!;
      const link = o.index ? { index: o.index, id: p.id } : { url: o.url };
      showPreview(await api.fetchGameProfile(link), link, p);
    });

  const downloadOne = (p: GameProfile) =>
    run(async () => {
      const yaml = await api.gameProfileYAML(p.id);
      saveBlob(new Blob([yaml], { type: "text/yaml" }), p.file || `${p.id}.yaml`);
    });

  const downloadAll = () =>
    run(async () => {
      saveBlob(await api.exportGameProfiles(), "game-profiles.zip");
    });

  async function checkUpdates() {
    setChecking(true);
    await run(async () => {
      const st = await api.checkGameProfiles();
      setState(st);
      const n = st.profiles.filter((p) => p.origin?.update).length;
      setNotice(n ? `${n} profile${n === 1 ? " has" : "s have"} an update.` : "No updates.");
    });
    setChecking(false);
  }

  const linked = state?.profiles.filter((p) => p.origin) ?? [];
  const yours = state?.profiles.filter((p) => p.source === "custom") ?? [];

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
                        {p.origin && (
                          <div className="small muted">
                            From{" "}
                            <a href={p.origin.url} target="_blank" rel="noreferrer noopener">
                              {hostOf(p.origin.index || p.origin.url)}
                            </a>
                            {p.origin.changedLocally && " · changed here since"}
                            {p.origin.checked && (
                              <>
                                {" "}
                                · checked <Ago iso={p.origin.checked} />
                              </>
                            )}
                            {p.origin.updateError && (
                              <span className="run-failed"> · couldn&apos;t check: {p.origin.updateError}</span>
                            )}
                          </div>
                        )}
                        {p.origin?.update && <span className="badge update-available">Update available</span>}
                      </>
                    ) : (
                      <span className="muted">Built-in</span>
                    )}
                  </td>
                  <td className="actions">
                    {p.origin?.update && (
                      <button className="btn small primary" onClick={() => reviewUpdate(p)}>
                        Review update
                      </button>
                    )}
                    <button className="btn small" onClick={() => view(p.id)}>
                      {shown?.id === p.id ? "Hide" : "View"}
                    </button>
                    <button className="btn small" onClick={() => editCopy(p.id)}>
                      {p.source === "custom" ? "Edit" : "Edit a copy"}
                    </button>
                    <button className="btn small" onClick={() => downloadOne(p)}>
                      Download
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
      {preview && (
        <ProfilePreview
          preview={preview.preview}
          link={preview.link}
          update={preview.update}
          changedLocally={preview.changedLocally}
          onCancel={() => setPreview(undefined)}
          onSaved={(msg) => {
            setPreview(undefined);
            setNotice(msg);
            load();
          }}
        />
      )}
      {asking && <ImportFromLink onPreview={(p, link) => showPreview(p, link)} onCancel={() => setAsking(false)} />}
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
          <button
            className="btn small"
            onClick={() => {
              setPreview(undefined);
              setAsking(true);
            }}
          >
            Import from a link…
          </button>
          {linked.length > 0 && (
            <button className="btn small" onClick={checkUpdates} disabled={checking}>
              {checking ? "Checking…" : "Check for updates"}
            </button>
          )}
          {yours.length > 0 && (
            <button className="btn small" onClick={downloadAll}>
              Download all of yours
            </button>
          )}
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
      <ProfileIndexes onPreview={(p, link) => showPreview(p, link)} />
      {state && (
        <p className="muted small">
          Your profiles are kept in <code>{state.dir}</code>. You can also put <code>.yaml</code> files there and read
          the folder again; they&apos;re in hlm&apos;s configuration backups.
        </p>
      )}
    </div>
  );
}
