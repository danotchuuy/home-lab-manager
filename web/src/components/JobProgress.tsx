import type { Job } from "../types";
import { formatBytes } from "../util";
import { AppIcon } from "./common";

const KIND_LABEL: Record<Job["kind"], string> = {
  install: "Installing",
  update: "Updating",
  reconfigure: "Applying settings to",
};

/** Describes where a job is, e.g. "Downloading image: 1.2 GiB of 2.9 GiB · 3 of 9 layers". */
function describe(job: Job): { text: string; percent?: number } {
  if (job.error) return { text: job.error };
  if (job.finished) return { text: job.kind === "install" ? "Installed" : "Done" };
  const p = job.pull;
  if (job.stage === "pull" && p.layers > 0) {
    if (p.extracting) return { text: "Extracting image…" };
    const layers = `${p.done} of ${p.layers} layers`;
    if (p.total > 0) {
      return {
        text: `Downloading image: ${formatBytes(p.downloaded)} of ${formatBytes(p.total)} · ${layers}`,
        percent: (p.downloaded / p.total) * 100,
      };
    }
    return { text: `Downloading image: ${layers}`, percent: (p.done / p.layers) * 100 };
  }
  return { text: job.message || "Working…" };
}

/** A progress bar and status line for a job. */
export function JobStatus({ job }: { job: Job }) {
  const { text, percent } = describe(job);
  const state = job.error ? "failed" : job.finished ? "done" : "running";
  return (
    <div className={`job-status ${state}`}>
      {state === "running" && (
        <div className={`job-bar${percent === undefined ? " indeterminate" : ""}`}>
          <span style={percent === undefined ? undefined : { width: `${Math.min(percent, 100)}%` }} />
        </div>
      )}
      <div className="small job-text">
        {state === "done" && "✓ "}
        {state === "failed" && "✕ "}
        {text}
        {percent !== undefined && <span className="job-percent"> {percent.toFixed(0)}%</span>}
      </div>
    </div>
  );
}

/** A card for a server that is being installed and isn't in the server list yet. */
export function InstallCard({ job }: { job: Job }) {
  return (
    <div className="card server-card job-card">
      <div className="server-main">
        <AppIcon src={job.icon} />
        <div>
          <div className="server-name">
            {KIND_LABEL[job.kind]} {job.server}
          </div>
          <div className="muted small">
            {job.app} {job.image && <>· <code>{job.image}</code></>}
          </div>
          <JobStatus job={job} />
        </div>
      </div>
    </div>
  );
}
