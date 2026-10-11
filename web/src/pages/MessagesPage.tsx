// Diagnosis messages (§10 principle 8): regex errors, policy ties, lists
// that cannot be fetched, certificate expiry, disk/WAL size, serial
// divergence, follower read-only and more, from GET /info/messages.
import { Badge } from '@astryxdesign/core/Badge';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { useMessages } from '../api/hooks';
import type { DiagnosisMessage } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { Link } from '../lib/router';

/** A parsed message object reference ("Kind/namespace/name" or "Kind/name"). */
export interface ObjectRef {
  kind: string;
  namespace?: string;
  name: string;
}

export function parseObjectRef(ref: string | undefined): ObjectRef | null {
  if (!ref) return null;
  const parts = ref.split('/');
  if (parts.length === 2 && parts[0] && parts[1]) return { kind: parts[0], name: parts[1] };
  if (parts.length >= 3 && parts[0]) return { kind: parts[0], namespace: parts[1], name: parts.slice(2).join('/') };
  return null;
}

/** The page an object reference in a message links to. */
export function objectLink(o: ObjectRef): string {
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

/** A stable React key: messages carry no id, and identical ones may repeat. */
export function messageKeys(items: DiagnosisMessage[]): string[] {
  const seen = new Map<string, number>();
  return items.map((m) => {
    const base = [m.node, m.source, m.severity, m.reason, m.object ?? '', m.message].join('|');
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    return n ? `${base}#${n}` : base;
  });
}

const ORDER = { error: 0, warning: 1, info: 2 } as const;

export default function MessagesPage() {
  const q = useMessages();
  const items = [...(q.data?.items ?? [])].sort((a, b) => ORDER[a.severity] - ORDER[b.severity]);
  const keys = messageKeys(items);
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
          {items.map((m, i) => {
            const ref = parseObjectRef(m.object);
            return (
              <li key={keys[i]} className="k-panel">
                <div className="k-row">
                  <Badge variant={m.severity === 'error' ? 'error' : m.severity === 'warning' ? 'warning' : 'info'} label={m.severity} />
                  <strong>{m.reason}</strong>
                  <span className="k-spacer" />
                  {m.source ? <span className="k-small k-muted">{m.source}</span> : null}
                  {m.node ? <span className="k-small k-muted">node {m.node}</span> : null}
                </div>
                <p>{m.message}</p>
                {m.hint ? <p className="k-small k-muted">{m.hint}</p> : null}
                {ref ? (
                  <Link href={objectLink(ref)} className="k-small">
                    {ref.kind} {ref.namespace ? `${ref.namespace}/` : ''}
                    {ref.name}
                  </Link>
                ) : m.object ? (
                  <span className="k-small k-mono">{m.object}</span>
                ) : null}
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}
