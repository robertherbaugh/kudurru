// One upstream group (/upstreams/:group): its current state (servers
// healthy now, strategy, timeout, traffic), each server's health, RTT and
// 24 h traffic, the typed settings with their own Save, what uses the
// group (policies, forward zones, conditional rules) and Delete. A group
// that something still names cannot silently disappear: Delete lists them.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { HStack } from '@astryxdesign/core/Stack';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { Policy } from '../api/types';
import { ErrorBanner, errorText, Loading } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { UpstreamWizardDialog } from '../components/wizards/UpstreamWizard';
import { GroupFields, groupFieldErrors, healthSummary, ServersTable, strategyLabel, trafficFor, useUpstreams } from '../components/UpstreamGroup';
import { formatExact } from '../lib/format';
import { useNavigate } from '../lib/router';
import { DEFAULT_GROUP, type UpstreamGroupConfig } from '../lib/upstreams';
import type { PageProps } from '../routes';

/** Group names in the page title keep the heading size in the code font. */
const MONO = { fontFamily: 'var(--font-family-code)' };

export interface GroupUser {
  kind: 'policy' | 'zone' | 'conditional';
  name: string;
  href?: string;
}

/** What names a group: policies (spec.upstreamGroup), forward zones and conditional rules. */
export function useGroupUsers(name: string): { users: GroupUser[]; isLoading: boolean } {
  const api = useApi();
  const can = useCan();
  const policies = useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies'), enabled: can('policies', 'read') });
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list(), enabled: can('zones', 'read') });
  const users: GroupUser[] = [
    ...(policies.data?.items ?? [])
      .filter((p) => (p.spec.upstreamGroup ?? '') === name)
      .map((p) => ({ kind: 'policy' as const, name: `${p.metadata.namespace ?? 'default'}/${p.metadata.name}`, href: `/policies/${encodeURIComponent(p.metadata.namespace ?? 'default')}/${encodeURIComponent(p.metadata.name)}` })),
    ...(zones.data?.items ?? [])
      .filter((z) => z.type === 'forward' && (z.upstreamGroup || DEFAULT_GROUP) === name)
      .map((z) => ({ kind: 'zone' as const, name: z.zoneName, href: `/zones/${encodeURIComponent(z.name)}` })),
  ];
  return { users, isLoading: policies.isLoading || zones.isLoading };
}

export default function UpstreamGroupPage({ params }: PageProps) {
  const name = params.group ?? '';
  const navigate = useNavigate();
  const u = useUpstreams();
  const g = u.cfg.groups[name];
  const { users, isLoading: usersLoading } = useGroupUsers(name);
  const conditional = Object.entries(u.cfg.conditional)
    .filter(([, grp]) => grp === name)
    .map(([suffix]) => suffix);
  const [draft, setDraft] = useState<UpstreamGroupConfig | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [saveErr, setSaveErr] = useState<string>();
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string>();
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [creating, setCreating] = useState(false);
  const gKey = JSON.stringify(g ?? null);
  useEffect(() => {
    setDraft(g ? { ...g, strategy: g.strategy || 'parallel_best', timeout: g.timeout || '2s' } : null);
    setErrors({});
    // eslint-disable-next-line react-hooks/exhaustive-deps -- reset when the saved group changes
  }, [gKey]);

  if (u.settings.isLoading) return <Loading />;
  if (u.settings.error) return <ErrorBanner error={u.settings.error} />;
  if (!g || !draft) {
    return (
      <Page>
        <PageHeader
          title={
            <Text type="inherit" style={MONO}>
              {name}
            </Text>
          }
        />
        <Banner status="warning" title="No such upstream group" description={`upstreams.groups has no group named ${name}.`} />
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Link href="/upstreams">All upstream groups</Link>
          <Button label="New group" size="sm" variant="secondary" isDisabled={!u.canWrite} onClick={() => setCreating(true)} />
        </HStack>
        <UpstreamWizardDialog isOpen={creating} onClose={() => setCreating(false)} />
      </Page>
    );
  }

  const servers = u.stats.data?.servers;
  const traffic = u.stats.data?.upstreams;
  const h = healthSummary(servers, name, g.servers);
  const queries = g.servers.reduce((sum, e) => sum + (trafficFor(traffic, name, e)?.total ?? 0), 0);
  const saved = { ...g, strategy: g.strategy || 'parallel_best', timeout: g.timeout || '2s' };
  const dirty = JSON.stringify(draft) !== JSON.stringify(saved);

  const save = async () => {
    const clean = { ...draft, servers: draft.servers.map((s) => s.trim()).filter(Boolean) };
    const e = groupFieldErrors(clean);
    setErrors(e);
    if (Object.keys(e).length) return;
    setBusy(true);
    setSaveErr(undefined);
    try {
      await u.save({ groups: { [name]: clean } }, `Group ${name}`);
    } catch (err) {
      setSaveErr(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (conditional.length) {
      setDeleteErr(`Conditional forwarding still sends ${conditional.join(', ')} to ${name}; change those rules first.`);
      return;
    }
    setDeleteBusy(true);
    setDeleteErr(undefined);
    try {
      await u.save({ groups: { [name]: null } }, `Group ${name} deleted;`);
      navigate('/upstreams');
    } catch (err) {
      setDeleteErr(errorText(err));
    } finally {
      setDeleteBusy(false);
    }
  };

  return (
    <Page>
      <PageHeader
        back={{ href: '/upstreams', label: 'Upstreams' }}
        title={
          <Text type="inherit" style={MONO}>
            {name}
          </Text>
        }
        meta={
          <HStack gap={2} vAlign="center" wrap="wrap" data-testid="group-state">
            {name === DEFAULT_GROUP ? <Badge variant="neutral" label="default" /> : null}
            <Badge variant="blue" label={strategyLabel(g.strategy)} />
            {h.known === 0 ? (
              <Text type="supporting">no health data yet</Text>
            ) : (
              <HStack gap={2} vAlign="center">
                <StatusDot variant={h.healthy === h.known ? 'success' : h.healthy === 0 ? 'error' : 'warning'} label={`${h.healthy} of ${h.known} healthy`} />
                <Text size="sm">
                  {h.healthy} of {h.known} servers healthy
                </Text>
              </HStack>
            )}
            <Text type="supporting">
              {g.servers.length} server{g.servers.length === 1 ? '' : 's'} · timeout {g.timeout || '2s'} · {formatExact(queries)} queries in 24 h
            </Text>
          </HStack>
        }
        actions={
          <Button
            label="Delete"
            variant="ghost"
            isDisabled={!u.canWrite || name === DEFAULT_GROUP}
            tooltip={name === DEFAULT_GROUP ? 'The default group is required' : undefined}
            onClick={() => {
              setDeleteErr(undefined);
              setDeleting(true);
            }}
          />
        }
      />
      {u.readOnlyReason ? <Banner status="info" title="Read-only here" description={u.readOnlyReason} /> : null}
      {u.notPersisted ? (
        <Banner status="warning" title="Kept in memory only" description="This server has no settings file, so the change is lost on restart." isDismissable onDismiss={u.clearNotPersisted} />
      ) : null}
      {u.stats.error ? <ErrorBanner error={u.stats.error} title="Upstream health is unavailable" /> : null}

      <Panel title="Servers" aria-label="Servers">
        <ServersTable name={name} g={g} servers={servers} traffic={traffic} />
      </Panel>

      <Panel title="Settings">
        <GroupFields value={draft} onChange={setDraft} errors={errors} isDisabled={!u.canWrite} />
        {saveErr ? <Banner status="error" title="The server rejected the change" description={saveErr} /> : null}
        {dirty ? (
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
            <Button label="Cancel" size="sm" variant="ghost" onClick={() => setDraft(saved)} />
          </HStack>
        ) : null}
      </Panel>

      <Panel title="Used by" data-testid="used-by">
        {name === DEFAULT_GROUP ? (
          <Text as="p" size="sm">
            Every query no policy, forward zone or conditional rule sends elsewhere.
          </Text>
        ) : null}
        {usersLoading ? <Loading /> : null}
        {users.length || conditional.length ? (
          <List density="compact" hasDividers edgeCompensation="inline" aria-label={`What uses ${name}`}>
            {users.map((x) => (
              <ListItem key={`${x.kind}/${x.name}`} label={x.name} href={x.href} endContent={<Text type="supporting">{x.kind === 'policy' ? 'Policy' : 'Forward zone'}</Text>} />
            ))}
            {conditional.map((s) => (
              <ListItem key={s} label={s} endContent={<Text type="supporting">Conditional rule</Text>} />
            ))}
          </List>
        ) : !usersLoading && name !== DEFAULT_GROUP ? (
          <Text as="p" type="supporting">
            No policy, forward zone or conditional rule names this group.
          </Text>
        ) : null}
      </Panel>

      <FormDialog
        isOpen={deleting}
        onClose={() => setDeleting(false)}
        title={`Delete upstream group ${name}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setDeleting(false)} />
            <Button label="Delete" variant="destructive" isLoading={deleteBusy} onClick={() => void remove()} />
          </>
        }
      >
        <Text as="p">Queries routed to a group that does not exist fail with SERVFAIL, so change the policies and forward zones that name it first.</Text>
        {users.length ? <Banner status="warning" title="Still in use" description={`Named by ${users.map((x) => `${x.kind} ${x.name}`).join(', ')}.`} /> : null}
        {deleteErr ? <Banner status="error" title="The group was not deleted" description={deleteErr} /> : null}
      </FormDialog>
    </Page>
  );
}
