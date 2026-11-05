import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type RefObject } from "react";
import { useSearchParams } from "react-router-dom";
import { api, ApiError } from "../api";
import type { ConfigFiles, FileContent, FileEntry, FileListing, FileMatch, FileMount } from "../types";
import { detectIndent, fromEditor, indent, newline, offsetOfLine, outdent, toEditor, usesCRLF, type Edit } from "../editing";
import { MAX_COLOR_SIZE, syntaxOf, tokenize, type Syntax } from "../highlight";
import { formatBytes, timeAgo } from "../util";
import { formatOf, validate, type Problem } from "../validate";
import { Ago, Spinner } from "./common";
import { SettingsForm } from "./SettingsForm";
import { FORM_FILE, parseSettings } from "../settingsform";

/** The largest file the editor opens; the backend enforces the same limit. */
export const MAX_EDIT_SIZE = 1024 * 1024;

/** File types the editor refuses (program code, not settings); matches the backend. */
const BLOCKED_EXT = /\.dll$/i;

/** Why a file can't be opened in the editor, or undefined if it can. */
function notOpenable(name: string, size: number, isFile: boolean): { badge: string; title: string } | undefined {
  if (BLOCKED_EXT.test(name)) return { badge: "can't be opened", title: "DLL files are program code, not settings" };
  if (isFile && size > MAX_EDIT_SIZE) return { badge: "too large to open", title: "Larger than 1 MB, so it can't be opened here" };
  return undefined;
}

const START_KEY = "hlm.files.start.";
/** START_KEY before the short name changed from gsh to hlm. */
const LEGACY_START_KEY = "gsh.files.start.";

/** The starting folder last picked for a server, if any. */
function savedStart(server: string): string | null {
  try {
    return localStorage.getItem(START_KEY + server) ?? localStorage.getItem(LEGACY_START_KEY + server);
  } catch {
    return null;
  }
}

function saveStart(server: string, dir: string) {
  try {
    localStorage.setItem(START_KEY + server, dir);
  } catch {
    // Not remembered; the first data folder is used next time.
  }
}

const WRAP_KEY = "hlm.files.wrap";
/** WRAP_KEY before the short name changed from gsh to hlm. */
const LEGACY_WRAP_KEY = "gsh.files.wrap";

/** Whether the editor wraps long lines; remembered in this browser for every file. */
function savedWrap(): boolean {
  try {
    return (localStorage.getItem(WRAP_KEY) ?? localStorage.getItem(LEGACY_WRAP_KEY)) === "1";
  } catch {
    return false;
  }
}

const VIEW_KEY = "hlm.files.view";

/** Whether settings files open as a form (the default) or as text. */
function savedView(): "form" | "text" {
  try {
    return localStorage.getItem(VIEW_KEY) === "text" ? "text" : "form";
  } catch {
    return "form";
  }
}

function saveView(v: "form" | "text") {
  try {
    localStorage.setItem(VIEW_KEY, v);
  } catch {
    // Storage may be unavailable; the choice just won't persist.
  }
}

function baseName(p: string): string {
  return p.slice(p.lastIndexOf("/") + 1);
}

function saveWrap(on: boolean) {
  try {
    localStorage.setItem(WRAP_KEY, on ? "1" : "0");
  } catch {
    // Not remembered; wrapping starts off next time.
  }
}

/**
 * The height each line takes in the textarea when long lines wrap, so the
 * line numbers can line up: measured in a hidden copy of the text with the
 * textarea's width and font. Undefined when not wrapping.
 */
function useWrappedHeights(
  area: RefObject<HTMLTextAreaElement | null>,
  mirror: RefObject<HTMLDivElement | null>,
  text: string,
  wrap: boolean,
) {
  const [heights, setHeights] = useState<number[]>();
  const [width, setWidth] = useState(0);

  useEffect(() => {
    const el = area.current;
    if (!wrap || !el) return;
    // The text's width changes with the window and when a scrollbar appears.
    const ro = new ResizeObserver(() => setWidth(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, [area, wrap]);

  useLayoutEffect(() => {
    const el = area.current;
    const m = mirror.current;
    if (!wrap || !el || !m) {
      setHeights(undefined);
      return;
    }
    const cs = getComputedStyle(el);
    m.style.width = `${el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)}px`;
    setHeights(Array.from(m.children, (c) => c.getBoundingClientRect().height));
  }, [area, mirror, text, wrap, width]);

  return heights;
}

/** Checks text as the user types, waiting for a pause. Undefined until checked, or for files with no format to check. */
function useValidation(path: string, text: string, enabled: boolean) {
  const format = formatOf(path);
  const [problems, setProblems] = useState<Problem[]>();
  useEffect(() => {
    if (!format || !enabled || !text.trim()) {
      setProblems(undefined);
      return;
    }
    const stop = new AbortController();
    const timer = setTimeout(async () => {
      try {
        const found = await validate(format, text, stop.signal);
        if (!stop.signal.aborted) {
          clearTimeout(stale);
          setProblems(found);
        }
      } catch {
        // Stopped, or the YAML checker failed to load; show no result rather than a wrong one.
        if (!stop.signal.aborted) setProblems(undefined);
      }
    }, 250);
    // A large file takes a while: don't leave the last result up as if it were current.
    const stale = setTimeout(() => setProblems(undefined), 1000);
    return () => {
      stop.abort();
      clearTimeout(timer);
      clearTimeout(stale);
    };
  }, [format, enabled, text]);
  return { format, problems };
}

/** One line of colored text. Memoized so typing only redraws the line it changes. */
const ColoredLine = memo(function ColoredLine({ line, syntax }: { line: string; syntax: Syntax }) {
  return (
    <>
      {tokenize(line, syntax).map((t, i) => (t.kind ? <span key={i} className={`tok-${t.kind}`}>{t.text}</span> : t.text))}
      {"\n"}
    </>
  );
});

/**
 * The colored copy of the text, which sits behind the textarea. The textarea's
 * own text is transparent, so typing, selecting and undo are all still the
 * browser's. It has the same size, font and scrollbars, so the two line up.
 */
const ColorLayer = memo(function ColorLayer({ text, syntax, wrap, layerRef }: { text: string; syntax: Syntax; wrap: boolean; layerRef: RefObject<HTMLDivElement | null> }) {
  return (
    <div className={`editor-area editor-colors${wrap ? " wrap" : ""}`} ref={layerRef} aria-hidden="true">
      {text.split("\n").map((line, i) => (
        <ColoredLine key={i} line={line} syntax={syntax} />
      ))}
    </div>
  );
});

/** The innermost mount containing p, if any. */
function mountFor(mounts: FileMount[], p: string): FileMount | undefined {
  return mounts
    .filter((m) => p === m.path || p.startsWith(m.path + "/"))
    .sort((a, b) => b.path.length - a.path.length)[0];
}

const CONFIG_EXT = /\.(json5?|jsonc|ini|cfg|conf|config|properties|ya?ml|toml|xml|txt|env|lua|cnf|vdf|acf)$/i;

function join(dir: string, name: string): string {
  return dir === "/" ? `/${name}` : `${dir}/${name}`;
}

function parent(p: string): string {
  const i = p.lastIndexOf("/");
  return i <= 0 ? "/" : p.slice(0, i);
}

function icon(e: FileEntry): string {
  if (e.isMount) return "🗄";
  if (e.type === "dir") return "📁";
  if (e.type === "symlink") return "↪";
  return CONFIG_EXT.test(e.name) ? "⚙" : "📄";
}

/**
 * Browses a server's files and edits text files. The open folder and file
 * are kept in the URL so a reload returns to them.
 */
export function FileBrowser({ server, running, canEdit }: { server: string; running: boolean; canEdit: boolean }) {
  const [params, setParams] = useSearchParams();
  const [listing, setListing] = useState<FileListing>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();
  const [filter, setFilter] = useState("");
  const [search, setSearch] = useState<{ dir: string; q: string; matches?: FileMatch[]; truncated?: boolean; error?: string }>();
  const dirParam = params.get("dir");
  const fileParam = params.get("file");
  // The server's pinned settings files, and its game's usual ones hlm found.
  const [configs, setConfigs] = useState<ConfigFiles>();
  useEffect(() => {
    api.configFiles(server).then(setConfigs, () => {});
  }, [server]);
  const isPinned = (p: string) => !!configs?.pinned.some((f) => f.path === p);
  async function togglePin(p: string) {
    if (!configs) return;
    const next = isPinned(p) ? configs.pinned.filter((f) => f.path !== p) : [...configs.pinned, { path: p }];
    try {
      setConfigs(await api.setConfigFiles(server, next));
    } catch (e) {
      setError((e as Error).message);
    }
  }

  const setLocation = useCallback(
    (next: { dir?: string; file?: string | null }) => {
      const p = new URLSearchParams(params);
      p.set("tab", "files");
      if (next.dir !== undefined) p.set("dir", next.dir);
      if (next.file === null) p.delete("file");
      else if (next.file !== undefined) p.set("file", next.file);
      setParams(p, { replace: next.file === undefined });
    },
    [params, setParams],
  );

  const load = useCallback(
    async (dir: string, quiet = false) => {
      setLoading(true);
      try {
        const l = await api.listFiles(server, dir);
        setListing(l);
        setError(undefined);
        return l;
      } catch (e) {
        if (!quiet) setError((e as Error).message);
      } finally {
        setLoading(false);
      }
    },
    [server],
  );

  // Open the folder in the URL, else the starting point picked last time,
  // else the backend's default (the first data folder). Listing / first
  // would be slow for big game servers.
  const loaded = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (dirParam && loaded.current === dirParam) return; // already showing it
    let cancelled = false;
    (async () => {
      const saved = dirParam ? null : savedStart(server);
      let l = saved ? await load(saved, true) : undefined;
      if (!l && !cancelled) l = await load(dirParam ?? "");
      if (!l || cancelled) return;
      loaded.current = l.path;
      if (!dirParam) setLocation({ dir: l.path });
    })();
    return () => {
      cancelled = true;
    };
    // setLocation changes with every URL change; only react to the folder.
  }, [dirParam, load, server]);

  async function findConfigs(q = "") {
    if (!listing) return;
    const dir = listing.path;
    setSearch({ dir, q });
    try {
      const res = await api.searchFiles(server, dir, q);
      setSearch({ dir, q, ...res });
    } catch (e) {
      setSearch({ dir, q, error: (e as Error).message });
    }
  }

  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    return (listing?.entries ?? []).filter((e) => !f || e.name.toLowerCase().includes(f));
  }, [listing, filter]);

  if (fileParam) {
    return (
      <FileEditor
        canEdit={canEdit}
        server={server}
        path={fileParam}
        running={running}
        pinned={isPinned(fileParam)}
        onPin={canEdit && configs ? () => togglePin(fileParam) : undefined}
        onClose={() => setLocation({ file: null, dir: parent(fileParam) })}
      />
    );
  }

  const crumbs = (listing?.path ?? dirParam ?? "/").split("/").filter(Boolean);

  return (
    <div className="file-browser card">
      {configs && (configs.pinned.length > 0 || configs.suggested.length > 0) && (
        <div className="config-bar">
          <span className="field-label">Config files</span>
          {configs.pinned.map((f) => (
            <button key={f.path} className="chip" title={f.path} onClick={() => setLocation({ file: f.path })}>
              ★ {f.label || baseName(f.path)}
            </button>
          ))}
          {configs.suggested.map((f) => (
            <span key={f.path} className="chip suggested" title={`${f.path}\nThe usual settings file of this game`}>
              <button className="link-button" onClick={() => setLocation({ file: f.path })}>
                {baseName(f.path)}
              </button>
              {canEdit && (
                <button className="link-button" onClick={() => togglePin(f.path)} title="Pin it, for one-click opening">
                  Pin
                </button>
              )}
            </span>
          ))}
        </div>
      )}
      {listing && (
        <LocationPicker
          mounts={listing.mounts}
          current={listing.path}
          onPick={(dir) => {
            saveStart(server, dir);
            setSearch(undefined);
            setFilter("");
            setLocation({ dir });
          }}
        />
      )}

      <div className="file-toolbar">
        <nav className="crumbs" aria-label="Folder">
          <button className="link-button" onClick={() => setLocation({ dir: "/" })}>
            /
          </button>
          {crumbs.map((c, i) => {
            const p = "/" + crumbs.slice(0, i + 1).join("/");
            return (
              <span key={p}>
                <button className="link-button" onClick={() => setLocation({ dir: p })}>
                  {c}
                </button>
                {i < crumbs.length - 1 && <span className="muted">/</span>}
              </span>
            );
          })}
        </nav>
        <div className="spacer" />
        <input
          className="file-filter"
          type="search"
          placeholder="Filter this folder"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <button className="btn small" onClick={() => findConfigs()} disabled={!listing}>
          Find config files
        </button>
      </div>

      {error && <div className="alert error">{error}</div>}
      {listing?.truncated && (
        <div className="alert warn">
          {running
            ? "This folder is too large to list completely; some entries are missing."
            : "Only part of this folder could be listed: while the server is stopped, Docker has to send a large folder's whole contents to list it. Start the server to browse it fully."}
        </div>
      )}

      {search && (
        <div className="search-results">
          <div className="search-head">
            <strong>
              {search.q ? `Files matching "${search.q}"` : "Config files"} in {search.dir}
            </strong>
            <form
              onSubmit={(e) => {
                e.preventDefault();
                findConfigs(new FormData(e.currentTarget).get("q") as string);
              }}
            >
              <input name="q" type="search" placeholder="Search by name…" defaultValue={search.q} />
            </form>
            <button className="btn ghost small" onClick={() => setSearch(undefined)}>
              Close
            </button>
          </div>
          {search.error && <div className="alert error">{search.error}</div>}
          {!search.matches && !search.error && <Spinner />}
          {search.matches?.length === 0 && <p className="muted small">Nothing found.</p>}
          <ul className="match-list">
            {search.matches?.map((m) => {
              const blocked = notOpenable(m.path, m.size, true);
              return (
                <li key={m.path}>
                  <button
                    className="link-button"
                    disabled={!!blocked}
                    title={blocked?.title}
                    onClick={() => setLocation({ file: m.path })}
                  >
                    {m.path.slice(search.dir === "/" ? 1 : search.dir.length + 1)}
                  </button>
                  <span className="muted small">
                    {blocked && <span className="badge">{blocked.badge}</span>} {formatBytes(m.size)} ·{" "}
                    <Ago iso={m.mtime} />
                  </span>
                </li>
              );
            })}
          </ul>
          {search.truncated && <p className="muted small">Showing the first results only; search a subfolder to narrow it down.</p>}
        </div>
      )}

      <div className={`file-list${loading ? " loading" : ""}`} role="list">
        {listing && listing.path !== "/" && (
          <button className="file-row" onClick={() => setLocation({ dir: parent(listing.path) })}>
            <span className="file-icon">↩</span>
            <span className="file-name">..</span>
          </button>
        )}
        {shown.map((e) => {
          const p = join(listing!.path, e.name);
          const isDir = e.type === "dir" || (e.type === "symlink" && !CONFIG_EXT.test(e.name) && !e.name.includes("."));
          const blocked = isDir ? undefined : notOpenable(e.name, e.size, e.type === "file");
          return (
            <button
              key={e.name}
              className="file-row"
              role="listitem"
              disabled={!!blocked}
              title={blocked?.title}
              onClick={() => (isDir ? setLocation({ dir: p }) : setLocation({ file: p }))}
            >
              <span className="file-icon" aria-hidden="true">
                {icon(e)}
              </span>
              <span className="file-name">
                {e.name}
                {e.link && <span className="muted small"> → {e.link}</span>}
                {e.isMount && <span className="badge">data folder</span>}
                {blocked && <span className="badge">{blocked.badge}</span>}
              </span>
              <span className="file-size muted small">{e.type === "file" ? formatBytes(e.size) : ""}</span>
              <span className="file-time muted small">{e.mtime && !e.mtime.startsWith("0001") ? timeAgo(e.mtime) : ""}</span>
            </button>
          );
        })}
        {listing && shown.length === 0 && <p className="muted small empty-folder">{filter ? "No matches." : "This folder is empty."}</p>}
        {!listing && !error && <Spinner />}
      </div>
      {listing && (
        <p className="muted small file-source">
          {listing.source === "host" ? "Read from the host folder" : "Read through Docker"}
          {listing.readOnly && " · read-only mount"}
        </p>
      )}
    </div>
  );
}

function FileEditor({
  server,
  path,
  running,
  canEdit,
  pinned,
  onPin,
  onClose,
}: {
  server: string;
  path: string;
  running: boolean;
  canEdit: boolean;
  pinned: boolean;
  /** Pins or unpins the file; missing when the caller can't. */
  onPin?: () => void;
  onClose: () => void;
}) {
  const [file, setFile] = useState<FileContent>();
  const [text, setText] = useState("");
  const [error, setError] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [conflict, setConflict] = useState(false);
  const areaRef = useRef<HTMLTextAreaElement>(null);
  const gutterRef = useRef<HTMLDivElement>(null);
  const mirrorRef = useRef<HTMLDivElement>(null);
  const colorsRef = useRef<HTMLDivElement>(null);
  const [wrap, setWrapState] = useState(savedWrap);
  const [view, setViewState] = useState(savedView);
  const setView = (v: "form" | "text") => {
    setViewState(v);
    saveView(v);
  };
  const setWrap = (on: boolean) => {
    setWrapState(on);
    saveWrap(on);
  };

  const open = useCallback(async () => {
    try {
      const f = await api.readFile(server, path);
      setFile(f);
      setText(toEditor(f.content));
      setError(undefined);
      setConflict(false);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [server, path]);

  useEffect(() => {
    open();
  }, [open]);

  const original = useMemo(() => toEditor(file?.content ?? ""), [file?.content]);
  // Settings files can be edited as a form, if the file as opened reads as one.
  const formAvailable = useMemo(
    () => !!file && !file.binary && FORM_FILE.test(path) && parseSettings(original) !== undefined,
    [file, path, original],
  );
  const asForm = formAvailable && view === "form";
  const crlf = useMemo(() => usesCRLF(file?.content ?? ""), [file?.content]);
  const dirty = !!file && text !== original;
  // The size as saved: CRLF makes the file larger than the text shown here.
  const size = useMemo(() => new TextEncoder().encode(fromEditor(text, crlf)).length, [text, crlf]);
  const tooLarge = size > MAX_EDIT_SIZE;

  // Warn before leaving with unsaved changes.
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const { format, problems } = useValidation(path, text, !!file && !file.binary);
  const badLines = useMemo(() => new Set(problems?.map((p) => p.line)), [problems]);
  // One Tab's worth of indentation, taken from the file as it was opened.
  const unit = useMemo(() => detectIndent(file?.content ?? "", path), [file?.content, path]);

  async function save(force = false) {
    if (!file || tooLarge) return;
    setSaving(true);
    try {
      const f = await api.writeFile(server, path, fromEditor(text, crlf), force ? undefined : file.mtime);
      setFile(f);
      setText(toEditor(f.content));
      setConflict(false);
      setError(undefined);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setConflict(true);
      else setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  function close() {
    if (dirty && !confirm("Discard your unsaved changes?")) return;
    onClose();
  }

  function onKey(e: KeyboardEvent<HTMLTextAreaElement>) {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      if (dirty && !saving && !tooLarge) save();
      return;
    }
    if (e.altKey && e.code === "KeyZ") {
      // Alt+Z, as in VS Code.
      e.preventDefault();
      setWrap(!wrap);
      return;
    }
    if (readOnly) return; // Tab moves on to the next control, as usual.
    const el = e.currentTarget;
    const { value, selectionStart: start, selectionEnd: end } = el;
    if (e.key === "Tab") {
      e.preventDefault();
      apply(el, e.shiftKey ? outdent(value, start, end, unit) : indent(value, start, end, unit));
    } else if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      apply(el, newline(value, start, end));
    }
  }

  /** Makes an edit the way typing would, so Ctrl+Z undoes it. */
  function apply(el: HTMLTextAreaElement, edit: Edit | undefined) {
    if (!edit) return;
    el.setSelectionRange(edit.from, edit.to);
    const done = edit.insert ? document.execCommand("insertText", false, edit.insert) : document.execCommand("delete");
    if (!done) {
      // No undo here, but the edit still happens.
      el.setRangeText(edit.insert, edit.from, edit.to);
      setText(el.value);
    }
    el.setSelectionRange(edit.selStart, edit.selEnd);
  }

  function goToLine(line: number) {
    const el = areaRef.current;
    if (!el) return;
    const at = offsetOfLine(el.value, line);
    const rowHeight = parseFloat(getComputedStyle(el).lineHeight);
    const top = heights ? heights.slice(0, line - 1).reduce((sum, h) => sum + h, 0) : (line - 1) * rowHeight;
    el.focus();
    el.setSelectionRange(at, at);
    el.scrollTop = top - el.clientHeight / 2;
  }

  const lineTexts = useMemo(() => text.split("\n"), [text]);
  const lines = lineTexts.length;
  const readOnly = !file || file.readOnly || file.binary || !canEdit;
  const heights = useWrappedHeights(areaRef, mirrorRef, text, wrap && !!file && !file.binary);
  const syntax = size <= MAX_COLOR_SIZE ? syntaxOf(path) : undefined;

  // The layer changes size after the textarea does, so match it again.
  useLayoutEffect(() => {
    const area = areaRef.current;
    if (colorsRef.current && area) {
      colorsRef.current.scrollTop = area.scrollTop;
      colorsRef.current.scrollLeft = area.scrollLeft;
    }
  }, [text, wrap, syntax]);

  return (
    <div className="file-editor card">
      <div className="editor-head">
        <button className="btn ghost small" onClick={close}>
          ← Back
        </button>
        <code className="editor-path">{path}</code>
        {onPin ? (
          <button
            className={`btn small ghost pin-button${pinned ? " pinned" : ""}`}
            onClick={onPin}
            title={pinned ? "Unpin: stop showing it under Config files" : "Pin it under Config files, for one-click opening"}
          >
            {pinned ? "★ Pinned" : "☆ Pin"}
          </button>
        ) : (
          pinned && <span className="badge">★ pinned</span>
        )}
        {dirty && <span className="badge update-available">unsaved</span>}
        {saved && !dirty && <span className="badge state-running">saved</span>}
        <div className="spacer" />
        {formAvailable && (
          <div className="view-toggle" role="group" aria-label="View">
            <button className={asForm ? "active" : ""} onClick={() => setView("form")}>
              Form
            </button>
            <button className={!asForm ? "active" : ""} onClick={() => setView("text")}>
              Text
            </button>
          </div>
        )}
        {file && !file.binary && !asForm && (
          <label className="radio small editor-wrap" title="Alt+Z">
            <input type="checkbox" checked={wrap} onChange={(e) => setWrap(e.target.checked)} /> Word wrap
          </label>
        )}
        {file && !readOnly && (
          <>
            <button className="btn small" onClick={() => setText(original)} disabled={!dirty || saving}>
              Revert
            </button>
            <button className="btn small primary" onClick={() => save()} disabled={!dirty || saving || tooLarge}>
              {saving ? "Saving…" : "Save"}
            </button>
          </>
        )}
      </div>

      {running && file && !readOnly && (
        <div className="alert warn small">
          The server is running. Many game servers only read their settings at startup and some rewrite them when they shut
          down, so stop the server before saving, then start it again.
        </div>
      )}
      {conflict && (
        <div className="alert error">
          This file changed since you opened it (perhaps the server rewrote it).{" "}
          <button className="link-button" onClick={open}>
            Load the new version
          </button>{" "}
          (your changes are lost) or{" "}
          <button className="link-button" onClick={() => save(true)}>
            overwrite it with yours
          </button>
          .
        </div>
      )}
      {error && <div className="alert error">{error}</div>}
      {!file && !error && <Spinner />}
      {file?.binary && <div className="alert info">This is a binary file, so it can't be edited here.</div>}
      {file?.readOnly && <div className="alert info">This file is on a read-only mount.</div>}

      {file && !file.binary && asForm && (
        <div
          onKeyDown={(e) => {
            if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
              e.preventDefault();
              if (dirty && !saving && !tooLarge) save();
            }
          }}
        >
          <SettingsForm text={text} original={original} readOnly={readOnly} onChange={setText} />
        </div>
      )}

      {file && !file.binary && !asForm && (
        <div className="editor-body">
          <div className="editor-gutter" ref={gutterRef} aria-hidden="true">
            {Array.from({ length: lines }, (_, i) => (
              <div key={i} className={badLines.has(i + 1) ? "bad-line" : undefined} style={heights?.[i] ? { height: heights[i] } : undefined}>
                {i + 1}
              </div>
            ))}
          </div>
          {wrap && (
            <div className="editor-mirror" ref={mirrorRef} aria-hidden="true">
              {lineTexts.map((l, i) => (
                // A zero-width space keeps empty lines one row tall.
                <div key={i}>{l || "\u200b"}</div>
              ))}
            </div>
          )}
          <div className="editor-pane">
            {syntax && <ColorLayer text={text} syntax={syntax} wrap={wrap} layerRef={colorsRef} />}
            <textarea
              ref={areaRef}
              className={`editor-area${wrap ? " wrap" : ""}${syntax ? " colored" : ""}`}
              value={text}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={onKey}
              onScroll={(e) => {
                if (gutterRef.current) gutterRef.current.scrollTop = e.currentTarget.scrollTop;
                if (colorsRef.current) {
                  colorsRef.current.scrollTop = e.currentTarget.scrollTop;
                  colorsRef.current.scrollLeft = e.currentTarget.scrollLeft;
                }
              }}
              readOnly={readOnly}
              spellCheck={false}
              autoCapitalize="off"
              autoCorrect="off"
              wrap={wrap ? "soft" : "off"}
              aria-label={`Contents of ${path}`}
            />
          </div>
        </div>
      )}

      {file && (
        <div className="editor-foot muted small">
          <span>
            {formatBytes(file.size)} · {file.mode} · modified <Ago iso={file.mtime} /> ·{" "}
            {file.source === "host" ? "host folder" : "via Docker"}
          </span>
          {problems && problems.length === 0 && <span className="editor-valid">✓ Valid {format}</span>}
          {problems && problems.length > 0 && <ProblemNote format={format!} problem={problems[0]} more={problems.length - 1} onGo={goToLine} />}
          {tooLarge && (
            <span className="form-error size-error">
              Over the 1 MiB limit by {(size - MAX_EDIT_SIZE).toLocaleString()} bytes, so it can't be saved here.
            </span>
          )}
          {asForm ? (
            <span>Only the values you change are rewritten · Ctrl+S to save</span>
          ) : (
            <span>
              {lines} lines · {unit === "\t" ? "tabs" : `${unit.length} spaces`} · {crlf ? "CRLF" : "LF"}
              {syntaxOf(path) && !syntax && " · too large for colors"} · Ctrl+S to save · Alt+Z to wrap
            </span>
          )}
        </div>
      )}
    </div>
  );
}

/** The first syntax problem in a file, which jumps to its line when it has one. */
function ProblemNote({ format, problem, more, onGo }: { format: string; problem: Problem; more: number; onGo: (line: number) => void }) {
  const { line, message } = problem;
  return (
    <span className="form-error">
      {format} error
      {line ? (
        <>
          {" "}
          on{" "}
          <button className="link-button" onClick={() => onGo(line)}>
            line {line}
          </button>
        </>
      ) : null}
      : {message}
      {more > 0 && ` (and ${more} more)`}
    </span>
  );
}

/** Picks where to browse from: one of the server's mounts, or the container root. */
function LocationPicker({ mounts, current, onPick }: { mounts: FileMount[]; current: string; onPick: (dir: string) => void }) {
  const mount = mountFor(mounts, current);
  return (
    <div className="location-picker">
      <label className="field">
        <span className="field-label">Location</span>
        <select value={mount?.path ?? "/"} onChange={(e) => onPick(e.target.value)}>
          {mounts.map((m) => (
            <option key={m.path} value={m.path}>
              {m.path}
              {m.readOnly ? " (read-only)" : ""}
            </option>
          ))}
          <option value="/">Container root (/)</option>
        </select>
      </label>
      <p className="muted small location-detail">
        {mount ? (
          <>
            {mount.type === "volume" ? "Docker volume" : "Host folder"} <code>{mount.source}</code>
            {mount.readOnly ? " · read-only" : ""}
          </>
        ) : mounts.length > 0 ? (
          "The container's own files, outside its data folders."
        ) : (
          "This server has no data folders; browsing the container's own files."
        )}
      </p>
    </div>
  );
}
