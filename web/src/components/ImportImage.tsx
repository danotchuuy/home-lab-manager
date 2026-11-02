import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api";
import type { DeployEvent } from "../types";
import { DeployProgress } from "./DeployProgress";

/**
 * Pulls a Docker image and makes an install template from the defaults built
 * into it, then opens that template. With initial set, re-reads an image.
 */
export function ImportImage({ initial, onClose }: { initial?: string; onClose?: () => void }) {
  const navigate = useNavigate();
  const [image, setImage] = useState(initial ?? "");
  const [events, setEvents] = useState<DeployEvent[]>([]);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setEvents([]);
    let id: string | undefined;
    try {
      await api.importImage(image.trim(), (ev) => {
        if (ev.stage === "done") id = ev.message;
        else setEvents((prev) => [...prev, ev]);
      });
    } catch (err) {
      setEvents((prev) => [...prev, { stage: "error", message: (err as Error).message }]);
    } finally {
      setBusy(false);
    }
    if (id) {
      onClose?.();
      navigate(`/catalog/${id}`, { state: { refreshed: Date.now() } });
    }
  }

  return (
    <form className="card import-image" onSubmit={submit}>
      <h2>Add a Docker image</h2>
      <p className="muted small no-margin">
        Use any image, even one without a Community Applications template. hlm pulls it and builds an install form from
        the defaults inside it: its exposed ports, its volumes (as data folders) and its environment variables.
      </p>
      <div className="input-row">
        <input
          value={image}
          onChange={(e) => setImage(e.target.value)}
          placeholder="e.g. itzg/minecraft-server:latest or ghcr.io/owner/image:tag"
          spellCheck={false}
          autoFocus
        />
        <button className="btn primary" type="submit" disabled={busy || !image.trim()}>
          {busy ? "Pulling…" : initial ? "Read again" : "Pull & read"}
        </button>
        {onClose && (
          <button type="button" className="btn ghost" onClick={onClose} disabled={busy}>
            Cancel
          </button>
        )}
      </div>
      {events.length > 0 && <DeployProgress events={events} />}
    </form>
  );
}
