// Stacked time-series bars on uPlot (§10 principle 2): the 24 h
// total/blocked/cached/forwarded/authoritative/other chart and the
// per-client chart. uPlot has no stacking, so each series is drawn at its
// cumulative height from the top of the stack down; the front bars hide
// the lower part of the ones behind. Legend and tooltip values show the
// raw (non-cumulative) counts: the hovered bucket's, or the totals over the
// whole range while the pointer is elsewhere.
//
// Clicking a bucket calls onBucketClick with the bucket's start time; a
// keyboard-reachable data table under the chart offers the same drill-down.
import { useEffect, useRef, useState } from 'react';
import uPlot from 'uplot';
import { axisColors, resolveCSSColor, withAlpha } from '../lib/colors';
import { formatExact, formatTime } from '../lib/format';
import { useChartTheme } from './useChartTheme';

export interface StackSeries {
  key: string;
  label: string;
  /** A CSS colour expression, e.g. "var(--q-blocked)". */
  color: string;
  values: number[];
}

export interface StackedBarsProps {
  /** Bucket start times in seconds since the epoch. */
  x: number[];
  /** Series from the bottom of the stack to the top. */
  series: StackSeries[];
  /** Bucket width in seconds. */
  step: number;
  height?: number;
  label: string;
  onBucketClick?: (tsSeconds: number) => void;
  /** Rendered as the drill-down link of each table row. */
  bucketHref?: (tsSeconds: number) => string;
}

/** Cumulative sums from the bottom series up: out[i][j] = Σ_{k≤i} series[k][j]. */
export function stack(series: number[][]): number[][] {
  const out: number[][] = [];
  series.forEach((vals, i) => {
    const prev = out[i - 1];
    out.push(vals.map((v, j) => v + (prev ? (prev[j] as number) : 0)));
  });
  return out;
}

export function StackedBars({ x, series, step, height = 240, label, onBucketClick, bucketHref }: StackedBarsProps) {
  const host = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const [width, setWidth] = useState(0);
  const themeKey = useChartTheme();
  const click = useRef(onBucketClick);
  click.current = onBucketClick;

  useEffect(() => {
    const el = host.current;
    if (!el) return undefined;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver((entries) => {
      const w = Math.floor(entries[0]?.contentRect.width ?? el.clientWidth);
      setWidth(w);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    const el = host.current;
    if (!el || width <= 0 || typeof HTMLCanvasElement === 'undefined') return undefined;
    const ctx = document.createElement('canvas').getContext?.('2d');
    if (!ctx) return undefined; // no canvas (tests): the table below still renders
    const cum = stack(series.map((s) => s.values));
    const { text, grid } = axisColors(el);
    const bars = uPlot.paths.bars?.({ size: [0.9, 40], gap: 1, align: 1 });
    // Top of the stack first so lower series paint over it.
    const order = series.map((_, i) => i).reverse();
    const opts: uPlot.Options = {
      width,
      height,
      padding: [8, 8, 0, 0],
      cursor: { drag: { x: false, y: false }, points: { show: false } },
      legend: { show: true, live: true },
      scales: { x: { time: true }, y: { range: (_u, _min, max) => [0, Math.max(1, max * 1.05)] } },
      axes: [
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid } },
        { stroke: text, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, size: 56 },
      ],
      series: [
        { label: 'Time', value: (_u, v) => (v == null ? 'total' : formatTime(v * 1000)) },
        ...order.map((i) => {
          const s = series[i] as StackSeries;
          const color = resolveCSSColor(s.color, el) || '#888';
          const total = s.values.reduce((a, b) => a + b, 0);
          return {
            label: s.label,
            stroke: color,
            fill: withAlpha(color, 0.85),
            width: 0,
            paths: bars,
            points: { show: false },
            value: (_u: uPlot, _v: number | null, _si: number, idx: number | null) =>
              formatExact(idx == null ? total : s.values[idx]),
          } satisfies uPlot.Series;
        }),
      ],
      hooks: {
        ready: [
          (u) => {
            u.over.addEventListener('click', () => {
              const idx = u.cursor.idx;
              if (idx == null || !click.current) return;
              const ts = u.data[0][idx];
              if (ts != null) click.current(ts);
            });
          },
        ],
      },
    };
    const data: uPlot.AlignedData = [x, ...order.map((i) => cum[i] as number[])];
    const u = new uPlot(opts, data, el);
    plot.current = u;
    return () => {
      u.destroy();
      plot.current = null;
    };
    // Rebuild on data, size and theme changes.
  }, [x, series, width, height, themeKey]);

  return (
    <div className="k-stack">
      <div ref={host} className="k-chart" role="img" aria-label={label} style={{ minHeight: height }} />
      <details className="k-chart-table">
        <summary>Data table</summary>
        <div className="k-table-wrap">
          <table className="k-table">
            <caption className="k-visually-hidden">{label}</caption>
            <thead>
              <tr>
                <th scope="col">Bucket ({Math.round(step / 60)} min)</th>
                {series.map((s) => (
                  <th key={s.key} scope="col" className="num">
                    {s.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {x.map((ts, j) => (
                <tr key={ts}>
                  <td>{bucketHref ? <a href={bucketHref(ts)} onClick={(e) => {
                    if (!onBucketClick) return;
                    e.preventDefault();
                    onBucketClick(ts);
                  }}>{formatTime(ts * 1000)}</a> : formatTime(ts * 1000)}</td>
                  {series.map((s) => (
                    <td key={s.key} className="num">
                      {formatExact(s.values[j])}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </div>
  );
}
