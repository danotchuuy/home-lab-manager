import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import type { DeployEvent, WorkshopItem, WorkshopOverview } from "../types";
import { formatBytes } from "../util";
import { Ago } from "./common";
import { DeployProgress } from "./DeployProgress";

const itemLink = (id: string) => `https://steamcommunity.com/sharedfiles/filedetails/?id=${encodeURIComponent(id)}`;

/** An item's ID from its ID or Workshop link, as the server reads it. */
function idOf(s: string) {
  s = s.trim();
  if (/^\d{1,20}$/.test(s)) return s;
  const m = /[?&]id=(\d{1,20})\b/.exec(s);
  return m && /steamcommunity\.com\/(sharedfiles|workshop)\//.test(s) ? m[1] : undefined;
}

/**
 * A server's Steam Workshop items: the image downloads them, given the list
 * in one of its variables. Changing the list recreates the server; if it
 * doesn't come back, the old list is put back.
 */
export function WorkshopMods({
  server,
  ov,
  canManage,
  onChanged,
}: {
  server: string;
  ov: WorkshopOverview;
  canManage: boolean;
  onChanged: () => void;
}) {
  const [draft, setDraft] = useState<WorkshopItem[]>(ov.items);
  const [newId, setNewId] = useState("");
  const [newName, setNewName] = useState("");
  const [error, setError] = useState<string>();
  const [events, setEvents] = useState<DeployEvent[]>();
  const [applying, setApplying] = useState(false);

  const saved = ov.items.map((i) => `${i.id}:${i.name ?? ""}`).join(",");
  // A new saved list (after applying, or a refresh) replaces the draft;
  // the last change's progress stays shown.
  useEffect(() => {
    setDraft(ov.items);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saved]);
  const changed = draft.map((i) => `${i.id}:${i.name ?? ""}`).join(",") !== saved;
  const minutes = Math.round(ov.timeoutNs / 6e10);
  const newer = ov.items.filter((i) => i.newer);

  function add(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    const id = idOf(newId);
    if (!id) return setError("Paste an item's Workshop link, or its ID (the number in the link).");
    if (draft.some((i) => i.id === id)) return setError(`${id} is in the list already.`);
    if (ov.namesEnv && !newName.trim()) return setError("This server also needs the item's mod name.");
    setDraft([...draft, { id, name: ov.namesEnv ? newName.trim() : undefined }]);
    setNewId("");
    setNewName("");
  }

  const move = (i: number, by: number) => {
    const d = [...draft];
    [d[i], d[i + by]] = [d[i + by], d[i]];
    setDraft(d);
  };

  async function apply() {
    setApplying(true);
    setError(undefined);
    const got: DeployEvent[] = [];
    setEvents([]);
    try {
      await api.applyWorkshop(
        server,
        draft.map((i) => ({ id: i.id, name: i.name })),
        (e) => {
          got.push(e);
          setEvents([...got]);
        },
      );
    } catch (e) {
      got.push({ stage: "error", message: (e as Error).message });
      setEvents([...got]);
    }
    setApplying(false);
    onChanged();
  }

  return (
    <div className="card mods workshop">
      <h2>Steam Workshop</h2>
      <p className="muted small">
        {ov.game} downloads these Workshop items itself when it starts: hlm keeps the list in its <code>{ov.env}</code>{" "}
        variable
        {ov.namesEnv && (
          <>
            , with their mod names in <code>{ov.namesEnv}</code>
          </>
        )}
        . Changing the list recreates the server with it.
        {ov.running &&
          ` hlm then waits up to ${minutes} minutes for it to download them and answer; if it doesn't, the old list is put back.`}{" "}
        Find items on the{" "}
        <a href={`https://steamcommunity.com/app/${ov.appId}/workshop/`} target="_blank" rel="noreferrer noopener">
          {ov.game} Workshop
        </a>
        .
      </p>
      {ov.warnings?.map((w) => (
        <div key={w} className="alert warning small">
          {w}
        </div>
      ))}
      {newer.length > 0 && ov.running && (
        <div className="alert info small">
          {newer.length === 1 ? `${newer[0].title || newer[0].id} was` : `${newer.length} items were`} updated on Steam
          since the server started. It gets updates when it next starts, if the image updates mods then.
        </div>
      )}

      {draft.length === 0 ? (
        <p className="muted small">No Workshop items.</p>
      ) : (
        <table className="data-table">
          <tbody>
            {draft.map((it, i) => {
              const known = ov.items.find((x) => x.id === it.id);
              return (
                <tr key={it.id}>
                  <td>
                    <div>
                      <a href={itemLink(it.id)} target="_blank" rel="noreferrer noopener">
                        {known?.title || it.id}
                      </a>{" "}
                      {it.name && <code className="muted small">{it.name}</code>}
                      {!known && <span className="badge">new</span>}
                      {known?.missing && <span className="badge">not on Steam (or private)</span>}
                      {known?.wrongGame && <span className="badge">for another game</span>}
                    </div>
                    <div className="muted small">
                      {it.id}
                      {known?.size ? <> · {formatBytes(known.size)}</> : null}
                      {known?.updated && (
                        <>
                          {" "}
                          · updated <Ago iso={known.updated} />
                        </>
                      )}
                    </div>
                  </td>
                  {canManage && (
                    <td className="actions">
                      <button className="btn small ghost" disabled={i === 0 || applying} onClick={() => move(i, -1)}>
                        ↑
                      </button>
                      <button
                        className="btn small ghost"
                        disabled={i === draft.length - 1 || applying}
                        onClick={() => move(i, 1)}
                      >
                        ↓
                      </button>
                      <button
                        className="btn small danger"
                        disabled={applying}
                        onClick={() => setDraft(draft.filter((_, j) => j !== i))}
                      >
                        Remove
                      </button>
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      {canManage && (
        <>
          <form className="mod-search" onSubmit={add}>
            <input
              value={newId}
              onChange={(e) => setNewId(e.target.value)}
              placeholder="Workshop link or item ID"
              aria-label="Workshop link or item ID"
            />
            {ov.namesEnv && (
              <input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="Mod name"
                aria-label="Mod name"
              />
            )}
            <button className="btn small" disabled={applying}>
              Add
            </button>
          </form>
          {ov.namesEnv && (
            <p className="muted small">
              The mod name is the mod&apos;s ID inside the item (its Workshop page usually lists it as “Mod ID”).
            </p>
          )}
          {error && <div className="alert error small">{error}</div>}
          {changed && (
            <div className="mod-plan">
              <p className="small">
                {ov.running
                  ? "Applying recreates the server with the new list and waits for it to come back."
                  : "The server is stopped: the new list is saved, and the items download when it next starts."}{" "}
                New items are checked with Steam first. The order is kept: in some games the first mod wins when two
                change the same thing.
              </p>
              <div className="btn-row">
                <button className="btn small primary" onClick={apply} disabled={applying}>
                  {applying ? "Applying…" : ov.running ? "Apply and restart" : "Apply"}
                </button>
                <button className="btn small" onClick={() => setDraft(ov.items)} disabled={applying}>
                  Undo changes
                </button>
              </div>
            </div>
          )}
          {events && (
            <section className="mod-progress">
              <h3>Applying the change</h3>
              <DeployProgress events={events} />
              {!applying && (
                <button className="btn small" onClick={() => setEvents(undefined)}>
                  Close
                </button>
              )}
            </section>
          )}
        </>
      )}
    </div>
  );
}
