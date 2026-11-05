import { useEffect, useMemo, useState } from "react";
import { api } from "../api";
import type { CleanupItem, CleanupKind, CleanupPlan, CleanupResult, CleanupSettings } from "../types";
import { formatBytes, timeAgo } from "../util";
import { useHosts } from "./HostPicker";

const GROUPS: { kind: CleanupKind; title: string; help: string }[] = [
  {
    kind: "old-image",
    title: "Old images",
    help: "Untagged images no container uses: what's left of an image after an update moved its tag to a newer one.",
  },
  {
    kind: "helper",
    title: "Leftover containers",
    help: "Stopped containers hlm left behind: helpers, and the old container of a server that was recreated.",
  },
  {
    kind: "unused-image",
    title: "Unused images",
    help: "Tagged images no container uses, such as one for a server you deleted. They'd be downloaded again if needed.",
  },
  { kind: "build-cache", title: "Build cache", help: "Docker's cache from building images." },
  {
    kind: "volume",
    title: "Unused volumes",
    help: "Docker volumes no container uses, other than deleted servers' data (that's below). Their data is gone for good once deleted.",
  },
];

const key = (it: CleanupItem) => `${it.kind} ${it.id ?? ""}`;

/** Host page → Clean up: free disk space Docker holds on to, on any Docker host. */
export function DiskCleanup({ onCleaned }: { onCleaned?: () => void }) {
  const hosts = useHosts();
  const [host, setHost] = useState("local");
  const [plan, setPlan] = useState<CleanupPlan>();
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<CleanupResult>();
  const [settings, setSettings] = useState<CleanupSettings>();

  useEffect(() => {
    api.cleanupSettings().then(setSettings, () => {});
  }, []);

  async function find(h = host) {
    setBusy("find");
    setError(undefined);
    try {
      const p = await api.cleanupCandidates(h);
      setPlan(p);
      setChosen(new Set(p.items.filter((i) => i.default).map(key)));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const selected = useMemo(() => plan?.items.filter((i) => chosen.has(key(i))) ?? [], [plan, chosen]);
  const total = selected.reduce((n, i) => n + Math.max(i.size, 0), 0);

  async function clean() {
    const volumes = selected.filter((i) => i.kind === "volume");
    let msg = `Delete ${selected.length} item${selected.length === 1 ? "" : "s"} (up to ${formatBytes(total)})?`;
    if (volumes.length > 0)
      msg += `\n\nThat includes ${volumes.length} volume${volumes.length === 1 ? "" : "s"}: ${volumes.map((v) => v.name).join(", ")}. Their data can't be got back.`;
    if (!confirm(msg)) return;
    setBusy("clean");
    setError(undefined);
    try {
      const ids = (kind: CleanupKind | CleanupKind[]) =>
        selected.filter((i) => ([] as CleanupKind[]).concat(kind).includes(i.kind)).map((i) => i.id!);
      setResult(
        await api.cleanup(
          {
            images: ids(["old-image", "unused-image"]),
            containers: ids("helper"),
            volumes: ids("volume"),
            buildCache: selected.some((i) => i.kind === "build-cache"),
          },
          host,
        ),
      );
      onCleaned?.();
      await find();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  async function saveSettings(change: Partial<CleanupSettings>) {
    if (!settings) return;
    try {
      setSettings(await api.saveCleanupSettings({ ...settings, ...change }));
    } catch (e) {
      setError((e as Error).message);
    }
  }

  function toggle(items: CleanupItem[], on: boolean) {
    setChosen((prev) => {
      const next = new Set(prev);
      for (const i of items) {
        if (on) next.add(key(i));
        else next.delete(key(i));
      }
      return next;
    });
  }

  return (
    <section className="card disk-cleanup">
      <div className="section-head">
        <h2>Clean up</h2>
        {hosts && hosts.length > 1 && (
          <select
            value={host}
            onChange={(e) => {
              setHost(e.target.value);
              setPlan(undefined);
              setResult(undefined);
            }}
          >
            {hosts.map((h) => (
              <option key={h.id} value={h.id}>
                {h.name}
              </option>
            ))}
          </select>
        )}
      </div>
      <p className="muted small no-margin">
        Images, containers and cache Docker keeps after updates and deletes. Nothing a container uses is ever deleted,
        running or not, and deleted servers&apos; data is left to <strong>Deleted servers</strong>.
      </p>
      {settings && (
        <div className="cleanup-settings">
          <label className="radio small">
            <input
              type="checkbox"
              checked={!settings.keepOldImages}
              onChange={(e) => saveSettings({ keepOldImages: !e.target.checked })}
            />
            Delete a server&apos;s old image once an update has worked (if nothing else uses it)
          </label>
          <label className="radio small">
            <input
              type="checkbox"
              checked={settings.weekly}
              onChange={(e) => saveSettings({ weekly: e.target.checked })}
            />
            Delete old images and leftover containers every week, on every host
            {settings.lastRun && (
              <span className="muted">
                {" "}
                (last {timeAgo(settings.lastRun)}: {formatBytes(settings.lastFreed)} freed
                {settings.lastError ? `; ${settings.lastError}` : ""})
              </span>
            )}
          </label>
        </div>
      )}
      {error && <div className="alert error small">{error}</div>}
      {result && (
        <div className={`alert ${result.failed.length ? "warn" : "success"} small`}>
          Deleted {result.removed.length} item{result.removed.length === 1 ? "" : "s"}, freeing{" "}
          {formatBytes(result.freed)}.
          {result.failed.map((f) => (
            <div key={key(f)}>
              {f.name}: {f.error}
            </div>
          ))}
        </div>
      )}
      {!plan ? (
        <div className="btn-row">
          <button className="btn" onClick={() => find()} disabled={!!busy}>
            {busy === "find" ? "Looking…" : "Find what can be freed"}
          </button>
        </div>
      ) : plan.items.length === 0 ? (
        <p className="muted small">Nothing to clean up.</p>
      ) : (
        <>
          {GROUPS.map((g) => {
            const items = plan.items.filter((i) => i.kind === g.kind);
            if (items.length === 0) return null;
            const all = items.every((i) => chosen.has(key(i)));
            return (
              <div key={g.kind} className="cleanup-group">
                <label className="radio cleanup-group-head">
                  <input type="checkbox" checked={all} onChange={(e) => toggle(items, e.target.checked)} />
                  <strong>{g.title}</strong>{" "}
                  <span className="muted small">
                    {items.length} · {formatBytes(items.reduce((n, i) => n + Math.max(i.size, 0), 0))}
                  </span>
                </label>
                <div className="muted small">{g.help}</div>
                {g.kind !== "build-cache" && (
                  <ul className="cleanup-items">
                    {items.map((i) => (
                      <li key={key(i)}>
                        <label className="radio small">
                          <input
                            type="checkbox"
                            checked={chosen.has(key(i))}
                            onChange={(e) => toggle([i], e.target.checked)}
                          />
                          <code>{i.name || i.id?.slice(0, 12)}</code>
                          <span className="muted">
                            {" "}
                            {i.size >= 0 ? formatBytes(i.size) : "size unknown"}
                            {i.created ? ` · ${timeAgo(i.created)}` : ""}
                          </span>
                        </label>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            );
          })}
          <p className="muted small">
            Image sizes include layers other images may share, so a little less may be freed.
          </p>
          <div className="btn-row">
            <button className="btn primary" onClick={clean} disabled={!!busy || selected.length === 0}>
              {busy === "clean" ? "Cleaning up…" : `Clean up ${formatBytes(total)}`}
            </button>
            <button className="btn ghost" onClick={() => find()} disabled={!!busy}>
              Look again
            </button>
          </div>
        </>
      )}
    </section>
  );
}
