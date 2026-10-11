// Top-N tables (domains, clients, upstreams, lists, zones) where every row
// drills down into the query log (§10 principle 3), with the "approximate"
// badge when the server merged lossy top-N rollups (principle 12).
import { Badge } from '@astryxdesign/core/Badge';
import type { TopDim, TopResult } from '../api/types';
import { formatExact, formatPercent } from '../lib/format';
import { Link } from '../lib/router';
import { ErrorBanner, Loading } from './Feedback';

/** The query-log route for one top-list row. */
export function drillHref(dim: TopDim, key: string, blocked: boolean, extra: Record<string, string> = {}): string {
  const p = new URLSearchParams();
  const field = dim === 'domains' ? 'domain' : dim === 'clients' ? 'client' : dim === 'upstreams' ? 'upstream' : dim === 'lists' ? 'list' : 'zone';
  p.set(field, key);
  if (blocked) p.set('status_class', 'blocked');
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return `/queries?${p.toString()}`;
}

export interface TopListProps {
  title: string;
  dim: TopDim;
  blocked?: boolean;
  data: TopResult | undefined;
  isLoading?: boolean;
  error?: unknown;
  /** Extra filters carried into the drill-down (since/until for long-term views). */
  extra?: Record<string, string>;
}

export function TopList({ title, dim, blocked = false, data, isLoading, error, extra }: TopListProps) {
  const max = data?.items.reduce((m, i) => Math.max(m, i.count), 0) ?? 0;
  const headingId = `top-${dim}-${blocked ? 'b' : 'p'}-${title.replace(/\W+/g, '-')}`;
  return (
    <section className="k-panel" aria-labelledby={headingId}>
      <div className="k-panel-title">
        <h2 id={headingId}>{title}</h2>
        {data?.approximate ? (
          <span title="Merged from top-N rollups; ranks beyond the cutoff may be missing">
            <Badge variant="warning" label="approximate" />
          </span>
        ) : null}
      </div>
      {error ? <ErrorBanner error={error} /> : null}
      {isLoading && !data ? <Loading /> : null}
      {data && data.items.length === 0 ? <p className="k-muted">No queries in this window.</p> : null}
      {data && data.items.length > 0 ? (
        <div className="k-table-wrap">
          <table className="k-table">
            <thead>
              <tr>
                <th scope="col">{dim === 'domains' ? 'Domain' : dim === 'clients' ? 'Client' : dim === 'upstreams' ? 'Upstream' : dim === 'lists' ? 'List' : 'Zone'}</th>
                <th scope="col" className="num">
                  {blocked ? 'Blocked' : 'Queries'}
                </th>
                <th scope="col" className="k-col-bar">
                  <span className="k-visually-hidden">Share</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((i) => (
                <tr key={i.key}>
                  <td className="k-break">
                    <Link href={drillHref(dim, i.key, blocked, extra)} title={`Show these queries in the log`}>
                      {i.name ? `${i.name}` : i.key}
                    </Link>
                    {i.name && i.name !== i.key ? <span className="k-muted k-small"> {i.key}</span> : null}
                  </td>
                  <td className="num" title={formatPercent(i.percent)}>
                    {formatExact(i.count)}
                  </td>
                  <td className="k-col-bar">
                    <div className="k-bar" data-tone={blocked ? 'blocked' : undefined} aria-hidden="true">
                      <span style={{ width: `${max ? (i.count / max) * 100 : 0}%` }} />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}
