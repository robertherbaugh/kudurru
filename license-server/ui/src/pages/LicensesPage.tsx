// /licenses (licenses.read): every license, searchable (id, licensee,
// customer) and filtered by state, plan, expiry within 30/60/90 days and
// customer, the filters in the URL so a view can be shared. "Issue license"
// opens the wizard (?new=1) and "Import key" takes a key signed before the
// portal existed.
import { Button } from '@astryxdesign/core/Button';
import { Pagination } from '@astryxdesign/core/Pagination';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Token } from '@astryxdesign/core/Token';
import { Plus, Upload } from 'lucide-react';
import { useState } from 'react';
import type { LicenseListQuery } from '@shared/api';
import { DEFAULT_PAGE_LIMIT } from '@shared/api';
import { PLANS, stateLabel, type LicenseState, type Plan } from '@shared/catalog';
import { useCan } from '../api/context';
import { useLicenses } from '../api/hooks';
import { licenseColumns, type LicenseRow } from '../components/licenseColumns';
import { ImportKeyDialog } from '../components/ImportKeyDialog';
import { IssueLicenseWizardDialog } from '../components/IssueLicenseWizard';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { SelectField } from '../ui/fields/SelectField';
import { TextField } from '../ui/fields/TextField';
import { useNewParam, useSearchParams } from '../ui/lib/router';

type Row = LicenseRow;

const STATES: LicenseState[] = ['active', 'expiring', 'grace', 'expired', 'withdrawn'];

const columns = licenseColumns();

export default function LicensesPage() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const [wizard, setWizard] = useNewParam();
  const [importing, setImporting] = useState(false);
  const q = sp.get('q') ?? '';
  const state = (sp.get('state') ?? '') as LicenseState | '';
  const plan = (sp.get('plan') ?? '') as Plan | '';
  const expiring = sp.get('expiring') ?? '';
  const customerId = sp.get('customerId') ?? '';
  const page = Math.max(1, Number(sp.get('page') ?? '1') || 1);
  const query: LicenseListQuery = {
    ...(q.trim() ? { q: q.trim() } : {}),
    ...(state ? { state } : {}),
    ...(plan ? { plan } : {}),
    ...(expiring ? { expiringWithinDays: Number(expiring) } : {}),
    ...(customerId ? { customerId } : {}),
    limit: DEFAULT_PAGE_LIMIT,
    offset: (page - 1) * DEFAULT_PAGE_LIMIT,
  };
  const list = useLicenses(query);
  const items = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  const filtered = Boolean(q || state || plan || expiring || customerId);

  return (
    <Page>
      <PageHeader
        title="Licenses"
        description="Every license the portal issued or imported. Keys are verified offline on each node; withdrawing a license does not stop a key that is already installed."
        actions={
          can('licenses.issue') ? (
            <>
              <Button label="Import key" variant="secondary" icon={<Upload size={16} aria-hidden="true" />} onClick={() => setImporting(true)} />
              <Button label="Issue license" variant="primary" icon={<Plus size={16} aria-hidden="true" />} onClick={() => setWizard(true)} />
            </>
          ) : null
        }
      />
      <Panel title="Licenses" isRegion>
        <HStack gap={2} wrap="wrap" vAlign="start">
          <TextField label="Search" startIcon="search" value={q} onChange={(v) => setParams({ q: v, page: null })} hasClear placeholder="License ID, licensee or customer" fieldWidth={320} />
          <SelectField label="State" value={state} emptyLabel="Any state" options={STATES.map((s) => ({ value: s, label: stateLabel(s) }))} onChange={(v) => setParams({ state: v, page: null })} width={200} />
          <SelectField label="Plan" value={plan} emptyLabel="Any plan" options={PLANS.map((p) => ({ value: p.key, label: p.label }))} onChange={(v) => setParams({ plan: v, page: null })} width={220} />
          <SelectField
            label="Expires"
            value={expiring}
            emptyLabel="Any time"
            options={[30, 60, 90].map((d) => ({ value: String(d), label: `Within ${d} days` }))}
            onChange={(v) => setParams({ expiring: v, page: null })}
            width={180}
          />
        </HStack>
        {customerId ? (
          <HStack gap={2} vAlign="center">
            <Token label={`Customer ${items[0]?.customer.id === customerId ? items[0].customer.name : customerId}`} onRemove={() => setParams({ customerId: null, page: null })} />
          </HStack>
        ) : null}
        {list.error ? <ErrorBanner error={list.error} /> : null}
        {list.isPending ? <Loading /> : null}
        {list.data ? (
          <VStack gap={2}>
            <Table<Row>
              data={items as Row[]}
              columns={columns}
              idKey="id"
              density="compact"
              emptyState={<Text color="secondary">{filtered ? 'No license matches these filters.' : 'No licenses yet. Issue the first one.'}</Text>}
            />
            {total > DEFAULT_PAGE_LIMIT ? <Pagination page={page} totalItems={total} pageSize={DEFAULT_PAGE_LIMIT} onChange={(p) => setParams({ page: p === 1 ? null : p })} variant="count" /> : null}
          </VStack>
        ) : null}
      </Panel>
      <IssueLicenseWizardDialog isOpen={wizard && can('licenses.issue')} onClose={() => setWizard(false)} />
      <ImportKeyDialog isOpen={importing} onClose={() => setImporting(false)} />
    </Page>
  );
}
