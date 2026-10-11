// Diagnosis messages (§10 principle 8): regex errors, policy ties, lists
// that cannot be fetched, certificate expiry, disk/WAL size, serial
// divergence, follower read-only and more, from GET /info/messages.
import { Badge } from '@astryxdesign/core/Badge';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { useMessages } from '../api/hooks';
import type { DiagnosisMessage } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { formatRelative } from '../lib/format';
import { Link } from '../lib/router';

/** The page an object reference in a message links to. */
export function objectLink(o: NonNullable<DiagnosisMessage['object']>): string {
  switch (o.kind) {
    case 'DNSZone':
      return `/zones/${encodeURIComponent(o.name)}`;
    case 'BlockList':
      return `/lists?q=${encodeURIComponent(o.name)}`;
    case 'ClientGroup':
      return `/groups?q=${encodeURIComponent(o.name)}`;
    case 'Policy':
      return `/policies?q=${encodeURIComponent(o.name)}`;
    default:
      return '/nodes';
  }
}

const ORDER = { error: 0, warning: 1, info: 2 } as const;

export default function MessagesPage() {
  const q = useMessages();
  const items = [...(q.data?.items ?? [])].sort((a, b) => ORDER[a.severity] - ORDER[b.severity]);
  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Diagnosis</h1>
      </div>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && items.length === 0 ? <EmptyState title="Nothing to report" description="No configuration problems, failing lists or node issues were found." /> : null}
      {items.length ? (
        <ul className="k-messages">
          {items.map((m) => (
            <li key={m.id} className="k-panel">
              <div className="k-row">
                <Badge variant={m.severity === 'error' ? 'error' : m.severity === 'warning' ? 'warning' : 'info'} label={m.severity} />
                <strong>{m.reason}</strong>
                <span className="k-spacer" />
                {m.node ? <span className="k-small k-muted">node {m.node}</span> : null}
                {m.since ? <span className="k-small k-muted">since {formatRelative(m.since)}</span> : null}
              </div>
              <p>{m.message}</p>
              {m.hint ? <p className="k-small k-muted">{m.hint}</p> : null}
              {m.object ? (
                <Link href={objectLink(m.object)} className="k-small">
                  {m.object.kind} {m.object.namespace ? `${m.object.namespace}/` : ''}
                  {m.object.name}
                </Link>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
