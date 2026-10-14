// The columns of a license list (the Licenses page, a customer's page):
// licensee and License ID, customer, plan, state, nodes and expiry (UTC),
// each row linking to the license's page.
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import type { LicenseSummary } from '@shared/api';
import { planLabel } from '@shared/catalog';
import { utcDay } from '../lib/dates';
import { formatExact } from '../ui/lib/format';
import { StateBadge } from './badges';

export type LicenseRow = LicenseSummary & Record<string, unknown>;

export function licenseColumns(opts: { customer?: boolean } = {}): TableColumn<LicenseRow>[] {
  const cols: TableColumn<LicenseRow>[] = [
    {
      key: 'licensee',
      header: 'License',
      width: proportional(2, { minWidth: 200 }),
      renderCell: (l) => (
        <VStack gap={0.5}>
          <Link href={`/licenses/${encodeURIComponent(l.id)}`}>{l.licensee}</Link>
          <Text type="supporting" size="sm">
            {l.id}
          </Text>
        </VStack>
      ),
    },
  ];
  if (opts.customer !== false) {
    cols.push({ key: 'customer', header: 'Customer', width: proportional(2, { minWidth: 160 }), renderCell: (l) => <Link href={`/customers/${encodeURIComponent(l.customer.id)}`}>{l.customer.name}</Link> });
  }
  cols.push(
    { key: 'plan', header: 'Plan', width: proportional(1, { minWidth: 120 }), renderCell: (l) => planLabel(l.plan) },
    { key: 'state', header: 'State', width: pixel(170), renderCell: (l) => <StateBadge state={l.state} /> },
    { key: 'nodes', header: 'Nodes', width: pixel(90), align: 'end', renderCell: (l) => formatExact(l.nodes) },
    { key: 'expiresOn', header: 'Expires (UTC)', width: pixel(170), renderCell: (l) => utcDay(l.expiresOn) },
  );
  return cols;
}
