import { useState, type FormEvent } from "react";
import { api } from "../api";
import type { DiffLine, GameProfileLink, GameProfilePreview, ProfileIndexListing } from "../types";
import { Spinner } from "./common";

/** Saves a blob as a file in the browser. */
export function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** The host a link is on, for showing where a profile came from. */
export function hostOf(link: string) {
  try {
    return new URL(link).host;
  } catch {
    return link;
  }
}

/** A line-by-line difference; long runs of unchanged lines are folded. */
function DiffView({ lines }: { lines: DiffLine[] }) {
  const out: { key: number; line?: DiffLine; skipped?: number }[] = [];
  const near = (i: number) => lines.slice(Math.max(0, i - 2), i + 3).some((l) => l.op !== " ");
  let skipped = 0;
  lines.forEach((l, i) => {
    if (l.op === " " && !near(i)) {
      skipped++;
      return;
    }
    if (skipped) out.push({ key: -i, skipped });
    skipped = 0;
    out.push({ key: i, line: l });
  });
  if (skipped) out.push({ key: -lines.length - 1, skipped });
  return (
    <pre className="op-output profile-diff">
      {out.map((o) =>
        o.line ? (
          <div key={o.key} className={o.line.op === "+" ? "diff-add" : o.line.op === "-" ? "diff-del" : ""}>
            {o.line.op} {o.line.text}
          </div>
        ) : (
          <div key={o.key} className="muted">
            … {o.skipped} unchanged line{o.skipped === 1 ? "" : "s"}
          </div>
        ),
      )}
    </pre>
  );
}

/** A fetched profile, before it's saved: what it is, and what it changes. */
export function ProfilePreview({
  preview,
  link,
  update,
  changedLocally,
  onSaved,
  onCancel,
}: {
  preview: GameProfilePreview;
  link: GameProfileLink;
  /** It's an update to a profile you imported before. */
  update?: boolean;
  changedLocally?: boolean;
  onSaved: (message: string) => void;
  onCancel: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [showAll, setShowAll] = useState(false);

  async function save() {
    setBusy(true);
    setError(undefined);
    try {
      const p = await api.importGameProfileLink(link, preview.sha256);
      onSaved(`Saved ${p.name} from ${hostOf(preview.url)} as ${p.file}.`);
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  }

  const what = !preview.replaces
    ? "It adds a game hlm doesn't know yet."
    : preview.replaces === "built-in"
      ? "It replaces hlm's built-in profile for this game. Delete it later to use the built-in one again."
      : "It replaces your own profile for this game.";

  return (
    <div className="profile-preview">
      <h3>
        {preview.name} <code className="muted">{preview.id}</code>
      </h3>
      <p className="small no-margin">
        From{" "}
        <a href={preview.url} target="_blank" rel="noreferrer noopener">
          {preview.url}
        </a>
        {preview.index && <span className="muted"> (listed in {hostOf(preview.index)})</span>}
      </p>
      <p className="small">{what}</p>
      {changedLocally && (
        <div className="alert warning small">
          You changed this profile since importing it. Updating replaces your changes: they&apos;re the lines marked “-”
          below.
        </div>
      )}
      {preview.same ? (
        <p className="muted small">It&apos;s exactly the profile already in use.</p>
      ) : preview.diff && !showAll ? (
        <>
          <p className="muted small no-margin">What changes (“-” goes, “+” comes):</p>
          <DiffView lines={preview.diff} />
          <button className="btn small ghost" onClick={() => setShowAll(true)}>
            Show the whole profile
          </button>
        </>
      ) : (
        <pre className="op-output game-profile-yaml">{preview.yaml}</pre>
      )}
      <p className="muted small">
        Profiles are only data, but they decide which commands hlm sends to a game&apos;s console. Look before saving.
      </p>
      {error && <div className="alert error small">{error}</div>}
      <div className="btn-row">
        <button className="btn primary small" onClick={save} disabled={busy || preview.same}>
          {busy ? "Saving…" : update ? "Update" : "Save it"}
        </button>
        <button className="btn small" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}

/** Asks for a link, and fetches the profile there. */
export function ImportFromLink({
  onPreview,
  onCancel,
}: {
  onPreview: (p: GameProfilePreview, link: GameProfileLink) => void;
  onCancel: () => void;
}) {
  const [url, setUrl] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function fetchIt(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const link = { url: url.trim() };
      onPreview(await api.fetchGameProfile(link), link);
    } catch (err) {
      setError((err as Error).message);
    }
    setBusy(false);
  }

  return (
    <form className="profile-link" onSubmit={fetchIt}>
      <label className="field">
        <span className="field-label">Link to a profile</span>
        <input
          type="url"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://github.com/someone/profiles/blob/main/mygame.yaml"
          autoFocus
        />
      </label>
      <p className="muted small no-margin">
        An HTTPS link to a <code>.yaml</code> file: a GitHub file or gist, or any web page serving one. hlm fetches it
        and shows it before anything is saved, and checks the link for updates every day.
      </p>
      {error && <div className="alert error small">{error}</div>}
      <div className="btn-row">
        <button className="btn primary small" disabled={busy || !url.trim()}>
          {busy ? "Fetching…" : "Fetch"}
        </button>
        <button type="button" className="btn small" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

/** Profile indexes: lists of profiles to browse and import. */
export function ProfileIndexes({ onPreview }: { onPreview: (p: GameProfilePreview, link: GameProfileLink) => void }) {
  const [links, setLinks] = useState<string>();
  const [listings, setListings] = useState<ProfileIndexListing[]>();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();

  async function run(what: string, f: () => Promise<void>) {
    setBusy(what);
    setError(undefined);
    try {
      await f();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(undefined);
  }

  const open = () =>
    run("load", async () => {
      const { indexes } = await api.gameProfileIndexes();
      setLinks(indexes.join("\n"));
      if (indexes.length) setListings(await api.browseGameProfileIndexes());
    });
  const save = () =>
    run("save", async () => {
      const { indexes } = await api.setGameProfileIndexes(
        (links ?? "")
          .split("\n")
          .map((l) => l.trim())
          .filter(Boolean),
      );
      setLinks(indexes.join("\n"));
      setListings(indexes.length ? await api.browseGameProfileIndexes() : []);
    });
  const look = (index: string, id: string) =>
    run("look:" + index + id, async () => {
      const link = { index, id };
      onPreview(await api.fetchGameProfile(link), link);
    });

  return (
    <details
      className="profile-indexes"
      onToggle={(e) => (e.currentTarget as HTMLDetailsElement).open && links === undefined && open()}
    >
      <summary>Profile indexes</summary>
      <p className="muted small">
        An index is a list of profiles someone publishes (an <code>index.yaml</code> with each profile&apos;s id, name,
        link and SHA-256), such as a community repository. hlm imports a profile from one only if it matches the SHA-256
        the index lists. None is set up by default.
      </p>
      {links === undefined ? (
        <Spinner />
      ) : (
        <>
          <label className="field">
            <span className="field-label">Index links, one per line</span>
            <textarea
              className="code-input"
              rows={Math.max(2, links.split("\n").length + 1)}
              value={links}
              onChange={(e) => setLinks(e.target.value)}
              placeholder="https://example.com/hlm-profiles/index.yaml"
              spellCheck={false}
            />
          </label>
          <div className="btn-row">
            <button className="btn small" onClick={save} disabled={!!busy}>
              {busy === "save" ? "Saving…" : "Save and browse"}
            </button>
          </div>
        </>
      )}
      {error && <div className="alert error small">{error}</div>}
      {listings?.map((l) => (
        <div key={l.url} className="profile-index">
          <h4>
            {l.name || hostOf(l.url)} <span className="muted small">{l.url}</span>
          </h4>
          {l.error ? (
            <div className="alert error small">{l.error}</div>
          ) : (
            <table className="data-table">
              <tbody>
                {l.profiles.map((p) => (
                  <tr key={p.id}>
                    <td>
                      {p.name} <code className="muted">{p.id}</code>
                      {p.description && <div className="muted small">{p.description}</div>}
                    </td>
                    <td className="muted small">
                      {p.installed
                        ? p.current
                          ? "Imported"
                          : "Imported; the index has a newer one"
                        : p.inUse === "built-in"
                          ? "Replaces a built-in profile"
                          : p.inUse === "custom"
                            ? "Replaces one of yours"
                            : ""}
                    </td>
                    <td className="actions">
                      <button className="btn small" onClick={() => look(l.url, p.id)} disabled={!!busy}>
                        {busy === "look:" + l.url + p.id ? "Fetching…" : "Look at it"}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      ))}
    </details>
  );
}
