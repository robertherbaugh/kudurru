// The audit trail: retained events, request context and integrity verification.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, useTableRowExpansion, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useInfiniteQuery, useMutation } from '@tanstack/react-query';
import { useCallback, useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import { useFeature } from '../api/hooks';
import type { AuditEvent } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { FeatureLocked } from '../components/FeatureLocked';
import { EnumSelect, type EnumOption } from '../components/fields/EnumSelect';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatDateTime } from '../lib/format';

const AUDIT_PAGE = 100;

/** The actor of an audit event as one line. */
export function actorText(a: AuditEvent['actor']): string {
  const who = a.name || a.id || 'unknown';
  return a.kind && a.kind !== 'User' ? `${who} (${a.kind})` : who;
}

/** The resource of an audit event as "Kind ns/name". */
export function resourceText(r: AuditEvent['resource']): string {
  const name = r.name ? `${r.namespace ? `${r.namespace}/` : ''}${r.name}` : '';
  return name ? `${r.kind} ${name}` : r.kind;
}

/** Resource kinds the audit log records (objects and runtime actions). */
const AUDIT_KINDS: EnumOption[] = [
  'DNSZone',
  'DNSRecordSet',
  'BlockList',
  'ClientGroup',
  'Policy',
  'APIToken',
  'LocalUser',
  'ServiceAccount',
  'RoleBinding',
  'IdentityProvider',
  'settings',
  'blocking',
  'cache',
  'querylog',
  'audit',
].map((k) => ({
  value: k,
  label: k,
}));

/** An audit event with the row key it is listed under (events may carry no id). */
type AuditRow = AuditEvent & { rowKey: string } & Record<string, unknown>;

/** Read only safe token fields; old events may have no document. */
function tokenAuditDetails(a: AuditEvent): string[] {
  const doc = a.after ?? a.before;
  if (!doc || typeof doc !== 'object' || !('spec' in doc) || !doc.spec || typeof doc.spec !== 'object') return [];
  const spec = doc.spec;
  const details: string[] = [];
  if ('description' in spec && typeof spec.description === 'string' && spec.description) details.push(`Label: ${spec.description}`);
  if ('owner' in spec && spec.owner && typeof spec.owner === 'object') {
    const owner = spec.owner;
    if ('kind' in owner && typeof owner.kind === 'string' && 'name' in owner && typeof owner.name === 'string') {
      const ns = 'namespace' in owner && typeof owner.namespace === 'string' ? owner.namespace : '';
      details.push(`Owner: ${resourceText({ kind: owner.kind, name: owner.name, namespace: ns })}`);
    }
  }
  if ('expiresAt' in spec) {
    if (spec.expiresAt === null) details.push('Expires: never');
    else if (typeof spec.expiresAt === 'string') details.push(`Expires: ${formatDateTime(spec.expiresAt)}`);
  }
  if ('scopes' in spec && Array.isArray(spec.scopes) && spec.scopes.every((s) => typeof s === 'string') && spec.scopes.length) details.push(`Scopes: ${spec.scopes.join(', ')}`);
  return details;
}

function AuditObject({ event }: { event: AuditEvent }) {
  const details = event.resource.kind === 'APIToken' ? tokenAuditDetails(event) : [];
  return (
    <VStack gap={0.5}>
      <Text type="code" size="sm">
        {resourceText(event.resource)}
      </Text>
      {details.map((detail) => (
        <Text key={detail} type="supporting">
          {detail}
        </Text>
      ))}
      {event.resource.kind === 'APIToken' && !details.length ? <Text type="supporting">Token details were not recorded.</Text> : null}
    </VStack>
  );
}

/** Older SQLite documents may contain credential hashes or private metadata. */
function safeDetailDocument(kind: string, doc: unknown): unknown {
  if (kind === 'LocalUser') return undefined;
  if (kind !== 'APIToken' || !doc || typeof doc !== 'object') return doc;
  if (!('spec' in doc) || !doc.spec || typeof doc.spec !== 'object') return undefined;
  const spec: Record<string, unknown> = Object.fromEntries(Object.entries(doc.spec).filter(([key]) => ['owner', 'description', 'expiresAt', 'scopes'].includes(key)));
  if (spec.owner && typeof spec.owner === 'object') {
    spec.owner = Object.fromEntries(Object.entries(spec.owner).filter(([key]) => ['kind', 'name', 'namespace', 'id', 'provider'].includes(key)));
  }
  return { spec };
}

function AuditDetails({ event }: { event: AuditRow }) {
  return (
    <VStack id={`audit-details-${event.rowKey}`} role="group" aria-label={`Audit event details for ${event.action} ${event.resource.name ?? event.resource.kind}`} gap={3} paddingBlock={2}>
      <AuditObject event={event} />
      <CodeBlock
        language="json"
        code={JSON.stringify(
          { ...event, before: safeDetailDocument(event.resource.kind, event.before), after: safeDetailDocument(event.resource.kind, event.after) },
          (key, value: unknown) => (['rowKey', 'secretHash', 'passwordHash', 'totpSecret'].includes(key) ? undefined : value),
          2,
        )}
      />
    </VStack>
  );
}

const auditColumns: TableColumn<AuditRow>[] = [
  { key: 'ts', header: 'Time', width: pixel(210), renderCell: (a) => <Text textWrap="nowrap">{formatDateTime(a.ts)}</Text> },
  { key: 'actor', header: 'Actor', width: proportional(1.5, { minWidth: 120 }), renderCell: (a) => <Text>{actorText(a.actor)}</Text> },
  {
    key: 'action',
    header: 'Action',
    width: proportional(1.5, { minWidth: 120 }),
    renderCell: (a) => (
      <Text type="code" size="sm">
        {a.action}
      </Text>
    ),
  },
  {
    key: 'resource',
    header: 'Object',
    width: proportional(2, { minWidth: 140 }),
    renderCell: (a) => <AuditObject event={a} />,
  },
  {
    key: 'result',
    header: 'Result',
    width: proportional(1, { minWidth: 100 }),
    renderCell: (a) => (
      <VStack gap={0.5} align="start">
        <Badge variant={a.result === 'success' ? 'success' : a.result === 'denied' ? 'warning' : 'error'} label={a.result} />
        {a.reason ? <Text type="supporting">{a.reason}</Text> : null}
      </VStack>
    ),
  },
  {
    key: 'from',
    header: 'From',
    width: proportional(1, { minWidth: 110 }),
    renderCell: (a) => (
      <VStack gap={0.5}>
        <Text size="sm">{a.actor.auth === 'unix' ? 'Local admin socket' : a.request?.ip || '–'}</Text>
        {a.request?.path ? (
          <Text type="supporting">
            {a.request.method} {a.request.path}
          </Text>
        ) : null}
        {a.request?.ua ? <Text type="supporting">{a.request.ua}</Text> : null}
        {a.authz ? <Text type="supporting">via {a.authz}</Text> : null}
      </VStack>
    ),
  },
];

/** The audit trail is licensed (feature "audit"); locked, the tab asks for nothing. */
function Audit() {
  const f = useFeature('audit');
  if (!f.ready) return <Loading />;
  if (!f.allowed) {
    return (
      <Panel title="Audit log" headingId="audit-title">
        <FeatureLocked feature="audit" what="The audit trail" />
      </Panel>
    );
  }
  return <AuditLog />;
}

function AuditLog() {
  const api = useApi();
  const can = useCan();
  const [actor, setActor] = useState('');
  const [kind, setKind] = useState('');
  const [order, setOrder] = useState<'asc' | 'desc'>('desc');
  const [expandedKeys, setExpandedKeys] = useState(new Set<string>());
  const toggle = useCallback((key: string) => {
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);
  const verify = useMutation({ mutationFn: () => api.audit.verify() });
  const q = useInfiniteQuery({
    queryKey: qk.audit(actor, kind, order),
    queryFn: ({ pageParam }) =>
      api.audit.list({ actor: actor.trim() || undefined, kind: kind.trim() || undefined, limit: AUDIT_PAGE, order, ...(pageParam ? { cursor: pageParam } : {}) }),
    initialPageParam: '',
    getNextPageParam: (last) => last.continue || last.cursor_next || undefined,
  });
  const events = useMemo<AuditRow[]>(
    () => (q.data?.pages.flatMap((p) => p.items) ?? []).map((a, i) => ({ ...a, rowKey: a.uid ?? (a.id !== undefined ? String(a.id) : `${a.ts}-${i}`) })),
    [q.data],
  );
  const expansion = useTableRowExpansion<AuditRow>({
    expandedKeys,
    onToggle: toggle,
    getRowKey: (event) => event.rowKey,
    renderExpanded: (event) => <AuditDetails event={event} />,
    hasRowClickExpansion: true,
  });
  const keyboard: TablePlugin<AuditRow> = {
    transformBodyRow: (props, event, index) => ({
      ...props,
      htmlProps: {
        ...props.htmlProps,
        className: [props.htmlProps.className, 'k-log-row'].filter(Boolean).join(' '),
        tabIndex: 0,
        'data-audit-row-index': index,
        'aria-expanded': expandedKeys.has(event.rowKey),
        'aria-controls': expandedKeys.has(event.rowKey) ? `audit-details-${event.rowKey}` : undefined,
        style: { ...props.htmlProps.style, ...(expandedKeys.has(event.rowKey) ? { background: 'var(--color-overlay-pressed)' } : {}) },
        onKeyDown: (e) => {
          props.htmlProps.onKeyDown?.(e);
          if (e.defaultPrevented || e.target !== e.currentTarget) return;
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            toggle(event.rowKey);
          } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
            e.preventDefault();
            const to = index + (e.key === 'ArrowDown' ? 1 : -1);
            e.currentTarget.closest('table')?.querySelector<HTMLTableRowElement>(`tr[data-audit-row-index="${to}"]`)?.focus();
          }
        },
      },
    }),
  };
  return (
    <>
      <Panel
        title="Events"
        headingId="audit-title"
        actions={
          <>
            <Button label="Refresh" variant="secondary" size="sm" isLoading={q.isRefetching} onClick={() => void q.refetch()} />
            <Button label="Verify integrity" variant="secondary" size="sm" isLoading={verify.isPending} onClick={() => verify.mutate()} />
            {can('audit', 'export') ? (
              <Link href={api.audit.exportURL()} download="audit.jsonl" hasUnderline>
                Export
              </Link>
            ) : null}
          </>
        }
      >
        <HStack gap={2} wrap="wrap" align="start">
          <TextField label="Actor" startIcon="search" value={actor} onChange={setActor} hasClear />
          <EnumSelect label="Kind" value={kind} options={AUDIT_KINDS} onChange={setKind} emptyLabel="Any kind" />
          <EnumSelect
            label="Time order"
            value={order}
            options={[
              { value: 'desc', label: 'Newest first' },
              { value: 'asc', label: 'Oldest first' },
            ]}
            onChange={(v) => setOrder(v === 'asc' ? 'asc' : 'desc')}
          />
        </HStack>
        {q.hasNextPage ? (
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Button
              label={order === 'desc' ? 'Load older events' : 'Load newer events'}
              variant="secondary"
              size="sm"
              isLoading={q.isFetchingNextPage}
              onClick={() => void q.fetchNextPage()}
            />
            <Text type="supporting">Showing {events.length} events · More available</Text>
          </HStack>
        ) : null}
        <Text type="supporting">
          Stored events use hash chains to detect changed records and broken links. Verification checks the complete retained trail, including available batch seals.
          Results are reused for up to 30 seconds.
        </Text>
        {verify.error ? <ErrorBanner error={verify.error} title="Verification unavailable" /> : null}
        {verify.data ? (
          <Banner
            status={verify.data.verified ? 'success' : 'error'}
            title={verify.data.verified ? 'Audit integrity verified' : 'Audit verification failed'}
            description={
              verify.data.verified
                ? `Retained trail checked at ${formatDateTime(verify.data.checkedAt)}. Keep external logs or exports for an independent record.`
                : (verify.data.reason ?? 'The stored audit trail could not be verified.')
            }
          />
        ) : null}
        {q.error ? <ErrorBanner error={q.error} /> : null}
        {q.isLoading ? <Loading /> : null}
        {q.data ? (
          <VStack gap={2}>
            <Table<AuditRow>
              aria-label="Audit entries"
              density="compact"
              verticalAlign="top"
              hasHover
              idKey="rowKey"
              data={events}
              columns={auditColumns}
              plugins={{ expansion, keyboard }}
              emptyState={<Text color="secondary">No audit events{actor || kind ? ' match' : ''}.</Text>}
            />
            <HStack gap={2} wrap="wrap" vAlign="center">
              <Text type="supporting" role="status">
                {events.length} events loaded{q.hasNextPage ? '' : ' · End of retained events'}
              </Text>
            </HStack>
          </VStack>
        ) : null}
      </Panel>
    </>
  );
}

export default function AuditPage() {
  const can = useCan();
  return (
    <Page>
      <PageHeader title="Audit Log" />
      {can('audit', 'read') ? <Audit /> : <Banner status="warning" title="Not permitted" description="Reading the audit log requires audit.read." />}
    </Page>
  );
}
