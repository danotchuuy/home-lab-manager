import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { Server } from "../types";

/** A server's tags, as chips that open the list filtered by them; managers can change them. */
export function TagEditor({
  server,
  canEdit,
  onChange,
}: {
  server: Server;
  canEdit: boolean;
  onChange: (tags: string[]) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState<string>();
  const tags = server.tags ?? [];

  async function save(e: FormEvent) {
    e.preventDefault();
    try {
      const r = await api.setTags(server.id, text.split(","));
      onChange(r.tags);
      setEditing(false);
      setError(undefined);
    } catch (err) {
      setError((err as Error).message);
    }
  }

  if (editing) {
    return (
      <form className="tag-editor" onSubmit={save}>
        <input
          autoFocus
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="survival, friends, weekend"
          aria-label="Tags, separated by commas"
          onKeyDown={(e) => e.key === "Escape" && setEditing(false)}
        />
        <button className="btn small primary" type="submit">
          Save
        </button>
        <button className="btn small ghost" type="button" onClick={() => setEditing(false)}>
          Cancel
        </button>
        {error && <span className="error-text small">{error}</span>}
      </form>
    );
  }
  if (tags.length === 0 && !canEdit) return null;
  return (
    <div className="tag-chips">
      {tags.map((t) => (
        <Link key={t} className="tag-chip" to={`/?tag=${encodeURIComponent(t)}`} title={`Servers tagged ${t}`}>
          {t}
        </Link>
      ))}
      {canEdit && (
        <button
          type="button"
          className="link-button small"
          onClick={() => {
            setText(tags.join(", "));
            setEditing(true);
          }}
        >
          {tags.length ? "Edit tags" : "+ Add tags"}
        </button>
      )}
    </div>
  );
}
