// A donut chart on ECharts (query types, upstreams) with an HTML legend
// that doubles as the keyboard-accessible drill-down (§10 principle 3).
// ECharts is loaded on first use (charts/echarts.ts) and recolored from
// the CSS palette when the theme changes.
import { useEffect, useMemo, useRef } from 'react';
import { categorical, resolveCSSColor } from '../lib/colors';
import { formatExact, formatPercent } from '../lib/format';
import { Link } from '@astryxdesign/core/Link';
import { HStack, Stack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Square } from 'lucide-react';
import { useChartTheme } from './useChartTheme';

export interface PieItem {
  name: string;
  value: number;
  /** Drill-down route for the slice and its legend row. */
  href?: string;
  /** A CSS color for the slice; the next palette color when unset. */
  color?: string;
}

export interface PieChartProps {
  label: string;
  items: PieItem[];
  onSliceClick?: (item: PieItem) => void;
  height?: number;
  /** Most slices (and legend rows) shown; the smallest beyond it fold into one "Other" slice. */
  maxItems?: number;
}

/** As many slices as the categorical palette has colors, so no two share one. */
export const MAX_PIE_ITEMS = 10;

/**
 * Keeps the largest maxItems - 1 items and folds the rest into one
 * "Other (n)" item, so a long tail neither fills the panel nor repeats
 * the palette's colors.
 */
export function foldPieItems(items: PieItem[], maxItems = MAX_PIE_ITEMS): PieItem[] {
  if (items.length <= maxItems) return items;
  const sorted = [...items].sort((a, b) => b.value - a.value);
  const keep = Math.max(maxItems - 1, 1);
  const rest = sorted.slice(keep);
  return [
    ...sorted.slice(0, keep),
    { name: `Other (${rest.length})`, value: rest.reduce((a, b) => a + b.value, 0), color: 'var(--q-other)' },
  ];
}

/** Whether this environment can paint on canvas (false under jsdom). */
export function canvasAvailable(): boolean {
  try {
    return typeof document !== 'undefined' && !!document.createElement('canvas').getContext?.('2d');
  } catch {
    return false;
  }
}

export function PieChart({ label, items: all, onSliceClick, height = 220, maxItems = MAX_PIE_ITEMS }: PieChartProps) {
  const items = useMemo(() => foldPieItems(all, maxItems), [all, maxItems]);
  const host = useRef<HTMLElement>(null);
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
            data: items.map((i) => ({
              name: i.name,
              value: i.value,
              ...(i.color ? { itemStyle: { color: resolveCSSColor(i.color, el) || undefined } } : {}),
            })),
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
    <HStack gap={4} vAlign="center">
      {/* ECharts owns the DOM inside this mount node; it keeps its size while the legend shrinks. */}
      <Stack ref={host} role="img" aria-label={label} width={height} height={height} style={FIXED} />
      <StackItem size="fill">
        <VStack as="ul" gap={1.5} aria-label={`${label} legend`} style={LIST_RESET}>
          {items.map((i, idx) => (
            <HStack as="li" key={i.name} gap={2} vAlign="center">
              <Square size={10} fill={(i.color && resolveCSSColor(i.color)) || palette[idx % palette.length]} stroke="none" aria-hidden="true" style={FIXED} />
              <StackItem size="fill">
                <Text maxLines={2} wordBreak="break-all">
                  {i.href ? (
                    <Link href={i.href} color="inherit" hasUnderline>
                      {i.name}
                    </Link>
                  ) : (
                    i.name
                  )}
                </Text>
              </StackItem>
              <Text type="code" color="secondary" hasTabularNumbers textWrap="nowrap">
                {formatExact(i.value)}
              </Text>
              <Text type="code" hasTabularNumbers textWrap="nowrap">
                {total ? formatPercent((i.value / total) * 100) : '–'}
              </Text>
            </HStack>
          ))}
        </VStack>
      </StackItem>
    </HStack>
  );
}

/** No Stack prop stops a flex item from shrinking. */
const FIXED = { flex: 'none' } as const;
/** A legend drawn by Stacks: no markers or indent. */
const LIST_RESET = { listStyle: 'none', padding: 0, margin: 0 } as const;
