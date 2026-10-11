// One policy (/policies/:ns/:name): what it does right now (active,
// scheduled off, disabled or shadowed, the next schedule change, priority,
// conditions) from GET /policies/{ns}/{name}/state (ux-plan C3), and one
// section per concept, each with its own Edit and Save: who, what, blocked
// services (logo toggles saved in place), safe search, how, when, and the
// last 24 hours of activity. Enable/disable, delete and "View as YAML" sit
// in the header; objects managed elsewhere show the same page read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { Policy, PolicySpec, PolicyState } from '../api/types';
import { EditableSection } from '../components/EditableSection';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { ManagedBadge } from '../components/ManagedBadge';
import { cleanPolicySpec, HowFields, howError, SafeSearchFields, ServicesFields, WhatFields, WhenFields, whenError, WhoFields, whoError } from '../components/PolicyFields';
import { EngineList } from '../components/ServiceGrid';
import { ServiceIcon } from '../components/ServiceIcon';
import { YamlDialog } from '../components/YamlDialog';
import { formatDateTime, formatExact, formatPercent, formatRelative } from '../lib/format';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { blockResponseLabel, clientHref, objectHref, policyBadge, shortRef } from '../lib/policies';
import { Link, useNavigate } from '../lib/router';
import { usePolicyState } from '../lib/usePolicyData';
import type { PageProps } from '../routes';

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
    await put({ ...policy, spec: cleanPolicySpec(spec) });
    notify.success(`Saved ${what} of ${name}`);
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
      <div className="k-page">
        <Link href="/policies" className="k-small">
          ← Policies
        </Link>
        <h1>Policy {name}</h1>
        <ErrorBanner error={q.error} />
      </div>
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
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/policies" className="k-small">
            ← Policies
          </Link>
          <h1>{name}</h1>
          <span className="k-row k-small">
            {badge ? <Badge variant={badge.variant} label={badge.label} /> : null}
            <span>priority {spec.priority ?? 0}</span>
            {ns !== 'default' ? <span className="k-muted">namespace {ns}</span> : null}
            <ManagedBadge owner={policy.metadata.annotations?.[MANAGED_BY]} />
          </span>
          {comment ? <p className="k-muted k-small">{comment}</p> : null}
        </div>
        <div className="k-actions">
          <Button label={enabled ? 'Disable' : 'Enable'} variant="secondary" isDisabled={!!roReason} tooltip={roReason || undefined} isLoading={toggle.isPending} onClick={() => toggle.mutate(!enabled)} />
          <Button label="View as YAML" variant="ghost" onClick={() => setYaml(true)} />
          <Button label="Delete" variant="destructive" isDisabled={!!roReason || !can('policies', 'delete')} tooltip={roReason || undefined} onClick={() => setConfirmDelete(true)} />
        </div>
      </div>
      {!own.editable ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}

      <StatePanel state={state} derived={st.derived} loading={st.isLoading} error={st.error} />

      <EditableSection<PolicySpec>
        title="Who"
        description="The client groups this policy applies to."
        value={spec}
        readOnlyReason={roReason}
        testId="section-who"
        view={<GroupsView state={state} />}
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
        view={<ListsView state={state} allowOnly={spec.allowOnly === true} />}
        edit={(d, set) => <WhatFields spec={d} onChange={set} {...fieldProps} />}
        onSave={(d) => saveSpec(d, 'the lists')}
      />
      <ServicesSection policy={policy} state={state} readOnlyReason={roReason} onSave={(ids) => saveSpec({ ...spec, blockedServices: ids }, 'the blocked services')} />
      <EditableSection<PolicySpec>
        title="Safe search"
        value={spec}
        readOnlyReason={roReason}
        testId="section-safesearch"
        view={
          state?.safeSearch.enabled ? (
            <div className="k-stack">
              <span>
                On, <strong>{state.safeSearch.mode === 'moderate' ? 'moderate (YouTube)' : 'strict'}</strong>
              </span>
              <EngineList engines={state.safeSearch.engines} mode={state.safeSearch.mode} />
            </div>
          ) : (
            <p className="k-muted">Off: search results are not filtered.</p>
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
          <dl className="k-kv">
            <dt>Block response</dt>
            <dd>{blockResponseLabel(state?.blockMode ?? spec.blockMode)}</dd>
            <dt>Block TTL</dt>
            <dd>{state?.blockTTL ?? spec.blockTTL ?? <span className="k-muted">default (blocking.blockTTL)</span>}</dd>
            <dt>Upstream group</dt>
            <dd>
              {spec.upstreamGroup ? (
                <>
                  <Link href={`/upstreams/${encodeURIComponent(spec.upstreamGroup)}`}>{spec.upstreamGroup}</Link>
                  {state?.upstreamGroup && !state.upstreamGroup.exists ? <Badge variant="error" label="not configured" /> : null}
                </>
              ) : (
                <span className="k-muted">default</span>
              )}
            </dd>
            <dt>Query log</dt>
            <dd>{spec.queryLog === false ? 'Not logged' : 'Logged'}</dd>
          </dl>
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
        <p>Its groups fall back to their other policies (or the default policy). This cannot be undone.</p>
      </FormDialog>
    </div>
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
    <section className="k-panel" aria-labelledby="policy-now" data-testid="policy-state">
      <div className="k-panel-title">
        <h2 id="policy-now">Right now</h2>
        {derived ? <span className="k-muted k-small">derived from the objects; this server does not report policy state</span> : null}
      </div>
      <dl className="k-kv">
        <dt>State</dt>
        <dd>
          <Badge variant={badge.variant} label={badge.label} /> <span className="k-muted k-small">{badge.help}</span>
        </dd>
        <dt>Schedule</dt>
        <dd>
          {state.schedule ? (
            <>
              {state.schedule.summary} · {state.schedule.now ? 'on now' : 'off now'}
              {next ? (
                <span className="k-muted">
                  {' '}
                  · turns {state.schedule.now ? 'off' : 'on'} <time dateTime={next} title={formatDateTime(next)}>{formatRelative(next)}</time>
                </span>
              ) : null}
            </>
          ) : (
            'Always'
          )}
        </dd>
        <dt>Priority</dt>
        <dd>{state.priority}</dd>
        <dt>Shadowed by</dt>
        <dd>
          {state.shadowedBy?.length ? (
            state.shadowedBy.map((r, i) => (
              <span key={r}>
                {i ? ', ' : ''}
                <Link href={objectHref('policies', r)}>{shortRef(r)}</Link>
              </span>
            ))
          ) : (
            <span className="k-muted">nothing ranks higher for these groups</span>
          )}
        </dd>
        <dt>Conditions</dt>
        <dd>
          {ready ? (
            <span>
              <Badge variant={ready.status === 'True' ? 'success' : 'error'} label={ready.status === 'True' ? 'Ready' : 'Not ready'} /> <span className="k-small">{ready.message || ready.reason}</span>
            </span>
          ) : (
            <span className="k-muted">not reconciled yet</span>
          )}
          {problems.map((c) => (
            <div key={c.type} className="k-small">
              <Badge variant="warning" label={c.type} /> {c.message || c.reason}
            </div>
          ))}
        </dd>
      </dl>
    </section>
  );
}

function GroupsView({ state }: { state: PolicyState | undefined }) {
  if (!state) return null;
  if (!state.groups.length) return <p className="k-muted">No groups: the policy applies to nobody.</p>;
  return (
    <div className="k-table-wrap">
      <table className="k-table" aria-label="Groups of the policy">
        <thead>
          <tr>
            <th scope="col">Group</th>
            <th scope="col" className="num">
              Matchers
            </th>
            <th scope="col" className="num">
              Clients seen (24 h)
            </th>
          </tr>
        </thead>
        <tbody>
          {state.groups.map((g) => (
            <tr key={g.ref}>
              <td>
                <Link href={objectHref('groups', g.ref)}>{shortRef(g.ref)}</Link> {g.exists ? null : <Badge variant="error" label="missing" />}
              </td>
              <td className="num">{formatExact(g.clients)}</td>
              <td className="num">{formatExact(g.seen24h)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ListsView({ state, allowOnly }: { state: PolicyState | undefined; allowOnly: boolean }) {
  if (!state) return null;
  return (
    <div className="k-stack">
      {allowOnly ? <Banner status="warning" title="Allow only" description="Everything that is not on an allow list is blocked." /> : null}
      {state.lists.length ? (
        <div className="k-table-wrap">
          <table className="k-table" aria-label="Lists of the policy">
            <thead>
              <tr>
                <th scope="col">List</th>
                <th scope="col">Mode</th>
                <th scope="col" className="num">
                  Entries
                </th>
                <th scope="col" className="num">
                  Sources on
                </th>
                <th scope="col">Health</th>
              </tr>
            </thead>
            <tbody>
              {state.lists.map((l) => (
                <tr key={`${l.mode}-${l.ref}`}>
                  <td>
                    <Link href={objectHref('lists', l.ref)}>{shortRef(l.ref)}</Link>
                  </td>
                  <td>
                    <Badge variant={l.mode === 'allow' ? 'green' : 'red'} label={l.mode === 'allow' ? 'allow' : 'block'} />
                  </td>
                  <td className="num">{formatExact(l.entries)}</td>
                  <td className="num">
                    {l.sourcesEnabled} of {l.sourcesTotal}
                  </td>
                  <td>{!l.exists ? <Badge variant="error" label="missing" /> : l.ready ? <Badge variant="success" label="ready" /> : <Badge variant="warning" label="not ready" />}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="k-muted">No lists.</p>
      )}
    </div>
  );
}

/** Blocked services: the logo grid, toggled in place and saved with one click. */
function ServicesSection({ policy, state, readOnlyReason: ro, onSave }: { policy: Policy; state: PolicyState | undefined; readOnlyReason: string; onSave: (ids: string[]) => Promise<void> }) {
  const current = policy.spec.blockedServices ?? [];
  const [draft, setDraft] = useState<string[] | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const value = draft ?? current;
  const dirty = draft !== null && (draft.length !== current.length || draft.some((d) => !current.includes(d)));
  const blocked = state?.services ?? [];
  return (
    <section className="k-panel k-section" aria-labelledby="sec-services" data-testid="section-services">
      <div className="k-panel-title">
        <h2 id="sec-services">Blocked services</h2>
        {blocked.length ? (
          <ul className="k-logo-stack" aria-label="Blocked now">
            {blocked.map((s) => (
              <li key={s.id} title={s.known ? `${s.name}: ${s.rules} rules` : `${s.id}: not in the catalogue`}>
                <ServiceIcon id={s.id} name={s.name} icon={s.icon} size={24} />
              </li>
            ))}
          </ul>
        ) : null}
      </div>
      <ServicesFields spec={{ ...policy.spec, blockedServices: value }} onChange={(s) => setDraft(s.blockedServices ?? [])} namespace={policy.metadata.namespace ?? 'default'} isDisabled={!!ro || saving} isLabelHidden />
      {error ? <ErrorBanner error={error} title="Could not save the blocked services" /> : null}
      {dirty ? (
        <div className="k-row">
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
          <span className="k-muted k-small">{value.length} selected (was {current.length})</span>
        </div>
      ) : null}
    </section>
  );
}

function ScheduleView({ state }: { state: PolicyState | undefined }) {
  if (!state?.schedule) return <p>Always: no schedule.</p>;
  const next = state.schedule.nextChange;
  return (
    <p>
      {state.schedule.summary}. {state.schedule.now ? 'On now' : 'Off now'}
      {next ? (
        <>
          ; turns {state.schedule.now ? 'off' : 'on'} at <time dateTime={next}>{formatDateTime(next)}</time>.
        </>
      ) : (
        '.'
      )}
    </p>
  );
}

function ActivitySection({ policyRef, state, derived }: { policyRef: string; state: PolicyState | undefined; derived: boolean }) {
  const log = `/queries?policy=${encodeURIComponent(policyRef)}`;
  const s = state?.stats24h;
  return (
    <section className="k-panel k-section" aria-labelledby="sec-activity" data-testid="section-activity">
      <div className="k-panel-title">
        <h2 id="sec-activity">Last 24 hours</h2>
        {!derived ? <Link href={log}>Open in the query log</Link> : null}
      </div>
      {derived || !s ? (
        <p className="k-muted">This server does not attribute queries to policies yet; the statistics appear once it serves GET /policies/{'{namespace}'}/{'{name}'}/state.</p>
      ) : (
        <div className="k-stack-lg">
          <dl className="k-stat-row">
            <div>
              <dt>Queries</dt>
              <dd>{formatExact(s.queries)}</dd>
            </div>
            <div>
              <dt>Blocked</dt>
              <dd>
                <Link href={`${log}&status_class=blocked`}>{formatExact(s.blocked)}</Link>
              </dd>
            </div>
            <div>
              <dt>Blocked share</dt>
              <dd>{formatPercent(s.blockedPercent)}</dd>
            </div>
          </dl>
          <div className="k-grid k-grid-2">
            <div className="k-stack">
              <h3 className="k-h3">Top blocked domains</h3>
              {s.topBlocked.length ? (
                <ol className="k-rank">
                  {s.topBlocked.map((d) => (
                    <li key={d.domain}>
                      <Link href={`${log}&domain=${encodeURIComponent(d.domain)}`} className="k-mono">
                        {d.domain}
                      </Link>
                      <span className="num">{formatExact(d.count)}</span>
                    </li>
                  ))}
                </ol>
              ) : (
                <p className="k-muted k-small">Nothing blocked.</p>
              )}
            </div>
            <div className="k-stack">
              <h3 className="k-h3">Top clients</h3>
              {s.topClients.length ? (
                <ol className="k-rank">
                  {s.topClients.map((c) => (
                    <li key={c.key}>
                      <Link href={clientHref(c.key)}>{c.name ?? c.key}</Link>
                      <span className="num">{formatExact(c.count)}</span>
                    </li>
                  ))}
                </ol>
              ) : (
                <p className="k-muted k-small">No queries.</p>
              )}
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
