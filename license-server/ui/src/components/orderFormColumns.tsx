// The Order Forms table's columns, shared by the Order Forms page and a
// customer's page: the number (linking to the form), the customer, the
// edition and nodes, the annual fee, the subscription, the state and when
// it was sent. `customer: false` drops the customer column.
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import type { OrderFormSummary } from '@shared/api';
import { editionLabel } from '@shared/orderForms';
import { usd } from '@shared/pricing';
import { utcDay } from '../lib/dates';
import { formatExact } from '../ui/lib/format';
import { OrderFormStatusBadge } from './badges';

export type OrderFormRow = OrderFormSummary & Record<string, unknown>;

export function orderFormColumns(opts: { customer?: boolean } = {}): TableColumn<OrderFormRow>[] {
  const cols: TableColumn<OrderFormRow>[] = [
    {
      key: 'number',
      header: 'Order Form No.',
      width: pixel(170),
      renderCell: (f) => (
        <VStack gap={0.5}>
          <Link href={`/orders/${encodeURIComponent(f.id)}`}>{f.number}</Link>
          <Text type="supporting">Version {f.version}</Text>
        </VStack>
      ),
    },
  ];
  if (opts.customer !== false) {
    cols.push({
      key: 'customer',
      header: 'Customer',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (f) => <Link href={`/customers/${encodeURIComponent(f.customer.id)}`}>{f.customer.name}</Link>,
    });
  }
  cols.push(
    { key: 'edition', header: 'Plan', width: proportional(1, { minWidth: 140 }), renderCell: (f) => editionLabel(f.edition) },
    { key: 'nodes', header: 'Nodes', width: pixel(90), align: 'end', renderCell: (f) => formatExact(f.nodes) },
    { key: 'annualFeeCents', header: 'Annual fee', width: pixel(130), align: 'end', renderCell: (f) => usd(f.annualFeeCents) },
    {
      key: 'term',
      header: 'Term (UTC)',
      width: pixel(200),
      renderCell: (f) => (
        <Text>
          {f.subscriptionStart} to {f.subscriptionEnd}
        </Text>
      ),
    },
    {
      key: 'status',
      header: 'State',
      width: pixel(210),
      renderCell: (f) => (
        <VStack gap={0.5} hAlign="start">
          <OrderFormStatusBadge status={f.status} />
          {f.licenseId ? <Link href={`/licenses/${encodeURIComponent(f.licenseId)}`}>{f.licenseId}</Link> : null}
        </VStack>
      ),
    },
    { key: 'sentAt', header: 'Sent (UTC)', width: pixel(140), renderCell: (f) => (f.sentAt ? utcDay(f.sentAt) : '–') },
  );
  return cols;
}
