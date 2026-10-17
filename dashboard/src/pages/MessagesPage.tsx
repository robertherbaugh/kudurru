// Diagnosis messages (§10 principle 8): regex errors, policy ties, lists
// that cannot be fetched, certificate expiry, disk/WAL size, serial
// divergence, follower read-only and more, from GET /info/messages.
import { Badge } from '@astryxdesign/core/Badge';
import { Card } from '@astryxdesign/core/Card';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Link } from '@astryxdesign/core/Link';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMessages } from '../api/hooks';
import type { DiagnosisMessage } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { Page, PageHeader } from '../components/layout/Page';

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
    <Page>
      <PageHeader title="Diagnosis" />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data && items.length === 0 ? <EmptyState title="Nothing to report" description="No configuration problems, failing lists or node issues were found." /> : null}
      {items.length ? (
        <VStack role="list" aria-label="Diagnosis messages" gap={2}>
          {items.map((m, i) => {
            const ref = parseObjectRef(m.object);
            return (
              <StackItem role="listitem" key={keys[i]}>
                <Card>
                  <VStack gap={2}>
                    <HStack gap={2} vAlign="center" wrap="wrap">
                      <Badge variant={m.severity === 'error' ? 'error' : m.severity === 'warning' ? 'warning' : 'info'} label={m.severity} />
                      <Text weight="semibold">{m.reason}</Text>
                      <StackItem size="fill" />
                      {m.source ? <Text type="supporting">{m.source}</Text> : null}
                      {m.node ? <Text type="supporting">node {m.node}</Text> : null}
                    </HStack>
                    <Text as="p">{m.message}</Text>
                    {m.hint ? (
                      <Text type="supporting" as="p">
                        {m.hint}
                      </Text>
                    ) : null}
                    {ref ? (
                      <Text type="supporting">
                        <Link href={objectLink(ref)} hasUnderline>
                          {ref.kind} {ref.namespace ? `${ref.namespace}/` : ''}
                          {ref.name}
                        </Link>
                      </Text>
                    ) : m.object ? (
                      <Text type="code" size="sm">
                        {m.object}
                      </Text>
                    ) : null}
                  </VStack>
                </Card>
              </StackItem>
            );
          })}
        </VStack>
      ) : null}
    </Page>
  );
}
