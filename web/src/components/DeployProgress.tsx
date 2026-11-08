import { useMemo } from "react";
import { describeCopy } from "../copyprogress";
import type { CopyProgress, DeployEvent } from "../types";
import { formatBytes } from "../util";

interface Layer {
  status: string;
  current: number;
  total: number;
}

/** Shows the progress of an install, update or reconfigure from its event stream. */
export function DeployProgress({ events }: { events: DeployEvent[] }) {
  const { steps, layers, warnings, error, done, copy } = useMemo(() => {
    const steps: string[] = [];
    const warnings: string[] = [];
    const layers = new Map<string, Layer>();
    let error: string | undefined;
    let done: string | undefined;
    let copying = false; // the last step is a copy's progress, updated in place
    let copy: CopyProgress | undefined; // while data is being copied
    for (const e of events) {
      const progress = e.stage === "copy" && !!e.message?.endsWith("so far");
      if (e.stage === "warning" && e.message) warnings.push(e.message);
      else if (e.stage === "error") error = e.message;
      else if (e.stage === "done") done = e.message || "Server is up.";
      else if (e.message && progress && copying) steps[steps.length - 1] = e.message;
      else if (e.message) steps.push(e.message);
      if (e.message && e.stage !== "warning") copying = progress;
      // The bar shows while the copy runs, and goes when the next step comes.
      if (progress && e.copy) copy = e.copy;
      else if (e.message && e.stage !== "warning") copy = undefined;

      const p = e.progress;
      if (p?.id && p.status && !p.status.startsWith("Pulling from")) {
        const prev = layers.get(p.id) ?? { status: "", current: 0, total: 0 };
        const next = { ...prev, status: p.status };
        if (p.status === "Downloading" && p.progressDetail?.total) {
          next.current = p.progressDetail.current ?? 0;
          next.total = p.progressDetail.total;
        }
        if (/complete|already exists/i.test(p.status) && next.total) next.current = next.total;
        layers.set(p.id, next);
      }
    }
    return { steps, layers, warnings, error, done, copy: error ? undefined : copy };
  }, [events]);

  let current = 0;
  let total = 0;
  let finished = 0;
  for (const l of layers.values()) {
    current += l.current;
    total += l.total;
    if (/pull complete|already exists/i.test(l.status)) finished++;
  }
  const pulling = layers.size > 0 && finished < layers.size && !error;

  return (
    <div className="deploy-progress">
      <ol className="steps" aria-live="polite">
        {steps.map((s, i) => (
          <li key={i}>{s}</li>
        ))}
      </ol>

      {copy && <CopyBar copy={copy} />}

      {layers.size > 0 && (
        <div className="pull">
          <div className="pull-summary">
            <span>
              Image layers: {finished}/{layers.size}
            </span>
            {total > 0 && (
              <span>
                {formatBytes(current)} / {formatBytes(total)}
              </span>
            )}
          </div>
          <progress
            max={layers.size}
            value={pulling && total > 0 ? (current / total) * layers.size : finished}
          />
        </div>
      )}

      {warnings.length > 0 && (
        <ul className="warnings">
          {warnings.map((w, i) => (
            <li key={i}>⚠ {w}</li>
          ))}
        </ul>
      )}
      {error && <div className="alert error">{error}</div>}
      {done && <div className="alert success">{done}</div>}
    </div>
  );
}

/** A data copy's progress: how much of how much, how fast, and how long to go. */
function CopyBar({ copy }: { copy: CopyProgress }) {
  const d = describeCopy(copy);
  return (
    <div className="pull copy-progress">
      <div className="pull-summary">
        <span>{d.copied}</span>
        {d.speed && <span>{d.speed}</span>}
      </div>
      {d.percent === undefined ? (
        <progress aria-label="Copying data" />
      ) : (
        <progress max={100} value={d.percent} aria-label={`Copying data: ${d.percent.toFixed(0)}%`} />
      )}
      <div className="pull-summary">
        <span>{d.left}</span>
        {d.percent !== undefined && <span>{d.percent.toFixed(0)}%</span>}
      </div>
    </div>
  );
}
