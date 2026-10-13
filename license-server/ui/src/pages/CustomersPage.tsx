// /customers (customers.read): every customer, searchable by name, contact
// email and member email, paged. "New customer" is a short FormDialog
// (name, kind, contact email, notes) that opens the new customer's page.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { Pagination } from '@astryxdesign/core/Pagination';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Plus } from 'lucide-react';
import { useState } from 'react';
import type { Customer, CustomerKind } from '@shared/api';
import { DEFAULT_PAGE_LIMIT } from '@shared/api';
import { useCan } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useCustomers, useRouteMutation } from '../api/hooks';
import { utcDay } from '../lib/dates';
import { emailError, nameError } from '../lib/validate';
import { KIND_LABEL, KIND_OPTIONS } from '../lib/words';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { SelectField } from '../ui/fields/SelectField';
import { TextAreaField, TextField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';
import { useNavigate, useNewParam, useSearchParams } from '../ui/lib/router';

type Row = Customer & Record<string, unknown>;

const columns: TableColumn<Row>[] = [
  { key: 'name', header: 'Name', width: proportional(2, { minWidth: 200 }), renderCell: (c) => <Link href={`/customers/${encodeURIComponent(c.id)}`}>{c.name}</Link> },
  { key: 'kind', header: 'Kind', width: pixel(130), renderCell: (c) => KIND_LABEL[c.kind] },
  { key: 'contactEmail', header: 'Contact', width: proportional(2, { minWidth: 180 }), renderCell: (c) => c.contactEmail ?? '–' },
  { key: 'licenseCount', header: 'Licenses', width: pixel(100), align: 'end', renderCell: (c) => formatExact(c.licenseCount) },
  { key: 'memberCount', header: 'Members', width: pixel(100), align: 'end', renderCell: (c) => formatExact(c.memberCount) },
  { key: 'updatedAt', header: 'Updated (UTC)', width: pixel(170), renderCell: (c) => utcDay(c.updatedAt) },
];

export default function CustomersPage() {
  const can = useCan();
  const [sp, setParams] = useSearchParams();
  const [creating, setCreating] = useNewParam();
  const q = sp.get('q') ?? '';
  const page = Math.max(1, Number(sp.get('page') ?? '1') || 1);
  const list = useCustomers({ ...(q.trim() ? { q: q.trim() } : {}), limit: DEFAULT_PAGE_LIMIT, offset: (page - 1) * DEFAULT_PAGE_LIMIT });
  const items = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  return (
    <Page>
      <PageHeader
        title="Customers"
        description="The people and organisations licenses are issued to. Their members see the licenses and keys in the portal."
        actions={can('customers.write') ? <Button label="New customer" variant="primary" icon={<Plus size={16} aria-hidden="true" />} onClick={() => setCreating(true)} /> : null}
      />
      <Panel title="Customers">
        <HStack gap={2} wrap="wrap">
          <TextField label="Search" startIcon="search" value={q} onChange={(v) => setParams({ q: v, page: null })} hasClear placeholder="Name, contact or member email" fieldWidth={340} />
        </HStack>
        {list.error ? <ErrorBanner error={list.error} /> : null}
        {list.isPending ? <Loading /> : null}
        {list.data ? (
          <VStack gap={2}>
            <Table<Row> data={items as Row[]} columns={columns} idKey="id" density="compact" emptyState={<Text color="secondary">{q ? 'No customer matches.' : 'No customers yet.'}</Text>} />
            {total > DEFAULT_PAGE_LIMIT ? <Pagination page={page} totalItems={total} pageSize={DEFAULT_PAGE_LIMIT} onChange={(p) => setParams({ page: p === 1 ? null : p })} variant="count" /> : null}
          </VStack>
        ) : null}
      </Panel>
      <FormDialog isOpen={creating && can('customers.write')} onClose={() => setCreating(false)} title="New customer" width={520}>
        <NewCustomerForm onClose={() => setCreating(false)} />
      </FormDialog>
    </Page>
  );
}

/** The body fields the controls show (a 400 names them inline). */
const CUSTOMER_FIELDS = ['name', 'kind', 'contactEmail', 'notes'];

function NewCustomerForm({ onClose }: { onClose: () => void }) {
  const notify = useNotify();
  const navigate = useNavigate();
  const m = useRouteMutation('customerCreate');
  const [name, setName] = useState('');
  const [kind, setKind] = useState<CustomerKind>('organization');
  const [email, setEmail] = useState('');
  const [notes, setNotes] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const nameErr = nameError(name, 'the customer name');
  const emailErr = emailError(email, { optional: true });
  const fe = useFieldErrors(m.error, CUSTOMER_FIELDS);

  const submit = () => {
    setShowErrors(true);
    if (nameErr || emailErr) return;
    m.mutate(
      { body: { name: name.trim(), kind, contactEmail: email.trim() || null, ...(notes.trim() ? { notes: notes.trim() } : {}) } },
      {
        onSuccess: (c) => {
          notify.success(`Created ${c.name}`);
          onClose();
          navigate(`/customers/${encodeURIComponent(c.id)}`);
        },
      },
    );
  };

  return (
    <DialogForm
      onSubmit={submit}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Create customer" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <TextField
        label="Name"
        value={name}
        onChange={(v) => {
          setName(v);
          fe.edited('name');
        }}
        isRequired
        error={(showErrors ? nameErr : null) ?? fe.field('name')}
      />
      <SelectField
        label="Kind"
        value={kind}
        options={KIND_OPTIONS}
        onChange={(v) => {
          setKind(v as CustomerKind);
          fe.edited('kind');
        }}
        error={fe.field('kind')}
      />
      <TextField
        label="Contact email"
        type="email"
        value={email}
        onChange={(v) => {
          setEmail(v);
          fe.edited('contactEmail');
        }}
        isOptional
        help="Where keys are mailed when you choose to."
        error={(showErrors ? emailErr : null) ?? fe.field('contactEmail')}
      />
      <TextAreaField
        label="Notes"
        value={notes}
        onChange={(v) => {
          setNotes(v);
          fe.edited('notes');
        }}
        isOptional
        help="Internal; customers never see them."
        error={fe.field('notes')}
      />
      {fe.summary ? <Banner status="error" title="The customer was not created" description={fe.summary} /> : null}
    </DialogForm>
  );
}
