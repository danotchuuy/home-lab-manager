import { Fragment, useEffect, useLayoutEffect, useMemo, useRef, useState, type PointerEvent } from "react";

export interface Series {
  key: string;
  label: string;
  /** CSS color, normally a var(--series-n) token. */
  color: string;
  /** One per time; null where nothing was recorded (the line breaks there). */
  values: (number | null)[];
  /** Drawn dashed: a secondary reading of the same thing, such as the peak beside the average. */
  dashed?: boolean;
}

interface ChartProps {
  times: number[]; // epoch ms, one per value
  series: Series[];
  format: (v: number) => string;
  /** Fixed top of the y-axis (e.g. 100 for percentages); otherwise fit to the data. */
  max?: number;
  /** Round the fitted axis top in powers of 1024, for values shown in KiB/MiB/GiB. */
  binary?: boolean;
  height?: number;
  label: string;
  /** Shown instead of a chart with fewer than two points. */
  empty?: string;
}

const PAD = { top: 10, right: 12, bottom: 22, left: 64 };

/**
 * Rounds a maximum up to a "nice" axis top. With binary, values are rounded
 * in units of 1024^n first, so byte axes read 100 KiB/s rather than 97.7.
 */
function niceMax(v: number, binary = false): number {
  if (v <= 0) return 1;
  if (binary) {
    const unit = Math.pow(1024, Math.max(0, Math.floor(Math.log(v) / Math.log(1024))));
    return niceMax(v / unit) * unit;
  }
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (m * exp >= v) return m * exp;
  }
  return 10 * exp;
}

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(600);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver(([e]) => setWidth(e.contentRect.width));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width] as const;
}

const clock = (t: number) => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const day = (t: number) => new Date(t).toLocaleDateString([], { month: "short", day: "numeric" });

/** Time labels that suit the span: clock times within a day, dates beyond. */
function timeLabels(span: number) {
  const DAY = 86400e3;
  if (span <= DAY) return { axis: clock, tip: clock };
  const both = (t: number) => `${day(t)} ${clock(t)}`;
  return { axis: span > 3 * DAY ? day : both, tip: both };
}

/** The runs of consecutive recorded values, as [index, value] pairs. */
function runs(values: (number | null)[]): [number, number][][] {
  const out: [number, number][][] = [];
  let cur: [number, number][] = [];
  values.forEach((v, i) => {
    if (v === null || v === undefined || Number.isNaN(v)) {
      if (cur.length) out.push(cur);
      cur = [];
    } else cur.push([i, v]);
  });
  if (cur.length) out.push(cur);
  return out;
}

/**
 * A line chart over time with one shared y-axis, a crosshair that snaps to
 * the nearest sample, and a tooltip listing every series. A single series gets
 * a light area wash; two or more get a legend.
 */
export function TimeSeriesChart({ times, series, format, max, binary, height = 180, label, empty }: ChartProps) {
  const [wrapRef, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);

  const n = times.length;
  const innerW = Math.max(width - PAD.left - PAD.right, 10);
  const innerH = height - PAD.top - PAD.bottom;
  const top = useMemo(() => {
    if (max !== undefined) return max;
    const peak = Math.max(0, ...series.flatMap((s) => s.values.filter((v): v is number => v !== null)));
    return niceMax(peak * 1.1, binary);
  }, [series, max, binary]);

  const t0 = times[0] ?? 0;
  const t1 = times[n - 1] ?? 1;
  const x = (t: number) => PAD.left + (t1 > t0 ? ((t - t0) / (t1 - t0)) * innerW : innerW);
  const y = (v: number) => PAD.top + innerH - (Math.min(v, top) / top) * innerH;

  const ticks = [0, top / 2, top];
  const labels = timeLabels(t1 - t0);
  // Time labels at the start, middle and end.
  const xTicks = n > 1 ? [times[0], times[Math.floor((n - 1) / 2)], times[n - 1]] : [];

  function onMove(e: PointerEvent<SVGRectElement>) {
    if (n === 0) return;
    const rect = (e.currentTarget as SVGRectElement).getBoundingClientRect();
    const px = e.clientX - rect.left;
    const t = t0 + (px / rect.width) * (t1 - t0);
    let best = 0;
    for (let i = 1; i < n; i++) if (Math.abs(times[i] - t) < Math.abs(times[best] - t)) best = i;
    setHover(best);
  }

  const single = series.length === 1;
  const hasData = series.some((s) => s.values.some((v) => v !== null && v !== undefined));

  return (
    <figure className="viz chart" ref={wrapRef} aria-label={label}>
      {!single && (
        <div className="legend" role="list">
          {series.map((s) => (
            <span key={s.key} className="legend-item" role="listitem">
              <span className={`legend-line${s.dashed ? " dashed" : ""}`} style={{ background: s.color }} />
              {s.label}
            </span>
          ))}
        </div>
      )}
      {n < 2 || !hasData ? (
        <div className="chart-empty muted small" style={{ height }}>
          {empty ?? "Collecting data… the chart fills in over the next minute."}
        </div>
      ) : (
        <div className="chart-plot">
          <svg width={width} height={height} role="img" aria-label={label}>
            {ticks.map((v) => (
              <g key={v}>
                <line className="grid" x1={PAD.left} x2={PAD.left + innerW} y1={y(v)} y2={y(v)} />
                <text className="axis-label" x={PAD.left - 8} y={y(v)} dy="0.32em" textAnchor="end">
                  {/* Axis ticks are round numbers; drop a trailing ".0". */}
                  {format(v).replace(/\.0(?=\D|$)/, "")}
                </text>
              </g>
            ))}
            {xTicks.map((t, i) => (
              <text
                key={i}
                className="axis-label"
                x={x(t)}
                y={height - 6}
                textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}
              >
                {labels.axis(t)}
              </text>
            ))}
            {series.map((s) => (
              <g key={s.key}>
                {runs(s.values).map((run) => {
                  const pts = run.map(([i, v]) => `${x(times[i]).toFixed(1)},${y(v).toFixed(1)}`);
                  const first = run[0][0];
                  const last = run[run.length - 1][0];
                  return (
                    <Fragment key={first}>
                      {single && (
                        <path
                          className="area"
                          d={`M${x(times[first])},${y(0)} L${pts.join(" L")} L${x(times[last])},${y(0)} Z`}
                          style={{ fill: s.color }}
                        />
                      )}
                      {run.length === 1 ? (
                        <circle className="dot" cx={x(times[first])} cy={y(run[0][1])} r={2} style={{ fill: s.color }} />
                      ) : (
                        <polyline
                          className="line"
                          points={pts.join(" ")}
                          style={{ stroke: s.color, strokeDasharray: s.dashed ? "4 3" : undefined }}
                        />
                      )}
                    </Fragment>
                  );
                })}
              </g>
            ))}
            {hover !== null && (
              <g>
                <line className="crosshair" x1={x(times[hover])} x2={x(times[hover])} y1={PAD.top} y2={PAD.top + innerH} />
                {series.map((s) => {
                  const v = s.values[hover];
                  return v === null || v === undefined ? null : (
                    <circle key={s.key} className="dot" cx={x(times[hover])} cy={y(v)} r={4} style={{ fill: s.color }} />
                  );
                })}
              </g>
            )}
            <rect
              className="hit"
              x={PAD.left}
              y={PAD.top}
              width={innerW}
              height={innerH}
              onPointerMove={onMove}
              onPointerLeave={() => setHover(null)}
            />
          </svg>
          {hover !== null && (
            <div
              className="tooltip"
              style={{
                left: Math.min(x(times[hover]) + 12, width - 170),
                top: PAD.top,
              }}
            >
              <div className="tooltip-time">{labels.tip(times[hover])}</div>
              {series.map((s) => (
                <div key={s.key} className="tooltip-row">
                  <span className={`legend-line${s.dashed ? " dashed" : ""}`} style={{ background: s.color }} />
                  <strong>{s.values[hover] === null || s.values[hover] === undefined ? "—" : format(s.values[hover]!)}</strong>
                  <span className="muted">{s.label}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </figure>
  );
}

export interface Segment {
  key: string;
  label: string;
  color: string;
  value: number;
}

/**
 * A horizontal stacked bar on a shared scale (total = full width), with a 2px
 * surface gap between segments and a tooltip per segment.
 */
export function StackedBar({ segments, scaleMax, format }: { segments: Segment[]; scaleMax: number; format: (v: number) => string }) {
  const [hover, setHover] = useState<string | null>(null);
  const shown = segments.filter((s) => s.value > 0);
  const hovered = shown.find((s) => s.key === hover);
  return (
    <div className="viz stacked">
      <div className="stacked-track">
        {shown.map((s) => (
          <span
            key={s.key}
            className={`stacked-seg${hover === s.key ? " hovered" : ""}`}
            style={{ width: `${scaleMax > 0 ? (s.value / scaleMax) * 100 : 0}%`, background: s.color }}
            tabIndex={0}
            role="img"
            aria-label={`${s.label}: ${format(s.value)}`}
            onPointerEnter={() => setHover(s.key)}
            onPointerLeave={() => setHover(null)}
            onFocus={() => setHover(s.key)}
            onBlur={() => setHover(null)}
          />
        ))}
      </div>
      {hovered && (
        <div className="tooltip stacked-tooltip">
          <div className="tooltip-row">
            <span className="legend-swatch" style={{ background: hovered.color }} />
            <strong>{format(hovered.value)}</strong>
            <span className="muted">{hovered.label}</span>
          </div>
        </div>
      )}
    </div>
  );
}

/** Polls fn every interval ms while mounted; returns the latest result. */
export function usePoll<T>(fn: () => Promise<T>, interval: number): [T | undefined, string | undefined, () => void] {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const run = async () => {
      try {
        const d = await fnRef.current();
        if (!cancelled) {
          setData(d);
          setError(undefined);
        }
      } catch (e) {
        if (!cancelled) setError((e as Error).message);
      }
      if (!cancelled) timer = setTimeout(run, interval);
    };
    run();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [interval, tick]);
  return [data, error, () => setTick((t) => t + 1)];
}
