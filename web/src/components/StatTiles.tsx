// The four stat tiles (§10 principle 1): Total queries, Blocked, % blocked
// and Domains on lists, with active/total clients in the footer. Each tile
// is a link into the pre-filtered query log (or the lists page).
import type { Summary } from '../api/types';
import { formatCount, formatExact, formatPercent } from '../lib/format';
import { Link } from '../lib/router';

export interface Tile {
  key: 'total' | 'blocked' | 'percent' | 'lists';
  label: string;
  value: string;
  title: string;
  foot: string;
  href: string;
}

/** The tiles for a summary; exported for tests and the long-term page. */
export function tilesFor(s: Summary | undefined, window = '24h'): Tile[] {
  const q = s?.queries;
  const since = s?.from;
  const range = since ? `&since=${encodeURIComponent(since)}` : '';
  const clients = s ? `${formatExact(s.clients.active)} active of ${formatExact(s.clients.total)} clients` : '–';
  return [
    {
      key: 'total',
      label: `Total queries (${window})`,
      value: formatCount(q?.total),
      title: formatExact(q?.total),
      foot: clients,
      href: `/queries${range ? `?${range.slice(1)}` : ''}`,
    },
    {
      key: 'blocked',
      label: 'Queries blocked',
      value: formatCount(q?.blocked),
      title: formatExact(q?.blocked),
      foot: s ? `${formatExact(s.queries.unique_domains)} unique domains` : '–',
      href: `/queries?status_class=blocked${range}`,
    },
    {
      key: 'percent',
      label: 'Percent blocked',
      value: formatPercent(q?.percent_blocked),
      title: formatPercent(q?.percent_blocked, 3),
      foot: s ? `${formatCount(s.queries.cached)} answered from cache` : '–',
      href: `/queries?status_class=blocked${range}`,
    },
    {
      key: 'lists',
      label: 'Domains on lists',
      value: formatCount(s?.domains_on_lists),
      title: formatExact(s?.domains_on_lists),
      foot: 'across all enabled block lists',
      href: '/lists',
    },
  ];
}

export function StatTiles({ summary, window = '24h' }: { summary: Summary | undefined; window?: string }) {
  const tiles = tilesFor(summary, window);
  return (
    <div className="k-grid k-grid-tiles" role="list" aria-label="Summary">
      {tiles.map((t) => (
        <Link key={t.key} href={t.href} className="k-tile" data-tone={t.key} role="listitem" aria-label={`${t.label}: ${t.title}`}>
          <span className="k-tile-label">{t.label}</span>
          <span className="k-tile-value" title={t.title} data-testid={`tile-${t.key}`}>
            {summary ? t.value : '…'}
          </span>
          <span className="k-tile-foot">{t.foot}</span>
        </Link>
      ))}
    </div>
  );
}
