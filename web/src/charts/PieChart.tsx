// A donut chart on ECharts (query types, upstreams) with an HTML legend
// that doubles as the keyboard-accessible drill-down (§10 principle 3).
// ECharts is loaded on first use (charts/echarts.ts) and recoloured from
// the CSS palette when the theme changes.
import { useEffect, useRef } from 'react';
import { categorical, resolveCSSColor } from '../lib/colors';
import { formatExact, formatPercent } from '../lib/format';
import { Link } from '../lib/router';
import { useChartTheme } from './useChartTheme';

export interface PieItem {
  name: string;
  value: number;
  /** Drill-down route for the slice and its legend row. */
  href?: string;
}

export interface PieChartProps {
  label: string;
  items: PieItem[];
  onSliceClick?: (item: PieItem) => void;
  height?: number;
}

/** Whether this environment can paint on canvas (false under jsdom). */
export function canvasAvailable(): boolean {
  try {
    return typeof document !== 'undefined' && !!document.createElement('canvas').getContext?.('2d');
  } catch {
    return false;
  }
}

export function PieChart({ label, items, onSliceClick, height = 220 }: PieChartProps) {
  const host = useRef<HTMLDivElement>(null);
  const themeKey = useChartTheme();
  const click = useRef(onSliceClick);
  click.current = onSliceClick;
  const total = items.reduce((a, b) => a + b.value, 0);

  useEffect(() => {
    const el = host.current;
    if (!el || !canvasAvailable()) return undefined;
    let disposed = false;
    let chart: { dispose: () => void; resize: () => void } | undefined;
    let ro: ResizeObserver | undefined;
    void import('./echarts').then(({ echarts }) => {
      if (disposed) return;
      const palette = categorical(el);
      const text = resolveCSSColor('var(--color-text-primary)', el) || '#333';
      const surface = resolveCSSColor('var(--color-background-card)', el) || '#fff';
      const c = echarts.init(el, undefined, { renderer: 'canvas' });
      c.setOption({
        color: palette,
        animation: !window.matchMedia?.('(prefers-reduced-motion: reduce)').matches,
        tooltip: { trigger: 'item', formatter: '{b}: {c} ({d}%)' },
        series: [
          {
            type: 'pie',
            radius: ['48%', '78%'],
            avoidLabelOverlap: true,
            itemStyle: { borderColor: surface, borderWidth: 2 },
            label: { show: false, color: text },
            data: items.map((i) => ({ name: i.name, value: i.value })),
          },
        ],
      });
      c.on('click', (p: { name?: string }) => {
        const item = items.find((i) => i.name === p.name);
        if (item) click.current?.(item);
      });
      chart = c;
      ro = new ResizeObserver(() => c.resize());
      ro.observe(el);
    });
    return () => {
      disposed = true;
      ro?.disconnect();
      chart?.dispose();
    };
  }, [items, themeKey]);

  const palette = categorical();
  return (
    <div className="k-pie">
      <div ref={host} className="k-pie-canvas" role="img" aria-label={label} style={{ height }} />
      <ul className="k-legend" aria-label={`${label} legend`}>
        {items.map((i, idx) => (
          <li key={i.name}>
            <span className="k-swatch" style={{ background: palette[idx % palette.length] }} aria-hidden="true" />
            {i.href ? (
              <Link href={i.href} className="k-break">
                {i.name}
              </Link>
            ) : (
              <span className="k-break">{i.name}</span>
            )}
            <span className="k-spacer" />
            <span className="k-mono k-muted">{formatExact(i.value)}</span>
            <span className="k-mono k-legend-pct">{total ? formatPercent((i.value / total) * 100) : '–'}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
