// Zones (§10 principle 11): type, serial, record count, Ready and the
// manager of every zone, transfer/NOTIFY status, and a form for new
// primary or forward zones (PUT /objects/dnszones/{name}).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Dialog } from '@astryxdesign/core/Dialog';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { DNSZone, ZoneSummary, ZoneType } from '../api/types';
import { ErrorBanner, errorText, Loading, useNotify } from '../components/Feedback';
import { ManagedBadge } from '../components/ManagedBadge';
import { formatExact, formatRelative } from '../lib/format';
import { nameError } from '../lib/objectSchemas';
import { Link, useNavigate } from '../lib/router';
import { hostError } from '../lib/rr';

export function TransferCell({ z }: { z: ZoneSummary }) {
  const t = z.transfer;
  if (!t) return <span className="k-muted">–</span>;
  return (
    <span className="k-small">
      AXFR {t.zoneTransfer}, NOTIFY {t.notify}
      {t.lastNotify ? (
        <>
          <br />
          <span className={t.lastNotify.ok ? 'q-permitted' : 'q-blocked'} title={t.lastNotify.error ?? t.lastNotify.targets.join(', ')}>
            notified {formatRelative(t.lastNotify.at)}
            {t.lastNotify.ok ? '' : ' (failed)'}
          </span>
        </>
      ) : null}
      {t.lastTransfer ? (
        <>
          <br />
          <span className={t.lastTransfer.ok ? 'k-muted' : 'q-blocked'}>
            {t.lastTransfer.kind} to {t.lastTransfer.peer} {formatRelative(t.lastTransfer.at)}
          </span>
        </>
      ) : null}
    </span>
  );
}

function NewZoneDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const titleId = useId();
  const typeId = useId();
  const [zoneName, setZoneName] = useState('');
  const [type, setType] = useState<ZoneType>('primary');
  const [nameservers, setNameservers] = useState('');
  const [upstream, setUpstream] = useState('');
  const [err, setErr] = useState<Record<string, string>>({});
  const [serverError, setServerError] = useState<string>();
  const objectName = zoneName.trim().replace(/\.$/, '').toLowerCase();
  const m = useMutation({
    mutationFn: async () => {
      const zone: DNSZone = {
        apiVersion: 'kudurru.io/v1alpha1',
        kind: 'DNSZone',
        metadata: { name: objectName },
        spec: {
          zoneName: `${objectName}.`,
          type,
          ...(type === 'primary' && nameservers.trim() ? { nameservers: nameservers.split(/\s+/).filter(Boolean) } : {}),
          ...(type === 'forward' && upstream.trim() ? { forward: { upstreamGroup: upstream.trim() } } : {}),
        },
      };
      return api.objects.put('dnszones', zone);
    },
    onSuccess: () => {
      notify.success(`Created zone ${objectName}`);
      void qc.invalidateQueries({ queryKey: qk.zones });
      onClose();
      navigate(`/zones/${encodeURIComponent(objectName)}`);
    },
    onError: (e) => setServerError(errorText(e)),
  });
  const submit = () => {
    const e: Record<string, string> = {};
    const he = hostError(zoneName);
    if (he) e.zoneName = he;
    else if (nameError(objectName)) e.zoneName = 'Use lower-case letters, digits, "-" and "."';
    for (const ns of nameservers.split(/\s+/).filter(Boolean)) {
      const nse = hostError(ns);
      if (nse) e.nameservers = `${ns}: ${nse}`;
    }
    setErr(e);
    if (!Object.keys(e).length) m.mutate();
  };
  return (
    <Dialog isOpen={open} onOpenChange={(o) => !o && onClose()} purpose="form" width={520} aria-labelledby={titleId}>
      <form
        className="k-stack-lg"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <h2 id={titleId} className="k-dialog-title">
          New zone
        </h2>
        <TextInput label="Zone name" placeholder="home.arpa" value={zoneName} onChange={setZoneName} isRequired status={err.zoneName ? { type: 'error', message: err.zoneName } : undefined} />
        <label className="k-label" htmlFor={typeId}>
          Type
          <select id={typeId} className="k-native-select" value={type} onChange={(e) => setType(e.target.value as ZoneType)}>
            <option value="primary">Primary (records served here)</option>
            <option value="forward">Forward (to an upstream group)</option>
          </select>
        </label>
        {type === 'primary' ? (
          <TextArea label="Name servers" description="One per line; defaults to this server" value={nameservers} onChange={setNameservers} rows={2} status={err.nameservers ? { type: 'error', message: err.nameservers } : undefined} />
        ) : (
          <TextInput label="Upstream group" placeholder="default" value={upstream} onChange={setUpstream} />
        )}
        <p className="k-muted k-small">Secondary, stub and catalog zones are declared in YAML (`kudurru apply`) in this release.</p>
        {serverError ? <ErrorBanner error={new Error(serverError)} title="The zone could not be created" /> : null}
        <div className="k-row">
          <span className="k-spacer" />
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Create zone" variant="primary" isLoading={m.isPending} />
        </div>
      </form>
    </Dialog>
  );
}

export default function ZonesPage() {
  const api = useApi();
  const can = useCan();
  const [creating, setCreating] = useState(false);
  const q = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list(), refetchInterval: 30_000 });
  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Zones</h1>
          <p className="k-muted k-small">Authoritative and forward zones. Zones managed by a file or Kubernetes are read-only here.</p>
        </div>
        <div className="k-actions">
          <Button label="New zone" variant="primary" isDisabled={!can('zones', 'write')} tooltip={can('zones', 'write') ? undefined : 'Needs zones.write'} onClick={() => setCreating(true)} />
        </div>
      </div>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <div className="k-panel k-table-wrap">
          <table className="k-table" aria-label="Zones">
            <thead>
              <tr>
                <th scope="col">Zone</th>
                <th scope="col">Type</th>
                <th scope="col" className="num">
                  Serial
                </th>
                <th scope="col" className="num">
                  Records
                </th>
                <th scope="col">Ready</th>
                <th scope="col">Transfers</th>
                <th scope="col">Managed by</th>
              </tr>
            </thead>
            <tbody>
              {q.data.items.length === 0 ? (
                <tr>
                  <td colSpan={7} className="k-muted">
                    No zones yet.
                  </td>
                </tr>
              ) : null}
              {q.data.items.map((z) => (
                <tr key={z.name}>
                  <td>
                    <Link href={`/zones/${encodeURIComponent(z.name)}`} className="k-mono">
                      {z.zoneName}
                    </Link>
                  </td>
                  <td>
                    <Badge variant="neutral" label={z.type} />
                  </td>
                  <td className="num k-mono">{z.serial}</td>
                  <td className="num">{formatExact(z.recordCount)}</td>
                  <td>{z.ready ? <Badge variant="success" label="Ready" /> : <Badge variant="error" label="Not ready" />}</td>
                  <td>
                    <TransferCell z={z} />
                  </td>
                  <td>
                    <ManagedBadge owner={z.managedBy} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
      <NewZoneDialog open={creating} onClose={() => setCreating(false)} />
    </div>
  );
}
