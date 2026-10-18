// /requests (requests.read), in tabs:
// - Personal: personal key requests that wait for a license admin (open
//   first, filtered by state), why each waits, what approving would renew,
//   and the decisions: Approve (a wizard: customer, key, review) and Reject
//   (a FormDialog whose note is mailed to the requester). The admin mail of
//   each new request links here and, for an ordinary request, to the
//   one-click /approve page.
// - Business (?tab=business): business license requests from the
//   kudurru.io form (open first, filtered by state), and the decisions:
//   Issue license (the issue wizard with the request filled in; the
//   request becomes issued with the license), Draft Order Form (the Order
//   Form wizard with the request filled in) and Close (a FormDialog whose
//   note stays with the request; nothing is mailed). The state shows the
//   newest Order Form drafted from the request. The admin mail of each new
//   business request links here.
// - Blocked emails: the addresses whose personal requests always wait; a
//   superadmin removes them.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Tab, TabList } from '@astryxdesign/core/TabList';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { BlockedEmail, BusinessRequest, BusinessRequestStatus, LicenseRequest, RequestStatus } from '@shared/api';
import { BUSINESS_EDITIONS } from '@shared/catalog';
import { useCan } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useBlockedEmails, useBusinessRequests, useRequests, useRouteMutation } from '../api/hooks';
import { ApproveRequestWizardDialog } from '../components/ApproveRequestWizard';
import { BusinessRequestStatusBadge, OrderFormStatusBadge, RequestStatusBadge } from '../components/badges';
import { IssueLicenseWizardDialog } from '../components/IssueLicenseWizard';
import { OrderFormWizardDialog } from '../components/OrderFormWizard';
import { utcDay } from '../lib/dates';
import { BUSINESS_REQUEST_STATUS_LABEL, REQUEST_SOURCE_LABEL, REQUEST_STATUS_LABEL, REVIEW_REASON_LABEL } from '../lib/words';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { SelectField } from '../ui/fields/SelectField';
import { TextAreaField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';
import { useSearchParams } from '../ui/lib/router';

type RequestRow = LicenseRequest & Record<string, unknown>;
type BusinessRow = BusinessRequest & Record<string, unknown>;
type BlockedRow = BlockedEmail & Record<string, unknown>;

const STATUSES: RequestStatus[] = ['open', 'approved', 'rejected', 'canceled'];

type Section = 'requests' | 'business' | 'blocked';

export default function RequestsPage() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const asked = sp.get('tab');
  const tab: Section = asked === 'business' ? 'business' : asked === 'blocked' && can('blocklist.read') ? 'blocked' : 'requests';
  return (
    <Page>
      <PageHeader
        title="Requests"
        description="Requests that wait for a license admin: free personal keys to approve or reject, and business licenses asked for on the kudurru.io form to issue or close."
      />
      <TabList value={tab} onChange={(v) => setParams({ tab: v === 'requests' ? null : v, status: null })} aria-label="Request sections">
        <Tab value="requests" label="Personal" />
        <Tab value="business" label="Business" />
        {can('blocklist.read') ? <Tab value="blocked" label="Blocked emails" /> : null}
      </TabList>
      {tab === 'requests' ? <RequestList /> : tab === 'business' ? <BusinessRequestList /> : <BlockedList />}
    </Page>
  );
}

function RequestList() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const status = (sp.get('status') ?? '') as RequestStatus | '';
  const q = useRequests(status ? { status } : {});
  const [approving, setApproving] = useState<LicenseRequest | null>(null);
  const [rejecting, setRejecting] = useState<LicenseRequest | null>(null);
  const decide = can('requests.decide');

  const columns: TableColumn<RequestRow>[] = [
    { key: 'createdAt', header: 'Requested (UTC)', width: pixel(140), renderCell: (r) => utcDay(r.createdAt) },
    {
      key: 'name',
      header: 'Requester',
      width: proportional(2, { minWidth: 180 }),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <Text>{r.name}</Text>
          <Text type="supporting">
            {r.requesterEmail} · {REQUEST_SOURCE_LABEL[r.source]}
          </Text>
        </VStack>
      ),
    },
    {
      key: 'use',
      header: 'Use',
      width: proportional(3, { minWidth: 160 }),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <Text>{r.use}</Text>
          {r.renews ? <RenewsLine renews={r.renews} /> : null}
        </VStack>
      ),
    },
    { key: 'reviewReason', header: 'Why it waits', width: proportional(2, { minWidth: 140 }), renderCell: (r) => (r.reviewReason ? REVIEW_REASON_LABEL[r.reviewReason] : '–') },
    {
      key: 'status',
      header: 'State',
      width: pixel(140),
      renderCell: (r) => (
        <VStack gap={0.5} hAlign="start">
          <RequestStatusBadge status={r.status} />
          {r.resultingLicenseId ? <Link href={`/licenses/${encodeURIComponent(r.resultingLicenseId)}`}>{r.resultingLicenseId}</Link> : null}
          {r.decisionNote ? <Text type="supporting">{r.decisionNote}</Text> : null}
        </VStack>
      ),
    },
  ];
  if (decide) {
    columns.push({
      key: 'actions',
      header: '',
      width: pixel(170),
      align: 'end',
      renderCell: (r) =>
        r.status === 'open' ? (
          <HStack gap={1} hAlign="end">
            <Button label={`Approve the request of ${r.name}`} size="sm" variant="primary" onClick={() => setApproving(r)}>
              Approve
            </Button>
            <Button label={`Reject the request of ${r.name}`} size="sm" variant="ghost" onClick={() => setRejecting(r)}>
              Reject
            </Button>
          </HStack>
        ) : null,
    });
  }

  return (
    <Panel
      title="Personal key requests"
      description="Requests from a blocked address, or from a person who had a license withdrawn, always wait here."
    >
      <HStack gap={2}>
        <SelectField label="State" value={status} emptyLabel="Every state, open first" options={STATUSES.map((s) => ({ value: s, label: REQUEST_STATUS_LABEL[s] }))} onChange={(v) => setParams({ status: v })} width={260} />
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {q.data ? (
        <Table<RequestRow>
          data={q.data.items as RequestRow[]}
          columns={columns}
          idKey="id"
          density="compact"
          verticalAlign="top"
          emptyState={<Text color="secondary">{status === 'open' ? 'Nothing waits for approval.' : 'No requests.'}</Text>}
        />
      ) : null}
      <ApproveRequestWizardDialog request={approving} isOpen={approving !== null} onClose={() => setApproving(null)} />
      <FormDialog isOpen={rejecting !== null} onClose={() => setRejecting(null)} title="Reject request" subtitle={rejecting ? `${rejecting.name} · ${rejecting.requesterEmail}` : undefined} width={520}>
        {rejecting ? <RejectForm request={rejecting} onClose={() => setRejecting(null)} /> : null}
      </FormDialog>
    </Panel>
  );
}

/** "Renews lic_… (expires D UTC)": approving renews this license instead of issuing a new one. */
function RenewsLine({ renews }: { renews: NonNullable<LicenseRequest['renews']> }) {
  return (
    <Text type="supporting" data-testid="request-renews">
      Renews <Link href={`/licenses/${encodeURIComponent(renews.licenseId)}`}>{renews.licenseId}</Link> (expires {renews.expiresOn} UTC)
    </Text>
  );
}

function RejectForm({ request: r, onClose }: { request: LicenseRequest; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('requestReject');
  const [note, setNote] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const err = note.trim() ? (note.length > 1000 ? 'At most 1000 characters' : null) : 'Write a note for the requester';
  const fe = useFieldErrors(m.error, ['note']);
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (err) return;
        m.mutate(
          { params: { id: r.id }, body: { note: note.trim() } },
          {
            onSuccess: () => {
              notify.success(`Rejected the request of ${r.name}`);
              onClose();
            },
          },
        );
      }}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Reject and send note" variant="destructive" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">They asked for: {r.use}</Text>
      <TextAreaField
        label="Note to the requester"
        value={note}
        onChange={(v) => {
          setNote(v);
          fe.edited('note');
        }}
        isRequired
        help={`Mailed to ${r.requesterEmail}. Say why, and what they can do instead.`}
        error={(showErrors ? err : null) ?? fe.field('note')}
      />
      {fe.summary ? <Banner status="error" title="The request was not rejected" description={fe.summary} /> : null}
    </DialogForm>
  );
}

const BUSINESS_STATUSES: BusinessRequestStatus[] = ['open', 'issued', 'closed'];

function BusinessRequestList() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const status = (sp.get('status') ?? '') as BusinessRequestStatus | '';
  const q = useBusinessRequests(status ? { status } : {});
  const [issuing, setIssuing] = useState<BusinessRequest | null>(null);
  const [closing, setClosing] = useState<BusinessRequest | null>(null);
  const [drafting, setDrafting] = useState<BusinessRequest | null>(null);
  const decide = can('requests.decide');
  const issue = decide && can('licenses.issue');
  const order = can('orders.write');

  const columns: TableColumn<BusinessRow>[] = [
    { key: 'createdAt', header: 'Received (UTC)', width: pixel(140), renderCell: (r) => utcDay(r.createdAt) },
    {
      key: 'company',
      header: 'Requester',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <Text>{r.company}</Text>
          <Text type="supporting">
            {r.name} · <Link href={`mailto:${r.email}`}>{r.email}</Link>
          </Text>
        </VStack>
      ),
    },
    {
      key: 'edition',
      header: 'Asks for',
      width: proportional(2, { minWidth: 140 }),
      renderCell: (r) => (
        <VStack gap={0.5}>
          <Text>{BUSINESS_EDITIONS[r.edition].label}</Text>
          <Text type="supporting">
            {formatExact(r.nodes)} {r.nodes === 1 ? 'node' : 'nodes'}
          </Text>
        </VStack>
      ),
    },
    { key: 'details', header: 'Details', width: proportional(3, { minWidth: 150 }), renderCell: (r) => (r.details ? <Text>{r.details}</Text> : <Text color="secondary">–</Text>) },
    {
      key: 'status',
      header: 'State',
      width: pixel(220),
      renderCell: (r) => (
        <VStack gap={0.5} hAlign="start">
          <BusinessRequestStatusBadge status={r.status} />
          {r.resultingLicenseId ? <Link href={`/licenses/${encodeURIComponent(r.resultingLicenseId)}`}>{r.resultingLicenseId}</Link> : null}
          {r.orderForm ? (
            <VStack gap={0.5} hAlign="start" data-testid="request-order-form">
              <Link href={`/orders/${encodeURIComponent(r.orderForm.id)}`}>Order Form {r.orderForm.number}</Link>
              <OrderFormStatusBadge status={r.orderForm.status} />
            </VStack>
          ) : null}
          {r.decisionNote ? <Text type="supporting">{r.decisionNote}</Text> : null}
        </VStack>
      ),
    },
  ];
  if (decide || order) {
    columns.push({
      key: 'actions',
      header: '',
      width: pixel(360),
      align: 'end',
      renderCell: (r) =>
        r.status === 'open' ? (
          <HStack gap={1} hAlign="end">
            {issue ? (
              <Button label={`Issue a license for ${r.company}`} size="sm" variant="primary" onClick={() => setIssuing(r)}>
                Issue license
              </Button>
            ) : null}
            {order ? (
              <Button label={`Draft an Order Form for ${r.company}`} size="sm" variant="secondary" onClick={() => setDrafting(r)}>
                Draft Order Form
              </Button>
            ) : null}
            {decide ? (
              <Button label={`Close the request of ${r.company}`} size="sm" variant="ghost" onClick={() => setClosing(r)}>
                Close
              </Button>
            ) : null}
          </HStack>
        ) : null,
    });
  }

  return (
    <Panel
      title="Business requests"
      description="Asked for on the kudurru.io form. The requester was told we reply by email: write to them, then draft an Order Form or issue a license from the request, or close it with a note."
    >
      <HStack gap={2}>
        <SelectField
          label="State"
          value={status}
          emptyLabel="Every state, open first"
          options={BUSINESS_STATUSES.map((s) => ({ value: s, label: BUSINESS_REQUEST_STATUS_LABEL[s] }))}
          onChange={(v) => setParams({ status: v })}
          width={260}
        />
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {q.data ? (
        <Table<BusinessRow>
          data={q.data.items as BusinessRow[]}
          columns={columns}
          idKey="id"
          density="compact"
          verticalAlign="top"
          emptyState={<Text color="secondary">{status === 'open' ? 'No business request waits.' : 'No business requests.'}</Text>}
        />
      ) : null}
      <IssueLicenseWizardDialog key={issuing?.id ?? 'none'} request={issuing ?? undefined} isOpen={issuing !== null} onClose={() => setIssuing(null)} />
      <OrderFormWizardDialog key={`order-${drafting?.id ?? 'none'}`} request={drafting ?? undefined} isOpen={drafting !== null} onClose={() => setDrafting(null)} />
      <FormDialog isOpen={closing !== null} onClose={() => setClosing(null)} title="Close business request" subtitle={closing ? `${closing.company} · ${closing.email}` : undefined} width={520}>
        {closing ? <CloseBusinessForm request={closing} onClose={() => setClosing(null)} /> : null}
      </FormDialog>
    </Panel>
  );
}

function CloseBusinessForm({ request: r, onClose }: { request: BusinessRequest; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('businessRequestClose');
  const [note, setNote] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const err = note.trim() ? (note.length > 2000 ? 'At most 2000 characters' : null) : 'Write a note: why it is closed, or what was agreed';
  const fe = useFieldErrors(m.error, ['note']);
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (err) return;
        m.mutate(
          { params: { id: r.id }, body: { note: note.trim() } },
          {
            onSuccess: () => {
              notify.success(`Closed the request of ${r.company}`);
              onClose();
            },
          },
        );
      }}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Close request" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">
        They asked for {BUSINESS_EDITIONS[r.edition].label}, {formatExact(r.nodes)} {r.nodes === 1 ? 'node' : 'nodes'}.
      </Text>
      <TextAreaField
        label="Note"
        value={note}
        onChange={(v) => {
          setNote(v);
          fe.edited('note');
        }}
        isRequired
        help={`Kept with the request for license admins; nothing is mailed. Reply to ${r.email} yourself.`}
        error={(showErrors ? err : null) ?? fe.field('note')}
      />
      {fe.summary ? <Banner status="error" title="The request was not closed" description={fe.summary} /> : null}
    </DialogForm>
  );
}

function BlockedList() {
  const can = useCan();
  const notify = useNotify();
  const q = useBlockedEmails();
  const remove = useRouteMutation('blockedEmailRemove');
  const [removing, setRemoving] = useState<BlockedEmail | null>(null);
  const columns: TableColumn<BlockedRow>[] = [
    { key: 'email', header: 'Email', width: proportional(2, { minWidth: 200 }) },
    { key: 'reason', header: 'Reason', width: proportional(3, { minWidth: 200 }) },
    { key: 'blockedBy', header: 'Blocked by', width: proportional(1, { minWidth: 160 }), renderCell: (b) => b.blockedBy.label },
    { key: 'blockedAt', header: 'Blocked (UTC)', width: pixel(170), renderCell: (b) => utcDay(b.blockedAt) },
  ];
  if (can('blocklist.write')) {
    columns.push({
      key: 'actions',
      header: '',
      width: pixel(120),
      align: 'end',
      renderCell: (b) => (
        <Button label={`Unblock ${b.email}`} size="sm" variant="ghost" onClick={() => setRemoving(b)}>
          Unblock
        </Button>
      ),
    });
  }
  return (
    <Panel title="Blocked emails" description="Personal key requests from these addresses always wait for a signed-in license admin: their mail has no one-click approval link.">
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {q.data ? <Table<BlockedRow> data={q.data.items as BlockedRow[]} columns={columns} idKey="email" density="compact" emptyState={<Text color="secondary">No address is blocked.</Text>} /> : null}
      <AlertDialog
        isOpen={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        title={`Unblock ${removing?.email ?? ''}?`}
        description="Their next personal key request is handled like anyone's again."
        actionLabel="Unblock"
        isActionLoading={remove.isPending}
        onAction={() => {
          if (!removing) return;
          remove.mutate(
            { params: { email: removing.email } },
            {
              onSuccess: () => {
                notify.success(`Unblocked ${removing.email}`);
                setRemoving(null);
              },
              onError: (err) => notify.error(err, 'Could not unblock the address'),
            },
          );
        }}
      />
    </Panel>
  );
}
