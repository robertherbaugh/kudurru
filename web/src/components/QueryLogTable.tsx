// The query log table (§10 principle 5): TanStack Table for the column
// model, TanStack Virtual for rendering thousands of rows, an expandable
// "why" row per query and per-row Allow/Deny actions. Each virtual item is
// its own <tbody> (row + optional detail row), measured as one unit, so
// expanding a row keeps scrolling smooth.
//
// Keyboard: rows are focusable; Enter or Space toggles the "why" row,
// ArrowUp/ArrowDown move between rows.
import { Button } from '@astryxdesign/core/Button';
import { createColumnHelper, flexRender, getCoreRowModel, useReactTable } from '@tanstack/react-table';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import type { QueryRow } from '../api/types';
import { edeText } from '../lib/ede';
import { formatDateTime, formatMS, formatTime } from '../lib/format';
import { Link } from '../lib/router';

export interface QueryLogTableProps {
  rows: QueryRow[];
  /** Ids that arrived over the live tail and get a short highlight. */
  fresh?: ReadonlySet<number>;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  onAction?: (row: QueryRow, action: 'allow' | 'deny') => void;
  canAct: boolean;
  /** Pixel rect used before the scroll container is measured (tests, SSR). */
  initialRect?: { width: number; height: number };
}

const col = createColumnHelper<QueryRow>();

/** The status label shown in the table: "blocked (denylist)" style. */
export function statusLabel(r: QueryRow): string {
  return r.status.replace(/_/g, ' ');
}

const columns = [
  col.accessor('ts', {
    header: 'Time',
    cell: (c) => (
      <time dateTime={c.getValue()} title={formatDateTime(c.getValue())}>
        {formatTime(c.getValue())}
      </time>
    ),
    size: 92,
  }),
  col.accessor('status_class', {
    header: 'Status',
    cell: (c) => (
      <span className={`q-${c.getValue()}`}>
        <span className="q-dot" aria-hidden="true" />
        {statusLabel(c.row.original)}
      </span>
    ),
    size: 150,
  }),
  col.accessor('qtype', { header: 'Type', size: 64 }),
  col.accessor('domain', {
    header: 'Domain',
    cell: (c) => <span className="k-mono" title={c.getValue()}>{c.getValue()}</span>,
    size: 320,
  }),
  col.accessor((r) => r.client.name ?? r.client.ip, {
    id: 'client',
    header: 'Client',
    cell: (c) => <span title={c.row.original.client.ip}>{c.getValue()}</span>,
    size: 160,
  }),
  col.accessor((r) => r.reply.time_ms, {
    id: 'reply',
    header: 'Reply',
    cell: (c) => (
      <span className="k-nowrap">
        {c.row.original.reply.type} <span className="k-muted">{formatMS(c.getValue())}</span>
      </span>
    ),
    size: 120,
  }),
  col.accessor('node', { header: 'Node', size: 96 }),
];

const ROW_HEIGHT = 34;

export function QueryLogTable({ rows, fresh, hasMore, loadingMore, onLoadMore, onAction, canAct, initialRect }: QueryLogTableProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(new Set());
  const table = useReactTable({ data: rows, columns, getCoreRowModel: getCoreRowModel(), getRowId: (r) => String(r.id) });
  const model = table.getRowModel().rows;

  const virtualizer = useVirtualizer({
    count: model.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
    getItemKey: (i) => model[i]?.id ?? i,
    ...(initialRect ? { initialRect } : {}),
  });
  const items = virtualizer.getVirtualItems();

  // Fetch the next page when the last rendered row is close to the end.
  const last = items[items.length - 1];
  useEffect(() => {
    if (last && last.index >= model.length - 20 && hasMore && !loadingMore) onLoadMore();
  }, [last, model.length, hasMore, loadingMore, onLoadMore]);

  const toggle = useCallback((id: number) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const onRowKey = (e: KeyboardEvent<HTMLTableRowElement>, id: number, index: number) => {
    if (e.target !== e.currentTarget) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggle(id);
    } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const to = index + (e.key === 'ArrowDown' ? 1 : -1);
      if (to < 0 || to >= model.length) return;
      virtualizer.scrollToIndex(to);
      window.requestAnimationFrame(() => {
        scrollRef.current?.querySelector<HTMLTableRowElement>(`tr[data-row-index="${to}"]`)?.focus();
      });
    }
  };

  const ncols = columns.length + 1;
  const total = virtualizer.getTotalSize();
  const padTop = items[0]?.start ?? 0;
  const padBottom = last ? total - last.end : 0;

  return (
    <div className="k-stack">
      <div ref={scrollRef} className="k-log" tabIndex={-1} data-testid="query-log-scroll">
        <table aria-rowcount={rows.length} aria-label="Query log">
          <colgroup>
            {table.getFlatHeaders().map((h) => (
              <col key={h.id} style={{ width: h.getSize() }} />
            ))}
            <col style={{ width: 150 }} />
          </colgroup>
          <thead>
            {table.getHeaderGroups().map((g) => (
              <tr key={g.id}>
                {g.headers.map((h) => (
                  <th key={h.id} scope="col">
                    {flexRender(h.column.columnDef.header, h.getContext())}
                  </th>
                ))}
                <th scope="col">
                  <span className="k-visually-hidden">Actions</span>
                </th>
              </tr>
            ))}
          </thead>
          {padTop > 0 ? (
            <tbody aria-hidden="true">
              <tr>
                <td colSpan={ncols} style={{ height: padTop, padding: 0, border: 0 }} />
              </tr>
            </tbody>
          ) : null}
          {items.map((vi) => {
            const row = model[vi.index];
            if (!row) return null;
            const r = row.original;
            const open = expanded.has(r.id);
            const whyId = `why-${r.id}`;
            return (
              <tbody key={vi.key} data-index={vi.index} ref={virtualizer.measureElement}>
                <tr
                  className={`k-log-row${fresh?.has(r.id) ? ' k-new' : ''}`}
                  tabIndex={0}
                  data-row-index={vi.index}
                  aria-rowindex={vi.index + 1}
                  aria-expanded={open}
                  aria-controls={open ? whyId : undefined}
                  onClick={(e) => {
                    if ((e.target as HTMLElement).closest('button, a')) return;
                    toggle(r.id);
                  }}
                  onKeyDown={(e) => onRowKey(e, r.id, vi.index)}
                >
                  {row.getVisibleCells().map((c) => (
                    <td key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</td>
                  ))}
                  <td className="k-row-actions">
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
                  </td>
                </tr>
                {open ? (
                  <tr id={whyId}>
                    <td className="k-why" colSpan={ncols}>
                      <WhyDetails row={r} />
                    </td>
                  </tr>
                ) : null}
              </tbody>
            );
          })}
          {padBottom > 0 ? (
            <tbody aria-hidden="true">
              <tr>
                <td colSpan={ncols} style={{ height: padBottom, padding: 0, border: 0 }} />
              </tr>
            </tbody>
          ) : null}
        </table>
        {rows.length === 0 ? <p className="k-log-empty k-muted">No queries match these filters.</p> : null}
      </div>
      <div className="k-row">
        <span className="k-muted k-small" role="status" aria-live="polite" data-testid="log-count">
          Showing {rows.length.toLocaleString()} {rows.length === 1 ? 'query' : 'queries'}
          {hasMore ? '; more available' : ''}
        </span>
        <span className="k-spacer" />
        {hasMore ? <Button label="Load more" size="sm" variant="secondary" isLoading={loadingMore} onClick={onLoadMore} /> : null}
      </div>
    </div>
  );
}

/** The expandable explanation of one answer: how and why it was produced. */
export function WhyDetails({ row: r }: { row: QueryRow }) {
  const cache = r.flags.Stale ? 'stale answer served from cache' : r.flags.Cached ? 'answered from cache' : r.flags.Prefetch ? 'prefetched' : 'not cached';
  const items: [string, ReactNode][] = [
    ['Status', <span className={`q-${r.status_class}`}>{statusLabel(r)} ({r.rcode})</span>],
    [
      'Matched list',
      r.list ? (
        <Link href={`/lists?q=${encodeURIComponent(r.list)}`}>{r.list}</Link>
      ) : (
        <span className="k-muted">none</span>
      ),
    ],
    ['Rule', r.rule ? <code className="k-mono">{r.rule}</code> : <span className="k-muted">none</span>],
    ['CNAME chain', r.cname ? <span className="k-mono">{`${r.domain} → ${r.cname}`}</span> : <span className="k-muted">none</span>],
    ['Upstream', r.upstream ?? (r.zone ? <span>zone {r.zone}</span> : <span className="k-muted">none</span>)],
    ['Cache', cache],
    ['DNSSEC', r.dnssec || '–'],
    ['Extended DNS error', edeText(r.ede)],
    ['Reply', `${r.reply.type}${r.reply.time_ms !== undefined ? `, ${formatMS(r.reply.time_ms)}` : ''}${r.reply.answer ? `: ${r.reply.answer}` : ''}`],
    ['Node', r.node],
    ['Client', `${r.client.name ? `${r.client.name} ` : ''}${r.client.ip}${r.client.group ? ` (group ${r.client.group})` : ''}`],
    ['Protocol', `${r.proto}${r.ecs ? `, ECS ${r.ecs}` : ''}`],
    ['Time', formatDateTime(r.ts)],
  ];
  if (r.trace_id) items.push(['Trace', <code className="k-mono">{r.trace_id}</code>]);
  return (
    <dl className="k-why-grid" aria-label={`Why ${r.domain} got this answer`}>
      {items.map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}
