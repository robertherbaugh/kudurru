// One client group (/groups/:ns/:name): its matchers as a typed editor
// (IP address, CIDR range, client ID, MAC address, each validated), the
// clients of the last 24 hours that resolved to it, and the policies that
// apply to its members in the order the server ranks them. Enable/disable,
// delete and "View as YAML" sit in the header.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, ClientItem, ClientMatcher } from '../api/types';
import { EditableSection } from '../components/EditableSection';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { cleanMatchers, kindLabel, matcherKind, matchersError, matcherValue, MatchersField } from '../components/fields/MatchersField';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { ManagedBadge } from '../components/ManagedBadge';
import { PolicyWizardDialog } from '../components/wizards/PolicyWizard';
import { YamlDialog } from '../components/YamlDialog';
import { formatExact, formatRelative } from '../lib/format';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { clientHref, DEFAULT_GROUP, derivePolicyState, identifyClient, objectHref, policiesOfGroup, policyBadge, refOfObject, scheduleSummary } from '../lib/policies';
import { useNavigate } from '../lib/router';
import { useDeriveContext, usePolicyLimit } from '../lib/usePolicyData';
import type { PageProps } from '../routes';

const BACK = { href: '/groups', label: 'Groups' };

/** Lists drawn by Stacks: no markers or indent. */
const LIST_RESET = { listStyle: 'none', padding: 0, margin: 0 } as const;

export default function GroupDetailPage({ params }: PageProps) {
  const ns = params.ns ?? 'default';
  const name = params.name ?? '';
  const ref = `${ns}/${name}`;
  const api = useApi();
  const can = useCan();
  const scope = useScopeParams();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [yaml, setYaml] = useState(false);
  const [newPolicy, setNewPolicy] = useState(false);

  const q = useQuery({ queryKey: [...qk.objects('clientgroups'), ns, name], queryFn: () => api.objects.get<ClientGroup>('clientgroups', name, ns) });
  const clients = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list(scope, { window: '24h' }), refetchInterval: 60_000, enabled: can('clients', 'read') });
  const { ctx, ready } = useDeriveContext();
  const policyLimit = usePolicyLimit(ctx.policies);
  const group = q.data;

  const put = async (next: ClientGroup) => {
    await api.objects.put('clientgroups', next);
    await qc.invalidateQueries({ queryKey: qk.objects('clientgroups') });
  };
  const toggle = useMutation({
    mutationFn: (enabled: boolean) => put(withMeta(group as ClientGroup, objectComment(group as ClientGroup), enabled)),
    onSuccess: (_r, enabled) => notify.success(`${name} ${enabled ? 'enabled' : 'disabled'}`),
    onError: (err) => notify.error(err, `Could not change ${name}`),
  });
  const del = useMutation({
    mutationFn: () => api.objects.delete('clientgroups', name, ns),
    onSuccess: () => {
      notify.success(`Deleted group ${name}`);
      // Only the list: refetching this page's object would answer 404.
      navigate('/groups');
      void qc.invalidateQueries({ queryKey: qk.objects('clientgroups'), exact: true });
    },
    onError: (err) => notify.error(err, 'Delete failed'),
  });

  // Clients the server resolved to this group, plus those the matchers
  // would put into it (a client can be in several groups of one tier).
  const members = useMemo(() => {
    const all = clients.data?.items ?? [];
    return all.filter((c) => c.group === ref || (ready && identifyClient(c.key, ctx.groups).groups.includes(ref)));
  }, [clients.data, ref, ready, ctx.groups]);

  if (q.error) {
    return (
      <Page>
        <PageHeader title={`Group ${name}`} back={BACK} />
        <ErrorBanner error={q.error} />
      </Page>
    );
  }
  if (!group) return <Loading />;

  const own = objectOwnership(group);
  const canWrite = can('clientgroups', 'write');
  const roReason = !own.editable ? readOnlyReason(own) : !canWrite ? 'Needs clientgroups.write' : '';
  const enabled = objectEnabled(group);
  const comment = objectComment(group);
  const matchers = group.spec.clients ?? [];
  const applied = policiesOfGroup(group, ctx.policies);
  const ready_ = group.status?.conditions?.find((c) => c.type === 'Ready');
  const problems = (group.status?.conditions ?? []).filter((c) => c.type !== 'Ready' && c.status === 'True');

  return (
    <Page>
      <PageHeader
        title={name}
        back={BACK}
        meta={
          <>
            <Badge variant={enabled ? 'success' : 'neutral'} label={enabled ? 'Enabled' : 'Disabled'} />
            <Text size="sm">
              {matchers.length} matcher{matchers.length === 1 ? '' : 's'}
            </Text>
            <Text size="sm">{formatExact(group.status?.members ?? 0)} members counted</Text>
            {ready_ ? <Badge variant={ready_.status === 'True' ? 'success' : 'error'} label={ready_.status === 'True' ? 'Ready' : 'Not ready'} /> : null}
            <ManagedBadge owner={group.metadata.annotations?.[MANAGED_BY]} />
          </>
        }
        description={comment || undefined}
        actions={
          <>
            <Button label={enabled ? 'Disable' : 'Enable'} variant="secondary" isDisabled={!!roReason} tooltip={roReason || undefined} isLoading={toggle.isPending} onClick={() => toggle.mutate(!enabled)} />
            <Button label="View as YAML" variant="ghost" onClick={() => setYaml(true)} />
            <Button label="Delete" variant="destructive" isDisabled={!!roReason || !can('clientgroups', 'delete') || name === DEFAULT_GROUP} tooltip={name === DEFAULT_GROUP ? 'The default group is recreated at start-up' : roReason || undefined} onClick={() => setConfirmDelete(true)} />
          </>
        }
      />
      {!own.editable ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      {problems.map((c) => (
        <Banner key={c.type} status="warning" title={c.reason ?? c.type} description={c.message} />
      ))}

      <EditableSection<ClientMatcher[]>
        title="Members"
        description="A client is in the group when any one matcher matches it."
        value={matchers}
        readOnlyReason={roReason}
        testId="section-matchers"
        view={<MatchersView matchers={matchers} />}
        edit={(d, set, showErrors) => <MatchersField value={d} onChange={set} showErrors={showErrors} />}
        validate={matchersError}
        onSave={async (d) => {
          await put({ ...group, spec: { ...group.spec, clients: cleanMatchers(d) } });
          notify.success(`Saved the members of ${name}`);
        }}
      />

      <EditableSection<boolean>
        title="UDP rate limit"
        description="When several groups match at the winning identification tier, an exemption in any of them applies. DNS policies and access rules still apply."
        value={group.spec.rateLimitExempt ?? false}
        readOnlyReason={roReason}
        view={<Text>{group.spec.rateLimitExempt ? 'Members are exempt from the UDP query rate limit.' : 'Members follow the configured UDP query rate limit and global exemptions.'}</Text>}
        edit={(draft, setDraft) => <CheckboxInput label="Exempt members from the UDP rate limit" value={draft} onChange={setDraft} />}
        onSave={async (rateLimitExempt) => {
          await put({ ...group, spec: { ...group.spec, rateLimitExempt } });
          notify.success(`Saved the UDP rate limit for ${name}`);
        }}
      />

      <Panel
        title="Seen in the last 24 hours"
        headingId="sec-seen"
        actions={
          <Link href="/clients" hasUnderline>
            All clients
          </Link>
        }
      >
        {clients.error ? <ErrorBanner error={clients.error} /> : null}
        {clients.isLoading ? <Loading /> : null}
        {clients.data ? (
          members.length ? (
            <MembersTable members={members} groupRef={ref} label={`Clients of ${name}`} />
          ) : (
            <Text color="secondary" as="p">
              No client of this group sent a query in the last 24 hours.
            </Text>
          )
        ) : null}
      </Panel>

      <Panel
        title="Policies that apply"
        headingId="sec-pols"
        actions={
          <Button
            label="New policy"
            size="sm"
            variant="secondary"
            isDisabled={!can('policies', 'write') || !!policyLimit}
            tooltip={can('policies', 'write') ? (policyLimit?.title ?? 'A policy for this group') : 'Needs policies.write'}
            onClick={() => setNewPolicy(true)}
          />
        }
      >
        {applied.length ? (
          <>
            <VStack as="ol" gap={1.5} aria-label={`Policies of ${name}`} style={LIST_RESET}>
              {applied.map((p, i) => {
                const b = policyBadge(derivePolicyState(p, ctx));
                return (
                  <HStack as="li" key={p.metadata.name} gap={2} vAlign="center" wrap="wrap">
                    <Text type="supporting" hasTabularNumbers>
                      {i + 1}.
                    </Text>
                    <Link href={objectHref('policies', refOfObject(p))} hasUnderline>
                      {p.metadata.name}
                    </Link>
                    <Badge variant={b.variant} label={b.label} />
                    <Text type="supporting">
                      priority {p.spec.priority ?? 0} · {p.spec.schedule ? scheduleSummary(p.spec.schedule) : 'always'}
                    </Text>
                  </HStack>
                );
              })}
            </VStack>
            <Text type="supporting" as="p">
              The first active policy decides the block response, upstream group and safe search; the lists of every active policy are merged.
            </Text>
          </>
        ) : (
          <Text color="secondary" as="p">
            No policy names this group, so its members get the default policy.
          </Text>
        )}
      </Panel>

      <YamlDialog obj={group} open={yaml} onClose={() => setYaml(false)} />
      <PolicyWizardDialog isOpen={newPolicy} onClose={() => setNewPolicy(false)} namespace={ns} groups={[name]} />
      <FormDialog
        isOpen={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title={`Delete group ${name}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirmDelete(false)} />
            <Button label="Delete group" variant="destructive" isLoading={del.isPending} onClick={() => del.mutate()} />
          </>
        }
      >
        <Text as="p">
          Its members fall back to their other groups or the default group
          {applied.length ? `, and ${applied.map((p) => p.metadata.name).join(', ')} stop applying to them` : ''}. This cannot be undone.
        </Text>
      </FormDialog>
    </Page>
  );
}

type MemberRow = ClientItem & Record<string, unknown>;

/** The clients of the last 24 hours that resolved to the group or that its matchers would put into it. */
function MembersTable({ members, groupRef, label }: { members: ClientItem[]; groupRef: string; label: string }) {
  const columns: TableColumn<MemberRow>[] = [
    {
      key: 'key',
      header: 'Client',
      width: proportional(3, { minWidth: 180 }),
      renderCell: (c) => (
        <Text>
          <Link href={clientHref(c.key)} type="inherit" color="inherit" hasUnderline>
            {c.name ?? c.key}
          </Link>
          {c.name ? (
            <Text type="supporting">
              {' '}
              <Code color="inherit" size="inherit">
                {c.key}
              </Code>
            </Text>
          ) : null}
          {c.group && c.group !== groupRef ? <Text type="supporting"> (resolved to {c.group.replace(/^default\//, '')})</Text> : null}
        </Text>
      ),
    },
    {
      key: 'last_seen',
      header: 'Last seen',
      width: proportional(1.5, { minWidth: 110 }),
      renderCell: (c) =>
        c.last_seen ? (
          <Tooltip content={c.last_seen}>
            <Text>{formatRelative(c.last_seen)}</Text>
          </Tooltip>
        ) : (
          <Text color="secondary">–</Text>
        ),
    },
    { key: 'count', header: 'Queries', align: 'end', renderCell: (c) => <Text hasTabularNumbers>{formatExact(c.count)}</Text> },
    { key: 'blocked', header: 'Blocked', align: 'end', renderCell: (c) => <Text hasTabularNumbers>{formatExact(c.blocked)}</Text> },
  ];
  return <Table<MemberRow> aria-label={label} density="compact" idKey="key" data={members} columns={columns} />;
}

type MatcherRow = ClientMatcher & { index: number } & Record<string, unknown>;

function MatchersView({ matchers }: { matchers: ClientMatcher[] }) {
  if (!matchers.length) return <Text color="secondary">No matchers: the group matches nobody.</Text>;
  const columns: TableColumn<MatcherRow>[] = [
    {
      key: 'kind',
      header: 'Kind',
      width: proportional(1, { minWidth: 120 }),
      renderCell: (m) => {
        const kind = matcherKind(m);
        return (
          <HStack gap={1} vAlign="center" wrap="wrap">
            <Text>{kindLabel(kind)}</Text>
            {kind === 'mac' || kind === 'hostname' || kind === 'nameGlob' ? (
              <Tooltip content={kind === 'mac' ? 'Matches only when the transport supplies MAC addresses (inventory integrations: v0.2).' : 'No hostname source in this version: never matches.'}>
                <Badge variant="warning" label="limited" />
              </Tooltip>
            ) : null}
          </HStack>
        );
      },
    },
    { key: 'value', header: 'Value', width: proportional(1.5, { minWidth: 140 }), renderCell: (m) => <Text type="code">{matcherValue(m)}</Text> },
    { key: 'name', header: 'Display name', renderCell: (m) => (m.name ? <Text>{m.name}</Text> : <Text color="secondary">–</Text>) },
    { key: 'comment', header: 'Comment', renderCell: (m) => <Text size="sm">{m.comment ?? ''}</Text> },
  ];
  return <Table<MatcherRow> aria-label="Matchers" density="compact" idKey="index" data={matchers.map((m, index) => ({ ...m, index }))} columns={columns} />;
}
