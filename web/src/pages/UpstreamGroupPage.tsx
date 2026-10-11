// One upstream group (/upstreams/:group): its current state (servers
// healthy now, strategy, timeout, traffic), each server's health, RTT and
// 24 h traffic, the typed settings with their own Save, what uses the
// group (policies, forward zones, conditional rules) and Delete. A group
// that something still names cannot silently disappear: Delete lists them.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useQuery } from '@tanstack/react-query';
import { useEffect, useId, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { Policy } from '../api/types';
import { ErrorBanner, errorText, Loading } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { GroupFields, groupFieldErrors, healthSummary, ServersTable, strategyLabel, trafficFor, useUpstreams } from '../components/UpstreamGroup';
import { formatExact } from '../lib/format';
import { Link, useNavigate } from '../lib/router';
import { DEFAULT_GROUP, type UpstreamGroupConfig } from '../lib/upstreams';
import type { PageProps } from '../routes';

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
  const settingsId = useId();
  const usedId = useId();
  const [draft, setDraft] = useState<UpstreamGroupConfig | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [saveErr, setSaveErr] = useState<string>();
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string>();
  const [deleteBusy, setDeleteBusy] = useState(false);
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
      <div className="k-page">
        <h1 className="k-mono">{name}</h1>
        <Banner status="warning" title="No such upstream group" description={`upstreams.groups has no group named ${name}.`} />
        <p>
          <Link href="/upstreams">All upstream groups</Link> · <Link href="/upstreams/new">New group</Link>
        </p>
      </div>
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
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <p className="k-small k-muted">
            <Link href="/upstreams">Upstreams</Link> /
          </p>
          <h1 className="k-mono">{name}</h1>
          <div className="k-header-state" data-testid="group-state">
            {name === DEFAULT_GROUP ? <Badge variant="neutral" label="default" /> : null}
            <Badge variant="blue" label={strategyLabel(g.strategy)} />
            <span className="k-row k-small">
              {h.known === 0 ? (
                <span className="k-muted">no health data yet</span>
              ) : (
                <>
                  <StatusDot variant={h.healthy === h.known ? 'success' : h.healthy === 0 ? 'error' : 'warning'} label={`${h.healthy} of ${h.known} healthy`} />
                  {h.healthy} of {h.known} servers healthy
                </>
              )}
            </span>
            <span className="k-small k-muted">
              {g.servers.length} server{g.servers.length === 1 ? '' : 's'} · timeout {g.timeout || '2s'} · {formatExact(queries)} queries in 24 h
            </span>
          </div>
        </div>
        <div className="k-actions">
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
        </div>
      </div>
      {u.readOnlyReason ? <Banner status="info" title="Read-only here" description={u.readOnlyReason} /> : null}
      {u.notPersisted ? (
        <Banner status="warning" title="Kept in memory only" description="This server has no settings file, so the change is lost on restart." isDismissable onDismiss={u.clearNotPersisted} />
      ) : null}
      {u.stats.error ? <ErrorBanner error={u.stats.error} title="Upstream health is unavailable" /> : null}

      <section className="k-panel" aria-label="Servers">
        <h2 className="k-h3">Servers</h2>
        <ServersTable name={name} g={g} servers={servers} traffic={traffic} />
      </section>

      <section className="k-panel" aria-labelledby={settingsId}>
        <h2 id={settingsId} className="k-h3">
          Settings
        </h2>
        <GroupFields value={draft} onChange={setDraft} errors={errors} isDisabled={!u.canWrite} />
        {saveErr ? <Banner status="error" title="The server rejected the change" description={saveErr} /> : null}
        {dirty ? (
          <div className="k-row">
            <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
            <Button label="Cancel" size="sm" variant="ghost" onClick={() => setDraft(saved)} />
          </div>
        ) : null}
      </section>

      <section className="k-panel" aria-labelledby={usedId} data-testid="used-by">
        <h2 id={usedId} className="k-h3">
          Used by
        </h2>
        {name === DEFAULT_GROUP ? <p className="k-small">Every query no policy, forward zone or conditional rule sends elsewhere.</p> : null}
        {usersLoading ? <Loading /> : null}
        {users.length || conditional.length ? (
          <ul className="k-link-list k-stack-sm">
            {users.map((x) => (
              <li key={`${x.kind}/${x.name}`}>
                {x.kind === 'policy' ? 'Policy' : 'Forward zone'} {x.href ? <Link href={x.href} className="k-mono">{x.name}</Link> : <span className="k-mono">{x.name}</span>}
              </li>
            ))}
            {conditional.map((s) => (
              <li key={s}>
                Conditional rule <span className="k-mono">{s}</span>
              </li>
            ))}
          </ul>
        ) : !usersLoading && name !== DEFAULT_GROUP ? (
          <p className="k-muted k-small">No policy, forward zone or conditional rule names this group.</p>
        ) : null}
      </section>

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
        <p>Queries routed to a group that does not exist fail with SERVFAIL, so change the policies and forward zones that name it first.</p>
        {users.length ? <Banner status="warning" title="Still in use" description={`Named by ${users.map((x) => `${x.kind} ${x.name}`).join(', ')}.`} /> : null}
        {deleteErr ? <Banner status="error" title="The group was not deleted" description={deleteErr} /> : null}
      </FormDialog>
    </div>
  );
}
