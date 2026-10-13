// "Request a free personal key" in the portal (POST /api/v1/me/requests):
// a FormDialog with the name printed on the key, what kudurru runs for,
// and the certification (catalog ATTESTATION_STATEMENT, word for word as
// the kudurru.io form shows it) that must be ticked. The request needs an
// email address the identity provider verified; without one the dialog
// says so and points at the public form. The answer is shown in the same
// dialog: usually the request waits for a license admin's approval and
// the key follows by mail; a current key that is not due is mailed again;
// with automatic approval (Catalog.approvalRequired false) a key was
// issued or renewed.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { Text } from '@astryxdesign/core/Text';
import { useState, type ReactNode } from 'react';
import type { PersonalRequestResult } from '@shared/api';
import { ATTESTATION_STATEMENT, LICENSE_PAGE_URL, NONCOMMERCIAL_LICENSE_URL, PERSONAL_MAX_NODES, PERSONAL_TERM_DAYS } from '@shared/catalog';
import { ApiError } from '../api/client';
import { useMe } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useRouteMutation } from '../api/hooks';
import { outcomeWords } from '../lib/words';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { CheckboxField, TextAreaField, TextField } from '../ui/fields/TextField';

const NAME_MAX = 200;
const USE_MAX = 1000;

/** The form's errors (the server repeats every check). */
export function personalRequestProblems(name: string, use: string, certified: boolean): Record<string, string> {
  const e: Record<string, string> = {};
  if (!name.trim()) e.name = 'Enter the name to print on the key';
  else if (name.length > NAME_MAX) e.name = `At most ${NAME_MAX} characters`;
  if (!use.trim()) e.use = 'Say what you run kudurru for';
  else if (use.length > USE_MAX) e.use = `At most ${USE_MAX} characters`;
  if (!certified) e.certified = 'Tick the certification to request a free personal key';
  return e;
}

/** The body fields the controls show (a 400 names them inline). */
const PERSONAL_FIELDS = ['name', 'use', 'noncommercial'];

export function PersonalRequestDialog({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Request a free personal key" width={560}>
      <PersonalRequestForm onClose={onClose} />
    </FormDialog>
  );
}

function PersonalRequestForm({ onClose }: { onClose: () => void }) {
  const me = useMe();
  const catalog = useCatalog();
  const [name, setName] = useState(me.user.name);
  const [use, setUse] = useState('');
  const [certified, setCertified] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const m = useRouteMutation('myRequestCreate');
  const serverError = m.error instanceof ApiError && m.error.key === 'email_unverified' ? null : m.error;
  const fe = useFieldErrors(serverError, PERSONAL_FIELDS);
  const verified = me.user.emailVerified && Boolean(me.user.email);

  const submit = () => {
    const e = personalRequestProblems(name, use, certified);
    setErrors(e);
    if (Object.keys(e).length) return;
    m.mutate({ body: { name: name.trim(), use: use.trim(), noncommercial: true } });
  };

  if (m.data) return <Outcome result={m.data} email={me.user.email} onClose={onClose} />;

  return (
    <DialogBody onSubmit={submit} footer={
      <>
        <Button label="Cancel" variant="ghost" onClick={onClose} />
        <Button type="submit" label="Request key" variant="primary" isLoading={m.isPending} isDisabled={!verified} tooltip={verified ? undefined : 'Needs a verified email address'} />
      </>
    }>
      <Text as="p">
        A free personal key covers up to {PERSONAL_MAX_NODES} nodes and every feature for {PERSONAL_TERM_DAYS} days, for non-commercial use: a home, a homelab, study, a charity or a school.{' '}
        {catalog.data?.approvalRequired === false ? 'We mail it' : 'A license admin approves each request; then we mail the key'} to {me.user.email ?? 'your verified address'} and show it here.
      </Text>
      {!verified || (m.error instanceof ApiError && m.error.key === 'email_unverified') ? (
        <Banner
          status="warning"
          title="Your sign-in did not share a verified email address"
          description="The portal mails keys only to an address your identity provider verified. Ask its administrator, or use the public form at kudurru.io/license."
          endContent={
            <Link href={LICENSE_PAGE_URL} isExternalLink>
              Public form
            </Link>
          }
        />
      ) : null}
      <TextField
        label="Name on the key"
        value={name}
        onChange={(v) => {
          setName(v);
          fe.edited('name');
        }}
        isRequired
        help="Printed on the key as the licensee: your name, or your charity's or school's."
        error={errors.name ?? fe.field('name')}
      />
      <TextAreaField
        label="What do you run kudurru for?"
        value={use}
        onChange={(v) => {
          setUse(v);
          fe.edited('use');
        }}
        isRequired
        help="A sentence is enough, for example: DNS for my home network."
        error={errors.use ?? fe.field('use')}
      />
      <CheckboxField
        label={ATTESTATION_STATEMENT}
        value={certified}
        onChange={(v) => {
          setCertified(v);
          fe.edited('noncommercial');
        }}
        error={errors.certified ?? fe.field('noncommercial')}
      />
      <Text type="supporting" as="p">
        <Link href={NONCOMMERCIAL_LICENSE_URL} isExternalLink>
          Read the PolyForm Noncommercial License 1.0.0
        </Link>
      </Text>
      {fe.summary ? <Banner status="error" title="The request was not sent" description={fe.summary} /> : null}
    </DialogBody>
  );
}

function Outcome({ result, email, onClose }: { result: PersonalRequestResult; email: string | null; onClose: () => void }) {
  const w = outcomeWords(result.outcome, email, 'portal');
  return (
    <DialogBody
      footer={
        <>
          {result.licenseId ? <Button label="Open the license" variant="secondary" href={`/my/licenses/${encodeURIComponent(result.licenseId)}`} onClick={onClose} /> : null}
          <Button label="Done" variant="primary" onClick={onClose} />
        </>
      }
    >
      <Banner status={result.outcome === 'pending_review' ? 'info' : 'success'} title={w.title} description={w.body} />
    </DialogBody>
  );
}

/** The dialog body with its pinned footer (FormDialog's DialogForm). */
function DialogBody({ onSubmit, footer, children }: { onSubmit?: () => void; footer: ReactNode; children: ReactNode }) {
  return (
    <DialogForm onSubmit={onSubmit} noValidate footer={footer} gap={3}>
      {children}
    </DialogForm>
  );
}
