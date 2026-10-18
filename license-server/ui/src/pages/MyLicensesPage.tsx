// /my/licenses (self.read): the licenses of every customer the signed-in
// person belongs to, their Order Forms (sent at least once; shown also
// before there is any license), their own personal key requests that wait for a
// license admin's approval, and the ways in when there is nothing yet: a
// waiting request, asking the licensor to add their address to an
// existing license, a free personal key (FormDialog) or a business
// enquiry by mail.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { KeyRound } from 'lucide-react';
import { useState } from 'react';
import type { MyLicense, MyOrderForm, MyRequest } from '@shared/api';
import { BUSINESS_CONTACT, BUSINESS_MAILTO, planLabel } from '@shared/catalog';
import { editionLabel } from '@shared/orderForms';
import { usd } from '@shared/pricing';
import { useCan, useMe } from '../api/context';
import { useMyLicenses, useMyOrderForms, useMyRequests } from '../api/hooks';
import { OrderFormStatusBadge, StateBadge } from '../components/badges';
import { PersonalRequestDialog } from '../components/PersonalRequestDialog';
import { utcDay } from '../lib/dates';
import { featuresInWords } from '../ui/fields/FeaturesField';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { formatExact } from '../ui/lib/format';

type LicenseRow = MyLicense & Record<string, unknown>;
type RequestRow = MyRequest & Record<string, unknown>;
type OrderRow = MyOrderForm & Record<string, unknown>;

const orderColumns: TableColumn<OrderRow>[] = [
  {
    key: 'number',
    header: 'Order Form No.',
    width: proportional(1, { minWidth: 170 }),
    renderCell: (f) => (
      <VStack gap={0.5}>
        <Link href={`/my/orders/${encodeURIComponent(f.id)}`}>{f.number}</Link>
        <Text type="supporting">{f.customer.name}</Text>
      </VStack>
    ),
  },
  { key: 'edition', header: 'Plan', width: proportional(1, { minWidth: 140 }), renderCell: (f) => editionLabel(f.terms.edition) },
  { key: 'nodes', header: 'Nodes', width: pixel(90), align: 'end', renderCell: (f) => formatExact(f.terms.nodes) },
  { key: 'fee', header: 'Annual fee', width: pixel(130), align: 'end', renderCell: (f) => usd(f.terms.annualFeeCents) },
  { key: 'term', header: 'Term (UTC)', width: pixel(200), renderCell: (f) => `${f.terms.subscriptionStart} to ${f.terms.subscriptionEnd}` },
  {
    key: 'status',
    header: 'State',
    width: pixel(230),
    renderCell: (f) => (
      <VStack gap={0.5} hAlign="start">
        <OrderFormStatusBadge status={f.status} />
        {f.status === 'sent' && f.authorized ? <Text type="supporting">Waiting for your acceptance</Text> : null}
      </VStack>
    ),
  },
];

const licenseColumns: TableColumn<LicenseRow>[] = [
  {
    key: 'licensee',
    header: 'Licensee',
    width: proportional(2, { minWidth: 180 }),
    renderCell: (l) => (
      <VStack gap={0.5}>
        <Link href={`/my/licenses/${encodeURIComponent(l.id)}`}>{l.licensee}</Link>
        <Text type="supporting">{l.customer.name}</Text>
      </VStack>
    ),
  },
  { key: 'plan', header: 'Plan', width: proportional(1, { minWidth: 140 }), renderCell: (l) => planLabel(l.plan) },
  { key: 'state', header: 'State', width: pixel(170), renderCell: (l) => <StateBadge state={l.state} /> },
  { key: 'nodes', header: 'Nodes', width: pixel(90), align: 'end', renderCell: (l) => formatExact(l.nodes) },
  { key: 'features', header: 'Features', width: proportional(2, { minWidth: 160 }), renderCell: (l) => featuresInWords(l.features) },
  { key: 'expiresOn', header: 'Expires (UTC)', width: pixel(170), renderCell: (l) => utcDay(l.expiresOn) },
];

const requestColumns: TableColumn<RequestRow>[] = [
  { key: 'createdAt', header: 'Requested (UTC)', width: pixel(170), renderCell: (r) => utcDay(r.createdAt) },
  { key: 'name', header: 'Name on the key', width: proportional(1, { minWidth: 160 }) },
  { key: 'use', header: 'Use', width: proportional(2, { minWidth: 200 }) },
  { key: 'status', header: 'State', width: pixel(160), renderCell: () => <Badge variant="warning" label="Waiting for approval" /> },
];

export default function MyLicensesPage() {
  const me = useMe();
  const can = useCan();
  const licenses = useMyLicenses();
  const requests = useMyRequests();
  const orders = useMyOrderForms();
  const orderItems = orders.data?.items ?? [];
  const [requesting, setRequesting] = useState(false);
  const open = (requests.data?.items ?? []).filter((r) => r.status === 'open');
  const items = licenses.data?.items ?? [];
  const canRequest = can('self.request');

  const requestButton = canRequest ? <Button label="Request a free personal key" variant="primary" onClick={() => setRequesting(true)} /> : null;
  const businessLink = <Button label="Ask about a business license" variant="secondary" href={BUSINESS_MAILTO} />;

  let empty = null;
  if (licenses.data && items.length === 0) {
    empty = open.length ? (
      <EmptyState
        title="Your request is waiting for approval"
        description="A license admin looks at your request for a free personal key. Your key arrives by email once it is approved, and appears here."
        icon={<KeyRound size={32} aria-hidden="true" />}
      />
    ) : (
      <EmptyState
        title="No licenses yet"
        description={`If your organization already has a license, ask us (${BUSINESS_CONTACT}) to add ${me.user.email ?? 'your email address'} to it. For eligible community use, request a free personal key.`}
        icon={<KeyRound size={32} aria-hidden="true" />}
        actions={
          <>
            {requestButton}
            {businessLink}
          </>
        }
      />
    );
  }

  return (
    <Page>
      <PageHeader
        title="My licenses"
        description="The licenses of every customer you belong to, with their current keys."
        actions={items.length ? <HStack gap={2}>{requestButton}{businessLink}</HStack> : null}
      />
      {licenses.error ? <ErrorBanner error={licenses.error} /> : null}
      {licenses.isPending ? <Loading /> : null}
      {empty ? <Panel isRegion={false}>{empty}</Panel> : null}
      {items.length ? (
        <Panel title="Licenses">
          <Table<LicenseRow> data={items as LicenseRow[]} columns={licenseColumns} idKey="id" density="compact" />
        </Panel>
      ) : null}
      {orders.error ? <ErrorBanner error={orders.error} title="Your Order Forms could not be loaded" /> : null}
      {orderItems.length ? (
        <Panel title="Order Forms" description="The Order Forms the licensor sent to your organizations. Its authorized contacts accept or decline each one here.">
          <Table<OrderRow> data={orderItems as OrderRow[]} columns={orderColumns} idKey="id" density="compact" verticalAlign="top" />
        </Panel>
      ) : null}
      {open.length ? (
        <Panel title="Requests waiting for approval">
          <Table<RequestRow> data={open as RequestRow[]} columns={requestColumns} idKey="id" density="compact" verticalAlign="top" />
        </Panel>
      ) : null}
      <PersonalRequestDialog isOpen={requesting} onClose={() => setRequesting(false)} />
    </Page>
  );
}
