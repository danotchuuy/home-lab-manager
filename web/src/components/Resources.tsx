import type { Stats } from "../types";
import { formatBytes } from "../util";

/** CPU use as a share of the whole host (Docker reports 100% per core). */
export function hostCpuPercent(s: Stats): number {
  return s.cpuPercent / Math.max(s.onlineCpus, 1);
}

export function memoryPercent(s: Stats): number {
  return s.memoryLimit ? (s.memoryUsage / s.memoryLimit) * 100 : 0;
}

function Meter({ percent }: { percent: number }) {
  const p = Math.min(Math.max(percent, 0), 100);
  const level = p >= 90 ? "high" : p >= 70 ? "mid" : "low";
  return (
    <span className="meter" aria-hidden="true">
      <span className={`meter-fill ${level}`} style={{ width: `${p}%` }} />
    </span>
  );
}

/** Compact CPU, memory and network readout for a server card. */
export function ResourceSummary({ stats }: { stats?: Stats }) {
  if (!stats) return <div className="resource-summary muted small">Measuring…</div>;
  const cpu = hostCpuPercent(stats);
  const cores = stats.cpuPercent / 100;
  return (
    <div className="resource-summary small">
      <span className="resource" title={`${cores.toFixed(2)} of ${stats.onlineCpus} CPU cores`}>
        <span className="resource-label">CPU</span>
        <Meter percent={cpu} />
        <span className="resource-value">{cpu.toFixed(1)}%</span>
      </span>
      <span
        className="resource"
        title={`${formatBytes(stats.memoryUsage)} of ${formatBytes(stats.memoryLimit)} (${memoryPercent(stats).toFixed(1)}%)`}
      >
        <span className="resource-label">RAM</span>
        <Meter percent={memoryPercent(stats)} />
        <span className="resource-value">{formatBytes(stats.memoryUsage)}</span>
      </span>
      <span className="resource" title="Total network traffic since the container started">
        <span className="resource-label">NET</span>
        <span className="resource-value">
          ↓ {formatBytes(stats.networkRx)} ↑ {formatBytes(stats.networkTx)}
        </span>
      </span>
    </div>
  );
}

/** Large stat tile with an optional meter, for the server page. */
export function StatTile({ label, value, detail, percent }: { label: string; value: string; detail?: string; percent?: number }) {
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {percent !== undefined && <Meter percent={percent} />}
      {detail && <div className="stat-detail muted small">{detail}</div>}
    </div>
  );
}

/** A container's resource usage as a row of tiles, for a server's or connection's page. */
export function StatTiles({ stats }: { stats: Stats }) {
  return (
    <div className="stats">
      <StatTile
        label="CPU"
        value={`${hostCpuPercent(stats).toFixed(1)}% of host`}
        detail={`${(stats.cpuPercent / 100).toFixed(2)} of ${stats.onlineCpus} cores`}
        percent={hostCpuPercent(stats)}
      />
      <StatTile
        label="Memory"
        value={formatBytes(stats.memoryUsage)}
        detail={`of ${formatBytes(stats.memoryLimit)} (${memoryPercent(stats).toFixed(1)}%)`}
        percent={memoryPercent(stats)}
      />
      <StatTile
        label="Network"
        value={`↓ ${formatBytes(stats.networkRx)} · ↑ ${formatBytes(stats.networkTx)}`}
        detail="since start"
      />
      <StatTile
        label="Disk I/O"
        value={`R ${formatBytes(stats.blockRead)} · W ${formatBytes(stats.blockWrite)}`}
        detail="since start"
      />
      <StatTile label="Processes" value={String(stats.pids)} />
    </div>
  );
}
