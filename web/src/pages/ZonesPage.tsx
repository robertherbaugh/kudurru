// Zones (§10 principle 11): type, serial, record count, Ready and the
// manager of every zone and transfer/NOTIFY status. New zones are created
// with the wizard at /zones/new; a BIND file is imported in a dialog.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { useQuery } from '@tanstack/react-query';
import { qk, useApi, useCan } from '../api/context';
import type { ZoneSummary } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ManagedBadge } from '../components/ManagedBadge';
import { ZoneImportDialog } from '../components/ZoneImportDialog';
import { formatExact, formatRelative } from '../lib/format';
import { Link, useNavigate, useSearchParams } from '../lib/router';

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

export default function ZonesPage() {
  const api = useApi();
  const can = useCan();
  const navigate = useNavigate();
  const [sp, setParams] = useSearchParams();
  const importing = sp.get('import') === '1';
  const setImporting = (on: boolean) => setParams({ import: on ? '1' : null });
  const q = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list(), refetchInterval: 30_000 });
  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Zones</h1>
          <p className="k-muted k-small">Authoritative and forward zones. Zones managed by a file or Kubernetes are read-only here.</p>
        </div>
        <div className="k-actions">
          <Button
            label="Import zone file"
            variant="secondary"
            isDisabled={!can('zones', 'write')}
            tooltip={can('zones', 'write') ? undefined : 'Needs zones.write'}
            onClick={() => setImporting(true)}
          />
          <Button label="New zone" variant="primary" isDisabled={!can('zones', 'write')} tooltip={can('zones', 'write') ? undefined : 'Needs zones.write'} onClick={() => navigate('/zones/new')} />
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
      <ZoneImportDialog isOpen={importing} onClose={() => setImporting(false)} />
    </div>
  );
}
