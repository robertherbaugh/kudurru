// One client (/clients/:key, an IP address or a client ID): its last 24
// hours (GET /clients), the groups it is in and why (the identification
// tiers of internal/policy: exact IP, longest CIDR, client ID, catch-all,
// default group), which policy decides for it right now and why (priority,
// schedule, enabled, the fallback policy), its recent queries with per-row
// allow/deny, and typed shortcuts: add it to a group, allow or deny a
// domain.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Code } from '@astryxdesign/core/Code';
import { Grid } from '@astryxdesign/core/Grid';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VStack } from '@astryxdesign/core/VStack';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent, type ReactNode } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, QueryRow } from '../api/types';
import { writeSummary } from '../components/domains/DomainEntryDialogs';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { matcherFor } from '../components/fields/MatchersField';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { QueryLogTable } from '../components/QueryLogTable';
import { formatDateTime, formatExact, formatPercent, formatRelative } from '../lib/format';
import { objectOwnership } from '../lib/managedBy';
import { listEntryError } from '../lib/objectSchemas';
import { identifyClient, objectHref, refOfObject, scheduleSummary, selectPolicy, shortRef, splitRef, TIER_TEXT, type Selection } from '../lib/policies';
import { useDeriveContext } from '../lib/usePolicyData';
import type { PageProps } from '../routes';

const RECENT = 50;

export default function ClientDetailPage({ params }: PageProps) {
  const key = params.key ?? '';
  const api = useApi();
  const can = useCan();
  const scope = useScopeParams();
  const qc = useQueryClient();
  const notify = useNotify();
  const [pick, setPick] = useState<string[]>([]);
  const [domain, setDomain] = useState('');

  const clients = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list({ window: '24h' }), refetchInterval: 60_000 });
  const recent = useQuery({ queryKey: qk.queries('client', scope, key), queryFn: () => api.queries.list(scope, { client: key, limit: RECENT }), enabled: can('querylog', 'read') });
  const { ctx, ready, error } = useDeriveContext();
  const c = clients.data?.items.find((x) => x.key === key);
  const membership = identifyClient(key, ctx.groups);
  const selection = selectPolicy(membership.groups, ctx.policies, ctx.now);
  const display = c?.name ?? recent.data?.queries.find((q) => q.client.name)?.client.name;

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: qk.objects('clientgroups') });
    void qc.invalidateQueries({ queryKey: qk.clients() });
  };
  const addTo = useMutation({
    mutationFn: async (name: string) => {
      const g = ctx.groups.find((x) => x.metadata.name === name && (x.metadata.namespace ?? 'default') === 'default');
      if (!g) throw new Error(`No group ${name}`);
      await api.objects.put<ClientGroup>('clientgroups', { ...g, spec: { ...g.spec, clients: [...(g.spec.clients ?? []), matcherFor({ key, name: display }, '')] } });
      return name;
    },
    onSuccess: (name) => {
      notify.success(`Added ${display ?? key} to ${name}`);
      setPick([]);
      invalidate();
    },
    onError: (err) => notify.error(err, 'Could not update the group'),
  });
  const removeFrom = useMutation({
    mutationFn: async (g: ClientGroup) => {
      const keep = (g.spec.clients ?? []).filter((m) => m.ip !== key && m.clientID !== key);
      await api.objects.put<ClientGroup>('clientgroups', { ...g, spec: { ...g.spec, clients: keep } });
      return g.metadata.name;
    },
    onSuccess: (name) => {
      notify.success(`Removed ${display ?? key} from ${name}`);
      invalidate();
    },
    onError: (err) => notify.error(err, 'Could not update the group'),
  });
  const quick = useMutation({
    // "*.x" is a wildcard entry (the name and its subdomains), anything else an exact one.
    mutationFn: (v: { action: 'allow' | 'deny'; domain: string }) =>
      api.domains.create(v.action, { values: [v.domain], type: v.domain.startsWith('*.') ? 'wildcard' : 'exact', origin: 'manual' }),
    onSuccess: (r, v) => {
      if (r.processed.success.length) notify.success(writeSummary(`${v.action === 'allow' ? 'Allowed' : 'Denied'} ${v.domain} (list ui-${v.action})`, r, v.action));
      for (const e of r.processed.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      setDomain('');
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
      void qc.invalidateQueries({ queryKey: qk.domains() });
    },
    onError: (err) => notify.error(err, 'Could not change the list'),
  });
  const rowAction = useMutation({
    mutationFn: (v: { row: QueryRow; action: 'allow' | 'deny' }) => api.domains.create(v.action, { values: [v.row.domain.replace(/\.$/, '')], type: 'exact', origin: 'querylog' }),
    onSuccess: (r, v) => {
      if (r.processed.success.length) notify.success(writeSummary(`${v.action === 'allow' ? 'Allowed' : 'Denied'} ${v.row.domain} (list ui-${v.action})`, r, v.action));
      for (const e of r.processed.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
      void qc.invalidateQueries({ queryKey: qk.domains() });
    },
    onError: (err, v) => notify.error(err, `Could not ${v.action} ${v.row.domain}`),
  });

  const writableGroups = ctx.groups.filter((g) => objectOwnership(g).editable && !membership.groups.includes(refOfObject(g)));
  const domainErr = domain.trim() ? listEntryError(domain.trim()) : null;
  const canLists = can('blocklists', 'write');
  const blockedPct = c && c.count ? (c.blocked / c.count) * 100 : undefined;

  return (
    <Page>
      <PageHeader
        title={display ?? key}
        back={{ href: '/clients', label: 'Clients' }}
        meta={
          <>
            {display ? <Code size="inherit">{key}</Code> : null}
            {c?.last_seen ? (
              <Text size="sm">
                last seen{' '}
                <Tooltip content={formatDateTime(c.last_seen)}>
                  <time dateTime={c.last_seen}>{formatRelative(c.last_seen)}</time>
                </Tooltip>
              </Text>
            ) : c ? null : (
              <Text type="supporting">no queries in the last 24 hours</Text>
            )}
          </>
        }
      />
      {clients.error ? <ErrorBanner error={clients.error} /> : null}
      {error ? <ErrorBanner error={error} /> : null}

      <Card>
        <MetadataList orientation="horizontal" aria-label="Last 24 hours">
          <MetadataListItem label="Queries (24 h)">
            <Stat>{formatExact(c?.count ?? 0)}</Stat>
          </MetadataListItem>
          <MetadataListItem label="Blocked">
            <Link href={`/queries?client=${encodeURIComponent(key)}&status_class=blocked`} size="2xl" weight="semibold" hasUnderline>
              {formatExact(c?.blocked ?? 0)}
            </Link>
          </MetadataListItem>
          <MetadataListItem label="Blocked share">
            <Stat>{formatPercent(blockedPct)}</Stat>
          </MetadataListItem>
        </MetadataList>
      </Card>

      {!ready ? (
        <Loading />
      ) : (
        <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
          <Panel title="Groups" headingId="sec-groups" data-testid="client-groups">
            {membership.groups.length ? (
              <VStack as="ul" gap={2} style={LIST_RESET}>
                {membership.groups.map((g) => {
                  const obj = ctx.groups.find((x) => refOfObject(x) === g);
                  const m = membership.matchers[g];
                  const removable = obj && objectOwnership(obj).editable && (obj.spec.clients ?? []).some((x) => x.ip === key || x.clientID === key);
                  return (
                    <HStack as="li" key={g} gap={2} vAlign="center" wrap="wrap">
                      <Link href={objectHref('groups', g)}>
                        <Badge variant={splitRef(g).name === 'default' ? 'neutral' : 'blue'} label={shortRef(g)} />
                      </Link>
                      <Text type="supporting">{m ? `matched by ${m.ip ?? m.cidr ?? m.clientID}` : 'matches everyone'}</Text>
                      {removable ? <Button label="Remove" size="sm" variant="ghost" isDisabled={!can('clientgroups', 'write')} isLoading={removeFrom.isPending} onClick={() => removeFrom.mutate(obj)} tooltip={`Remove ${key} from ${shortRef(g)}`} /> : null}
                    </HStack>
                  );
                })}
              </VStack>
            ) : (
              <Text color="secondary" as="p">
                No group matches this client.
              </Text>
            )}
            <Text size="sm" as="p">
              Matched by {TIER_TEXT[membership.tier]}.
            </Text>
            {c?.group && !membership.groups.includes(c.group) ? (
              <Banner status="info" title={`The last query resolved to ${shortRef(c.group)}`} description="Group changes apply to the next query; MAC and hostname matchers are not evaluated here." />
            ) : null}
            {/* The picker's help hangs under it, so the action goes below rather than beside. */}
            <VStack
              as="form"
              gap={2}
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                if (pick[0]) addTo.mutate(splitRef(pick[0]).name);
              }}
            >
              <ObjectRefPicker
                kind="clientgroups"
                label="Add to group"
                sameNamespace
                value={pick}
                onChange={setPick}
                emptyLabel="Choose a group…"
                filter={(o) => writableGroups.includes(o as ClientGroup)}
                isDisabled={!can('clientgroups', 'write')}
              />
              <HStack>
                <Button type="submit" label="Add" variant="secondary" isDisabled={!pick[0] || !can('clientgroups', 'write')} isLoading={addTo.isPending} />
              </HStack>
            </VStack>
          </Panel>
          <EffectivePolicy selection={selection} />
        </Grid>
      )}

      <Panel title="Allow or deny a domain" headingId="sec-quick" description="Adds the domain to ui-allow or ui-deny, the lists the query log's Allow and Deny buttons use; every policy with those lists follows.">
        <VStack as="form" gap={2} onSubmit={(e: FormEvent) => e.preventDefault()}>
          <TextField label="Domain" value={domain} onChange={setDomain} placeholder="ads.example.com" mono error={domainErr ?? undefined} />
          <HStack gap={2} wrap="wrap">
            <Button label="Allow" variant="secondary" isDisabled={!domain.trim() || !!domainErr || !canLists} tooltip={canLists ? undefined : 'Needs blocklists.write'} onClick={() => quick.mutate({ action: 'allow', domain: domain.trim() })} />
            <Button label="Deny" variant="secondary" isDisabled={!domain.trim() || !!domainErr || !canLists} tooltip={canLists ? undefined : 'Needs blocklists.write'} onClick={() => quick.mutate({ action: 'deny', domain: domain.trim() })} />
          </HStack>
        </VStack>
      </Panel>

      <VStack as="section" gap={2} aria-labelledby="sec-recent">
        <HStack gap={2} hAlign="between" vAlign="center" wrap="wrap">
          <Heading level={3} accessibilityLevel={2} id="sec-recent">
            Recent queries
          </Heading>
          <Link href={`/queries?client=${encodeURIComponent(key)}`} hasUnderline>
            All queries of this client
          </Link>
        </HStack>
        {recent.error ? <ErrorBanner error={recent.error} title="The query log could not be read" /> : null}
        {recent.isLoading ? <Loading /> : null}
        {recent.data ? <QueryLogTable rows={recent.data.queries} hasMore={false} loadingMore={false} onLoadMore={() => undefined} canAct={canLists} onAction={(row, a) => rowAction.mutate({ row, action: a })} /> : null}
      </VStack>
    </Page>
  );
}

/** Lists drawn by Stacks: no markers or indent. */
const LIST_RESET = { listStyle: 'none', padding: 0, margin: 0 } as const;

/** A figure of the 24 hours row. */
function Stat({ children }: { children: ReactNode }) {
  return (
    <Text size="2xl" weight="semibold" hasTabularNumbers>
      {children}
    </Text>
  );
}

function EffectivePolicy({ selection }: { selection: Selection }) {
  const w = selection.winner;
  return (
    <Panel title="Policy right now" headingId="sec-policy" data-testid="client-policy">
      {w ? (
        <Text as="p">
          <Link href={objectHref('policies', w.ref)} type="inherit" weight="semibold" hasUnderline>
            {shortRef(w.ref)}
          </Link>{' '}
          decides:{' '}
          {selection.fallback
            ? 'no policy of its groups is active, so the default policy applies.'
            : selection.active.length > 1
              ? `the highest priority (${w.policy.spec.priority ?? 0}) of ${selection.active.length} active policies; their lists are merged.`
              : 'the only active policy of its groups.'}
        </Text>
      ) : (
        <Text as="p">No policy applies: nothing is filtered for this client.</Text>
      )}
      {selection.candidates.length ? (
        <VStack as="ol" gap={1.5} aria-label="Candidate policies" style={LIST_RESET}>
          {selection.candidates.map((c, i) => (
            <HStack as="li" key={c.ref} gap={2} vAlign="center" wrap="wrap">
              <Text type="supporting" hasTabularNumbers>
                {i + 1}.
              </Text>
              <Link href={objectHref('policies', c.ref)} hasUnderline>
                {shortRef(c.ref)}
              </Link>
              {c.active ? <Badge variant={c === w ? 'success' : 'info'} label={c === w ? 'decides' : 'active, lists merged'} /> : <Badge variant="neutral" label={c.off ?? 'off'} />}
              <Text type="supporting">
                priority {c.policy.spec.priority ?? 0} · {c.policy.spec.schedule ? scheduleSummary(c.policy.spec.schedule) : 'always'}
              </Text>
            </HStack>
          ))}
        </VStack>
      ) : (
        <Text type="supporting" as="p">
          No policy names its groups.
        </Text>
      )}
    </Panel>
  );
}
