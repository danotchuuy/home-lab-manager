import { Fragment, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import { StackedBar, TimeSeriesChart, usePoll } from "../components/charts";
import { Ago, AppIcon, Spinner } from "../components/common";
import { DiskCleanup } from "../components/DiskCleanup";
import { DockerHosts } from "../components/DockerHosts";
import { HostHistory, RangePicker } from "../components/ResourceHistory";
import type { HistoryRange, DataFolder, DeletedStorage, HostSnapshot, ServerStorage, StorageReport } from "../types";
import { formatBytes, timeAgo } from "../util";

const rate = (v: number) => `${formatBytes(v)}/s`;
const pct = (v: number) => `${v.toFixed(v < 10 ? 1 : 0)}%`;

function formatUptime(s: number): string {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`;
}

/** A usage meter whose fill turns amber, then red, as it nears the limit. */
function Meter({ percent, label }: { percent: number; label: string }) {
  const p = Math.min(Math.max(percent, 0), 100);
  const level = p >= 90 ? "critical" : p >= 75 ? "warning" : "ok";
  return (
    <div className="host-meter" role="meter" aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
      <span className={`host-meter-fill ${level}`} style={{ width: `${p}%` }} />
    </div>
  );
}

function Tile({ label, value, detail, percent }: { label: string; value: string; detail?: string; percent?: number }) {
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {percent !== undefined && <Meter percent={percent} label={label} />}
      {detail && <div className="stat-detail muted small">{detail}</div>}
    </div>
  );
}

export function HostPage() {
  // Which host's metrics: this one, or one with hlm's agent.
  const [selected, setSelected] = useState("local");
  const [hostList] = usePoll(api.hosts, 30000);
  const agentHosts = hostList?.hosts.filter((h) => h.kind === "agent") ?? [];
  const local = selected === "local";
  const selectedName = local ? "This host" : (agentHosts.find((h) => h.id === selected)?.name ?? selected);
  const [polled, hostError, reloadHost] = usePoll(async () => ({ id: selected, snap: await api.host(selected) }), 5000);
  // Not the last host's figures under the new one's name.
  const host = polled?.id === selected ? polled.snap : undefined;
  const [storage, storageError, reloadStorage] = usePoll(api.storage, 15000);

  return (
    <div className="page host-page">
      <div className="page-header">
        <h1>Host</h1>
        {agentHosts.length > 0 && (
          <select
            aria-label="Host"
            className="host-picker"
            value={selected}
            onChange={(e) => {
              setSelected(e.target.value);
              setTimeout(reloadHost);
            }}
          >
            <option value="local">This host</option>
            {agentHosts.map((h) => (
              <option key={h.id} value={h.id}>
                {h.name} ({h.id})
              </option>
            ))}
          </select>
        )}
        {host?.available && (
          <span className="muted small">
            {host.hostname} · Linux {host.kernel} · up {formatUptime(host.uptime)}
          </span>
        )}
      </div>

      <DockerHosts />
      {hostError && <div className="alert error">{hostError}</div>}
      {!host && !hostError && <Spinner />}
      {host && !host.available && (
        <div className="alert info">
          {local ? "Host" : `${selectedName}'s`} CPU, memory, network and disk metrics aren't available: {host.reason}.
          {local && " Disk usage per server is still shown below from Docker's figures."}
        </div>
      )}
      {host?.available && <HostMetrics key={selected} host={host} hostId={selected} />}

      {!local ? (
        <p className="muted small">
          Disk usage by server and deleted servers&apos; data are for this host only; choose{" "}
          <button type="button" className="link-button" onClick={() => setSelected("local")}>
            This host
          </button>{" "}
          to see them. Cleanup, below, works on every host.
        </p>
      ) : (
        <StorageSection
          report={storage}
          error={storageError}
          onRefresh={async () => {
            await api.refreshStorage();
            reloadStorage();
          }}
        />
      )}
      <DiskCleanup onCleaned={reloadStorage} />
      {local && storage?.deleted && storage.deleted.length > 0 && (
        <DeletedSection deleted={storage.deleted} onChanged={reloadStorage} />
      )}
    </div>
  );
}

type HostRange = "live" | HistoryRange;
const HOST_RANGES: { value: HostRange; label: string }[] = [
  { value: "live", label: "Live" },
  { value: "6h", label: "6 h" },
  { value: "24h", label: "24 h" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
];

function HostMetrics({ host, hostId }: { host: HostSnapshot; hostId: string }) {
  const [showTable, setShowTable] = useState(false);
  const [range, setRange] = useState<HostRange>("live");
  const history = host.history ?? [];
  const times = useMemo(() => history.map((p) => new Date(p.t).getTime()), [history]);
  const mem = host.memory;
  const memPct = mem.total ? (mem.used / mem.total) * 100 : 0;
  const net = host.network ?? [];
  const rx = net.reduce((a, i) => a + i.rxRate, 0);
  const tx = net.reduce((a, i) => a + i.txRate, 0);
  const root = host.filesystems?.find((f) => f.mount === "/") ?? host.filesystems?.[0];

  return (
    <>
      <div className="stats">
        <Tile
          label="CPU"
          value={pct(host.cpu.percent)}
          percent={host.cpu.percent}
          detail={`${host.cpu.cores} cores · load ${host.cpu.load1.toFixed(2)}, ${host.cpu.load5.toFixed(2)}, ${host.cpu.load15.toFixed(2)}`}
        />
        <Tile
          label="Memory"
          value={formatBytes(mem.used)}
          percent={memPct}
          detail={`of ${formatBytes(mem.total)} (${pct(memPct)})${mem.swapTotal ? ` · swap ${formatBytes(mem.swapUsed)} of ${formatBytes(mem.swapTotal)}` : ""}`}
        />
        <Tile label="Network" value={`↓ ${rate(rx)}`} detail={`↑ ${rate(tx)} · ${net.map((i) => i.name).join(", ") || "no interfaces"}`} />
        {root && (
          <Tile
            label={`Disk ${root.mount}`}
            value={formatBytes(root.used)}
            percent={(root.used / root.total) * 100}
            detail={`of ${formatBytes(root.total)} · ${formatBytes(root.free)} free`}
          />
        )}
      </div>

      <div className="chart-range">
        <span className="muted small">
          {range === "live"
            ? "Every 5 seconds, last 30 minutes"
            : "History kept by hlm: a day by the minute, 30 days by the quarter hour"}
        </span>
        <RangePicker value={range} onChange={setRange} options={HOST_RANGES} />
      </div>

      {range !== "live" ? (
        <HostHistory range={range} memTotal={mem.total} cores={host.cpu.cores} host={hostId} />
      ) : (
        <LiveCharts host={host} history={history} times={times} showTable={showTable} setShowTable={setShowTable} />
      )}
      <HostDetails host={host} />
    </>
  );
}

function LiveCharts({
  host,
  history,
  times,
  showTable,
  setShowTable,
}: {
  host: HostSnapshot;
  history: NonNullable<HostSnapshot["history"]>;
  times: number[];
  showTable: boolean;
  setShowTable: (f: (s: boolean) => boolean) => void;
}) {
  const mem = host.memory;
  return (
    <>
      <div className="chart-grid">
        <section className="card">
          <h2>CPU</h2>
          <p className="muted small chart-sub">Percent of all {host.cpu.cores} cores, last 30 minutes</p>
          <TimeSeriesChart
            label="CPU usage over time"
            times={times}
            max={100}
            format={(v) => `${Math.round(v)}%`}
            series={[{ key: "cpu", label: "CPU", color: "var(--series-1)", values: history.map((p) => p.cpu) }]}
          />
        </section>
        <section className="card">
          <h2>Memory</h2>
          <p className="muted small chart-sub">Used of {formatBytes(mem.total)}, last 30 minutes</p>
          <TimeSeriesChart
            label="Memory usage over time"
            times={times}
            max={mem.total}
            format={formatBytes}
            series={[{ key: "mem", label: "Memory used", color: "var(--series-1)", values: history.map((p) => p.memUsed) }]}
          />
        </section>
        <section className="card chart-wide">
          <h2>Network</h2>
          <p className="muted small chart-sub">All host interfaces, per second, last 30 minutes</p>
          <TimeSeriesChart
            label="Network traffic over time"
            times={times}
            format={rate}
            binary
            series={[
              { key: "rx", label: "Received", color: "var(--series-1)", values: history.map((p) => p.rx) },
              { key: "tx", label: "Sent", color: "var(--series-2)", values: history.map((p) => p.tx) },
            ]}
          />
        </section>
      </div>

      <button className="link-button table-toggle" onClick={() => setShowTable((s) => !s)}>
        {showTable ? "Hide" : "Show"} chart data as a table
      </button>
      {showTable && (
        <div className="card table-card">
          <div className="table-scroll"><table className="data-table">
            <thead>
              <tr>
                <th>Time</th>
                <th className="num">CPU</th>
                <th className="num">Memory</th>
                <th className="num">Received</th>
                <th className="num">Sent</th>
              </tr>
            </thead>
            <tbody>
              {[...history].reverse().slice(0, 60).map((p) => (
                <tr key={p.t}>
                  <td>{new Date(p.t).toLocaleTimeString()}</td>
                  <td className="num">{pct(p.cpu)}</td>
                  <td className="num">{formatBytes(p.memUsed)}</td>
                  <td className="num">{rate(p.rx)}</td>
                  <td className="num">{rate(p.tx)}</td>
                </tr>
              ))}
            </tbody>
          </table></div>
        </div>
      )}
    </>
  );
}

function HostDetails({ host }: { host: HostSnapshot }) {
  const net = host.network ?? [];
  return (
    <>
      <div className="chart-grid">
        <section className="card">
          <h2>Filesystems</h2>
          <div className="table-scroll"><table className="data-table">
            <thead>
              <tr>
                <th>Mount</th>
                <th>Usage</th>
                <th className="num">Used</th>
                <th className="num">Free</th>
              </tr>
            </thead>
            <tbody>
              {(host.filesystems ?? []).map((f) => (
                <tr key={f.device}>
                  <td>
                    <div>{f.mount}</div>
                    <div className="muted small">
                      {f.device} · {f.fsType}
                    </div>
                  </td>
                  <td className="meter-cell">
                    <Meter percent={(f.used / f.total) * 100} label={`${f.mount} usage`} />
                    <span className="muted small">{pct((f.used / f.total) * 100)}</span>
                  </td>
                  <td className="num">
                    {formatBytes(f.used)}
                    <div className="muted small">of {formatBytes(f.total)}</div>
                  </td>
                  <td className="num">{formatBytes(f.free)}</td>
                </tr>
              ))}
            </tbody>
          </table></div>
        </section>
        <section className="card">
          <h2>Network interfaces and disks</h2>
          <div className="table-scroll"><table className="data-table">
            <thead>
              <tr>
                <th>Interface</th>
                <th className="num">Receiving</th>
                <th className="num">Sending</th>
                <th className="num">Total in / out</th>
              </tr>
            </thead>
            <tbody>
              {net.map((i) => (
                <tr key={i.name}>
                  <td>{i.name}</td>
                  <td className="num">{rate(i.rxRate)}</td>
                  <td className="num">{rate(i.txRate)}</td>
                  <td className="num">
                    {formatBytes(i.rxTotal)} / {formatBytes(i.txTotal)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table></div>
          <div className="table-scroll"><table className="data-table">
            <thead>
              <tr>
                <th>Disk</th>
                <th className="num">Reading</th>
                <th className="num">Writing</th>
              </tr>
            </thead>
            <tbody>
              {(host.diskIO ?? []).map((d) => (
                <tr key={d.device}>
                  <td>{d.device}</td>
                  <td className="num">{rate(d.readRate)}</td>
                  <td className="num">{rate(d.writeRate)}</td>
                </tr>
              ))}
            </tbody>
          </table></div>
        </section>
      </div>
    </>
  );
}

const STORAGE_PARTS = [
  { key: "data", label: "Data folders", color: "var(--series-1)" },
  { key: "writable", label: "Container layer", color: "var(--series-2)" },
  { key: "image", label: "Image", color: "var(--series-3)" },
] as const;

function partValue(s: ServerStorage, key: (typeof STORAGE_PARTS)[number]["key"]): number {
  return key === "data" ? s.dataTotal : key === "writable" ? s.writable : s.imageSize;
}

function StorageSection({
  report,
  error,
  onRefresh,
}: {
  report?: StorageReport;
  error?: string;
  onRefresh: () => void;
}) {
  const [open, setOpen] = useState<string | null>(null);
  const servers = report?.servers ?? [];
  const scaleMax = Math.max(0, ...servers.map((s) => s.dataTotal + s.writable + s.imageSize));
  const fmt = (v: number) => (v < 0 ? "unknown" : formatBytes(v));

  return (
    <section className="storage">
      <div className="page-header">
        <h2>Disk usage by server</h2>
        <div className="btn-row">
          <span className="muted small">
            {report?.calculating
              ? "Calculating…"
              : report?.calculatedAt
                ? `Calculated ${timeAgo(report.calculatedAt)}`
                : "Not calculated yet"}
          </span>
          <button className="btn small" onClick={onRefresh} disabled={report?.calculating}>
            Recalculate
          </button>
        </div>
      </div>
      {error && <div className="alert error">{error}</div>}
      {report?.dockerError && <div className="alert error">{report.dockerError}</div>}
      {report?.dataReason && <div className="alert info">Data folder sizes are unavailable: {report.dataReason}.</div>}

      {report?.docker && (
        <div className="stats">
          <Tile label="Docker images" value={formatBytes(report.docker.images)} detail="all images on the host" />
          <Tile label="Container layers" value={formatBytes(report.docker.containers)} detail="files written inside containers" />
          <Tile label="Volumes" value={formatBytes(report.docker.volumes)} detail="named Docker volumes" />
          <Tile label="Build cache" value={formatBytes(report.docker.buildCache)} />
        </div>
      )}

      {!report && !error && <Spinner />}
      {report && servers.length === 0 && !report.calculating && <p className="muted">No servers yet.</p>}
      {servers.length > 0 && (
        <div className="card">
          <div className="legend" role="list">
            {STORAGE_PARTS.map((p) => (
              <span key={p.key} className="legend-item" role="listitem">
                <span className="legend-swatch" style={{ background: p.color }} />
                {p.label}
              </span>
            ))}
          </div>
          <div className="table-scroll"><table className="data-table storage-table">
            <thead>
              <tr>
                <th>Server</th>
                <th className="bar-col">Usage</th>
                <th className="num">Data</th>
                <th className="num">Container</th>
                <th className="num">Image</th>
                <th className="num">Total</th>
              </tr>
            </thead>
            <tbody>
              {servers.map((s) => {
                const total = s.dataTotal + s.writable + s.imageSize;
                const expanded = open === s.id;
                return (
                  <Fragment key={s.id}>
                    <tr>
                      <td>
                        <div className="storage-server">
                          <AppIcon src={s.icon} size={28} />
                          <div>
                            <Link to={`/servers/${encodeURIComponent(s.name)}`}>{s.name}</Link>
                            <div className="muted small">{s.app}</div>
                          </div>
                        </div>
                      </td>
                      <td className="bar-col">
                        <StackedBar
                          scaleMax={scaleMax}
                          format={formatBytes}
                          segments={STORAGE_PARTS.map((p) => ({ ...p, value: partValue(s, p.key) }))}
                        />
                      </td>
                      <td className="num">
                        {s.data.length > 0 ? (
                          <button className="link-button" onClick={() => setOpen(expanded ? null : s.id)}>
                            {formatBytes(s.dataTotal)} {expanded ? "▴" : "▾"}
                          </button>
                        ) : (
                          "-"
                        )}
                      </td>
                      <td className="num">{formatBytes(s.writable)}</td>
                      <td className="num" title={s.imageShared ? "Shared with other servers using the same image" : undefined}>
                        {formatBytes(s.imageSize)}
                        {s.imageShared && <div className="muted small">shared</div>}
                      </td>
                      <td className="num strong">{formatBytes(total)}</td>
                    </tr>
                    {expanded && (
                      <tr className="folder-row">
                        <td colSpan={6}>
                          <table className="folder-table">
                            <tbody>
                              {s.data.map((f) => (
                                <tr key={f.target}>
                                  <td>
                                    <code>{f.target}</code>
                                  </td>
                                  <td>
                                    <code className="muted">{f.volume ? `volume ${f.path}` : f.path}</code>
                                    {f.shared && <span className="badge">shared with other servers</span>}
                                    {f.within && <span className="badge">inside {f.within}, counted there</span>}
                                    {f.error && <div className="form-error small">{f.error}</div>}
                                  </td>
                                  <td className="num">{fmt(f.size)}</td>
                                </tr>
                              ))}
                            </tbody>
                          </table>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table></div>
        </div>
      )}
    </section>
  );
}

/**
 * Deleted servers whose folders and volumes are still on disk, with their
 * current sizes. Each can be deleted, or no longer tracked.
 */
function DeletedSection({ deleted, onChanged }: { deleted: DeletedStorage[]; onChanged: () => void }) {
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  const total = deleted.reduce((sum, d) => sum + d.dataTotal, 0);

  async function run(key: string, fn: () => Promise<unknown>) {
    setBusy(key);
    setError(undefined);
    try {
      await fn();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(undefined);
    }
  }

  const describe = (f: DataFolder) => (f.volume ? `the volume ${f.path}` : f.path);

  const remove = (d: DeletedStorage, f: DataFolder) => {
    if (!confirm(`Permanently delete ${describe(f)} (${f.size >= 0 ? formatBytes(f.size) : "size unknown"}), left over from ${d.name}? This can't be undone.`))
      return;
    run(`${d.id}:${f.path}`, () => api.deleteDeletedData(d.id, f.path, !!f.volume));
  };

  return (
    <section className="storage deleted-storage">
      <div className="page-header">
        <h2>Deleted servers</h2>
        <span className="muted small">{formatBytes(total)} of data left on disk</span>
      </div>
      <p className="muted small">
        Deleting a server keeps its data. These folders and volumes are what deleted servers left behind, measured with
        the rest of the disk usage. <strong>Delete</strong> removes one from disk for good; <strong>Stop tracking</strong>{" "}
        only removes it from this list.
      </p>
      {error && <div className="alert error">{error}</div>}
      <div className="card">
        {deleted.map((d) => (
          <div key={d.id} className="deleted-server">
            <div className="deleted-head">
              <div className="storage-server">
                <AppIcon src={d.icon} size={28} />
                <div>
                  <strong>{d.name}</strong>
                  <div className="muted small">
                    {d.app ? `${d.app} · ` : ""}deleted <Ago iso={d.deletedAt} />
                  </div>
                </div>
              </div>
              <div className="deleted-total">
                <span className="strong">{formatBytes(d.dataTotal)}</span>
                <button className="btn small ghost" onClick={() => run(`${d.id}:*`, () => api.forgetDeletedData(d.id))} disabled={!!busy}>
                  Stop tracking all
                </button>
              </div>
            </div>
            <div className="table-scroll">
              <table className="folder-table">
                <tbody>
                  {d.data.map((f) => {
                    const key = `${d.id}:${f.path}`;
                    const inUse = (f.inUseBy?.length ?? 0) > 0;
                    return (
                      <tr key={key}>
                        <td>
                          <code>{f.target}</code>
                        </td>
                        <td>
                          <code className="muted">{f.volume ? `volume ${f.path}` : f.path}</code>
                          {f.missing && <span className="badge">already gone</span>}
                          {inUse && <span className="badge">used by {f.inUseBy!.join(", ")}</span>}
                          {f.within && <span className="badge">inside {f.within}</span>}
                          {f.error && <div className="form-error small">{f.error}</div>}
                        </td>
                        <td className="num">{f.missing ? "-" : f.size >= 0 ? formatBytes(f.size) : "unknown"}</td>
                        <td className="folder-actions">
                          {!f.missing && (
                            <button
                              className="btn small danger"
                              onClick={() => remove(d, f)}
                              disabled={!!busy || inUse}
                              title={inUse ? "Another server uses it" : undefined}
                            >
                              {busy === key ? "Deleting…" : "Delete"}
                            </button>
                          )}
                          <button
                            className="btn small ghost"
                            onClick={() => run(key, () => api.forgetDeletedData(d.id, f.path, !!f.volume))}
                            disabled={!!busy}
                          >
                            Stop tracking
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}
