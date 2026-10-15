// The query log table (§10 principle 5): an Astryx Table in children mode,
// TanStack Virtual for rendering thousands of rows, an expandable "why"
// row per query and per-row Allow/Deny actions. Each virtual item is its
// own TableBody (row + optional detail row), measured as one unit, so
// expanding a row keeps scrolling smooth; spacer bodies stand in for the
// rows above and below the window. The Card around the Table is the
// virtualiser's scroll element, both ways; a plugin keeps the Table's own
// (sideways) scroll region at least as wide as the table, so it never
// becomes a scroll container and the sticky header cells stick to the Card.
//
// Keyboard: rows are focusable; Enter or Space toggles the "why" row,
// ArrowUp/ArrowDown move between rows.
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { HStack } from '@astryxdesign/core/HStack';
import { Icon } from '@astryxdesign/core/Icon';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { StackItem, VStack } from '@astryxdesign/core/Stack';
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useCallback, useEffect, useRef, useState, type CSSProperties, type KeyboardEvent, type ReactNode, type SVGProps } from 'react';
import type { QueryRow, StatusClass } from '../api/types';
import { queryIdentity } from '../lib/queryIdentity';
import { edeText } from '../lib/ede';
import { formatDateTime, formatMS, formatTime } from '../lib/format';
import { objectHref } from '../lib/policies';

/** Where a blocked row's unblock link goes (see QueryLogPage's unblockLink). */
export interface UnblockLink {
  href: string;
  label: string;
  /** The sentence after the link. */
  note: string;
}

export interface QueryLogTableProps {
  rows: QueryRow[];
  /** Ids that arrived over the live tail and get a short highlight. */
  fresh?: ReadonlySet<string>;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  onAction?: (row: QueryRow, action: 'allow' | 'deny') => void;
  canAct: boolean;
  /** The "Requests" / "Request unblock" link of a blocked row's "why" row. */
  unblockLink?: (row: QueryRow) => UnblockLink | null;
  /** Pixel rect used before the scroll container is measured (tests, SSR). */
  initialRect?: { width: number; height: number };
  /** What to show before a live stream has delivered any matching rows. */
  emptyMessage?: string | undefined;
}

/** The status label shown in the table: "blocked (denylist)" style. */
export function statusLabel(r: QueryRow): string {
  return r.status.replace(/_/g, ' ');
}

/** The query class colour (theme tokens --q-*). */
const tone = (c: StatusClass): CSSProperties => ({ color: `var(--q-${c})` });

function Dot(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 16 16" {...props}>
      <circle cx="8" cy="8" r="5" fill="currentColor" />
    </svg>
  );
}

function StatusText({ r, children }: { r: QueryRow; children?: ReactNode }) {
  return (
    <Text style={tone(r.status_class)} textWrap="nowrap">
      <HStack as="span" gap={1} vAlign="center">
        <Icon icon={Dot} size="xsm" />
        {statusLabel(r)}
        {children}
      </HStack>
    </Text>
  );
}

interface Column {
  id: string;
  header: string;
  /** Structural width in px; the domain column takes the rest. */
  width?: number;
  cell: (r: QueryRow) => ReactNode;
}

const COLUMNS: Column[] = [
  {
    id: 'ts',
    header: 'Time',
    width: 104,
    cell: (r) => (
      <Tooltip content={formatDateTime(r.ts)} hasHoverIndication={false}>
        <Text>
          <time dateTime={r.ts}>{formatTime(r.ts)}</time>
        </Text>
      </Tooltip>
    ),
  },
  { id: 'status', header: 'Status', width: 140, cell: (r) => <StatusText r={r} /> },
  { id: 'qtype', header: 'Type', width: 64, cell: (r) => r.qtype },
  {
    id: 'domain',
    header: 'Domain',
    cell: (r) => (
      <Text type="code" maxLines={1}>
        {r.domain}
      </Text>
    ),
  },
  {
    id: 'client',
    header: 'Client',
    width: 150,
    cell: (r) =>
      r.client.name ? (
        <Tooltip content={r.client.ip} hasHoverIndication={false}>
          <Text maxLines={1} hasTruncateTooltip={false}>
            {r.client.name}
          </Text>
        </Tooltip>
      ) : (
        <Text maxLines={1}>{r.client.ip}</Text>
      ),
  },
  {
    id: 'reply',
    header: 'Reply',
    width: 110,
    cell: (r) => (
      <Text textWrap="nowrap">
        {r.reply.type} <Text color="secondary">{formatMS(r.reply.time_ms)}</Text>
      </Text>
    ),
  },
  { id: 'node', header: 'Node', width: 96, cell: (r) => <Text maxLines={1}>{r.node}</Text> },
];

const ACTIONS_WIDTH = 84;
const NCOLS = COLUMNS.length + 1;
/** Below this the table scrolls sideways (structural, px). */
const TABLE_MIN_WIDTH = 940;
const ROW_HEIGHT = 34;
/** The scroll region's height: most of the viewport, capped on tall screens. */
const SCROLL_HEIGHT = 'min(70vh, 900px)';

const headerCell = (width: number | undefined): CSSProperties => ({
  position: 'sticky',
  insetBlockStart: 0,
  zIndex: 1,
  background: 'var(--color-background-card)',
  // Truncating cells carry max-inline-size: 0; a fixed column needs its own.
  ...(width ? { inlineSize: width, maxInlineSize: width } : {}),
});

/**
 * The Table's sideways scroll region as wide as the table: it stays
 * overflow: clip (no scroll container), so the Card scrolls both ways and
 * the sticky header cells stick to it.
 */
const wideRegion: TablePlugin<Record<string, unknown>> = {
  transformScrollWrapper: (p) => ({ ...p, htmlProps: { ...p.htmlProps, style: { ...p.htmlProps.style, minInlineSize: TABLE_MIN_WIDTH } } }),
};
const PLUGINS = { wideRegion };

export function QueryLogTable({ rows, fresh, hasMore, loadingMore, onLoadMore, onAction, canAct, unblockLink, initialRect, emptyMessage = 'No queries match these filters.' }: QueryLogTableProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
    getItemKey: (i) => rows[i] ? queryIdentity(rows[i]) : i,
    ...(initialRect ? { initialRect } : {}),
  });
  const items = virtualizer.getVirtualItems();

  // Fetch the next page when the last rendered row is close to the end.
  const last = items[items.length - 1];
  useEffect(() => {
    if (last && last.index >= rows.length - 20 && hasMore && !loadingMore) onLoadMore();
  }, [last, rows.length, hasMore, loadingMore, onLoadMore]);

  const toggle = useCallback((id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const onRowKey = (e: KeyboardEvent<HTMLTableRowElement>, id: string, index: number) => {
    if (e.target !== e.currentTarget) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggle(id);
    } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const to = index + (e.key === 'ArrowDown' ? 1 : -1);
      if (to < 0 || to >= rows.length) return;
      virtualizer.scrollToIndex(to);
      window.requestAnimationFrame(() => {
        scrollRef.current?.querySelector<HTMLTableRowElement>(`tr[data-row-index="${to}"]`)?.focus();
      });
    }
  };

  const total = virtualizer.getTotalSize();
  const padTop = items[0]?.start ?? 0;
  const padBottom = last ? total - last.end : 0;

  return (
    <VStack gap={2}>
      <Card padding={0} ref={scrollRef} tabIndex={-1} data-testid="query-log-scroll" style={{ blockSize: SCROLL_HEIGHT, overflow: 'auto' }}>
        <Table<Record<string, unknown>>
          aria-label="Query log"
          aria-rowcount={rows.length}
          density="compact"
          hasHover
          textOverflow="truncate"
          plugins={PLUGINS}
          style={{ tableLayout: 'fixed', inlineSize: '100%', minInlineSize: TABLE_MIN_WIDTH }}
        >
          <TableHeader>
            <TableRow isHeaderRow>
              {COLUMNS.map((c) => (
                <TableHeaderCell key={c.id} scope="col" style={headerCell(c.width)}>
                  {c.header}
                </TableHeaderCell>
              ))}
              <TableHeaderCell scope="col" style={headerCell(ACTIONS_WIDTH)}>
                <VisuallyHidden>Actions</VisuallyHidden>
              </TableHeaderCell>
            </TableRow>
          </TableHeader>
          {padTop > 0 ? <Spacer height={padTop} /> : null}
          {items.map((vi) => {
            const r = rows[vi.index];
            if (!r) return null;
            const open = expanded.has(queryIdentity(r));
            return (
              <TableBody key={vi.key} data-index={vi.index} ref={virtualizer.measureElement}>
                <LogRow r={r} index={vi.index} open={open} isNew={!!fresh?.has(queryIdentity(r))} onToggle={toggle} onKeyDown={onRowKey}>
                  {COLUMNS.map((c) => (
                    <TableCell key={c.id}>{c.cell(r)}</TableCell>
                  ))}
                  <TableCell>
                    {onAction ? (
                      r.status_class === 'blocked' ? (
                        <Button
                          label="Allow"
                          size="sm"
                          variant="secondary"
                          isDisabled={!canAct}
                          tooltip={canAct ? `Add ${r.domain} to the allow list` : 'Needs blocklists.write'}
                          onClick={() => onAction(r, 'allow')}
                        />
                      ) : (
                        <Button
                          label="Deny"
                          size="sm"
                          variant="ghost"
                          isDisabled={!canAct}
                          tooltip={canAct ? `Add ${r.domain} to the deny list` : 'Needs blocklists.write'}
                          onClick={() => onAction(r, 'deny')}
                        />
                      )
                    ) : null}
                  </TableCell>
                </LogRow>
                {open ? (
                  <TableRow id={`why-${queryIdentity(r)}`}>
                    <TableCell colSpan={NCOLS} style={{ whiteSpace: 'normal', background: 'var(--color-overlay-hover)' }}>
                      <WhyDetails row={r} unblock={r.status_class === 'blocked' ? (unblockLink?.(r) ?? null) : null} />
                    </TableCell>
                  </TableRow>
                ) : null}
              </TableBody>
            );
          })}
          {padBottom > 0 ? <Spacer height={padBottom} /> : null}
          {rows.length === 0 ? (
            <TableBody>
              <TableRow>
                <TableCell colSpan={NCOLS}>
                  <Text color="secondary">{emptyMessage}</Text>
                </TableCell>
              </TableRow>
            </TableBody>
          ) : null}
        </Table>
      </Card>
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Text type="supporting" role="status" aria-live="polite" data-testid="log-count">
          Showing {rows.length.toLocaleString()} {rows.length === 1 ? 'query' : 'queries'}
          {hasMore ? '; more available' : ''}
        </Text>
        <StackItem size="fill" />
        {hasMore ? <Button label="Load more" size="sm" variant="secondary" isLoading={loadingMore} onClick={onLoadMore} /> : null}
      </HStack>
    </VStack>
  );
}

/** The rows above or below the rendered window. */
function Spacer({ height }: { height: number }) {
  return (
    <TableBody aria-hidden="true">
      <TableRow>
        <TableCell colSpan={NCOLS} style={{ blockSize: height, padding: 0, border: 0 }} />
      </TableRow>
    </TableBody>
  );
}

const FLASH: Keyframe[] = [{ backgroundColor: 'var(--color-accent-muted)' }, { backgroundColor: 'transparent' }];

interface LogRowProps {
  r: QueryRow;
  index: number;
  open: boolean;
  /** Arrived over the live tail: flashes once. */
  isNew: boolean;
  onToggle: (id: string) => void;
  onKeyDown: (e: KeyboardEvent<HTMLTableRowElement>, id: string, index: number) => void;
  children: ReactNode;
}

/**
 * One query: focusable, toggles its "why" row. The live-tail highlight is
 * a Web Animation (no stylesheet); k-log-row only draws the keyboard focus
 * ring, which no Table prop or token covers for a focusable row.
 */
function LogRow({ r, index, open, isNew, onToggle, onKeyDown, children }: LogRowProps) {
  const ref = useRef<HTMLTableRowElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!isNew || !el || typeof el.animate !== 'function') return;
    if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
    el.animate(FLASH, { duration: 1200, easing: 'ease-out' });
  }, [isNew]);
  return (
    <TableRow
      ref={ref}
      className="k-log-row"
      tabIndex={0}
      data-row-index={index}
      aria-rowindex={index + 1}
      aria-expanded={open}
      aria-controls={open ? `why-${queryIdentity(r)}` : undefined}
      style={{ cursor: 'pointer', ...(open ? { background: 'var(--color-overlay-pressed)' } : {}) }}
      onClick={(e) => {
        if ((e.target as HTMLElement).closest('button, a')) return;
        onToggle(queryIdentity(r));
      }}
      onKeyDown={(e) => onKeyDown(e, queryIdentity(r), index)}
    >
      {children}
    </TableRow>
  );
}

const none = <Text color="secondary">none</Text>;

/** The expandable explanation of one answer: how and why it was produced. */
export function WhyDetails({ row: r, unblock }: { row: QueryRow; unblock?: UnblockLink | null }) {
  const cache = r.flags.Stale ? 'stale answer served from cache' : r.flags.Cached ? 'answered from cache' : r.flags.Prefetch ? 'prefetched' : 'not cached';
  const items: [string, ReactNode][] = [
    ['Status', <StatusText r={r}> ({r.rcode})</StatusText>],
    [
      'Matched list',
      r.list ? (
        <Link href={`/lists?q=${encodeURIComponent(r.list.split('/').pop() ?? r.list)}`} color="inherit" hasUnderline>
          {r.list}
        </Link>
      ) : (
        none
      ),
    ],
    ['Rule', r.rule ? <Text type="code">{r.rule}</Text> : none],
    [
      'Policy',
      r.policy ? (
        <Link href={objectHref('policies', r.policy)} color="inherit" hasUnderline>
          {r.policy}
        </Link>
      ) : (
        none
      ),
    ],
    ['CNAME chain', r.cname ? <Text type="code">{`${r.domain} → ${r.cname}`}</Text> : none],
    ['Upstream', r.upstream ?? (r.zone ? `zone ${r.zone}` : none)],
    ['Cache', cache],
    ['DNSSEC', r.dnssec || '–'],
    ['Extended DNS error', edeText(r.ede)],
    ['Reply', `${r.reply.type}${r.reply.time_ms !== undefined ? `, ${formatMS(r.reply.time_ms)}` : ''}${r.reply.answer ? `: ${r.reply.answer}` : ''}`],
    ['Node', r.node],
    ['Client', `${r.client.name ? `${r.client.name} ` : ''}${r.client.ip}${r.client.group ? ` (group ${r.client.group})` : ''}`],
    ['Protocol', `${r.proto}${r.ecs ? `, ECS ${r.ecs}` : ''}`],
    ['Time', formatDateTime(r.ts)],
  ];
  if (r.trace_id) items.push(['Trace', <Text type="code">{r.trace_id}</Text>]);
  if (unblock) {
    items.push([
      'Unblock',
      <Text>
        <Link href={unblock.href} hasUnderline data-testid="why-unblock">
          {unblock.label}
        </Link>{' '}
        <Text type="supporting">{unblock.note}</Text>
      </Text>,
    ]);
  }
  return (
    // MetadataList forwards no ARIA props, so a group names the explanation.
    <VStack role="group" aria-label={`Why ${r.domain} got this answer`} paddingBlock={1}>
      <MetadataList columns="multi">
        {items.map(([k, v]) => (
          <MetadataListItem key={k} label={k}>
            {v}
          </MetadataListItem>
        ))}
      </MetadataList>
    </VStack>
  );
}
