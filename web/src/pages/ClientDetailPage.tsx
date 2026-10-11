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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../api/context';
import type { ClientGroup, QueryRow } from '../api/types';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { matcherFor } from '../components/fields/MatchersField';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { QueryLogTable } from '../components/QueryLogTable';
import { formatDateTime, formatExact, formatPercent, formatRelative } from '../lib/format';
import { objectOwnership } from '../lib/managedBy';
import { listEntryError } from '../lib/objectSchemas';
import { identifyClient, objectHref, refOfObject, scheduleSummary, selectPolicy, shortRef, splitRef, TIER_TEXT, type Selection } from '../lib/policies';
import { Link } from '../lib/router';
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
    mutationFn: (v: { action: 'allow' | 'deny'; domain: string }) => api.lists.addDomains(v.action, 'exact', [v.domain]),
    onSuccess: (r, v) => {
      if (r.processed.success.length) notify.success(`${v.action === 'allow' ? 'Allowed' : 'Denied'} ${v.domain} (list ui-${v.action})`);
      for (const e of r.processed.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      setDomain('');
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err) => notify.error(err, 'Could not change the list'),
  });
  const rowAction = useMutation({
    mutationFn: (v: { row: QueryRow; action: 'allow' | 'deny' }) => api.queries.action(v.row.id, v.action),
    onSuccess: (r, v) => {
      if (r.processed.success.length) notify.success(`${v.action === 'allow' ? 'Allowed' : 'Denied'} ${v.row.domain} (list ui-${v.action})`);
      for (const e of r.processed.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err, v) => notify.error(err, `Could not ${v.action} ${v.row.domain}`),
  });

  const writableGroups = ctx.groups.filter((g) => objectOwnership(g).editable && !membership.groups.includes(refOfObject(g)));
  const domainErr = domain.trim() ? listEntryError(domain.trim()) : null;
  const canLists = can('blocklists', 'write');
  const blockedPct = c && c.count ? (c.blocked / c.count) * 100 : undefined;

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/clients" className="k-small">
            ← Clients
          </Link>
          <h1>{display ?? key}</h1>
          <span className="k-row k-small">
            {display ? <span className="k-mono">{key}</span> : null}
            {c?.last_seen ? (
              <span>
                last seen <time dateTime={c.last_seen} title={formatDateTime(c.last_seen)}>{formatRelative(c.last_seen)}</time>
              </span>
            ) : c ? null : (
              <span className="k-muted">no queries in the last 24 hours</span>
            )}
          </span>
        </div>
      </div>
      {clients.error ? <ErrorBanner error={clients.error} /> : null}
      {error ? <ErrorBanner error={error} /> : null}

      <dl className="k-stat-row k-panel" aria-label="Last 24 hours">
        <div>
          <dt>Queries (24 h)</dt>
          <dd>{formatExact(c?.count ?? 0)}</dd>
        </div>
        <div>
          <dt>Blocked</dt>
          <dd>
            <Link href={`/queries?client=${encodeURIComponent(key)}&status_class=blocked`}>{formatExact(c?.blocked ?? 0)}</Link>
          </dd>
        </div>
        <div>
          <dt>Blocked share</dt>
          <dd>{formatPercent(blockedPct)}</dd>
        </div>
      </dl>

      {!ready ? (
        <Loading />
      ) : (
        <div className="k-grid k-grid-2">
          <section className="k-panel k-section" aria-labelledby="sec-groups" data-testid="client-groups">
            <h2 id="sec-groups" className="k-h3">
              Groups
            </h2>
            {membership.groups.length ? (
              <ul className="k-link-list k-stack">
                {membership.groups.map((g) => {
                  const obj = ctx.groups.find((x) => refOfObject(x) === g);
                  const m = membership.matchers[g];
                  const removable = obj && objectOwnership(obj).editable && (obj.spec.clients ?? []).some((x) => x.ip === key || x.clientID === key);
                  return (
                    <li key={g} className="k-row">
                      <Link href={objectHref('groups', g)} className="k-badge-link">
                        <Badge variant={splitRef(g).name === 'default' ? 'neutral' : 'blue'} label={shortRef(g)} />
                      </Link>
                      <span className="k-muted k-small">{m ? `matched by ${m.ip ?? m.cidr ?? m.clientID}` : 'matches everyone'}</span>
                      {removable ? <Button label="Remove" size="sm" variant="ghost" isDisabled={!can('clientgroups', 'write')} isLoading={removeFrom.isPending} onClick={() => removeFrom.mutate(obj)} tooltip={`Remove ${key} from ${shortRef(g)}`} /> : null}
                    </li>
                  );
                })}
              </ul>
            ) : (
              <p className="k-muted">No group matches this client.</p>
            )}
            <p className="k-small">Matched by {TIER_TEXT[membership.tier]}.</p>
            {c?.group && !membership.groups.includes(c.group) ? (
              <Banner status="info" title={`The last query resolved to ${shortRef(c.group)}`} description="Group changes apply to the next query; MAC and hostname matchers are not evaluated here." />
            ) : null}
            <form
              className="k-row-end"
              onSubmit={(e) => {
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
              <Button type="submit" label="Add" variant="secondary" isDisabled={!pick[0] || !can('clientgroups', 'write')} isLoading={addTo.isPending} />
            </form>
          </section>
          <EffectivePolicy selection={selection} />
        </div>
      )}

      <section className="k-panel k-section" aria-labelledby="sec-quick">
        <h2 id="sec-quick" className="k-h3">
          Allow or deny a domain
        </h2>
        <p className="k-muted k-small">Adds the domain to ui-allow or ui-deny, the lists the query log's Allow and Deny buttons use; every policy with those lists follows.</p>
        <form
          className="k-row-end"
          onSubmit={(e) => {
            e.preventDefault();
          }}
        >
          <label className="k-label">
            Domain
            <input className="k-native-input k-mono" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="ads.example.com" aria-invalid={domainErr ? true : undefined} spellCheck={false} />
            {domainErr ? <span className="k-error-text">{domainErr}</span> : null}
          </label>
          <Button label="Allow" variant="secondary" isDisabled={!domain.trim() || !!domainErr || !canLists} tooltip={canLists ? undefined : 'Needs blocklists.write'} onClick={() => quick.mutate({ action: 'allow', domain: domain.trim() })} />
          <Button label="Deny" variant="secondary" isDisabled={!domain.trim() || !!domainErr || !canLists} tooltip={canLists ? undefined : 'Needs blocklists.write'} onClick={() => quick.mutate({ action: 'deny', domain: domain.trim() })} />
        </form>
      </section>

      <section className="k-stack" aria-labelledby="sec-recent">
        <div className="k-panel-title">
          <h2 id="sec-recent" className="k-h3">
            Recent queries
          </h2>
          <Link href={`/queries?client=${encodeURIComponent(key)}`}>All queries of this client</Link>
        </div>
        {recent.error ? <ErrorBanner error={recent.error} title="The query log could not be read" /> : null}
        {recent.isLoading ? <Loading /> : null}
        {recent.data ? <QueryLogTable rows={recent.data.queries} hasMore={false} loadingMore={false} onLoadMore={() => undefined} canAct={canLists} onAction={(row, a) => rowAction.mutate({ row, action: a })} /> : null}
      </section>
    </div>
  );
}

function EffectivePolicy({ selection }: { selection: Selection }) {
  const w = selection.winner;
  return (
    <section className="k-panel k-section" aria-labelledby="sec-policy" data-testid="client-policy">
      <h2 id="sec-policy" className="k-h3">
        Policy right now
      </h2>
      {w ? (
        <p>
          <Link href={objectHref('policies', w.ref)}>
            <strong>{shortRef(w.ref)}</strong>
          </Link>{' '}
          decides:{' '}
          {selection.fallback
            ? 'no policy of its groups is active, so the default policy applies.'
            : selection.active.length > 1
              ? `the highest priority (${w.policy.spec.priority ?? 0}) of ${selection.active.length} active policies; their lists are merged.`
              : 'the only active policy of its groups.'}
        </p>
      ) : (
        <p>No policy applies: nothing is filtered for this client.</p>
      )}
      {selection.candidates.length ? (
        <ol className="k-policy-order" aria-label="Candidate policies">
          {selection.candidates.map((c) => (
            <li key={c.ref}>
              <Link href={objectHref('policies', c.ref)}>{shortRef(c.ref)}</Link>{' '}
              {c.active ? <Badge variant={c === w ? 'success' : 'info'} label={c === w ? 'decides' : 'active, lists merged'} /> : <Badge variant="neutral" label={c.off ?? 'off'} />}{' '}
              <span className="k-muted k-small">
                priority {c.policy.spec.priority ?? 0} · {c.policy.spec.schedule ? scheduleSummary(c.policy.spec.schedule) : 'always'}
              </span>
            </li>
          ))}
        </ol>
      ) : (
        <p className="k-muted k-small">No policy names its groups.</p>
      )}
    </section>
  );
}
