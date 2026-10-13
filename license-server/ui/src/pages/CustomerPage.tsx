// /customers/:id (customers.read): one customer. Details edited inline
// (name, kind, contact email, notes); the members with their state and
// linked identity (add, which shows the invite link once; re-invite, which
// replaces the link; remove, which ends access at once); the customer's
// licenses with "Issue license" (the wizard with this customer chosen);
// and the personal requests of its people.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Plus, UserPlus } from 'lucide-react';
import { useState } from 'react';
import type { CustomerDetail, CustomerKind, LicenseRequest, Member, MemberInviteResult } from '@shared/api';
import { INVITE_TTL_DAYS } from '@shared/catalog';
import { useCan } from '../api/context';
import { useCatalog, useCustomer, useRouteMutation } from '../api/hooks';
import { RecentAudit } from '../components/AuditTable';
import { MemberStatusBadge, RequestStatusBadge } from '../components/badges';
import { IssueLicenseWizardDialog } from '../components/IssueLicenseWizard';
import { licenseColumns, type LicenseRow } from '../components/licenseColumns';
import type { PageProps } from '../routes';
import { utcDay } from '../lib/dates';
import { emailError, nameError } from '../lib/validate';
import { KIND_LABEL, KIND_OPTIONS } from '../lib/words';
import { EditableSection } from '../ui/components/EditableSection';
import { errorText, ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { customerChoice } from '../ui/fields/CustomerField';
import { SelectField } from '../ui/fields/SelectField';
import { TextAreaField, TextField } from '../ui/fields/TextField';

type MemberRow = Member & Record<string, unknown>;
type RequestRow = LicenseRequest & Record<string, unknown>;

interface DetailsDraft {
  name: string;
  kind: CustomerKind;
  contactEmail: string;
  notes: string;
}

const licenseCols = licenseColumns({ customer: false });

const requestColumns: TableColumn<RequestRow>[] = [
  { key: 'createdAt', header: 'Requested (UTC)', width: pixel(170), renderCell: (r) => utcDay(r.createdAt) },
  { key: 'name', header: 'Name', width: proportional(1, { minWidth: 160 }) },
  { key: 'requesterEmail', header: 'Email', width: proportional(1, { minWidth: 180 }) },
  { key: 'status', header: 'State', width: pixel(130), renderCell: (r) => <RequestStatusBadge status={r.status} /> },
  { key: 'link', header: '', width: pixel(110), renderCell: (r) => (r.status === 'open' ? <Link href="/requests">Decide</Link> : null) },
];

export default function CustomerPage({ params }: PageProps) {
  const id = params.id ?? '';
  const can = useCan();
  const q = useCustomer(id);
  const update = useRouteMutation('customerUpdate');
  const [issuing, setIssuing] = useState(false);
  const c = q.data;
  return (
    <Page>
      <PageHeader
        back={{ href: '/customers', label: 'Customers' }}
        title={c ? c.name : 'Customer'}
        meta={c ? <Badge variant="neutral" label={KIND_LABEL[c.kind]} /> : null}
        actions={c && can('licenses.issue') ? <Button label="Issue license" variant="primary" icon={<Plus size={16} aria-hidden="true" />} onClick={() => setIssuing(true)} /> : null}
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {c ? (
        <>
          <EditableSection<DetailsDraft>
            title="Details"
            value={{ name: c.name, kind: c.kind, contactEmail: c.contactEmail ?? '', notes: c.notes }}
            readOnlyReason={can('customers.write') ? undefined : 'Needs customers.write'}
            view={
              <MetadataList>
                <MetadataListItem label="Name">{c.name}</MetadataListItem>
                <MetadataListItem label="Kind">{KIND_LABEL[c.kind]}</MetadataListItem>
                <MetadataListItem label="Contact email">{c.contactEmail ?? '–'}</MetadataListItem>
                <MetadataListItem label="Notes">{c.notes || '–'}</MetadataListItem>
                <MetadataListItem label="Created">
                  {utcDay(c.createdAt)} by {c.createdBy.label}
                </MetadataListItem>
              </MetadataList>
            }
            edit={(d, set, showErrors) => (
              <VStack gap={3}>
                <TextField label="Name" value={d.name} onChange={(v) => set({ ...d, name: v })} isRequired error={showErrors ? (nameError(d.name, 'the customer name') ?? undefined) : undefined} />
                <SelectField label="Kind" value={d.kind} options={KIND_OPTIONS} onChange={(v) => set({ ...d, kind: v as CustomerKind })} />
                <TextField label="Contact email" type="email" value={d.contactEmail} onChange={(v) => set({ ...d, contactEmail: v })} isOptional error={showErrors ? (emailError(d.contactEmail, { optional: true }) ?? undefined) : undefined} />
                <TextAreaField label="Notes" value={d.notes} onChange={(v) => set({ ...d, notes: v })} isOptional help="Internal; customers never see them." />
              </VStack>
            )}
            validate={(d) => nameError(d.name, 'the customer name') ?? emailError(d.contactEmail, { optional: true })}
            onSave={async (d) => {
              await update.mutateAsync({ params: { id: c.id }, body: { name: d.name.trim(), kind: d.kind, contactEmail: d.contactEmail.trim() || null, notes: d.notes.trim() } });
            }}
          />
          <Members customer={c} />
          <Panel title="Licenses">
            <Table<LicenseRow> data={c.licenses as LicenseRow[]} columns={licenseCols} idKey="id" density="compact" emptyState={<Text color="secondary">No licenses yet.</Text>} />
          </Panel>
          {c.requests.length ? (
            <Panel title="Personal key requests" description="Requests from this customer's people.">
              <Table<RequestRow> data={c.requests as RequestRow[]} columns={requestColumns} idKey="id" density="compact" />
            </Panel>
          ) : null}
          <RecentAudit targetType="customer" targetId={c.id} />
          <IssueLicenseWizardDialog isOpen={issuing} onClose={() => setIssuing(false)} customer={customerChoice(c)} />
        </>
      ) : null}
    </Page>
  );
}

function Members({ customer: c }: { customer: CustomerDetail }) {
  const can = useCan();
  const notify = useNotify();
  const writable = can('customers.write');
  const remove = useRouteMutation('memberRemove');
  const [adding, setAdding] = useState(false);
  const [reinvite, setReinvite] = useState<Member | null>(null);
  const [removing, setRemoving] = useState<Member | null>(null);

  const columns: TableColumn<MemberRow>[] = [
    {
      key: 'email',
      header: 'Email',
      width: proportional(2, { minWidth: 200 }),
      renderCell: (m) => (
        <VStack gap={0.5}>
          <Text>{m.email}</Text>
          <Text type="supporting">
            Added {utcDay(m.invitedAt)} by {m.invitedBy}
          </Text>
        </VStack>
      ),
    },
    { key: 'status', header: 'Access', width: pixel(110), renderCell: (m) => <MemberStatusBadge status={m.status} /> },
    {
      key: 'user',
      header: 'Signed in as',
      width: proportional(2, { minWidth: 180 }),
      renderCell: (m) =>
        m.user ? (
          <Link href={`/users/${encodeURIComponent(m.user.id)}`}>
            {m.user.name || m.user.email || m.user.id}
            {m.user.email && m.user.email !== m.email ? ` (${m.user.email})` : ''}
          </Link>
        ) : (
          <Text type="supporting">{m.inviteExpiresAt ? `Invite open until ${utcDay(m.inviteExpiresAt)}` : 'Not linked'}</Text>
        ),
    },
  ];
  if (writable) {
    columns.push({
      key: 'actions',
      header: '',
      width: pixel(180),
      align: 'end',
      renderCell: (m) => (
        <HStack gap={1} hAlign="end">
          {m.status === 'invited' ? <Button label={`New invite link for ${m.email}`} size="sm" variant="ghost" onClick={() => setReinvite(m)}>Re-invite</Button> : null}
          <Button label={`Remove ${m.email}`} size="sm" variant="ghost" onClick={() => setRemoving(m)}>
            Remove
          </Button>
        </HStack>
      ),
    });
  }

  return (
    <Panel
      title="Members"
      description="The people who see this customer's licenses and keys. An invite link links a member to whoever signs in with it."
      actions={writable ? <Button label="Add member" size="sm" variant="secondary" icon={<UserPlus size={16} aria-hidden="true" />} onClick={() => setAdding(true)} /> : null}
    >
      <Table<MemberRow> data={c.members as MemberRow[]} columns={columns} idKey="email" density="compact" emptyState={<Text color="secondary">No members yet. Add one to give someone access in the portal.</Text>} />
      <FormDialog isOpen={adding} onClose={() => setAdding(false)} title="Add a member" subtitle={c.name} width={560}>
        <AddMemberForm customerId={c.id} onClose={() => setAdding(false)} />
      </FormDialog>
      <FormDialog isOpen={reinvite !== null} onClose={() => setReinvite(null)} title="New invite link" subtitle={reinvite?.email} width={560}>
        {reinvite ? <ReinviteForm customerId={c.id} member={reinvite} onClose={() => setReinvite(null)} /> : null}
      </FormDialog>
      <AlertDialog
        isOpen={removing !== null}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        title={`Remove ${removing?.email ?? ''}?`}
        description="They lose access to this customer's licenses and keys at once. Keys they already installed keep working."
        actionLabel="Remove"
        isActionLoading={remove.isPending}
        onAction={() => {
          if (!removing) return;
          remove.mutate(
            { params: { id: c.id, email: removing.email } },
            {
              onSuccess: () => {
                notify.success(`Removed ${removing.email}`);
                setRemoving(null);
              },
              onError: (err) => notify.error(err, 'Could not remove the member'),
            },
          );
        }}
      />
    </Panel>
  );
}

/** The invite link, shown once (the server never shows it again). */
function InviteLink({ result }: { result: MemberInviteResult }) {
  const catalog = useCatalog();
  return (
    <VStack gap={2}>
      <Banner
        status="success"
        title={`Invite link for ${result.member.email}`}
        description={`Copy it now: it is shown only once. It works for ${INVITE_TTL_DAYS} days and only once.${catalog.data?.mail.configured ? ' It was also mailed to the address.' : ''}`}
        collapsible={false}
      />
      <CodeBlock code={result.inviteUrl} language="plaintext" isWrapped width="100%" size="sm" />
    </VStack>
  );
}

function AddMemberForm({ customerId, onClose }: { customerId: string; onClose: () => void }) {
  const m = useRouteMutation('memberAdd');
  const [email, setEmail] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const err = emailError(email);
  if (m.data) {
    return (
      <DialogForm footer={<Button label="Done" variant="primary" onClick={onClose} />}>
        <InviteLink result={m.data} />
      </DialogForm>
    );
  }
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (!err) m.mutate({ params: { id: customerId }, body: { email: email.trim() } });
      }}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Add member" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <TextField label="Email" type="email" value={email} onChange={setEmail} isRequired help="The person gets an invite link; whoever signs in with it sees this customer's licenses." error={showErrors ? (err ?? undefined) : undefined} />
      {m.error ? <Banner status="error" title="The member was not added" description={errorText(m.error)} /> : null}
    </DialogForm>
  );
}

function ReinviteForm({ customerId, member, onClose }: { customerId: string; member: Member; onClose: () => void }) {
  const m = useRouteMutation('memberInvite');
  if (m.data) {
    return (
      <DialogForm footer={<Button label="Done" variant="primary" onClick={onClose} />}>
        <InviteLink result={m.data} />
      </DialogForm>
    );
  }
  return (
    <DialogForm
      onSubmit={() => m.mutate({ params: { id: customerId, email: member.email } })}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Make a new link" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">A new invite link replaces the old one, which stops working.</Text>
      {m.error ? <Banner status="error" title="No new link was made" description={errorText(m.error)} /> : null}
    </DialogForm>
  );
}
