// One policy (/policies/:ns/:name): what it does right now (active,
// scheduled off, disabled or shadowed, the next schedule change, priority,
// conditions) from GET /policies/{ns}/{name}/state (ux-plan C3), and one
// section per concept, each with its own Edit and Save: who, what, blocked
// services (logo toggles and category switches saved in place), content
// categories (Block List Project lists, created on demand), safe search,
// how, when, and the last 24 hours of activity. Enable/disable, delete and "View as YAML" sit
// in the header; objects managed elsewhere show the same page read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid } from '@astryxdesign/core/Grid';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { Policy, PolicySpec, PolicyState } from '../api/types';
import { EditableSection } from '../components/EditableSection';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { ManagedBadge } from '../components/ManagedBadge';
import { cleanPolicySpec, ContentCategoryFields, HowFields, howError, SafeSearchFields, ServicesFields, WhatFields, WhenFields, whenError, WhoFields, whoError } from '../components/PolicyFields';
import { EngineList, LogoStack } from '../components/ServiceGrid';
import { ResponsePolicyFields, responsePolicyError, responsePolicySummary } from '../components/ResponsePolicyFields';
import { YamlDialog } from '../components/YamlDialog';
import { contentCategoryOfList, ensureContentLists } from '../lib/contentCategories';
import { formatDateTime, formatExact, formatPercent, formatRelative } from '../lib/format';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { blockResponseLabel, clientHref, objectHref, policyBadge, shortRef } from '../lib/policies';
import { useNavigate } from '../lib/router';
import { sameSelection, selectionOf, withSelection, type ServiceSelection } from '../lib/serviceCatalogue';
import { usePolicyState } from '../lib/usePolicyData';
import type { PageProps } from '../routes';

const BACK = { href: '/policies', label: 'Policies' };

export default function PolicyDetailPage({ params }: PageProps) {
  const ns = params.ns ?? 'default';
  const name = params.name ?? '';
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [yaml, setYaml] = useState(false);

  const q = useQuery({ queryKey: [...qk.objects('policies'), ns, name], queryFn: () => api.objects.get<Policy>('policies', name, ns) });
  const policy = q.data;
  const st = usePolicyState(ns, name, policy);

  const invalidate = async () => {
    await qc.invalidateQueries({ queryKey: qk.objects('policies') });
    await qc.invalidateQueries({ queryKey: qk.policyState(ns, name) });
  };
  const put = async (next: Policy) => {
    await api.objects.put('policies', next);
    await invalidate();
  };
  const saveSpec = async (spec: PolicySpec, what: string) => {
    if (!policy) return;
    // Content categories are BlockLists created the first time a policy names them.
    const created = await ensureContentLists(api, ns, spec.blockListRefs);
    if (created.length) await qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    await put({ ...policy, spec: cleanPolicySpec(spec) });
    notify.success(`Saved ${what} of ${name}${created.length ? ` and created ${created.join(', ')}` : ''}`);
  };

  const toggle = useMutation({
    mutationFn: (enabled: boolean) => put(withMeta(policy as Policy, objectComment(policy as Policy), enabled)),
    onSuccess: (_r, enabled) => notify.success(`${name} ${enabled ? 'enabled' : 'disabled'}`),
    onError: (err) => notify.error(err, `Could not change ${name}`),
  });
  const del = useMutation({
    mutationFn: () => api.objects.delete('policies', name, ns),
    onSuccess: () => {
      notify.success(`Deleted policy ${name}`);
      // Only the list: refetching this page's object would answer 404.
      navigate('/policies');
      void qc.invalidateQueries({ queryKey: qk.objects('policies'), exact: true });
    },
    onError: (err) => notify.error(err, 'Delete failed'),
  });

  if (q.error) {
    return (
      <Page>
        <PageHeader title={`Policy ${name}`} back={BACK} />
        <ErrorBanner error={q.error} />
      </Page>
    );
  }
  if (!policy) return <Loading />;

  const own = objectOwnership(policy);
  const canWrite = can('policies', 'write');
  const roReason = !own.editable ? readOnlyReason(own) : !canWrite ? 'Needs policies.write' : '';
  const enabled = objectEnabled(policy);
  const comment = objectComment(policy);
  const state = st.state;
  const badge = state ? policyBadge(state) : undefined;
  const spec = policy.spec;
  const fieldProps = { namespace: ns, self: `${ns}/${name}` };

  return (
    <Page>
      <PageHeader
        title={name}
        back={BACK}
        meta={
          <>
            {badge ? <Badge variant={badge.variant} label={badge.label} /> : null}
            <Text size="sm">priority {spec.priority ?? 0}</Text>
            {ns !== 'default' ? <Text type="supporting">namespace {ns}</Text> : null}
            <ManagedBadge owner={policy.metadata.annotations?.[MANAGED_BY]} />
          </>
        }
        description={comment || undefined}
        actions={
          <>
            <Button label={enabled ? 'Disable' : 'Enable'} variant="secondary" isDisabled={!!roReason} tooltip={roReason || undefined} isLoading={toggle.isPending} onClick={() => toggle.mutate(!enabled)} />
            <Button label="View as YAML" variant="ghost" onClick={() => setYaml(true)} />
            <Button label="Delete" variant="destructive" isDisabled={!!roReason || !can('policies', 'delete')} tooltip={roReason || undefined} onClick={() => setConfirmDelete(true)} />
          </>
        }
      />
      {!own.editable ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}

      <StatePanel state={state} derived={st.derived} loading={st.isLoading} error={st.error} />

      <EditableSection<PolicySpec>
        title="Who"
        description="The client groups this policy applies to."
        value={spec}
        readOnlyReason={roReason}
        testId="section-who"
        view={<GroupsView state={state} ns={ns} />}
        edit={(d, set, showErrors) => <WhoFields spec={d} onChange={set} showErrors={showErrors} {...fieldProps} />}
        validate={whoError}
        onSave={(d) => saveSpec(d, 'the groups')}
      />
      <EditableSection<PolicySpec>
        title="What"
        description="The block and allow lists it applies."
        value={spec}
        readOnlyReason={roReason}
        testId="section-what"
        view={<ListsView state={state} ns={ns} allowOnly={spec.allowOnly === true} />}
        edit={(d, set) => <WhatFields spec={d} onChange={set} {...fieldProps} />}
        onSave={(d) => saveSpec(d, 'the lists')}
      />
      <ServicesSection policy={policy} state={state} readOnlyReason={roReason} onSave={(sel) => saveSpec(withSelection(spec, sel), 'the blocked services')} />
      <EditableSection<PolicySpec>
        title="Content categories"
        description="Topic lists of the Block List Project, downloaded by URL and refreshed like any list."
        value={spec}
        readOnlyReason={roReason}
        testId="section-content"
        view={<ContentView state={state} ns={ns} />}
        edit={(d, set) => <ContentCategoryFields spec={d} onChange={set} {...fieldProps} />}
        onSave={(d) => saveSpec(d, 'the content categories')}
      />
      <EditableSection<PolicySpec>
        title="Safe search"
        value={spec}
        readOnlyReason={roReason}
        testId="section-safesearch"
        view={
          state?.safeSearch.enabled ? (
            <VStack gap={2}>
              <Text>
                On,{' '}
                <Text type="inherit" weight="semibold">
                  {state.safeSearch.mode === 'moderate' ? 'moderate (YouTube)' : 'strict'}
                </Text>
              </Text>
              <EngineList engines={state.safeSearch.engines} mode={state.safeSearch.mode} />
            </VStack>
          ) : (
            <Text color="secondary" as="p">
              Off: search results are not filtered.
            </Text>
          )
        }
        edit={(d, set) => <SafeSearchFields spec={d} onChange={set} {...fieldProps} />}
        onSave={(d) => saveSpec(d, 'safe search')}
      />
      <EditableSection<PolicySpec>
        title="How"
        description="What blocked clients get back and where allowed queries go."
        value={spec}
        readOnlyReason={roReason}
        testId="section-how"
        view={
          <MetadataList>
            <MetadataListItem label="Block response">{blockResponseLabel(state?.blockMode ?? spec.blockMode)}</MetadataListItem>
            <MetadataListItem label="Block TTL">{state?.blockTTL ?? spec.blockTTL ?? <Text color="secondary">default (blocking.blockTTL)</Text>}</MetadataListItem>
            <MetadataListItem label="Upstream group">
              {spec.upstreamGroup ? (
                <HStack gap={1} vAlign="center" wrap="wrap">
                  <Link href={`/upstreams/${encodeURIComponent(spec.upstreamGroup)}`} hasUnderline>
                    {spec.upstreamGroup}
                  </Link>
                  {state?.upstreamGroup && !state.upstreamGroup.exists ? <Badge variant="error" label="not configured" /> : null}
                </HStack>
              ) : (
                <Text color="secondary">default</Text>
              )}
            </MetadataListItem>
            <MetadataListItem label="Query log">{spec.queryLog === false ? 'Not logged' : 'Logged'}</MetadataListItem>
          </MetadataList>
        }
        edit={(d, set, showErrors) => <HowFields spec={d} onChange={set} showErrors={showErrors} {...fieldProps} />}
        validate={howError}
        onSave={(d) => saveSpec(d, 'the block settings')}
      />
      <EditableSection<PolicySpec>
        title="When"
        value={spec}
        readOnlyReason={roReason}
        testId="section-when"
        view={<ScheduleView state={state} />}
        edit={(d, set) => <WhenFields spec={d} onChange={set} {...fieldProps} />}
        validate={whenError}
        onSave={(d) => saveSpec(d, 'the schedule')}
      />
      <EditableSection<PolicySpec>
        title="Response processing"
        description="Client-specific rewrites, DNS64 and IPv6 filtering for forwarded queries."
        value={spec}
        readOnlyReason={roReason}
        testId="section-response"
        view={<Text>{responsePolicySummary(spec)}</Text>}
        edit={(d, set) => <ResponsePolicyFields spec={d} onChange={set} {...fieldProps} />}
        validate={responsePolicyError}
        onSave={(d) => saveSpec(d, 'response processing')}
      />
      <ActivitySection policyRef={`${ns}/${name}`} state={state} derived={st.derived} />

      <YamlDialog obj={policy} open={yaml} onClose={() => setYaml(false)} />
      <FormDialog
        isOpen={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete policy ${name}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirmDelete(false)} />
            <Button label="Delete policy" variant="destructive" isLoading={del.isPending} onClick={() => del.mutate()} />
          </>
        }
      >
        <Text as="p">Its groups fall back to their other policies (or the default policy). This cannot be undone.</Text>
      </FormDialog>
    </Page>
  );
}

function StatePanel({ state, derived, loading, error }: { state: PolicyState | undefined; derived: boolean; loading: boolean; error: unknown }) {
  if (error) return <ErrorBanner error={error} title="Could not load the policy state" />;
  if (loading || !state) return <Loading label="Loading state" />;
  const badge = policyBadge(state);
  const next = state.schedule?.nextChange;
  const ready = state.conditions.find((c) => c.type === 'Ready');
  const problems = state.conditions.filter((c) => c.type !== 'Ready' && c.status === 'True');
  return (
    <Panel title="Right now" headingId="policy-now" data-testid="policy-state" actions={derived ? <Text type="supporting">derived from the objects; this server does not report policy state</Text> : null}>
      <MetadataList>
        <MetadataListItem label="State">
          <HStack gap={1} vAlign="center" wrap="wrap">
            <Badge variant={badge.variant} label={badge.label} />
            <Text type="supporting">{badge.help}</Text>
          </HStack>
        </MetadataListItem>
        <MetadataListItem label="Schedule">
          {state.schedule ? (
            <VStack gap={1}>
              {state.schedule.error ? <FieldStatus type="error" variant="detached" message={`${state.schedule.error}; the policy applies at all times.`} /> : null}
              <Text>
                {state.schedule.summary} · {state.schedule.now ? 'on now' : 'off now'}
                {next ? (
                  <Text type="inherit" color="secondary">
                    {' '}
                    · turns {state.schedule.now ? 'off' : 'on'}{' '}
                    <Tooltip content={formatDateTime(next)}>
                      <time dateTime={next}>{formatRelative(next)}</time>
                    </Tooltip>
                  </Text>
                ) : null}
              </Text>
            </VStack>
          ) : (
            'Always'
          )}
        </MetadataListItem>
        <MetadataListItem label="Priority">{state.priority}</MetadataListItem>
        <MetadataListItem label="Shadowed by">
          {state.shadowedBy?.length ? (
            <Text>
              {state.shadowedBy.map((r, i) => (
                <Text key={r} type="inherit">
                  {i ? ', ' : ''}
                  <Link href={objectHref('policies', r)} type="inherit" hasUnderline>
                    {shortRef(r)}
                  </Link>
                </Text>
              ))}
            </Text>
          ) : (
            <Text color="secondary">nothing ranks higher for these groups</Text>
          )}
        </MetadataListItem>
        <MetadataListItem label="Conditions">
          <VStack gap={1}>
            {ready ? (
              <HStack gap={1} vAlign="center" wrap="wrap">
                <Badge variant={ready.status === 'True' ? 'success' : 'error'} label={ready.status === 'True' ? 'Ready' : 'Not ready'} />
                <Text size="sm">{ready.message || ready.reason}</Text>
              </HStack>
            ) : (
              <Text color="secondary">not reconciled yet</Text>
            )}
            {problems.map((c) => (
              <HStack key={c.type} gap={1} vAlign="center" wrap="wrap">
                <Badge variant="warning" label={c.type} />
                <Text size="sm">{c.message || c.reason}</Text>
              </HStack>
            ))}
          </VStack>
        </MetadataListItem>
      </MetadataList>
    </Panel>
  );
}

type GroupRow = PolicyState['groups'][number] & Record<string, unknown>;

function GroupsView({ state, ns }: { state: PolicyState | undefined; ns: string }) {
  if (!state) return null;
  if (!state.groups.length) return <Text color="secondary">No groups: the policy applies to nobody.</Text>;
  const columns: TableColumn<GroupRow>[] = [
    {
      key: 'ref',
      header: 'Group',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (g) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Link href={objectHref('groups', g.ref, ns)} color="inherit" hasUnderline>
            {shortRef(g.ref)}
          </Link>
          {!g.exists ? <Badge variant="error" label="missing" /> : !g.enabled ? <Badge variant="neutral" label="disabled, skipped" /> : null}
        </HStack>
      ),
    },
    { key: 'clients', header: 'Matchers', align: 'end', renderCell: (g) => <Text hasTabularNumbers>{formatExact(g.clients)}</Text> },
    {
      key: 'seen24h',
      header: 'Clients seen (24 h)',
      align: 'end',
      renderCell: (g) =>
        g.seen24h === undefined ? (
          <Tooltip content="No query log on this server">
            <Text color="secondary">–</Text>
          </Tooltip>
        ) : (
          <Text hasTabularNumbers>{formatExact(g.seen24h)}</Text>
        ),
    },
  ];
  return <Table<GroupRow> aria-label="Groups of the policy" density="compact" idKey="ref" data={state.groups} columns={columns} />;
}

type ListRow = PolicyState['lists'][number] & Record<string, unknown>;

function ListsView({ state, ns, allowOnly }: { state: PolicyState | undefined; ns: string; allowOnly: boolean }) {
  if (!state) return null;
  // A disabled list: its row in secondary text.
  const off = (l: ListRow) => l.exists && !l.enabled;
  const columns: TableColumn<ListRow>[] = [
    {
      key: 'ref',
      header: 'List',
      width: proportional(2, { minWidth: 140 }),
      renderCell: (l) => (
        <Link href={objectHref('lists', l.ref, ns)} color={off(l) ? 'secondary' : 'inherit'} hasUnderline>
          {shortRef(l.ref)}
        </Link>
      ),
    },
    {
      key: 'role',
      header: 'Used as',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: (l) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Badge variant={l.role === 'allow' ? 'green' : 'red'} label={l.role} />
          {l.exists && l.mode !== l.role ? <Text type="supporting">(an {l.mode} list)</Text> : null}
        </HStack>
      ),
    },
    {
      key: 'entries',
      header: 'Entries',
      align: 'end',
      renderCell: (l) => (
        <Text hasTabularNumbers color={off(l) ? 'secondary' : undefined}>
          {formatExact(l.entries)}
        </Text>
      ),
    },
    {
      key: 'sourcesEnabled',
      header: 'Sources on',
      align: 'end',
      renderCell: (l) => (
        <Text hasTabularNumbers color={off(l) ? 'secondary' : undefined}>
          {l.sourcesEnabled} of {l.sourcesTotal}
        </Text>
      ),
    },
    {
      key: 'ready',
      header: 'Health',
      renderCell: (l) =>
        !l.exists ? (
          <Badge variant="error" label="missing" />
        ) : !l.enabled ? (
          <Badge variant="neutral" label="disabled" />
        ) : l.ready ? (
          <Badge variant="success" label="ready" />
        ) : (
          <Badge variant="warning" label="not ready" />
        ),
    },
  ];
  return (
    <VStack gap={2}>
      {allowOnly ? <Banner status="warning" title="Allow only" description="Everything that is not on an allow list is blocked." /> : null}
      {state.lists.length ? (
        <Table<ListRow> aria-label="Lists of the policy" density="compact" idKey={(l) => `${l.role}-${l.ref}`} data={state.lists} columns={columns} />
      ) : (
        <Text color="secondary" as="p">
          No lists.
        </Text>
      )}
    </VStack>
  );
}

/** The content categories the policy turned on, with the health of their lists. */
function ContentView({ state, ns }: { state: PolicyState | undefined; ns: string }) {
  if (!state) return null;
  const on = state.lists.filter((l) => l.role === 'block' && contentCategoryOfList(l.ref));
  if (!on.length) return <Text color="secondary">None: no topic list applies.</Text>;
  return (
    <List density="compact" aria-label="Content categories on">
      {on.map((l) => (
        <ListItem
          key={l.ref}
          label={contentCategoryOfList(l.ref)?.name ?? l.ref}
          href={objectHref('lists', l.ref, ns)}
          endContent={
            !l.exists ? (
              <Badge variant="error" label="missing" />
            ) : !l.enabled ? (
              <Badge variant="neutral" label="disabled" />
            ) : l.ready ? (
              <Text type="supporting">{formatExact(l.entries)} names</Text>
            ) : (
              <Badge variant="warning" label="not ready" />
            )
          }
        />
      ))}
    </List>
  );
}

/** Blocked services: the logo grid and category switches, toggled in place and saved with one click. */
function ServicesSection({ policy, state, readOnlyReason: ro, onSave }: { policy: Policy; state: PolicyState | undefined; readOnlyReason: string; onSave: (sel: ServiceSelection) => Promise<void> }) {
  const current = selectionOf(policy.spec);
  const [draft, setDraft] = useState<ServiceSelection | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const value = draft ?? current;
  const dirty = draft !== null && !sameSelection(draft, current);
  const blocked = state?.effectiveServices ?? state?.services ?? [];
  return (
    <Panel
      title="Blocked services"
      headingId="sec-services"
      data-testid="section-services"
      actions={
        blocked.length ? (
          <LogoStack
            label="Blocked now"
            size={24}
            items={blocked.slice(0, 12).map((s) => ({ id: s.id, name: s.name, icon: s.icon, iconOnDark: s.iconOnDark, tooltip: s.known ? `${s.name}: ${s.rules} rules` : `${s.id}: not in the catalogue` }))}
            more={blocked.length - 12}
          />
        ) : null
      }
    >
      <ServicesFields spec={withSelection(policy.spec, value)} onChange={(s) => setDraft(selectionOf(s))} namespace={policy.metadata.namespace ?? 'default'} isDisabled={!!ro || saving} isLabelHidden />
      {error ? <ErrorBanner error={error} title="Could not save the blocked services" /> : null}
      {dirty ? (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Button
            label="Save services"
            variant="primary"
            isLoading={saving}
            onClick={() => {
              setSaving(true);
              setError(null);
              onSave(value)
                .then(() => setDraft(null))
                .catch(setError)
                .finally(() => setSaving(false));
            }}
          />
          <Button label="Discard" variant="ghost" onClick={() => setDraft(null)} />
          <Text type="supporting">
            {value.blocked.length} service{value.blocked.length === 1 ? '' : 's'}, {value.categories.length} categor{value.categories.length === 1 ? 'y' : 'ies'}
            {value.allowed.length ? `, ${value.allowed.length} exception${value.allowed.length === 1 ? '' : 's'}` : ''}
          </Text>
        </HStack>
      ) : null}
    </Panel>
  );
}

function ScheduleView({ state }: { state: PolicyState | undefined }) {
  if (!state?.schedule) return <Text as="p">Always: no schedule.</Text>;
  const next = state.schedule.nextChange;
  return (
    <Text as="p">
      {state.schedule.summary}. {state.schedule.now ? 'On now' : 'Off now'}
      {next ? (
        <>
          ; turns {state.schedule.now ? 'off' : 'on'} at <time dateTime={next}>{formatDateTime(next)}</time>.
        </>
      ) : (
        '.'
      )}
    </Text>
  );
}

function ActivitySection({ policyRef, state, derived }: { policyRef: string; state: PolicyState | undefined; derived: boolean }) {
  const log = `/queries?policy=${encodeURIComponent(policyRef)}`;
  const s = state?.stats24h;
  return (
    <Panel title="Last 24 hours" headingId="sec-activity" data-testid="section-activity" actions={
        !derived && s ? (
          <Link href={log} hasUnderline>
            Open in the query log
          </Link>
        ) : null
      }>
      {!state ? null : derived ? (
        <Text color="secondary" as="p">
          This server does not attribute queries to policies yet; the statistics appear once it serves GET /policies/{'{namespace}'}/{'{name}'}/state.
        </Text>
      ) : !s ? (
        <Text color="secondary" as="p">
          No query log on this server (querylog is off), so there are no statistics.
        </Text>
      ) : (
        <VStack gap={4}>
          <MetadataList orientation="horizontal">
            <MetadataListItem label="Queries">
              <Stat>{formatExact(s.queries)}</Stat>
            </MetadataListItem>
            <MetadataListItem label="Blocked">
              <Link href={`${log}&status_class=blocked`} size="2xl" weight="semibold" hasUnderline>
                {formatExact(s.blocked)}
              </Link>
            </MetadataListItem>
            <MetadataListItem label="Blocked share">
              <Stat>{formatPercent(s.blockedPercent)}</Stat>
            </MetadataListItem>
          </MetadataList>
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <VStack gap={2}>
              <Heading level={3}>Top blocked domains</Heading>
              {s.topBlocked.length ? (
                <RankList label="Top blocked domains" items={s.topBlocked.map((d) => ({ key: d.domain, label: d.domain, href: `${log}&domain=${encodeURIComponent(d.domain)}`, count: d.count }))} />
              ) : (
                <Text type="supporting">Nothing blocked.</Text>
              )}
            </VStack>
            <VStack gap={2}>
              <Heading level={3}>Top clients</Heading>
              {s.topClients.length ? (
                <RankList label="Top clients" items={s.topClients.map((c) => ({ key: c.key, label: c.name ?? c.key, href: clientHref(c.key), count: c.count }))} />
              ) : (
                <Text type="supporting">No queries.</Text>
              )}
            </VStack>
          </Grid>
        </VStack>
      )}
    </Panel>
  );
}

/** A figure of the 24 hours row. */
function Stat({ children }: { children: ReactNode }) {
  return (
    <Text size="2xl" weight="semibold" hasTabularNumbers>
      {children}
    </Text>
  );
}

/** A numbered top list: each row links to the query log, its count at the end. */
function RankList({ label, items }: { label: string; items: { key: string; label: string; href: string; count: number }[] }) {
  return (
    <List listStyle="decimal" density="compact" aria-label={label}>
      {items.map((i) => (
        <ListItem
          key={i.key}
          label={i.label}
          href={i.href}
          endContent={
            <Text type="supporting" hasTabularNumbers>
              {formatExact(i.count)}
            </Text>
          }
        />
      ))}
    </List>
  );
}
