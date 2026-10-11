// One block or allow list (/lists/:ns/:name): its current state (enabled,
// mode, health, entries, unsupported rules, last refresh, owner, which
// policies use it), the sources table with Pi-hole-style enable switches
// (spec.sources[].enabled, ux-plan C2), the inline entries browser
// (?source=<name>), the list-level settings with their own Save, and "View
// as YAML". Objects managed elsewhere show the same page read-only.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { BlockList, BlockResponse, Condition, ListSource, Policy } from '../api/types';
import { DurationField } from '../components/fields/DurationField';
import { EnumSelect } from '../components/fields/EnumSelect';
import { ErrorBanner, errorText, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { EntriesBrowser } from '../components/lists/EntriesBrowser';
import { SourceDialog } from '../components/lists/SourceDialog';
import { SourcesTable } from '../components/lists/SourcesTable';
import { ManagedBadge } from '../components/ManagedBadge';
import { YamlDialog } from '../components/YamlDialog';
import { formatDateTime, formatExact, formatRelative } from '../lib/format';
import { listHealth } from '../lib/listHealth';
import { listKey, MODE_OPTIONS, RESPONSE_OPTIONS, sourceName, sourcesSummary, withSourceEnabled } from '../lib/lists';
import { MANAGED_BY, objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { durationError } from '../lib/objectSchemas';
import { Link, useNavigate, useSearchParams } from '../lib/router';
import { isIPv4, isIPv6 } from '../lib/rr';
import type { PageProps } from '../routes';
import { inlineOnly, STATUS_POLL_MS } from './ListsPage';

export const REFRESH_PRESETS = [
  { value: '1h', label: 'Every hour' },
  { value: '6h', label: 'Every 6 hours' },
  { value: '12h', label: 'Every 12 hours' },
  { value: '24h', label: 'Every day' },
  { value: '72h', label: 'Every 3 days' },
  { value: '168h', label: 'Every week' },
];

/** The Ready condition in words: "Ready", "Ready: all sources disabled", "Degraded: …", "Not ready: …". */
export function readyText(c: Condition): string {
  const plain = !c.reason || c.reason === 'AsExpected' || c.reason === 'Ready';
  const head = c.status === 'True' ? (plain ? 'Ready' : c.reason) : c.status === 'False' ? 'Not ready' : (c.reason ?? 'Pending');
  return c.message ? `${head}: ${c.message}` : (head ?? 'Ready');
}

/** Policies that reference the list (bare names resolve in the policy's namespace). */
export function policiesUsing(policies: Policy[], ns: string, name: string): { policy: Policy; as: 'block' | 'allow' }[] {
  const refers = (p: Policy, ref: string) => ref === `${ns}/${name}` || (ref === name && (p.metadata.namespace ?? 'default') === ns);
  return policies.flatMap((p): { policy: Policy; as: 'block' | 'allow' }[] => {
    if ((p.spec.blockListRefs ?? []).some((r) => refers(p, r))) return [{ policy: p, as: 'block' }];
    if ((p.spec.allowListRefs ?? []).some((r) => refers(p, r))) return [{ policy: p, as: 'allow' }];
    return [];
  });
}

export default function ListDetailPage({ params }: PageProps) {
  const ns = params.ns ?? 'default';
  const name = params.name ?? '';
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  const key = listKey(ns, name);

  const [polling, setPolling] = useState(0);
  useEffect(() => {
    if (!polling) return undefined;
    const t = window.setTimeout(() => setPolling(0), STATUS_POLL_MS);
    return () => window.clearTimeout(t);
  }, [polling]);

  const q = useQuery({
    queryKey: key,
    queryFn: () => api.objects.get<BlockList>('blocklists', name, ns),
    refetchInterval: polling > 0 ? 2_000 : false,
  });
  const policies = useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies') });

  const [sourceDialog, setSourceDialog] = useState<{ index: number | null; key: number } | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const [deletingList, setDeletingList] = useState(false);
  const [yaml, setYaml] = useState(false);

  const invalidate = () => qc.invalidateQueries({ queryKey: qk.objects('blocklists') });

  /** Whole-object PUT with If-Match; the answer replaces the cached list. */
  const save = async (next: BlockList): Promise<BlockList> => {
    const saved = await api.objects.put<BlockList>('blocklists', next);
    qc.setQueryData(key, saved);
    void invalidate();
    setPolling((n) => n + 1);
    return saved;
  };

  const toggle = useMutation({
    mutationFn: (v: { base: BlockList; index: number; enabled: boolean }) => api.objects.put<BlockList>('blocklists', withSourceEnabled(v.base, v.index, v.enabled)),
    // Optimistic: the switch moves at once and moves back when the write fails.
    onMutate: async (v) => {
      await qc.cancelQueries({ queryKey: key });
      const prev = qc.getQueryData<BlockList>(key);
      if (prev) qc.setQueryData(key, withSourceEnabled(prev, v.index, v.enabled));
      return { prev };
    },
    onError: (err, v, ctx) => {
      if (ctx?.prev) qc.setQueryData(key, ctx.prev);
      const src = v.base.spec.sources[v.index];
      notify.error(err, `Could not ${v.enabled ? 'enable' : 'disable'} ${src ? sourceName(v.index, src) : 'the source'}`);
    },
    onSuccess: (saved, v) => {
      qc.setQueryData(key, saved);
      const src = v.base.spec.sources[v.index];
      notify.success(`${src ? sourceName(v.index, src) : 'Source'} ${v.enabled ? 'enabled' : 'disabled'}`);
      setPolling((n) => n + 1);
    },
    onSettled: () => void invalidate(),
  });

  const listToggle = useMutation({
    mutationFn: (v: { base: BlockList; enabled: boolean }) => save(withMeta(v.base, objectComment(v.base), v.enabled)),
    onSuccess: (_r, v) => notify.success(`${name} ${v.enabled ? 'enabled' : 'disabled'}`),
    onError: (err) => notify.error(err, `Could not change ${name}`),
  });

  const refresh = useMutation({
    mutationFn: () => api.lists.refresh(ns, name),
    onSuccess: () => {
      notify.success(`Refreshing ${name}`);
      setPolling((n) => n + 1);
      void invalidate();
    },
    onError: (err) => notify.error(err, `Could not refresh ${name}`),
  });

  const removeSource = useMutation({
    mutationFn: (v: { base: BlockList; index: number }) => save({ ...v.base, spec: { ...v.base.spec, sources: v.base.spec.sources.filter((_s, i) => i !== v.index) } }),
    onSuccess: () => {
      notify.success('Source deleted');
      setDeleting(null);
    },
    onError: (err) => notify.error(err, 'Could not delete the source'),
  });

  const removeList = useMutation({
    mutationFn: () => api.objects.delete('blocklists', name, ns),
    onSuccess: () => {
      notify.success(`Deleted ${name}`);
      qc.removeQueries({ queryKey: key });
      void invalidate();
      navigate('/lists');
    },
    onError: (err) => notify.error(err, `Could not delete ${name}`),
  });

  if (q.error) {
    return (
      <div className="k-page">
        <Link href="/lists" className="k-small">
          ← Lists
        </Link>
        <h1>List {name}</h1>
        <ErrorBanner error={q.error} />
      </div>
    );
  }
  const list = q.data;
  if (!list) return <Loading label="Loading list" />;

  const own = objectOwnership(list);
  const canWrite = can('blocklists', 'write');
  const editable = own.editable && canWrite;
  const health = listHealth(list);
  const enabled = objectEnabled(list);
  const comment = objectComment(list);
  const st = list.status;
  const sources = list.spec.sources;
  const usedBy = policiesUsing(policies.data?.items ?? [], ns, name);
  const browsing = sp.get('source');
  const browseIndex = browsing === null ? -1 : sources.findIndex((s, i) => sourceName(i, s) === browsing);
  const taken = (except: number | null) => sources.flatMap((s, i) => (i === except ? [] : [sourceName(i, s)]));
  const editingIndex = sourceDialog?.index ?? null;
  const ready = st?.conditions?.find((c) => c.type === 'Ready');

  const saveSource = async (src: ListSource) => {
    const next = editingIndex === null ? [...sources, src] : sources.map((s, i) => (i === editingIndex ? src : s));
    await save({ ...list, spec: { ...list.spec, sources: next } });
    notify.success(editingIndex === null ? `Added source ${src.name ?? ''}`.trim() : `Saved source ${src.name ?? ''}`.trim());
  };

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/lists" className="k-small">
            ← Lists
          </Link>
          <h1>
            {name}
            {ns !== 'default' ? <span className="k-muted k-small"> ({ns})</span> : null}
          </h1>
          {comment ? <p className="k-muted k-small">{comment}</p> : null}
          <div className="k-row k-small">
            <Switch
              label="Enabled"
              size="sm"
              value={enabled}
              isDisabled={!editable || listToggle.isPending}
              isLoading={listToggle.isPending}
              onChange={(on) => listToggle.mutate({ base: list, enabled: on })}
            />
            <Badge variant={list.spec.mode === 'allow' ? 'success' : 'error'} label={list.spec.mode === 'allow' ? 'allow list' : 'block list'} />
            <span title={health.detail}>
              <Badge variant={health.tone === 'neutral' ? 'neutral' : health.tone} label={health.label} />
            </span>
            <ManagedBadge owner={list.metadata.annotations?.[MANAGED_BY]} />
          </div>
        </div>
        <div className="k-actions">
          {sources.some((s) => s.url) ? (
            <Button label="Refresh now" variant="secondary" isDisabled={!can('blocklists', 'refresh') || !enabled} tooltip={enabled ? undefined : 'Disabled lists are not fetched'} isLoading={refresh.isPending} onClick={() => refresh.mutate()} />
          ) : null}
          <Button label="View as YAML" variant="ghost" onClick={() => setYaml(true)} />
          {own.editable ? <Button label="Delete list" variant="ghost" isDisabled={!can('blocklists', 'delete')} onClick={() => setDeletingList(true)} /> : null}
        </div>
      </div>

      {!own.editable ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      {!enabled ? <Banner status="warning" title="This list is disabled" description="It is kept but not fetched or applied; turn it on with the Enabled switch." /> : null}
      {policies.data && usedBy.length === 0 ? (
        <Banner status="warning" title="No policy uses this list" description="It has no effect until a policy references it (Policies → a policy → block or allow lists)." />
      ) : null}

      <section className="k-panel" aria-labelledby="list-state">
        <h2 id="list-state" className="k-visually-hidden">
          Current state
        </h2>
        <dl className="k-facts">
          <div>
            <dt>Entries</dt>
            <dd>{formatExact(st?.entries)}</dd>
          </div>
          <div>
            <dt>Sources</dt>
            <dd>{sourcesSummary(sources)}</dd>
          </div>
          <div>
            <dt>Invalid lines</dt>
            <dd>{formatExact(st?.invalid ?? 0)}</dd>
          </div>
          <div>
            <dt title="Valid rules this version parses but does not apply: $important, $badfilter, mid-pattern wildcards, hosts rewrites">Unsupported rules</dt>
            <dd data-testid="unsupported">
              {formatExact(st?.unsupported ?? 0)}
              {st?.unsupported ? <span className="k-muted k-small"> parsed, not applied</span> : null}
            </dd>
          </div>
          <div>
            <dt>Last refresh</dt>
            <dd>
              {st?.lastFetched ? (
                <span title={formatDateTime(st.lastFetched)}>{formatRelative(st.lastFetched)}</span>
              ) : inlineOnly(sources) ? (
                <span className="k-muted">inline, nothing to fetch</span>
              ) : (
                <span className="k-muted">not yet</span>
              )}
            </dd>
          </div>
          <div>
            <dt>Refresh interval</dt>
            <dd>{list.spec.refreshInterval ?? <span className="k-muted">24h (default)</span>}</dd>
          </div>
          <div>
            <dt>Status</dt>
            <dd className="k-break">{ready ? readyText(ready) : <span className="k-muted">pending</span>}</dd>
          </div>
          <div>
            <dt>Used by</dt>
            <dd>
              {usedBy.length
                ? usedBy.map(({ policy, as }, i) => (
                    <span key={`${policy.metadata.namespace ?? ''}/${policy.metadata.name}`}>
                      {i ? ', ' : ''}
                      <Link href={`/policies?q=${encodeURIComponent(policy.metadata.name)}`}>{policy.metadata.name}</Link>
                      {as === 'allow' && list.spec.mode !== 'allow' ? ' (as allow list)' : ''}
                    </span>
                  ))
                : '–'}
            </dd>
          </div>
        </dl>
      </section>

      <section className="k-panel k-stack" aria-labelledby="list-sources">
        <div className="k-panel-title">
          <h2 id="list-sources">Sources</h2>
          {editable ? <Button label="Add source" variant="primary" onClick={() => setSourceDialog({ index: null, key: Date.now() })} /> : null}
        </div>
        <p className="k-muted k-small">Turn a source off to stop fetching and applying it; its cached copy is kept, so turning it back on is instant.</p>
        <SourcesTable
          list={list}
          editable={editable}
          pending={toggle.isPending ? (toggle.variables?.index ?? null) : null}
          onToggle={(index, on) => toggle.mutate({ base: list, index, enabled: on })}
          onEdit={(index) => setSourceDialog({ index, key: Date.now() })}
          onDelete={(index) => setDeleting(index)}
          onBrowse={(index) => {
            const src = sources[index];
            if (src) setParams({ source: sourceName(index, src) }, { push: true });
          }}
        />
      </section>

      {browsing !== null ? (
        browseIndex >= 0 ? (
          <EntriesBrowser key={browsing} list={list} index={browseIndex} editable={editable} onClose={() => setParams({ source: null }, { push: true })} />
        ) : (
          <Banner status="warning" title="No such source" description={`${name} has no source named ${browsing}.`} />
        )
      ) : null}

      <ListSettings key={`${ns}/${name}`} list={list} editable={editable} onSave={save} />

      {sourceDialog ? (
        <SourceDialog
          key={sourceDialog.key}
          open
          onClose={() => setSourceDialog(null)}
          initial={editingIndex === null ? undefined : sources[editingIndex]}
          taken={taken(editingIndex)}
          onSave={saveSource}
        />
      ) : null}

      <FormDialog
        isOpen={deleting !== null}
        onClose={() => setDeleting(null)}
        title={`Delete source ${deleting !== null && sources[deleting] ? sourceName(deleting, sources[deleting]) : ''}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setDeleting(null)} />
            <Button label="Delete source" variant="destructive" isLoading={removeSource.isPending} onClick={() => deleting !== null && removeSource.mutate({ base: list, index: deleting })} />
          </>
        }
      >
        <p>Its entries leave the list at the next compile. To pause it instead, turn its switch off.</p>
      </FormDialog>

      <FormDialog
        isOpen={deletingList}
        onClose={() => setDeletingList(false)}
        title={`Delete list ${name}?`}
        width={460}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setDeletingList(false)} />
            <Button label="Delete list" variant="destructive" isLoading={removeList.isPending} onClick={() => removeList.mutate()} />
          </>
        }
      >
        <p>{usedBy.length ? `Policies ${usedBy.map((u) => u.policy.metadata.name).join(', ')} stop using it.` : 'No policy uses it.'} This cannot be undone.</p>
      </FormDialog>

      <YamlDialog obj={list} open={yaml} onClose={() => setYaml(false)} />
    </div>
  );
}

/** Mode, block response, refresh interval, custom IPs and the comment, with their own Save. */
function ListSettings({ list, editable, onSave }: { list: BlockList; editable: boolean; onSave: (l: BlockList) => Promise<BlockList> }) {
  const notify = useNotify();
  const initial = {
    mode: list.spec.mode ?? 'block',
    response: list.spec.response ?? '',
    refreshInterval: list.spec.refreshInterval ?? '',
    v4: list.spec.customIP?.v4 ?? '',
    v6: list.spec.customIP?.v6 ?? '',
    comment: objectComment(list),
  };
  const [form, setForm] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const set = (patch: Partial<typeof form>) => setForm((f) => ({ ...f, ...patch }));
  const dirty = (Object.keys(initial) as (keyof typeof initial)[]).some((k) => initial[k] !== form[k]);
  const customIP = form.mode !== 'allow' && form.response === 'customIP';
  const errors = {
    refreshInterval: durationError(form.refreshInterval.trim()),
    v4: form.v4 && !isIPv4(form.v4.trim()) ? 'Not an IPv4 address' : customIP && !form.v4.trim() && !form.v6.trim() ? 'Give an IPv4 or IPv6 address' : null,
    v6: form.v6 && !isIPv6(form.v6.trim()) ? 'Not an IPv6 address' : null,
  };
  const valid = !errors.refreshInterval && !errors.v4 && !errors.v6;

  const submit = async () => {
    if (!valid) return;
    setBusy(true);
    setError(null);
    const spec = { ...list.spec, mode: form.mode };
    const response = form.mode === 'allow' ? '' : form.response;
    if (response) spec.response = response as BlockResponse;
    else delete spec.response;
    if (form.refreshInterval.trim()) spec.refreshInterval = form.refreshInterval.trim();
    else delete spec.refreshInterval;
    if (customIP) spec.customIP = { ...(form.v4.trim() ? { v4: form.v4.trim() } : {}), ...(form.v6.trim() ? { v6: form.v6.trim() } : {}) };
    else delete spec.customIP;
    try {
      await onSave(withMeta({ ...list, spec }, form.comment, objectEnabled(list)));
      notify.success(`Saved ${list.metadata.name}`);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="k-panel" aria-labelledby="list-settings">
      <form
        className="k-form"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <h2 id="list-settings" className="k-h3">
          Settings
        </h2>
        <div className="k-form-grid">
          <EnumSelect label="Mode" value={form.mode} options={MODE_OPTIONS} isDisabled={!editable} onChange={(v) => set({ mode: v as 'block' | 'allow' })} />
          {form.mode !== 'allow' ? (
            <EnumSelect label="Block response" value={form.response} options={RESPONSE_OPTIONS} emptyLabel="Default (the policy's, else the server's)" isDisabled={!editable} onChange={(v) => set({ response: v })} />
          ) : null}
          <DurationField
            label="Refresh interval"
            description="How often URL sources are fetched again."
            value={form.refreshInterval}
            presets={REFRESH_PRESETS}
            emptyLabel="Default (every day)"
            isDisabled={!editable}
            error={errors.refreshInterval ?? undefined}
            onChange={(v) => set({ refreshInterval: v })}
          />
        </div>
        {customIP ? (
          <div className="k-form-grid">
            <TextInput label="Custom IPv4" placeholder="192.0.2.1" value={form.v4} onChange={(v) => set({ v4: v })} isDisabled={!editable} status={errors.v4 ? { type: 'error', message: errors.v4 } : undefined} />
            <TextInput label="Custom IPv6" placeholder="2001:db8::1" value={form.v6} onChange={(v) => set({ v6: v })} isDisabled={!editable} status={errors.v6 ? { type: 'error', message: errors.v6 } : undefined} isOptional />
          </div>
        ) : null}
        <TextInput label="Comment" value={form.comment} onChange={(v) => set({ comment: v })} isDisabled={!editable} isOptional />
        {form.mode !== initial.mode ? (
          <Banner status="info" title={`Switching to ${form.mode === 'allow' ? 'an allow' : 'a block'} list`} description="Policies keep the reference under the old field; the list is applied by its new mode and the policy reports the mismatch." />
        ) : null}
        {error ? <Banner status="error" title="Could not save" description={error} /> : null}
        {editable ? (
          <div className="k-row">
            <Button type="submit" label="Save settings" variant="primary" isDisabled={!dirty} isLoading={busy} />
            <Button label="Reset" variant="ghost" isDisabled={!dirty} onClick={() => setForm(initial)} />
          </div>
        ) : null}
      </form>
    </section>
  );
}
