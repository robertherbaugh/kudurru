// /orders/:id (orders.read): one Order Form for the licensor. The header
// with its number, state and version and the actions its state allows
// (draft: Send, Void; sent and declined: Revise, Void; accepted:
// Countersign, Void; executed without a license: Issue license; always:
// Download). A draft lists what Send still needs (the server's gaps) and is
// edited in four inline sections with their own Save (PATCH, only that
// section's terms); every other state shows them read-only. Then the
// acceptances (with where the customer's was given from), a decline note
// or a void reason, the document with its SHA-256, and the recent audit
// events.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { OrderForm } from '@shared/api';
import { LICENSOR_NAME } from '@shared/catalog';
import { editionLabel } from '@shared/orderForms';
import { ApiError } from '../api/client';
import { useCan, useMe } from '../api/context';
import { bannerError, MARKED, useFieldErrors } from '../api/fieldErrors';
import { useOrderForm, useRouteMutation } from '../api/hooks';
import { RecentAudit } from '../components/AuditTable';
import { OrderFormStatusBadge } from '../components/badges';
import { IssueLicenseWizardDialog } from '../components/IssueLicenseWizard';
import { DocumentPanel, DownloadOrderFormButton, SignatureItem } from '../components/OrderFormParts';
import { TermsFields, TermsView } from '../components/OrderTerms';
import type { PageProps } from '../routes';
import { utcDateTime, utcDay } from '../lib/dates';
import { patchDraft, SECTION_TITLE, sectionError, sectionTerms, SECTIONS, termLabel, TERMS_PATHS, termsToDraft, type TermsDraft, type TermsSection } from '../lib/orderForms';
import { nameError } from '../lib/validate';
import { EditableSection } from '../ui/components/EditableSection';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { TextAreaField, TextField } from '../ui/fields/TextField';

type Dialog = 'send' | 'revise' | 'void' | 'countersign' | 'issue';

export default function OrderFormPage({ params }: PageProps) {
  const id = params.id ?? '';
  const can = useCan();
  const q = useOrderForm(id);
  const f = q.data;
  const [dialog, setDialog] = useState<Dialog | null>(null);
  const write = can('orders.write');
  const close = () => setDialog(null);

  return (
    <Page>
      <PageHeader
        back={{ href: '/orders', label: 'Order Forms' }}
        title={f ? `Order Form ${f.number}` : 'Order Form'}
        meta={
          f ? (
            <>
              <OrderFormStatusBadge status={f.status} />
              <Badge variant="neutral" label={`Version ${f.version}`} />
              <Link href={`/customers/${encodeURIComponent(f.customer.id)}`}>{f.customer.name}</Link>
              <Text type="supporting">{editionLabel(f.edition)}</Text>
              {f.licenseId ? <Link href={`/licenses/${encodeURIComponent(f.licenseId)}`}>License {f.licenseId}</Link> : null}
            </>
          ) : null
        }
        actions={f ? <Actions form={f} write={write} canIssue={can('licenses.issue')} open={setDialog} /> : null}
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {f ? (
        <>
          <StateBanner form={f} />
          {SECTIONS.map((s) => (
            <TermsSectionPanel key={s} form={f} section={s} write={write} />
          ))}
          <AcceptancePanel form={f} />
          <DocumentPanel source={f} isDraft={f.status === 'draft'} />
          <RecentAudit targetType="order_form" targetId={f.id} />
          <SendDialog form={f} isOpen={dialog === 'send'} onClose={close} />
          <ReviseDialog form={f} isOpen={dialog === 'revise'} onClose={close} />
          <FormDialog isOpen={dialog === 'void'} onClose={close} title={`Void ${f.number}`} subtitle={f.customer.name} width={520}>
            <VoidForm form={f} onClose={close} />
          </FormDialog>
          <FormDialog isOpen={dialog === 'countersign'} onClose={close} title={`Countersign ${f.number}`} subtitle={f.customer.name} width={560}>
            <CountersignForm form={f} onClose={close} />
          </FormDialog>
          <IssueLicenseWizardDialog key={`issue-${f.id}`} isOpen={dialog === 'issue'} onClose={close} orderForm={f} />
        </>
      ) : null}
    </Page>
  );
}

function Actions({ form: f, write, canIssue, open }: { form: OrderForm; write: boolean; canIssue: boolean; open: (d: Dialog) => void }) {
  const gaps = f.gaps.length > 0;
  return (
    <>
      <DownloadOrderFormButton source={f} />
      {write && f.status === 'draft' ? (
        <Button label="Send" variant="primary" isDisabled={gaps} tooltip={gaps ? `Still needed: ${f.gaps.map(termLabel).join(', ')}` : undefined} onClick={() => open('send')} />
      ) : null}
      {write && (f.status === 'sent' || f.status === 'declined') ? <Button label="Revise" variant="primary" onClick={() => open('revise')} /> : null}
      {write && f.status === 'accepted' ? <Button label="Countersign" variant="primary" onClick={() => open('countersign')} /> : null}
      {f.status === 'executed' && !f.licenseId && canIssue ? <Button label="Issue license" variant="primary" onClick={() => open('issue')} /> : null}
      {write && f.status !== 'executed' && f.status !== 'void' ? <Button label="Void" variant="destructive" onClick={() => open('void')} /> : null}
    </>
  );
}

/** What the state means now, and what comes next. */
function StateBanner({ form: f }: { form: OrderForm }) {
  switch (f.status) {
    case 'draft':
      return f.gaps.length ? (
        <Banner status="warning" title="Before it can be sent" description="Fill in what is still missing:" collapsible={false}>
          <VStack gap={0.5} data-testid="order-form-gaps">
            {f.gaps.map((g) => (
              <Text key={g}>{termLabel(g)}</Text>
            ))}
          </VStack>
        </Banner>
      ) : (
        <Banner status="info" title="Ready to send" description="Sending freezes the text and mails each authorized contact a link to review and accept it." />
      );
    case 'sent':
      return (
        <Banner
          status="info"
          title={`Sent for acceptance on ${utcDay(f.sentAt)}${f.sentBy ? ` by ${f.sentBy.label}` : ''}`}
          description={`Version ${f.version} waits for one of its authorized contacts (${f.terms.authorizedContacts.join(', ')}) to accept or decline it in the portal. Revise it to change the text.`}
        />
      );
    case 'accepted':
      return (
        <Banner
          status="warning"
          title={`Accepted by ${f.customerAcceptance?.name ?? 'the customer'} on ${utcDay(f.customerAcceptance?.at)}`}
          description={`It waits for the licensor's countersignature (${LICENSOR_NAME}). Countersigning executes it.`}
        />
      );
    case 'executed':
      return (
        <Banner
          status="success"
          title={`Executed on ${utcDay(f.licensorAcceptance?.at)}`}
          description={f.licenseId ? 'Both parties accepted it, and a license was issued from it.' : 'Both parties accepted it. Issue the license it covers.'}
        />
      );
    case 'declined':
      return (
        <Banner status="error" title={`Declined on ${utcDay(f.declined?.at)}${f.declined ? ` by ${f.declined.by}` : ''}`} description="Revise it to send a new version, or void it." collapsible={false}>
          <Text as="p">Their note: {f.declined?.note || 'none'}</Text>
        </Banner>
      );
    case 'void':
      return (
        <Banner status="warning" title={`Void since ${utcDay(f.voided?.at)}${f.voided ? ` (${f.voided.by.label})` : ''}`} description="It can no longer be accepted, changed or licensed." collapsible={false}>
          <Text as="p">Reason (internal): {f.voided?.reason || 'none'}</Text>
        </Banner>
      );
  }
}

function TermsSectionPanel({ form: f, section, write }: { form: OrderForm; section: TermsSection; write: boolean }) {
  const update = useRouteMutation('orderFormUpdate');
  const fe = useFieldErrors(update.error, TERMS_PATHS);
  const readOnly = !write ? 'Needs orders.write' : f.status === 'draft' ? undefined : f.status === 'sent' || f.status === 'declined' ? 'Revise the form to change it' : 'Only a draft can be changed';
  return (
    <EditableSection<TermsDraft>
      title={SECTION_TITLE[section]}
      value={termsToDraft(f.terms)}
      readOnlyReason={readOnly}
      view={<TermsView section={section} terms={f.terms} />}
      edit={(d, set, showErrors) => (
        <TermsFields section={section} draft={d} onChange={(patch) => set(patchDraft(d, patch))} showErrors={showErrors} serverError={(p) => fe.field(p)} edited={fe.edited} />
      )}
      validate={(d) => sectionError(d, section)}
      describeError={(err) => bannerError(err, TERMS_PATHS) ?? MARKED}
      onSave={async (d) => {
        await update.mutateAsync({ params: { id: f.id }, body: { terms: sectionTerms(d, section) } });
      }}
    />
  );
}

function AcceptancePanel({ form: f }: { form: OrderForm }) {
  const c = f.customerAcceptance;
  const where = c ? [c.ip ? `from ${c.ip}` : null, c.userAgent ? `with ${c.userAgent}` : null].filter(Boolean).join(' ') : null;
  return (
    <Panel title="Acceptance" description="Each party accepts the document whose SHA-256 is shown below, signed in to the portal.">
      <MetadataList>
        <SignatureItem label="Customer" signature={c} extra={where ? `Given ${where}` : null} />
        <SignatureItem label={`Licensor (${LICENSOR_NAME})`} signature={f.licensorAcceptance} extra={f.licensorAcceptance ? `Countersigned by ${f.licensorAcceptance.by.label}` : null} />
        {f.declined ? (
          <MetadataListItem label="Declined">
            {utcDateTime(f.declined.at)} by {f.declined.by}: {f.declined.note}
          </MetadataListItem>
        ) : null}
        {f.voided ? (
          <MetadataListItem label="Voided">
            {utcDateTime(f.voided.at)} by {f.voided.by.label}: {f.voided.reason}
          </MetadataListItem>
        ) : null}
        <MetadataListItem label="Created">
          {utcDay(f.createdAt)} by {f.createdBy.label}
        </MetadataListItem>
        {f.businessRequestId ? (
          <MetadataListItem label="Business request">
            <Link href="/requests?tab=business">{f.businessRequestId}</Link>
          </MetadataListItem>
        ) : null}
      </MetadataList>
    </Panel>
  );
}

function SendDialog({ form: f, isOpen, onClose }: { form: OrderForm; isOpen: boolean; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('orderFormSend');
  const contacts = f.terms.authorizedContacts.join(', ');
  return (
    <AlertDialog
      isOpen={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={`Send ${f.number} for acceptance?`}
      description={`The text of version ${f.version} is frozen and its SHA-256 recorded; to change it later, revise it. Each authorized contact (${contacts}) becomes a member of ${f.customer.name} and gets a mail with a link to review and accept it.`}
      actionLabel="Send"
      actionVariant="primary"
      width={520}
      isActionLoading={m.isPending}
      onAction={() =>
        m.mutate(
          { params: { id: f.id } },
          {
            onSuccess: () => {
              notify.success(`Sent ${f.number} to ${contacts}`);
              onClose();
            },
            onError: (err) => {
              notify.error(err, 'The Order Form was not sent');
              onClose();
            },
          },
        )
      }
    />
  );
}

function ReviseDialog({ form: f, isOpen, onClose }: { form: OrderForm; isOpen: boolean; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('orderFormRevise');
  return (
    <AlertDialog
      isOpen={isOpen}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={`Revise ${f.number}?`}
      description={`It becomes a draft again as version ${f.version + 1}: version ${f.version} can no longer be accepted. Send it again once it is ready.`}
      actionLabel="Revise"
      actionVariant="primary"
      width={480}
      isActionLoading={m.isPending}
      onAction={() =>
        m.mutate(
          { params: { id: f.id } },
          {
            onSuccess: (d) => {
              notify.success(`${d.number} is a draft again (version ${d.version})`);
              onClose();
            },
            onError: (err) => {
              notify.error(err, 'The Order Form was not revised');
              onClose();
            },
          },
        )
      }
    />
  );
}

/** The longest void reason. */
const VOID_MAX = 1000;

function VoidForm({ form: f, onClose }: { form: OrderForm; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('orderFormVoid');
  const fe = useFieldErrors(m.error, ['reason']);
  const [reason, setReason] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const err = reason.trim() ? (reason.length > VOID_MAX ? `At most ${VOID_MAX} characters` : null) : 'Write why it is voided';
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (err) return;
        m.mutate(
          { params: { id: f.id }, body: { reason: reason.trim() } },
          {
            onSuccess: () => {
              notify.success(`Voided ${f.number}`);
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
          <Button type="submit" label="Void Order Form" variant="destructive" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">A void form can no longer be accepted, changed or licensed. This cannot be undone.</Text>
      <TextAreaField
        label="Reason"
        value={reason}
        onChange={(v) => {
          setReason(v);
          fe.edited('reason');
        }}
        isRequired
        help="Internal: the customer never sees it."
        error={(showErrors ? err : null) ?? fe.field('reason')}
      />
      {fe.summary ? <Banner status="error" title="The Order Form was not voided" description={fe.summary} /> : null}
    </DialogForm>
  );
}

const COUNTERSIGN_FIELDS = ['name', 'title', 'documentSha256'];

function CountersignForm({ form: f, onClose }: { form: OrderForm; onClose: () => void }) {
  const me = useMe();
  const notify = useNotify();
  const m = useRouteMutation('orderFormCountersign');
  const conflict = m.error instanceof ApiError && m.error.status === 409;
  const fe = useFieldErrors(conflict ? null : m.error, COUNTERSIGN_FIELDS);
  const [name, setName] = useState(me.user.name);
  const [title, setTitle] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const nameErr = nameError(name, 'your full name');
  const titleErr = nameError(title, 'your title or role');
  const sha = f.documentSha256 ?? '';
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (nameErr || titleErr || !sha) return;
        m.mutate(
          { params: { id: f.id }, body: { name: name.trim(), title: title.trim(), documentSha256: sha } },
          {
            onSuccess: () => {
              notify.success(`${f.number} is executed`);
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
          <Button type="submit" label="Countersign" variant="primary" isLoading={m.isPending} isDisabled={conflict} />
        </>
      }
    >
      <Text as="p">
        You accept version {f.version} of {f.number} for the licensor ({LICENSOR_NAME}), as {f.customerAcceptance?.name ?? 'the customer'} accepted it for {f.terms.customerLegalName}
        {f.customerAcceptance ? ` on ${utcDay(f.customerAcceptance.at)}` : ''}. The form is then executed, and its contacts are mailed the signed copy.
      </Text>
      <VStack gap={0.5}>
        <Text type="supporting">Document SHA-256</Text>
        <Text type="code">{sha || '–'}</Text>
      </VStack>
      <TextField
        label="Full name"
        value={name}
        onChange={(v) => {
          setName(v);
          fe.edited('name');
        }}
        isRequired
        error={(showErrors ? nameErr : null) ?? fe.field('name')}
      />
      <TextField
        label="Title"
        value={title}
        onChange={(v) => {
          setTitle(v);
          fe.edited('title');
        }}
        isRequired
        help="Your title or role, printed with the signature."
        error={(showErrors ? titleErr : null) ?? fe.field('title')}
      />
      {conflict ? (
        <Banner status="error" title="The Order Form changed" description="It is no longer the text the customer accepted, or no longer waits for a countersignature. Close this dialog and reload the page." />
      ) : fe.summary ? (
        <Banner status="error" title="The Order Form was not countersigned" description={fe.summary} />
      ) : null}
    </DialogForm>
  );
}
