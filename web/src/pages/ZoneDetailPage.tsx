// One zone: its records flattened across record sets, the typed record
// editor, BIND import/export, NOTIFY and the transfer status.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useMemo, useState, type ChangeEvent } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { ZoneImportResult, ZoneRecord } from '../api/types';
import { ErrorBanner, errorText, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { ManagedBadge } from '../components/ManagedBadge';
import { RecordEditor } from '../components/RecordEditor';
import { ownership, readOnlyReason } from '../lib/managedBy';
import { Link } from '../lib/router';
import { dnsNameToOwner } from '../lib/rr';
import type { PageProps } from '../routes';
import { TransferCell } from './ZonesPage';

function ImportDialog({ zone, open, onClose }: { zone: string; open: boolean; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const fileId = useId();
  const [text, setText] = useState('');
  const [result, setResult] = useState<ZoneImportResult | null>(null);
  const m = useMutation({
    mutationFn: () => api.zones.importBind(zone, text),
    onSuccess: (r) => {
      setResult(r);
      notify.success(`Imported ${r.imported} records into ${zone}`);
      void qc.invalidateQueries({ queryKey: qk.zone(zone) });
      void qc.invalidateQueries({ queryKey: qk.zones });
    },
    onError: (err) => notify.error(err, 'Import failed'),
  });
  const onFile = (e: ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0];
    if (f) void f.text().then(setText);
  };
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={`Import a BIND zone file into ${zone}`}
      width={720}
      onSubmit={() => {
        if (text.trim()) m.mutate();
      }}
      footer={
        <>
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Import" variant="primary" isDisabled={!text.trim()} isLoading={m.isPending} />
        </>
      }
    >
      <label className="k-label" htmlFor={fileId}>
        Zone file
        <input id={fileId} type="file" accept=".zone,.db,.txt,text/plain,text/dns" onChange={onFile} />
      </label>
      <TextArea label="Or paste the zone text" value={text} onChange={setText} rows={12} hasSpellCheck={false} placeholder={'$ORIGIN example.internal.\nwww 300 IN A 192.0.2.10'} />
      <p className="k-muted k-small">Records land in one record set managed by the API; SOA and apex NS are taken from the zone object.</p>
      {result ? (
        <Banner
          status={result.skipped?.length ? 'warning' : 'success'}
          title={`${result.imported} records imported${result.recordSet ? ` into ${result.recordSet}` : ''}`}
          description={result.skipped?.length ? result.skipped.map((s) => `line ${s.line}: ${s.error}`).join('; ') : undefined}
        />
      ) : null}
    </FormDialog>
  );
}

function ExportDialog({ zone, open, onClose }: { zone: string; open: boolean; onClose: () => void }) {
  const api = useApi();
  const q = useQuery({ queryKey: [...qk.zone(zone), 'export'], queryFn: () => api.zones.exportText(zone), enabled: open });
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={`${zone} as a BIND zone file`}
      width={760}
      footer={
        <>
          <Button label="Download" variant="secondary" href={api.zones.exportURL(zone)} target="_blank" rel="noopener" />
          <Button label="Close" variant="primary" onClick={onClose} />
        </>
      }
    >
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? <pre className="k-pre">{q.data}</pre> : null}
    </FormDialog>
  );
}

export default function ZoneDetailPage({ params }: PageProps) {
  const zone = params.zone ?? '';
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [editing, setEditing] = useState<ZoneRecord | 'new' | null>(null);
  const [importing, setImporting] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [filter, setFilter] = useState('');

  const z = useQuery({ queryKey: qk.zone(zone), queryFn: () => api.zones.get(zone) });
  const recs = useQuery({ queryKey: qk.records(zone), queryFn: () => api.zones.records(zone) });
  const own = ownership(z.data?.managedBy);
  const zoneEditable = own.editable && z.data?.type === 'primary';
  const canWrite = can('records', 'write');

  const rows = useMemo(() => {
    const all = recs.data?.items ?? [];
    const f = filter.trim().toLowerCase();
    return f ? all.filter((r) => r.dnsName.toLowerCase().includes(f) || r.recordType.toLowerCase() === f || r.targets.some((t) => t.toLowerCase().includes(f))) : all;
  }, [recs.data, filter]);

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: qk.records(zone) });
    void qc.invalidateQueries({ queryKey: qk.zone(zone) });
    void qc.invalidateQueries({ queryKey: qk.zones });
  };

  const del = useMutation({
    mutationFn: (r: ZoneRecord) => api.zones.deleteRecord(zone, r.dnsName, r.recordType, r.recordSet),
    onSuccess: (_x, r) => {
      notify.success(`Deleted ${r.dnsName} ${r.recordType}`);
      invalidate();
    },
    onError: (err) => notify.error(err, 'Delete failed'),
  });
  const notifyM = useMutation({
    mutationFn: () => api.zones.notify(zone),
    onSuccess: (r) => {
      notify.success(r.targets.length ? `NOTIFY sent to ${r.targets.join(', ')}` : 'No NOTIFY targets configured');
      invalidate();
    },
    onError: (err) => notify.error(err, 'NOTIFY failed'),
  });

  if (z.error) {
    return (
      <div className="k-page">
        <h1>Zone {zone}</h1>
        <ErrorBanner error={z.error} />
        <Link href="/zones">Back to zones</Link>
      </div>
    );
  }
  const zoneName = z.data?.zoneName ?? zone;
  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <Link href="/zones" className="k-small">
            ← Zones
          </Link>
          <h1 className="k-mono">{zoneName}</h1>
          {z.data ? (
            <span className="k-row k-small">
              <Badge variant="neutral" label={z.data.type} />
              <span>serial {z.data.serial}</span>
              <span>{z.data.recordCount} records</span>
              {z.data.ready ? <Badge variant="success" label="Ready" /> : <Badge variant="error" label="Not ready" />}
              <ManagedBadge owner={z.data.managedBy} />
            </span>
          ) : null}
        </div>
        <div className="k-actions">
          <Button label="Add record" variant="primary" isDisabled={!zoneEditable || !canWrite} onClick={() => setEditing('new')} />
          <Button label="Import BIND" variant="secondary" isDisabled={!zoneEditable || !canWrite} onClick={() => setImporting(true)} />
          <Button label="Export" variant="secondary" onClick={() => setExporting(true)} />
          <Button label="Send NOTIFY" variant="ghost" isDisabled={!can('zones', 'transfer')} isLoading={notifyM.isPending} onClick={() => notifyM.mutate()} />
        </div>
      </div>
      {z.data && !own.editable ? <Banner status="info" title={`Managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      {z.data && own.editable && z.data.type !== 'primary' ? (
        <Banner status="info" title={`A ${z.data.type} zone`} description="Records of secondary, forward and stub zones come from elsewhere and are read-only." />
      ) : null}
      {z.data?.transfer ? (
        <section className="k-panel" aria-labelledby="xfr">
          <h2 id="xfr" className="k-h3">
            Transfers and NOTIFY
          </h2>
          <TransferCell z={z.data} />
        </section>
      ) : null}

      <TextInput label="Filter records" isLabelHidden placeholder="Filter by name, type or value" value={filter} onChange={setFilter} hasClear />
      {recs.error ? <ErrorBanner error={recs.error} /> : null}
      {recs.isLoading ? <Loading /> : null}
      {recs.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label={`Records of ${zoneName}`}>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Type</th>
                <th scope="col" className="num">
                  TTL
                </th>
                <th scope="col">Value</th>
                <th scope="col">Record set</th>
                <th scope="col">
                  <span className="k-visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <td colSpan={6} className="k-muted">
                    {filter ? 'No records match.' : 'No records yet.'}
                  </td>
                </tr>
              ) : null}
              {rows.map((r) => {
                const recOwn = ownership(r.managedBy);
                const editable = zoneEditable && recOwn.editable && canWrite;
                return (
                  <tr key={`${r.recordSet}/${r.dnsName}/${r.recordType}/${r.setIdentifier ?? ''}`}>
                    <td className="k-mono">
                      {dnsNameToOwner(r.dnsName, zoneName)}
                      {r.kudurru?.disabled ? <Badge variant="warning" label="disabled" /> : null}
                      {r.kudurru?.comment ? <div className="k-muted k-small">{r.kudurru.comment}</div> : null}
                    </td>
                    <td>{r.recordType}</td>
                    <td className="num">{r.recordTTL ?? <span className="k-muted">default</span>}</td>
                    <td className="k-mono k-break k-small">
                      {r.targets.map((t) => (
                        <div key={t}>{t}</div>
                      ))}
                    </td>
                    <td className="k-small">
                      {r.recordSet}
                      {r.managedBy && !recOwn.editable ? (
                        <>
                          {' '}
                          <ManagedBadge owner={r.managedBy} />
                        </>
                      ) : null}
                    </td>
                    <td className="k-nowrap">
                      <Button label="Edit" size="sm" variant="ghost" isDisabled={!editable} onClick={() => setEditing(r)} />
                      <Button
                        label="Delete"
                        size="sm"
                        variant="ghost"
                        isDisabled={!editable || !can('records', 'delete')}
                        onClick={() => {
                          if (window.confirm(`Delete ${r.dnsName} ${r.recordType}?`)) del.mutate(r);
                        }}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      <FormDialog
        isOpen={editing !== null}
        onClose={() => setEditing(null)}
        title={editing === null ? '' : editing === 'new' ? `Add a record to ${zoneName}` : `Edit ${editing.dnsName} ${editing.recordType}`}
        width={720}
      >
        {editing !== null ? (
          <RecordEditor
            zoneName={zoneName}
            initial={editing === 'new' ? undefined : editing}
            defaultRecordSet={`api-${zone}`}
            onCancel={() => setEditing(null)}
            onSubmit={async (ep, recordSet) => {
              try {
                await api.zones.upsertRecord(zone, ep, recordSet);
              } catch (err) {
                throw new Error(errorText(err), { cause: err });
              }
              notify.success(`Saved ${ep.dnsName} ${ep.recordType}`);
              setEditing(null);
              invalidate();
            }}
          />
        ) : null}
      </FormDialog>
      <ImportDialog zone={zone} open={importing} onClose={() => setImporting(false)} />
      <ExportDialog zone={zone} open={exporting} onClose={() => setExporting(false)} />
    </div>
  );
}
