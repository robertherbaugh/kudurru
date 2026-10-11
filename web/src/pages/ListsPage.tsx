// Block and allow lists: health, counts and last update per list, refresh,
// multi-add of exact or regex domains into the ui-allow / ui-deny lists
// (POST /domains/{allow|deny}/{exact|regex}, processed{success,errors}),
// and "which lists contain this domain" (GET /blocklists/_search).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Dialog } from '@astryxdesign/core/Dialog';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { BlockList, Processed } from '../api/types';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { ObjectsPage } from '../components/ObjectsPage';
import { formatExact, formatRelative } from '../lib/format';
import { listHealth } from '../lib/listHealth';
import { listEntryError, SCHEMAS } from '../lib/objectSchemas';

function AddDomainsDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const titleId = useId();
  const [action, setAction] = useState<'deny' | 'allow'>('deny');
  const [kind, setKind] = useState<'exact' | 'regex'>('exact');
  const [text, setText] = useState('');
  const [result, setResult] = useState<Processed['processed'] | null>(null);
  const domains = text.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean);
  const localErrors = domains
    .map((d) => [d, kind === 'regex' ? listEntryError(d.startsWith('/') ? d : `/${d}/`) : listEntryError(d)] as const)
    .filter(([, e]) => e);
  const m = useMutation({
    mutationFn: () => api.lists.addDomains(action, kind, domains),
    onSuccess: (r) => {
      setResult(r.processed);
      if (r.processed.success.length) notify.success(`Added ${r.processed.success.length} to ui-${action}`);
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err) => notify.error(err, 'Could not add domains'),
  });
  return (
    <Dialog
      isOpen={open}
      onOpenChange={(o) => {
        if (!o) {
          setResult(null);
          onClose();
        }
      }}
      purpose="form"
      width={560}
      aria-labelledby={titleId}
    >
      <form
        className="k-stack-lg"
        onSubmit={(e) => {
          e.preventDefault();
          if (domains.length && localErrors.length === 0) m.mutate();
        }}
      >
        <h2 id={titleId} className="k-dialog-title">
          Add domains
        </h2>
        <div className="k-row">
          <SegmentedControl label="List" value={action} onChange={(v) => setAction(v as 'allow' | 'deny')}>
            <SegmentedControlItem value="deny" label="Deny (block)" />
            <SegmentedControlItem value="allow" label="Allow" />
          </SegmentedControl>
          <SegmentedControl label="Match" value={kind} onChange={(v) => setKind(v as 'exact' | 'regex')}>
            <SegmentedControlItem value="exact" label="Exact" />
            <SegmentedControlItem value="regex" label="Regex" />
          </SegmentedControl>
        </div>
        <TextArea
          label="Domains"
          description={`One per line or separated by spaces; they go into the "ui-${action}" list (created on demand).`}
          value={text}
          onChange={(v) => {
            setText(v);
            setResult(null);
          }}
          rows={6}
          hasSpellCheck={false}
          status={localErrors.length ? { type: 'error', message: `${localErrors[0]?.[0]}: ${localErrors[0]?.[1]}` } : undefined}
        />
        {result ? (
          <div className="k-stack" role="status">
            <p>
              {result.success.length} added, {result.errors.length} rejected.
            </p>
            {result.errors.length ? (
              <ul className="k-small">
                {result.errors.map((e) => (
                  <li key={e.item}>
                    <code>{e.item}</code>: {e.error}
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        ) : null}
        <div className="k-row">
          <span className="k-spacer" />
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label={`Add ${domains.length || ''}`.trim()} variant="primary" isDisabled={!domains.length || localErrors.length > 0} isLoading={m.isPending} />
        </div>
      </form>
    </Dialog>
  );
}

function ListSearch() {
  const api = useApi();
  const [domain, setDomain] = useState('');
  const [submitted, setSubmitted] = useState('');
  const q = useQuery({ queryKey: ['lists', 'search', submitted], queryFn: () => api.lists.search(submitted), enabled: !!submitted });
  return (
    <section className="k-panel" aria-labelledby="list-search">
      <h2 id="list-search" className="k-h3">
        Which lists contain a domain?
      </h2>
      <form
        className="k-row-end"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(domain.trim());
        }}
      >
        <TextInput label="Domain" value={domain} onChange={setDomain} placeholder="ads.example.com" />
        <Button type="submit" label="Search" variant="secondary" />
      </form>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.data ? (
        q.data.items.length === 0 ? (
          <p className="k-muted">No list contains {submitted}.</p>
        ) : (
          <ul>
            {q.data.items.map((h) => (
              <li key={`${h.namespace}/${h.list}/${h.rule}`}>
                <Badge variant={h.mode === 'allow' ? 'success' : 'error'} label={h.mode} /> <strong>{h.list}</strong> matches with <code>{h.rule}</code>
              </li>
            ))}
          </ul>
        )
      ) : null}
    </section>
  );
}

export default function ListsPage() {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const refresh = useMutation({
    mutationFn: (l: BlockList) => api.lists.refresh(l.metadata.namespace ?? 'default', l.metadata.name),
    onSuccess: (_r, l) => {
      notify.success(`Refreshing ${l.metadata.name}`);
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    },
    onError: (err, l) => notify.error(err, `Could not refresh ${l.metadata.name}`),
  });
  return (
    <ObjectsPage<BlockList>
      title="Lists"
      description="Block and allow lists from URLs, inline entries or ConfigMaps. Policies decide which client groups use them."
      schema={SCHEMAS.blocklists}
      headerActions={<Button label="Add domains" variant="secondary" isDisabled={!can('blocklists', 'write')} onClick={() => setAdding(true)} />}
      rowActions={(l) =>
        l.spec.sources.some((s) => s.url) ? (
          <Button label="Refresh" size="sm" variant="ghost" isDisabled={!can('blocklists', 'refresh')} isLoading={refresh.isPending && refresh.variables === l} onClick={() => refresh.mutate(l)} />
        ) : null
      }
      columns={[
        { header: 'Mode', cell: (l) => <Badge variant={l.spec.mode === 'allow' ? 'success' : 'error'} label={l.spec.mode ?? 'block'} /> },
        {
          header: 'Sources',
          cell: (l) => (
            <span className="k-small k-break">
              {l.spec.sources.map((s) => s.url ?? (s.inline ? `${s.inline.length} inline` : s.configMapRef ? `ConfigMap ${s.configMapRef.name}` : s.name)).join(', ')}
            </span>
          ),
        },
        { header: 'Entries', className: 'num', cell: (l) => formatExact(l.status?.entries) },
        {
          header: 'Health',
          cell: (l) => {
            const h = listHealth(l);
            return (
              <span title={h.detail}>
                <Badge variant={h.tone === 'neutral' ? 'neutral' : h.tone} label={h.label} />
              </span>
            );
          },
        },
        { header: 'Updated', cell: (l) => <span title={l.status?.lastFetched}>{l.status?.lastFetched ? formatRelative(l.status.lastFetched) : '–'}</span> },
      ]}
    >
      <ListSearch />
      <AddDomainsDialog open={adding} onClose={() => setAdding(false)} />
    </ObjectsPage>
  );
}
