import { useState } from "react";
import { api } from "../api";
import type { HistoryRange, MetricHistory } from "../types";
import { usePoll } from "../usePoll";
import { formatBytes } from "../util";
import { TimeSeriesChart, type Series } from "./charts";

const rate = (v: number) => `${formatBytes(v)}/s`;
const pct = (v: number) => `${v.toFixed(v < 10 ? 1 : 0)}%`;

export const RANGE_LABEL: Record<HistoryRange, string> = {
  "1h": "1 hour",
  "6h": "6 hours",
  "24h": "24 hours",
  "7d": "7 days",
  "30d": "30 days",
};

/** Buttons choosing a range. */
export function RangePicker<T extends string>({
  value,
  options,
  onChange,
  label = "Time range",
}: {
  value: T;
  options: { value: T; label: string }[];
  onChange: (v: T) => void;
  label?: string;
}) {
  return (
    <div className="view-toggle" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.value}
          className={value === o.value ? "active" : ""}
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** Loads a history, again every minute (a step is at least a minute). */
function useHistory(load: (range: HistoryRange) => Promise<MetricHistory>, range: HistoryRange, key: string) {
  const [data, setData] = useState<{ key: string; h: MetricHistory }>();
  const [error, setError] = useState<string>();
  usePoll(
    async () => {
      try {
        setData({ key: `${key}/${range}`, h: await load(range) });
        setError(undefined);
      } catch (e) {
        setError((e as Error).message);
      }
    },
    60_000,
    [range, key],
  );
  // Don't show the last range's data under the new one's title.
  return {
    history: data?.key === `${key}/${range}` ? data.h : undefined,
    error,
  };
}

function avg(h: MetricHistory, name: string) {
  return h.series[name]?.avg ?? h.times.map(() => null);
}
function peak(h: MetricHistory, name: string) {
  return h.series[name]?.max ?? h.times.map(() => null);
}

function stepWords(h: MetricHistory) {
  return h.step >= 900 ? "15-minute" : "minute";
}

const EMPTY = "Nothing recorded in this range yet: history starts when hlm does, and fills in a minute at a time.";

/** The host's history charts, for the Host page. */
export function HostHistory({ range, memTotal, cores }: { range: HistoryRange; memTotal: number; cores: number }) {
  const { history: h, error } = useHistory(api.hostHistory, range, "host");
  const mounts = h
    ? Object.keys(h.series)
        .filter((k) => k.startsWith("fs:"))
        .map((k) => k.slice(3))
    : [];
  const [mount, setMount] = useState<string>();
  const shownMount = mount && mounts.includes(mount) ? mount : (mounts.find((m) => m === "/") ?? mounts[0]);

  if (error) return <div className="alert error small">{error}</div>;
  if (!h) return <div className="chart-grid muted small">Loading history…</div>;
  const times = h.times;
  const sub = `${stepWords(h)} averages, last ${RANGE_LABEL[range]}`;
  return (
    <div className="chart-grid">
      <section className="card">
        <h2>CPU</h2>
        <p className="muted small chart-sub">
          Percent of all {cores} cores: {sub}, and each step&apos;s peak
        </p>
        <TimeSeriesChart
          label="CPU usage over time"
          times={times}
          max={100}
          format={(v) => `${Math.round(v)}%`}
          empty={EMPTY}
          series={[
            {
              key: "avg",
              label: "Average",
              color: "var(--series-1)",
              values: avg(h, "cpu"),
            },
            {
              key: "max",
              label: "Peak",
              color: "var(--series-1)",
              values: peak(h, "cpu"),
              dashed: true,
            },
          ]}
        />
      </section>
      <section className="card">
        <h2>Memory</h2>
        <p className="muted small chart-sub">
          Used of {formatBytes(memTotal)}: {sub}
        </p>
        <TimeSeriesChart
          label="Memory usage over time"
          times={times}
          max={memTotal || undefined}
          binary
          format={formatBytes}
          empty={EMPTY}
          series={[
            {
              key: "mem",
              label: "Memory used",
              color: "var(--series-1)",
              values: avg(h, "mem"),
            },
          ]}
        />
      </section>
      <section className="card">
        <h2>Network</h2>
        <p className="muted small chart-sub">All host interfaces, per second: {sub}</p>
        <TimeSeriesChart
          label="Network traffic over time"
          times={times}
          format={rate}
          binary
          empty={EMPTY}
          series={[
            {
              key: "rx",
              label: "Received",
              color: "var(--series-1)",
              values: avg(h, "rx"),
            },
            {
              key: "tx",
              label: "Sent",
              color: "var(--series-2)",
              values: avg(h, "tx"),
            },
          ]}
        />
      </section>
      <section className="card">
        <h2>Disk activity</h2>
        <p className="muted small chart-sub">All disks, per second: {sub}</p>
        <TimeSeriesChart
          label="Disk reads and writes over time"
          times={times}
          format={rate}
          binary
          empty={EMPTY}
          series={[
            {
              key: "read",
              label: "Read",
              color: "var(--series-1)",
              values: avg(h, "diskRead"),
            },
            {
              key: "write",
              label: "Written",
              color: "var(--series-2)",
              values: avg(h, "diskWrite"),
            },
          ]}
        />
      </section>
      {shownMount && (
        <section className="card chart-wide">
          <div className="chart-head">
            <h2>Disk space</h2>
            {mounts.length > 1 && (
              <select value={shownMount} onChange={(e) => setMount(e.target.value)} aria-label="Filesystem">
                {mounts.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            )}
          </div>
          <p className="muted small chart-sub">
            Percent of <code>{shownMount}</code> used: {sub}
          </p>
          <TimeSeriesChart
            label={`Space used on ${shownMount} over time`}
            times={times}
            max={100}
            format={pct}
            empty={EMPTY}
            series={[
              {
                key: "fs",
                label: "Used",
                color: "var(--series-1)",
                values: avg(h, `fs:${shownMount}`),
              },
            ]}
          />
        </section>
      )}
    </div>
  );
}

const OPEN_KEY = "hlm.serverHistoryOpen";
const SERVER_RANGES: HistoryRange[] = ["1h", "6h", "24h", "7d", "30d"];

/** A server's resource history, on its overview. */
export function ServerHistory({ server, memLimit }: { server: string; memLimit?: number }) {
  const [range, setRange] = useState<HistoryRange>("24h");
  // Remembered, so it stays open for whoever uses it.
  const [open, setOpenState] = useState(() => {
    try {
      return localStorage.getItem(OPEN_KEY) === "1";
    } catch {
      return false;
    }
  });
  const setOpen = (v: boolean) => {
    setOpenState(v);
    try {
      localStorage.setItem(OPEN_KEY, v ? "1" : "0");
    } catch {
      // Not remembered.
    }
  };
  return (
    <section className="card server-history">
      <div className="chart-head">
        <h2>
          <button className="link-button" aria-expanded={open} onClick={() => setOpen(!open)}>
            {open ? "▾" : "▸"} Resource history
          </button>
        </h2>
        {open && (
          <RangePicker value={range} onChange={setRange} options={SERVER_RANGES.map((r) => ({ value: r, label: r }))} />
        )}
      </div>
      {open && <ServerHistoryCharts server={server} range={range} memLimit={memLimit} />}
    </section>
  );
}

function ServerHistoryCharts({ server, range, memLimit }: { server: string; range: HistoryRange; memLimit?: number }) {
  const { history: h, error } = useHistory((r) => api.serverHistory(server, r), range, server);
  if (error) return <div className="alert error small">{error}</div>;
  if (!h) return <p className="muted small">Loading history…</p>;
  const sub = `${stepWords(h)} averages, last ${RANGE_LABEL[range]}`;
  const charts: {
    title: string;
    sub: string;
    props: Omit<Parameters<typeof TimeSeriesChart>[0], "times" | "label" | "empty">;
  }[] = [
    {
      title: "CPU",
      sub: `100% is one full core: ${sub}, and each step's peak`,
      props: {
        format: (v) => `${Math.round(v)}%`,
        series: [
          {
            key: "avg",
            label: "Average",
            color: "var(--series-1)",
            values: avg(h, "cpu"),
          },
          {
            key: "max",
            label: "Peak",
            color: "var(--series-1)",
            values: peak(h, "cpu"),
            dashed: true,
          },
        ] as Series[],
      },
    },
    {
      title: "Memory",
      sub: memLimit ? `Used of ${formatBytes(memLimit)}: ${sub}` : `Used: ${sub}`,
      props: {
        format: formatBytes,
        binary: true,
        series: [
          {
            key: "mem",
            label: "Memory used",
            color: "var(--series-1)",
            values: avg(h, "mem"),
          },
        ],
      },
    },
    {
      title: "Network",
      sub: `Per second: ${sub}`,
      props: {
        format: rate,
        binary: true,
        series: [
          {
            key: "rx",
            label: "Received",
            color: "var(--series-1)",
            values: avg(h, "rx"),
          },
          {
            key: "tx",
            label: "Sent",
            color: "var(--series-2)",
            values: avg(h, "tx"),
          },
        ],
      },
    },
    {
      title: "Disk activity",
      sub: `Per second: ${sub}`,
      props: {
        format: rate,
        binary: true,
        series: [
          {
            key: "read",
            label: "Read",
            color: "var(--series-1)",
            values: avg(h, "diskRead"),
          },
          {
            key: "write",
            label: "Written",
            color: "var(--series-2)",
            values: avg(h, "diskWrite"),
          },
        ],
      },
    },
  ];
  if (h.series.players) {
    charts.push({
      title: "Players",
      sub: `Online, from the game's query: ${stepWords(h)} averages and peaks`,
      props: {
        format: (v) => (Number.isInteger(v) ? String(v) : v.toFixed(1)),
        series: [
          {
            key: "avg",
            label: "Average",
            color: "var(--series-3)",
            values: avg(h, "players"),
          },
          {
            key: "max",
            label: "Peak",
            color: "var(--series-3)",
            values: peak(h, "players"),
            dashed: true,
          },
        ],
      },
    });
  }
  return (
    <div className="history-grid">
      {charts.map((c) => (
        <div key={c.title} className="history-chart">
          <h3>{c.title}</h3>
          <p className="muted small chart-sub">{c.sub}</p>
          <TimeSeriesChart {...c.props} times={h.times} label={`${c.title} over time`} height={150} empty={EMPTY} />
        </div>
      ))}
    </div>
  );
}
