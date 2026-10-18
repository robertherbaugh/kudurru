// /my/orders/:id (self.read): an Order Form as the customer's people see
// it (never a draft): the state, the terms read-only, both acceptances and
// the document with its SHA-256, and Download. While it is sent, one of its
// authorized contacts accepts it (a FormDialog: full name, title and the
// authority checkbox; the SHA-256 shown is sent with it, so a form revised
// in the meantime answers 409 and the dialog asks for a reload) or declines
// it with a note for the licensor. Everyone else is told who may accept.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { MyOrderForm } from '@shared/api';
import { COMMERCIAL_AGREEMENT_URL, LICENSOR_NAME } from '@shared/catalog';
import { editionLabel } from '@shared/orderForms';
import { ApiError } from '../api/client';
import { useCan, useMe } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useMyOrderForm, useRouteMutation } from '../api/hooks';
import { OrderFormStatusBadge } from '../components/badges';
import { DocumentPanel, DownloadOrderFormButton, SignatureItem } from '../components/OrderFormParts';
import { TermsView } from '../components/OrderTerms';
import type { PageProps } from '../routes';
import { utcDateTime, utcDay } from '../lib/dates';
import { SECTION_TITLE, SECTIONS } from '../lib/orderForms';
import { nameError } from '../lib/validate';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { CheckboxField, TextAreaField, TextField } from '../ui/fields/TextField';

/** The agreement's name, as the authority statement and the links say it. */
const AGREEMENT = 'Kudurru Commercial Software License Agreement';

/** The statement an authorized contact ticks to accept. */
export function authorityStatement(legalName: string): string {
  return `I am authorized to bind ${legalName} and accept this Order Form under the ${AGREEMENT}`;
}

export default function MyOrderFormPage({ params }: PageProps) {
  const id = params.id ?? '';
  const can = useCan();
  const q = useMyOrderForm(id);
  const f = q.data;
  const [dialog, setDialog] = useState<'accept' | 'decline' | null>(null);
  const mayAnswer = f ? f.status === 'sent' && f.authorized && can('self.order') : false;
  const close = () => setDialog(null);
  return (
    <Page>
      <PageHeader
        back={{ href: '/my/licenses', label: 'My licenses' }}
        title={f ? `Order Form ${f.number}` : 'Order Form'}
        meta={
          f ? (
            <>
              <OrderFormStatusBadge status={f.status} />
              <Badge variant="neutral" label={`Version ${f.version}`} />
              <Text type="supporting">
                {f.customer.name} · {editionLabel(f.terms.edition)}
              </Text>
            </>
          ) : null
        }
        actions={
          f ? (
            <>
              <DownloadOrderFormButton source={f} />
              {mayAnswer ? (
                <>
                  <Button label="Decline" variant="secondary" onClick={() => setDialog('decline')} />
                  <Button label="Accept" variant="primary" onClick={() => setDialog('accept')} />
                </>
              ) : null}
            </>
          ) : null
        }
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {f ? (
        <>
          <StateBanner form={f} />
          <Text as="p">
            This Order Form is entered into under the{' '}
            <Link href={COMMERCIAL_AGREEMENT_URL} isExternalLink>
              {AGREEMENT}
            </Link>
            , which it incorporates. It becomes binding when both parties have accepted it.
          </Text>
          {SECTIONS.map((s) => (
            <Panel key={s} title={SECTION_TITLE[s]}>
              <TermsView section={s} terms={f.terms} />
            </Panel>
          ))}
          <Panel title="Acceptance">
            <MetadataList>
              <SignatureItem label={f.terms.customerLegalName || f.customer.name} signature={f.customerAcceptance} />
              <SignatureItem label={`Licensor (${LICENSOR_NAME})`} signature={f.licensorAcceptance} />
              {f.declined ? (
                <MetadataListItem label="Declined">
                  {utcDateTime(f.declined.at)}: {f.declined.note}
                </MetadataListItem>
              ) : null}
            </MetadataList>
          </Panel>
          <DocumentPanel source={f} />
          <FormDialog isOpen={dialog === 'accept'} onClose={close} title={`Accept Order Form ${f.number}`} subtitle={f.terms.customerLegalName} width={600}>
            <AcceptForm form={f} onClose={close} onReload={() => void q.refetch()} />
          </FormDialog>
          <FormDialog isOpen={dialog === 'decline'} onClose={close} title={`Decline Order Form ${f.number}`} subtitle={f.terms.customerLegalName} width={520}>
            <DeclineForm form={f} onClose={close} />
          </FormDialog>
        </>
      ) : null}
    </Page>
  );
}

function StateBanner({ form: f }: { form: MyOrderForm }) {
  switch (f.status) {
    case 'sent':
      return f.authorized ? (
        <Banner
          status="info"
          title="Waiting for your acceptance"
          description={`Read the terms below. As an authorized contact you can accept the form for ${f.terms.customerLegalName}, or decline it with a note for the licensor.`}
        />
      ) : (
        <Banner
          status="info"
          title="Only its authorized contacts can accept or decline it"
          description={`Sent on ${utcDay(f.sentAt)}. The authorized contacts are: ${f.terms.authorizedContacts.join(', ')}.`}
        />
      );
    case 'accepted':
      return <Banner status="info" title={`Accepted on ${utcDay(f.customerAcceptance?.at)}`} description={`It waits for the licensor's countersignature (${LICENSOR_NAME}).`} />;
    case 'executed':
      return (
        <Banner
          status="success"
          title={`Executed on ${utcDay(f.licensorAcceptance?.at)}`}
          description={f.licenseId ? 'Both parties accepted it. Its license is on your licenses page.' : 'Both parties accepted it. The license follows.'}
        />
      );
    case 'declined':
      return <Banner status="warning" title={`Declined on ${utcDay(f.declined?.at)}`} description="The licensor may send a revised version." />;
    case 'void':
      return <Banner status="warning" title={`Withdrawn by the licensor on ${utcDay(f.voidedAt)}`} description="This Order Form can no longer be accepted." />;
    case 'draft':
      return null;
  }
}

const ACCEPT_FIELDS = ['name', 'title', 'authorized', 'documentSha256'];

function AcceptForm({ form: f, onClose, onReload }: { form: MyOrderForm; onClose: () => void; onReload: () => void }) {
  const me = useMe();
  const notify = useNotify();
  const m = useRouteMutation('myOrderFormAccept');
  const conflict = m.error instanceof ApiError && m.error.status === 409;
  const fe = useFieldErrors(conflict ? null : m.error, ACCEPT_FIELDS);
  const [name, setName] = useState(me.user.name);
  const [title, setTitle] = useState('');
  const [authorized, setAuthorized] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const legalName = f.terms.customerLegalName;
  const nameErr = nameError(name, 'your full name');
  const titleErr = nameError(title, 'your title or role');
  const authErr = authorized ? null : 'Tick the statement to accept the Order Form';
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (nameErr || titleErr || authErr) return;
        m.mutate(
          { params: { id: f.id }, body: { name: name.trim(), title: title.trim(), documentSha256: f.documentSha256, authorized: true } },
          {
            onSuccess: () => {
              notify.success(`You accepted ${f.number}`);
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
          <Button type="submit" label="Accept Order Form" variant="primary" isLoading={m.isPending} isDisabled={conflict} />
        </>
      }
    >
      <Text as="p">
        You accept version {f.version} of Order Form {f.number} for {legalName}, as shown on this page, with your name, your title, your email address ({me.user.email ?? 'your sign-in'}) and
        the time recorded. The licensor then countersigns it.
      </Text>
      <VStack gap={0.5}>
        <Text type="supporting">Document SHA-256</Text>
        <Text type="code">{f.documentSha256}</Text>
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
        help="Your title or role at the customer, printed with the signature."
        error={(showErrors ? titleErr : null) ?? fe.field('title')}
      />
      <CheckboxField
        label={authorityStatement(legalName)}
        value={authorized}
        onChange={(v) => {
          setAuthorized(v);
          fe.edited('authorized');
        }}
        error={(showErrors ? authErr : null) ?? fe.field('authorized')}
      />
      <Text type="supporting" as="p">
        <Link href={COMMERCIAL_AGREEMENT_URL} isExternalLink>
          Read the {AGREEMENT}
        </Link>
      </Text>
      {conflict ? (
        <Banner
          status="error"
          title="This Order Form changed"
          description="It was revised or answered since you opened it, so it is not the text you read. Reload it and read it again before you accept."
          endContent={
            <Button
              label="Reload"
              size="sm"
              variant="secondary"
              onClick={() => {
                onReload();
                onClose();
              }}
            />
          }
        />
      ) : fe.summary ? (
        <Banner status="error" title="The Order Form was not accepted" description={fe.summary} />
      ) : null}
    </DialogForm>
  );
}

/** The longest decline note. */
const DECLINE_MAX = 2000;

function DeclineForm({ form: f, onClose }: { form: MyOrderForm; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('myOrderFormDecline');
  const fe = useFieldErrors(m.error, ['note']);
  const [note, setNote] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const err = note.trim() ? (note.length > DECLINE_MAX ? `At most ${DECLINE_MAX} characters` : null) : 'Write a note for the licensor';
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (err) return;
        m.mutate(
          { params: { id: f.id }, body: { note: note.trim() } },
          {
            onSuccess: () => {
              notify.success(`You declined ${f.number}`);
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
          <Button type="submit" label="Decline Order Form" variant="destructive" isLoading={m.isPending} />
        </>
      }
    >
      <TextAreaField
        label="Note to the licensor"
        value={note}
        onChange={(v) => {
          setNote(v);
          fe.edited('note');
        }}
        isRequired
        help="Say why, or what you would change. The licensor may send a revised version."
        error={(showErrors ? err : null) ?? fe.field('note')}
      />
      {fe.summary ? <Banner status="error" title="The Order Form was not declined" description={fe.summary} /> : null}
    </DialogForm>
  );
}
